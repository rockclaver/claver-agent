package push

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rockclaver/claver/agent/internal/notifications"
	"github.com/rockclaver/claver/agent/internal/store"
)

// testServiceAccount builds an in-memory, validly-signed service account so
// tests exercise the real RSA sign + JWT encode path without needing a real
// GCP key on disk.
func testServiceAccount(t *testing.T) *ServiceAccount {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
	saJSON, _ := json.Marshal(map[string]string{
		"type":         "service_account",
		"project_id":   "claver-test",
		"private_key":  string(pemBytes),
		"client_email": "fcm@claver-test.iam.gserviceaccount.com",
		"token_uri":    "https://example.invalid/token",
	})
	sa, err := ParseServiceAccount(saJSON)
	if err != nil {
		t.Fatal(err)
	}
	return sa
}

// fakeHTTP records requests and replies with a script of canned responses.
type fakeHTTP struct {
	mu        sync.Mutex
	requests  []*http.Request
	reqBody   []string
	script    []*http.Response
	scriptErr []error
	i         int
}

func (f *fakeHTTP) Do(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body := ""
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}
	f.requests = append(f.requests, r)
	f.reqBody = append(f.reqBody, body)
	if f.i >= len(f.script) {
		return nil, errors.New("fakeHTTP: no more scripted responses")
	}
	resp, err := f.script[f.i], f.scriptErr[f.i]
	f.i++
	return resp, err
}

func (f *fakeHTTP) push(status int, body string) {
	f.script = append(f.script, &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{},
	})
	f.scriptErr = append(f.scriptErr, nil)
}

func TestLoadServiceAccount_RejectsMissingFields(t *testing.T) {
	if _, err := ParseServiceAccount([]byte(`{"project_id":"x"}`)); err == nil {
		t.Fatal("expected error")
	}
}

