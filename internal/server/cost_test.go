package server

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/rockclaver/claver/agent/internal/billing"
	"github.com/rockclaver/claver/agent/internal/cost"
	"github.com/rockclaver/claver/agent/internal/store"
)

func newCostTestServer(t *testing.T) (string, *store.Store, func()) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.CreateProject(store.Project{ID: "p1", Name: "Alpha"}); err != nil {
		t.Fatalf("project: %v", err)
	}
	now := func() time.Time { return time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC) }
	calc := cost.New(st, "srv-1")
	calc.Now = now
	bm := billing.New(st, billing.NewVault(filepath.Join(t.TempDir(), "billing.key")), "srv-1")
	bm.Now = now

	wsURL, stop := startTestServerWith(t, Config{Addr: "127.0.0.1:0", Cost: calc, Billing: bm})
	return wsURL, st, func() { stop(); _ = st.Close() }
}

func TestCostRollupRPC(t *testing.T) {
	wsURL, st, cleanup := newCostTestServer(t)
	defer cleanup()

	_ = st.CreateSession(store.Session{
		ID: "s1", ProjectID: "p1", Agent: "claude",
		StartedAt:   time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC),
		InputTokens: 1_000_000, OutputTokens: 0, CacheTokens: 0,
	})
	_, _ = st.AppendJournalEntry(store.JournalEntry{
		ProjectID: "p1", Kind: "pr", Summary: "shipped",
		OccurredAt: time.Date(2026, 5, 12, 0, 0, 0, 0, time.UTC),
	})

	resp := roundTrip(t, wsURL, "cost.rollup", map[string]any{"month": "2026-05"})
	if resp.Kind != "cost.rollup" {
		t.Fatalf("rollup resp: %s %s", resp.Kind, resp.Payload)
	}
	var r cost.Rollup
	if err := json.Unmarshal(resp.Payload, &r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if r.TotalAISpendCents != 1500 {
		t.Fatalf("ai spend = %d want 1500", r.TotalAISpendCents)
	}
	if r.PRsShipped != 1 || r.CostPerPRCents != 1500 {
		t.Fatalf("cost-per-PR wrong: %d PRs, %d cents", r.PRsShipped, r.CostPerPRCents)
	}
}

func TestCostExportRPC(t *testing.T) {
	wsURL, _, cleanup := newCostTestServer(t)
	defer cleanup()
	resp := roundTrip(t, wsURL, "cost.export", map[string]any{"month": "2026-05"})
	if resp.Kind != "cost.export" {
		t.Fatalf("export resp: %s", resp.Kind)
	}
	var out struct {
		CSV string `json:"csv"`
	}
	_ = json.Unmarshal(resp.Payload, &out)
	if out.CSV == "" {
		t.Fatal("empty CSV")
	}
}

func TestProviderCredentialRPC(t *testing.T) {
	wsURL, _, cleanup := newCostTestServer(t)
	defer cleanup()

	// set
	resp := roundTrip(t, wsURL, "provider.set", map[string]any{
		"provider": "digitalocean", "api_key": "do-secret",
	})
	if resp.Kind != "provider.set" {
		t.Fatalf("set resp: %s %s", resp.Kind, resp.Payload)
	}
	// bad provider => bad_payload
	bad := roundTrip(t, wsURL, "provider.set", map[string]any{
		"provider": "nope", "api_key": "x",
	})
	if bad.Kind != "error.bad_payload" {
		t.Fatalf("expected error.bad_payload, got %s", bad.Kind)
	}

	// list never returns key material
	resp = roundTrip(t, wsURL, "provider.list", nil)
	var listOut struct {
		Credentials []ProviderCredentialDTO `json:"credentials"`
		Supported   []string                `json:"supported"`
	}
	_ = json.Unmarshal(resp.Payload, &listOut)
	if len(listOut.Credentials) != 1 || listOut.Credentials[0].Provider != "digitalocean" {
		t.Fatalf("list = %+v", listOut.Credentials)
	}
	if len(listOut.Supported) != 4 {
		t.Fatalf("supported providers = %v", listOut.Supported)
	}

	// delete
	resp = roundTrip(t, wsURL, "provider.delete", map[string]any{"provider": "digitalocean"})
	if resp.Kind != "provider.delete" {
		t.Fatalf("delete resp: %s", resp.Kind)
	}
	resp = roundTrip(t, wsURL, "provider.list", nil)
	_ = json.Unmarshal(resp.Payload, &listOut)
	if len(listOut.Credentials) != 0 {
		t.Fatalf("after delete: %+v", listOut.Credentials)
	}
}

func TestCostUnavailableWhenUnconfigured(t *testing.T) {
	wsURL, stop := startTestServerWith(t, Config{Addr: "127.0.0.1:0"})
	defer stop()
	resp := roundTrip(t, wsURL, "cost.rollup", map[string]any{})
	if resp.Kind != "error.unavailable" {
		t.Fatalf("expected error.unavailable, got %s", resp.Kind)
	}
}
