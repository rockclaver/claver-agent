package cost

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rockclaver/claver/agent/internal/store"
)

func newCalc(t *testing.T) (*Calculator, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	c := New(st, "srv-1")
	c.Now = func() time.Time { return time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC) }
	return c, st
}

func may(day int) time.Time { return time.Date(2026, 5, day, 12, 0, 0, 0, time.UTC) }

func seedSession(t *testing.T, st *store.Store, id, project, agent string, started time.Time, in, out, cache, tools int) {
	t.Helper()
	if err := st.CreateSession(store.Session{
		ID: id, ProjectID: project, Agent: agent, StartedAt: started,
		InputTokens: in, OutputTokens: out, CacheTokens: cache, ToolCalls: tools,
	}); err != nil {
		t.Fatalf("create session %s: %v", id, err)
	}
}

func TestRollup_AISpendAndPerProject(t *testing.T) {
	c, st := newCalc(t)
	mustProject(t, st, "p1", "Alpha")
	mustProject(t, st, "p2", "Beta")

	// claude: input 1500/M, output 7500/M, cache 150/M.
	seedSession(t, st, "s1", "p1", "claude", may(10), 1_000_000, 1_000_000, 2_000_000, 5) // 9300
	// codex: input 250/M, output 1000/M.
	seedSession(t, st, "s2", "p1", "codex", may(11), 500_000, 100_000, 0, 2) // 225
	seedSession(t, st, "s3", "p2", "claude", may(12), 2_000_000, 0, 0, 1)    // 3000
	// April session — outside the May window, must be excluded.
	seedSession(t, st, "s4", "p2", "claude", time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC), 9_000_000, 9_000_000, 0, 9)

	r, err := c.Rollup("2026-05")
	if err != nil {
		t.Fatalf("rollup: %v", err)
	}
	if r.TotalAISpendCents != 12525 {
		t.Fatalf("total AI spend = %d, want 12525", r.TotalAISpendCents)
	}
	byID := map[string]ProjectCost{}
	for _, p := range r.Projects {
		byID[p.ProjectID] = p
	}
	if byID["p1"].AISpendCents != 9525 {
		t.Fatalf("p1 spend = %d, want 9525", byID["p1"].AISpendCents)
	}
	if byID["p1"].Sessions != 2 || byID["p1"].ToolCalls != 7 {
		t.Fatalf("p1 sessions/tools wrong: %+v", byID["p1"])
	}
	if byID["p1"].ProjectName != "Alpha" {
		t.Fatalf("p1 name = %q", byID["p1"].ProjectName)
	}
	if byID["p2"].AISpendCents != 3000 {
		t.Fatalf("p2 spend = %d, want 3000", byID["p2"].AISpendCents)
	}
}

// AC: "cost-per-PR calculator correctness."
func TestRollup_CostPerPR(t *testing.T) {
	c, st := newCalc(t)
	mustProject(t, st, "p1", "Alpha")
	seedSession(t, st, "s1", "p1", "claude", may(10), 1_000_000, 1_000_000, 2_000_000, 0) // 9300
	seedSession(t, st, "s2", "p1", "claude", may(11), 0, 1_000_000, 0, 0)                 // 7500
	// Total AI spend = 16800 cents.

	// 3 PRs shipped in May (+ one PR in April, + a non-PR May entry: neither counts).
	for _, day := range []int{5, 9, 20} {
		mustJournal(t, st, "p1", "pr", may(day))
	}
	mustJournal(t, st, "p1", "pr", time.Date(2026, 4, 28, 0, 0, 0, 0, time.UTC))
	mustJournal(t, st, "p1", "session", may(15))

	r, err := c.Rollup("2026-05")
	if err != nil {
		t.Fatalf("rollup: %v", err)
	}
	if r.TotalAISpendCents != 16800 {
		t.Fatalf("total = %d, want 16800", r.TotalAISpendCents)
	}
	if r.PRsShipped != 3 {
		t.Fatalf("PRs shipped = %d, want 3", r.PRsShipped)
	}
	// 16800 / 3 = 5600.
	if r.CostPerPRCents != 5600 {
		t.Fatalf("cost per PR = %d, want 5600", r.CostPerPRCents)
	}
}

func TestRollup_NoPRsYieldsZeroPerPR(t *testing.T) {
	c, st := newCalc(t)
	mustProject(t, st, "p1", "Alpha")
	seedSession(t, st, "s1", "p1", "claude", may(10), 1_000_000, 0, 0, 0)
	r, err := c.Rollup("2026-05")
	if err != nil {
		t.Fatal(err)
	}
	if r.PRsShipped != 0 || r.CostPerPRCents != 0 {
		t.Fatalf("expected zero PRs and zero per-PR, got %d / %d", r.PRsShipped, r.CostPerPRCents)
	}
}

