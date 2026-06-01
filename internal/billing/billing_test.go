package billing

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rockclaver/claver-agent/internal/store"
)

func newVault(t *testing.T) *Vault {
	t.Helper()
	return NewVault(filepath.Join(t.TempDir(), "billing.key"))
}

func TestVault_SealOpenRoundTrip(t *testing.T) {
	v := newVault(t)
	ct, nonce, err := v.Seal(ProviderVultr, "secret-key-123")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if bytes.Contains(ct, []byte("secret-key-123")) {
		t.Fatal("ciphertext leaks plaintext")
	}
	got, err := v.Open(ProviderVultr, ct, nonce)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got != "secret-key-123" {
		t.Fatalf("roundtrip mismatch: %q", got)
	}
}

func TestVault_WrongProviderFailsAuth(t *testing.T) {
	v := newVault(t)
	ct, nonce, err := v.Seal(ProviderVultr, "secret")
	if err != nil {
		t.Fatal(err)
	}
	// Provider is bound as additional authenticated data, so opening under a
	// different provider must fail rather than silently returning garbage.
	if _, err := v.Open(ProviderLinode, ct, nonce); err == nil {
		t.Fatal("expected AAD mismatch to fail")
	}
}

// Recorded-fixture parser tests (AC: "provider-billing fetchers with recorded
// fixtures"). Each fixture is a captured-shape response body.
func TestProviderParsers_Fixtures(t *testing.T) {
	cases := []struct {
		provider string
		fixture  string
		wantCost int64
		wantCcy  string
	}{
		{ProviderDigitalOcean, "digitalocean_balance.json", 2344, "USD"},
		{ProviderVultr, "vultr_account.json", 567, "USD"},
		{ProviderLinode, "linode_account.json", 1250, "USD"},
		// 4.51 + 8.09 EUR = 12.60 → 1260 cents.
		{ProviderHetzner, "hetzner_servers.json", 1260, "EUR"},
	}
	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			body := readFixture(t, tc.fixture)
			p := providerByName(tc.provider)
			if p == nil {
				t.Fatalf("no provider %q", tc.provider)
			}
			cents, ccy, err := p.Parse(body)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if cents != tc.wantCost {
				t.Fatalf("cents = %d, want %d", cents, tc.wantCost)
			}
			if ccy != tc.wantCcy {
				t.Fatalf("currency = %s, want %s", ccy, tc.wantCcy)
			}
		})
	}
}

func TestToCents_ClampsCredits(t *testing.T) {
	if got := toCents(-12.34); got != 0 {
		t.Fatalf("negative balance should clamp to 0, got %d", got)
	}
	if got := toCents(1.006); got != 101 {
		t.Fatalf("rounding: got %d want 101", got)
	}
}

// stubDoer returns a canned response (or error) per request, keyed by host.
type stubDoer struct {
	byHost map[string]stubResp
}

type stubResp struct {
	status int
	body   []byte
	err    error
}

func (s stubDoer) Do(req *http.Request) (*http.Response, error) {
	r, ok := s.byHost[req.URL.Host]
	if !ok {
		return nil, errors.New("no stub for " + req.URL.Host)
	}
	if r.err != nil {
		return nil, r.err
	}
	return &http.Response{
		StatusCode: r.status,
		Body:       io.NopCloser(bytes.NewReader(r.body)),
		Header:     make(http.Header),
	}, nil
}

func newTestManager(t *testing.T) (*Manager, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	m := New(st, newVault(t), "srv-1")
	m.Now = func() time.Time { return time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC) }
	return m, st
}

func TestFetchAll_PersistsAndDegrades(t *testing.T) {
	m, st := newTestManager(t)
	if err := m.SetCredential(ProviderDigitalOcean, "do-key"); err != nil {
		t.Fatal(err)
	}
	if err := m.SetCredential(ProviderVultr, "vultr-key"); err != nil {
		t.Fatal(err)
	}
	m.HTTP = stubDoer{byHost: map[string]stubResp{
		"api.digitalocean.com": {status: 200, body: readFixture(t, "digitalocean_balance.json")},
		// Vultr API is down: a transport error must degrade, not abort.
		"api.vultr.com": {err: errors.New("connection refused")},
	}}

	rows, err := m.FetchAll(context.Background())
	if err != nil {
		t.Fatalf("fetchall: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}

	persisted, err := st.ListInfraCost("2026-05")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]store.InfraCost{}
	for _, c := range persisted {
		got[c.Provider] = c
	}
	do := got[ProviderDigitalOcean]
	if do.Status != "ok" || do.AmountCents != 2344 || do.Currency != "USD" {
		t.Fatalf("digitalocean row wrong: %+v", do)
	}
	vu := got[ProviderVultr]
	if vu.Status != "unavailable" || vu.AmountCents != 0 {
		t.Fatalf("vultr should degrade to unavailable: %+v", vu)
	}
	if vu.Detail == "" {
		t.Fatal("unavailable row should carry a reason")
	}
}

func TestFetch_Non2xxDegrades(t *testing.T) {
	m, st := newTestManager(t)
	if err := m.SetCredential(ProviderLinode, "bad-key"); err != nil {
		t.Fatal(err)
	}
	m.HTTP = stubDoer{byHost: map[string]stubResp{
		"api.linode.com": {status: 401, body: []byte(`{"errors":[{"reason":"Invalid Token"}]}`)},
	}}
	if _, err := m.FetchAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, _ := st.ListInfraCost("2026-05")
	if len(rows) != 1 || rows[0].Status != "unavailable" {
		t.Fatalf("401 should degrade: %+v", rows)
	}
}

func TestCredentialLifecycle(t *testing.T) {
	m, _ := newTestManager(t)
	if err := m.SetCredential("HETZNER", " hz-key "); err == nil {
		// trailing spaces in the key are fine; provider normalizes case.
	}
	if err := m.SetCredential(ProviderHetzner, "hz-key"); err != nil {
		t.Fatal(err)
	}
	infos, err := m.ListCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].Provider != ProviderHetzner {
		t.Fatalf("list: %+v", infos)
	}
	if err := m.SetCredential("nope", "x"); !errors.Is(err, ErrBadProvider) {
		t.Fatalf("want ErrBadProvider, got %v", err)
	}
	if err := m.SetCredential(ProviderHetzner, ""); !errors.Is(err, ErrNoKey) {
		t.Fatalf("want ErrNoKey, got %v", err)
	}
	if err := m.DeleteCredential(ProviderHetzner); err != nil {
		t.Fatal(err)
	}
	infos, _ = m.ListCredentials()
	if len(infos) != 0 {
		t.Fatalf("after delete: %+v", infos)
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}
