// Package push delivers agent-side notifications to registered mobile devices
// via Firebase Cloud Messaging's HTTP v1 API.
//
// We deliberately do NOT take a dependency on firebase.google.com/go/v4: the
// admin SDK pulls in dozens of indirect Google-Cloud packages we do not
// otherwise use. FCM HTTP v1 only needs (a) a JWT signed with the service
// account's RSA private key, (b) an OAuth2 token exchange, (c) an HTTPS POST.
// All three are stdlib (or close to it), and the resulting blast radius for
// future supply-chain audits is tiny.
//
// The package surfaces three things:
//   - LoadServiceAccount: parse the on-disk JSON the user provisioned.
//   - Client.Send: deliver one Message to one device token.
//   - Hub: subscribes to notifications.Hub, fans selected notification kinds
//     out to every device registered in the store.
package push

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rockclaver/claver/agent/internal/notifications"
	"github.com/rockclaver/claver/agent/internal/store"
)

// fcmAPIURL is the FCM HTTP v1 send endpoint template. The {project} segment
// is filled from the service account's project_id.
const fcmAPIURL = "https://fcm.googleapis.com/v1/projects/%s/messages:send"

// fcmScope is the OAuth2 scope FCM requires for messages:send.
const fcmScope = "https://www.googleapis.com/auth/firebase.messaging"

// tokenURL is the Google OAuth2 token endpoint. Pulled from the service
// account JSON in production, but defaulted here so older keys (which
// sometimes omit the field) still work.
const defaultTokenURL = "https://oauth2.googleapis.com/token"

// ServiceAccount is the parsed shape of a Google service-account JSON key.
// Only the fields we actually use are declared so a key with future fields
// still parses cleanly.
type ServiceAccount struct {
	Type        string `json:"type"`
	ProjectID   string `json:"project_id"`
	PrivateKey  string `json:"private_key"`
	ClientEmail string `json:"client_email"`
	TokenURI    string `json:"token_uri,omitempty"`

	parsedKey *rsa.PrivateKey
}

// LoadServiceAccount reads and parses a service-account JSON file.
func LoadServiceAccount(path string) (*ServiceAccount, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read service account: %w", err)
	}
	return ParseServiceAccount(b)
}

// ParseServiceAccount parses raw service-account JSON bytes.
func ParseServiceAccount(b []byte) (*ServiceAccount, error) {
	var sa ServiceAccount
	if err := json.Unmarshal(b, &sa); err != nil {
		return nil, fmt.Errorf("parse service account JSON: %w", err)
	}
	if sa.ProjectID == "" || sa.ClientEmail == "" || sa.PrivateKey == "" {
		return nil, errors.New("service account missing project_id/client_email/private_key")
	}
	key, err := parseRSAPrivateKey(sa.PrivateKey)
	if err != nil {
		return nil, err
	}
	sa.parsedKey = key
	if sa.TokenURI == "" {
		sa.TokenURI = defaultTokenURL
	}
	return &sa, nil
}

func parseRSAPrivateKey(pem string) (*rsa.PrivateKey, error) {
	block, _ := decodePEM([]byte(pem))
	if block == nil {
		return nil, errors.New("private_key is not PEM")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	rsaKey, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key is %T, want RSA", parsed)
	}
	return rsaKey, nil
}

// decodePEM wraps encoding/pem.Decode so we can keep the import list tight.
func decodePEM(in []byte) (*pem.Block, []byte) { return pem.Decode(in) }

// Message is the minimal FCM message shape we send. We include both the
// `notification` block (so the OS draws the system banner / wakes the screen)
// AND a `data` payload (so the foreground client can deep-link without
// re-fetching the body). Apple and Android both deliver `data` to the app.
type Message struct {
	Token string            // FCM device token (required)
	Title string            // notification.title
	Body  string            // notification.body
	Data  map[string]string // arbitrary key/value pairs (deep_link, runbook_id, ...)
}

// HTTPDoer is satisfied by *http.Client and any test stub.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Client sends FCM messages. Construct with NewClient. Safe for concurrent use.
type Client struct {
	sa     *ServiceAccount
	http   HTTPDoer
	now    func() time.Time
	tokMu  sync.Mutex
	token  string
	tokExp time.Time
}

// NewClient constructs a Client. http and now default sensibly.
func NewClient(sa *ServiceAccount, httpClient HTTPDoer) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{sa: sa, http: httpClient, now: time.Now}
}

// Send delivers one message. Returns an error containing the FCM error code
// so the caller can prune UNREGISTERED tokens from the device registry.
func (c *Client) Send(ctx context.Context, m Message) error {
	if m.Token == "" {
		return errors.New("push: empty device token")
	}
	tok, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	body, err := buildSendPayload(m)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf(fcmAPIURL, c.sa.ProjectID), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("fcm send: %w", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return &SendError{HTTPStatus: resp.StatusCode, Body: string(rb)}
	}
	return nil
}

// SendError wraps a non-2xx FCM response so the caller can detect 404 /
// UNREGISTERED and prune the dead token from the registry.
type SendError struct {
	HTTPStatus int
	Body       string
}

func (e *SendError) Error() string {
	return fmt.Sprintf("fcm send: status=%d body=%s", e.HTTPStatus, truncate(e.Body, 256))
}