func TestRollup_InfraDegradation(t *testing.T) {
	c, st := newCalc(t)
	mustProject(t, st, "p1", "Alpha")
	if err := st.PutInfraCost(store.InfraCost{ServerID: "srv-1", Provider: "hetzner", Month: "2026-05", AmountCents: 1260, Currency: "EUR", Status: "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutInfraCost(store.InfraCost{ServerID: "srv-1", Provider: "vultr", Month: "2026-05", Status: "unavailable", Detail: "connection refused", Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	// Another server's row must not leak into this server's rollup.
	if err := st.PutInfraCost(store.InfraCost{ServerID: "srv-2", Provider: "linode", Month: "2026-05", AmountCents: 9999, Currency: "USD", Status: "ok"}); err != nil {
		t.Fatal(err)
	}

	r, err := c.Rollup("2026-05")
	if err != nil {
		t.Fatal(err)
	}
	if r.TotalInfraSpendCents != 1260 || r.InfraCurrency != "EUR" {
		t.Fatalf("infra total wrong: %d %s", r.TotalInfraSpendCents, r.InfraCurrency)
	}
	if !r.InfraAvailable {
		t.Fatal("infra should be available (one ok provider)")
	}
	if len(r.InfraUnavailable) != 1 || r.InfraUnavailable[0] != "vultr" {
		t.Fatalf("expected vultr unavailable, got %v", r.InfraUnavailable)
	}
	if len(r.Infra) != 2 {
		t.Fatalf("expected 2 infra rows for srv-1, got %d", len(r.Infra))
	}
}

func TestRollup_AllInfraUnavailable(t *testing.T) {
	c, st := newCalc(t)
	if err := st.PutInfraCost(store.InfraCost{ServerID: "srv-1", Provider: "vultr", Month: "2026-05", Status: "unavailable", Detail: "down", Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	r, err := c.Rollup("2026-05")
	if err != nil {
		t.Fatal(err)
	}
	if r.InfraAvailable {
		t.Fatal("infra should be unavailable when no provider is ok")
	}
}

func TestExportCSV(t *testing.T) {
	c, st := newCalc(t)
	mustProject(t, st, "p1", "Alpha")
	seedSession(t, st, "s1", "p1", "claude", may(10), 1_000_000, 0, 0, 3) // 1500
	mustJournal(t, st, "p1", "pr", may(7))
	if err := st.PutInfraCost(store.InfraCost{ServerID: "srv-1", Provider: "hetzner", Month: "2026-05", AmountCents: 1260, Currency: "EUR", Status: "ok"}); err != nil {
		t.Fatal(err)
	}

	out, err := c.ExportCSV("2026-05")
	if err != nil {
		t.Fatalf("csv: %v", err)
	}
	for _, want := range []string{
		"section,name,sessions",
		"summary,total_ai_spend,,,,,,15.00",
		"summary,ai_cost_per_pr,,,,,,15.00",
		"project,Alpha,1,1000000,0,0,3,15.00",
		"infra,hetzner,,,,,,,12.60,EUR,ok",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("CSV missing %q in:\n%s", want, out)
		}
	}
}

func TestRollup_DefaultsToCurrentMonth(t *testing.T) {
	c, st := newCalc(t) // Now = 2026-05-15
	mustProject(t, st, "p1", "Alpha")
	seedSession(t, st, "s1", "p1", "claude", may(10), 1_000_000, 0, 0, 0)
	r, err := c.Rollup("")
	if err != nil {
		t.Fatal(err)
	}
	if r.Month != "2026-05" || r.TotalAISpendCents != 1500 {
		t.Fatalf("default month rollup wrong: %s %d", r.Month, r.TotalAISpendCents)
	}
}

func TestRollup_RejectsBadMonth(t *testing.T) {
	c, _ := newCalc(t)
	if _, err := c.Rollup("nonsense"); err == nil {
		t.Fatal("expected error for bad month")
	}
}

func mustProject(t *testing.T, st *store.Store, id, name string) {
	t.Helper()
	if err := st.CreateProject(store.Project{ID: id, Name: name}); err != nil {
		t.Fatalf("create project: %v", err)
	}
}

func mustJournal(t *testing.T, st *store.Store, project, kind string, at time.Time) {
	t.Helper()
	if _, err := st.AppendJournalEntry(store.JournalEntry{
		ProjectID: project, Kind: kind, Summary: kind, OccurredAt: at,
	}); err != nil {
		t.Fatalf("append journal: %v", err)
	}
}