func TestClient_SendMintsTokenThenPosts(t *testing.T) {
	sa := testServiceAccount(t)
	hc := &fakeHTTP{}
	hc.push(200, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	hc.push(200, `{"name":"projects/claver-test/messages/123"}`)
	c := NewClient(sa, hc)
	c.now = func() time.Time { return time.Unix(1700000000, 0) }

	err := c.Send(context.Background(), Message{
		Token: "device-1", Title: "AI runbook", Body: "restart x",
		Data: map[string]string{"runbook_id": "rb1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hc.requests) != 2 {
		t.Fatalf("requests=%d, want 2 (token then send)", len(hc.requests))
	}
	// First request: token exchange.
	if !strings.Contains(hc.requests[0].URL.String(), "token") {
		t.Fatalf("first request URL=%s", hc.requests[0].URL)
	}
	if !strings.Contains(hc.reqBody[0], "grant_type=urn%3Aietf%3Aparams%3Aoauth%3Agrant-type%3Ajwt-bearer") {
		t.Fatalf("missing jwt-bearer grant: %s", hc.reqBody[0])
	}
	// Second request: send.
	if !strings.Contains(hc.requests[1].URL.String(), "projects/claver-test/messages:send") {
		t.Fatalf("second request URL=%s", hc.requests[1].URL)
	}
	if hc.requests[1].Header.Get("Authorization") != "Bearer tok" {
		t.Fatal("missing bearer token")
	}
	if !strings.Contains(hc.reqBody[1], `"runbook_id":"rb1"`) {
		t.Fatalf("data payload missing: %s", hc.reqBody[1])
	}
}

func TestClient_TokenCachedAcrossSends(t *testing.T) {
	sa := testServiceAccount(t)
	hc := &fakeHTTP{}
	hc.push(200, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	hc.push(200, `{}`)
	hc.push(200, `{}`)
	c := NewClient(sa, hc)
	c.now = func() time.Time { return time.Unix(1700000000, 0) }
	for range 2 {
		if err := c.Send(context.Background(), Message{Token: "d"}); err != nil {
			t.Fatal(err)
		}
	}
	if len(hc.requests) != 3 {
		t.Fatalf("requests=%d, want 1 token + 2 sends", len(hc.requests))
	}
}

func TestIsUnregistered_DetectsFCMUnregistered(t *testing.T) {
	if !IsUnregistered(&SendError{HTTPStatus: 404, Body: `{"error":{"status":"NOT_FOUND"}}`}) {
		t.Fatal("404 should be unregistered")
	}
	if !IsUnregistered(&SendError{HTTPStatus: 400, Body: `errorCode: UNREGISTERED`}) {
		t.Fatal("UNREGISTERED body should be unregistered")
	}
	if IsUnregistered(&SendError{HTTPStatus: 500, Body: "internal"}) {
		t.Fatal("500 is not unregistered")
	}
	if IsUnregistered(errors.New("non-send err")) {
		t.Fatal("non-SendError must not be unregistered")
	}
}

// stubSender records every Send and lets the test trigger errors.
type stubSender struct {
	mu     sync.Mutex
	sent   []Message
	errFor map[string]error
}

func (s *stubSender) Send(_ context.Context, m Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, m)
	if e := s.errFor[m.Token]; e != nil {
		return e
	}
	return nil
}

func TestHub_ForwardSelectedTypesAndPrunesUnregistered(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	for _, tok := range []string{"good-1", "good-2", "dead-3"} {
		if err := st.PutPushDevice(store.PushDevice{Token: tok, Platform: "ios"}); err != nil {
			t.Fatal(err)
		}
	}

	sender := &stubSender{errFor: map[string]error{
		"dead-3": &SendError{HTTPStatus: 404, Body: `UNREGISTERED`},
	}}
	hub := &Hub{
		Sender: sender,
		Store:  st,
		Types:  map[string]bool{"infra.runbook": true},
	}

	// Wrong type: ignored.
	hub.Forward(context.Background(), notifications.Notification{Type: "infra.alert"})
	if len(sender.sent) != 0 {
		t.Fatalf("non-selected type forwarded: %+v", sender.sent)
	}

	// Selected type: fan-out + prune.
	hub.Forward(context.Background(), notifications.Notification{
		ID: "n1", Type: "infra.runbook", Title: "AI runbook", Body: "x",
		Data: map[string]any{"runbook_id": "rb1", "step_count": 3},
	})
	if len(sender.sent) != 3 {
		t.Fatalf("sent=%d want 3", len(sender.sent))
	}
	for _, m := range sender.sent {
		if m.Data["runbook_id"] != "rb1" {
			t.Fatalf("data missing runbook_id: %+v", m.Data)
		}
		if m.Data["step_count"] != "3" {
			t.Fatalf("nested int not JSON-encoded: %+v", m.Data)
		}
		if m.Data["type"] != "infra.runbook" || m.Data["notif_id"] != "n1" {
			t.Fatalf("envelope keys missing: %+v", m.Data)
		}
	}

	devs, _ := st.ListPushDevices()
	if len(devs) != 2 {
		t.Fatalf("dead token not pruned: %+v", devs)
	}
	for _, d := range devs {
		if d.Token == "dead-3" {
			t.Fatal("dead-3 should be gone")
		}
	}
}

func TestHub_SubscribeBridgesNotificationHub(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.PutPushDevice(store.PushDevice{Token: "d1"})

	sender := &stubSender{}
	hub := &Hub{Sender: sender, Store: st, Types: map[string]bool{"infra.runbook": true}}
	nh := notifications.NewHub()
	cleanup := hub.Subscribe(context.Background(), nh)
	defer cleanup()

	_ = nh.Publish(context.Background(), notifications.Notification{
		Type: "infra.runbook", Title: "T", Body: "B",
	})
	if len(sender.sent) != 1 || sender.sent[0].Title != "T" {
		t.Fatalf("subscribe bridge missed message: %+v", sender.sent)
	}
}