// IsUnregistered reports whether err indicates the device token is no longer
// valid (uninstalled / token rotated). The caller should drop the token.
func IsUnregistered(err error) bool {
	var se *SendError
	if !errors.As(err, &se) {
		return false
	}
	if se.HTTPStatus == http.StatusNotFound {
		return true
	}
	// FCM v1 returns 404 with error.status="NOT_FOUND" and
	// errorCode="UNREGISTERED" for stale tokens; the string match is a
	// belt-and-braces fallback for the rare 400 phrasings.
	low := strings.ToLower(se.Body)
	return strings.Contains(low, "unregistered") || strings.Contains(low, "not_found")
}

func buildSendPayload(m Message) ([]byte, error) {
	msg := map[string]any{
		"token": m.Token,
		"notification": map[string]any{
			"title": m.Title,
			"body":  m.Body,
		},
	}
	if len(m.Data) > 0 {
		msg["data"] = m.Data
	}
	return json.Marshal(map[string]any{"message": msg})
}

// accessToken returns a cached OAuth2 access token, minting a new one when
// the cached one has fewer than 60 seconds remaining.
func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.tokMu.Lock()
	defer c.tokMu.Unlock()
	if c.token != "" && c.now().Add(60*time.Second).Before(c.tokExp) {
		return c.token, nil
	}
	now := c.now()
	jwt, err := buildJWT(c.sa, now)
	if err != nil {
		return "", err
	}
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
	form.Set("assertion", jwt)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.sa.TokenURI,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("oauth2 token exchange: %w", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("oauth2 token exchange: status=%d body=%s",
			resp.StatusCode, truncate(string(rb), 256))
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(rb, &tr); err != nil {
		return "", fmt.Errorf("oauth2 token parse: %w", err)
	}
	if tr.AccessToken == "" {
		return "", errors.New("oauth2 token exchange: empty access_token")
	}
	c.token = tr.AccessToken
	c.tokExp = now.Add(time.Duration(tr.ExpiresIn) * time.Second)
	return c.token, nil
}

// buildJWT signs a service-account assertion suitable for the OAuth2
// jwt-bearer grant. Hand-rolled because importing a JWT library for one
// 30-line function would be overkill.
func buildJWT(sa *ServiceAccount, now time.Time) (string, error) {
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	claims := map[string]any{
		"iss":   sa.ClientEmail,
		"scope": fcmScope,
		"aud":   sa.TokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}
	hb, _ := json.Marshal(header)
	cb, _ := json.Marshal(claims)
	enc := base64.RawURLEncoding
	signingInput := enc.EncodeToString(hb) + "." + enc.EncodeToString(cb)
	hash := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, sa.parsedKey, crypto.SHA256, hash[:])
	if err != nil {
		return "", fmt.Errorf("sign jwt: %w", err)
	}
	return signingInput + "." + enc.EncodeToString(sig), nil
}

// Sender is the narrow surface Hub needs. Decoupled so tests can stub.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// Hub wires the agent's notifications.Hub to an FCM Sender + the device
// registry in store. On every selected notification it queries the registry
// once and fans the message out to every device, pruning tokens that FCM
// rejects as unregistered.
//
// Selection is opt-in by notification Type: we only push high-signal events
// (alerts, runbooks) and not chatty things like session lifecycle ticks.
type Hub struct {
	Sender Sender
	Store  *store.Store
	// Types lists the notification types to forward. Empty -> no forwarding.
	Types map[string]bool
	// Logf, when non-nil, receives one-line operational messages (token
	// pruned, send error). Defaults to a no-op so tests stay quiet.
	Logf func(format string, args ...any)
}

// Forward pushes one notification to every registered device.
func (h *Hub) Forward(ctx context.Context, n notifications.Notification) {
	if h == nil || h.Sender == nil || h.Store == nil {
		return
	}
	if !h.Types[n.Type] {
		return
	}
	devices, err := h.Store.ListPushDevices()
	if err != nil {
		h.log("push: list devices: %v", err)
		return
	}
	data := flattenData(n)
	for _, d := range devices {
		err := h.Sender.Send(ctx, Message{
			Token: d.Token,
			Title: n.Title,
			Body:  n.Body,
			Data:  data,
		})
		if err == nil {
			continue
		}
		if IsUnregistered(err) {
			if delErr := h.Store.DeletePushDevice(d.Token); delErr != nil {
				h.log("push: prune token: %v", delErr)
			}
			continue
		}
		h.log("push: send %s...: %v", truncate(d.Token, 12), err)
	}
}

// Subscribe wires Forward onto the given notification hub. Returns a cleanup
// that unsubscribes. Returns nil cleanup when hub or h are nil.
func (h *Hub) Subscribe(ctx context.Context, nh *notifications.Hub) func() {
	if h == nil || nh == nil {
		return func() {}
	}
	return nh.Subscribe(func(n notifications.Notification) {
		h.Forward(ctx, n)
	})
}

func (h *Hub) log(format string, args ...any) {
	if h.Logf != nil {
		h.Logf(format, args...)
	}
}

// flattenData reshapes the notification's typed Data map into the FCM
// data-payload shape (string -> string). Nested values are JSON-encoded so
// the client can decode them back to structured payloads when it needs them
// (e.g. proposal_ids).
func flattenData(n notifications.Notification) map[string]string {
	out := map[string]string{
		"type":     n.Type,
		"notif_id": n.ID,
	}
	for k, v := range n.Data {
		switch s := v.(type) {
		case string:
			out[k] = s
		case nil:
			// drop
		default:
			b, err := json.Marshal(v)
			if err == nil {
				out[k] = string(b)
			}
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
