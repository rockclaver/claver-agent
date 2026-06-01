package billing

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rockclaver/claver-agent/internal/store"
)

// Errors surfaced to the server layer.
var (
	ErrBadProvider = errors.New("unknown billing provider")
	ErrNoKey       = errors.New("api key required")
	ErrNotFound    = store.ErrNotFound
)

// statusOK / statusUnavailable are the two infra_cost.status values. The UI
// degrades to "unavailable" (rather than a blank panel) whenever a provider API
// could not be reached or returned an error.
const (
	statusOK          = "ok"
	statusUnavailable = "unavailable"
)

// HTTPDoer is the subset of *http.Client the manager needs. Tests inject a fake
// that returns recorded fixtures.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Manager owns provider credentials and the daily billing fetch for one server.
type Manager struct {
	Store    *store.Store
	Vault    *Vault
	ServerID string
	HTTP     HTTPDoer
	Now      func() time.Time
	Logf     func(string, ...any)
}

// CredentialInfo is the non-secret description of a stored credential. The API
// key itself is never returned once sealed.
type CredentialInfo struct {
	Provider  string
	UpdatedAt time.Time
}

// New constructs a Manager. ServerID labels the cost rows this agent owns
// ("local" when unset); HTTP defaults to a client with a sane timeout.
func New(st *store.Store, vault *Vault, serverID string) *Manager {
	if serverID == "" {
		serverID = "local"
	}
	return &Manager{
		Store:    st,
		Vault:    vault,
		ServerID: serverID,
		HTTP:     &http.Client{Timeout: 20 * time.Second},
		Now:      time.Now,
		Logf:     func(string, ...any) {},
	}
}

// SetCredential seals and stores the billing API key for a provider.
func (m *Manager) SetCredential(provider, apiKey string) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if !SupportedProvider(provider) {
		return ErrBadProvider
	}
	if strings.TrimSpace(apiKey) == "" {
		return ErrNoKey
	}
	ciphertext, nonce, err := m.Vault.Seal(provider, apiKey)
	if err != nil {
		return err
	}
	return m.Store.PutProviderCredential(store.ProviderCredential{
		ServerID:   m.ServerID,
		Provider:   provider,
		Ciphertext: ciphertext,
		Nonce:      nonce,
	})
}

// DeleteCredential removes a stored credential.
func (m *Manager) DeleteCredential(provider string) error {
	return m.Store.DeleteProviderCredential(m.ServerID, strings.ToLower(strings.TrimSpace(provider)))
}

// ListCredentials returns the providers this server has a credential for,
// without exposing any key material.
func (m *Manager) ListCredentials() ([]CredentialInfo, error) {
	rows, err := m.Store.ListProviderCredentials(m.ServerID)
	if err != nil {
		return nil, err
	}
	out := make([]CredentialInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, CredentialInfo{Provider: r.Provider, UpdatedAt: r.UpdatedAt})
	}
	return out, nil
}

// Month returns the current billing month as "2006-01" in UTC.
func (m *Manager) Month() string {
	return m.Now().UTC().Format("2006-01")
}

// FetchAll pulls billing for every stored credential and persists the result.
// Each provider is independent: one failing API yields an "unavailable" row and
// does not abort the others. The persisted rows are returned for the caller.
func (m *Manager) FetchAll(ctx context.Context) ([]store.InfraCost, error) {
	creds, err := m.Store.ListProviderCredentials(m.ServerID)
	if err != nil {
		return nil, err
	}
	month := m.Month()
	out := make([]store.InfraCost, 0, len(creds))
	for _, cred := range creds {
		cost := m.fetchOne(ctx, cred, month)
		if err := m.Store.PutInfraCost(cost); err != nil {
			m.Logf("billing: persist %s cost: %v", cred.Provider, err)
			continue
		}
		out = append(out, cost)
	}
	return out, nil
}

// fetchOne resolves one credential to a normalized cost row, degrading to an
// "unavailable" row on any decrypt / network / parse failure.
func (m *Manager) fetchOne(ctx context.Context, cred store.ProviderCredential, month string) store.InfraCost {
	base := store.InfraCost{
		ServerID:  m.ServerID,
		Provider:  cred.Provider,
		Month:     month,
		FetchedAt: m.Now(),
	}
	unavailable := func(reason string) store.InfraCost {
		c := base
		c.Status = statusUnavailable
		c.Detail = reason
		c.Currency = "USD"
		return c
	}

	provider := providerByName(cred.Provider)
	if provider == nil {
		return unavailable("unsupported provider")
	}
	apiKey, err := m.Vault.Open(cred.Provider, cred.Ciphertext, cred.Nonce)
	if err != nil {
		m.Logf("billing: open %s credential: %v", cred.Provider, err)
		return unavailable("credential could not be decrypted")
	}
	amount, currency, err := m.fetch(ctx, provider, apiKey)
	if err != nil {
		m.Logf("billing: fetch %s: %v", cred.Provider, err)
		return unavailable(err.Error())
	}
	c := base
	c.Status = statusOK
	c.AmountCents = amount
	c.Currency = currency
	return c
}

// fetch performs the provider HTTP call and parses the body.
func (m *Manager) fetch(ctx context.Context, p Provider, apiKey string) (int64, string, error) {
	req, err := p.Request(ctx, apiKey)
	if err != nil {
		return 0, "", err
	}
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, "", fmt.Errorf("%s API returned HTTP %d", p.Name(), resp.StatusCode)
	}
	return p.Parse(body)
}

// StartDaily runs FetchAll immediately, then once every 24h until ctx is
// cancelled. It returns a cleanup func that stops the loop. Failures are logged
// and retried on the next tick — a flaky provider never wedges the schedule.
func (m *Manager) StartDaily(ctx context.Context) func() {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		// A short initial delay lets the rest of the agent finish booting
		// before the first network call.
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				m.runOnce(ctx)
			case <-ticker.C:
				m.runOnce(ctx)
			}
		}
	}()
	return cancel
}

func (m *Manager) runOnce(ctx context.Context) {
	if _, err := m.FetchAll(ctx); err != nil {
		m.Logf("billing: daily fetch: %v", err)
	}
}
