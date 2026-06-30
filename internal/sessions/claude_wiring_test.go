package sessions

import (
	"context"
	"io"
	"os"
	"testing"
	"time"
)

func TestRoutingRuntime_DispatchesByTransport(t *testing.T) {
	term := &fakeRuntime{}
	claude := &fakeRuntime{}
	lookup := func(id string) (string, string) {
		if id == "struct" {
			return "claude", TransportStructured
		}
		return "claude", TransportTerminal
	}
	rt := NewRoutingRuntime(term, map[string]Runtime{"claude": claude}, lookup)

	// Per-session ops route by the looked-up transport.
	if err := rt.SendPrompt(context.Background(), "struct", "hi"); err != nil {
		t.Fatal(err)
	}
	if err := rt.SendPrompt(context.Background(), "term", "yo"); err != nil {
		t.Fatal(err)
	}
	if len(claude.prompts) != 1 || claude.prompts[0] != "struct:hi" {
		t.Fatalf("claude prompts = %v", claude.prompts)
	}
	if len(term.prompts) != 1 || term.prompts[0] != "term:yo" {
		t.Fatalf("term prompts = %v", term.prompts)
	}

	// Start routes by spec.Transport directly.
	if err := rt.Start(context.Background(), RuntimeSpec{SessionID: "s2", Agent: "claude", Transport: TransportStructured, Output: io.Discard}); err != nil {
		t.Fatal(err)
	}
	if len(claude.started) != 1 || claude.started[0].SessionID != "s2" {
		t.Fatalf("claude started = %+v", claude.started)
	}
	if len(term.started) != 0 {
		t.Fatalf("terminal should not have started: %+v", term.started)
	}
}

func TestRoutingRuntime_FallsBackToTerminal(t *testing.T) {
	term := &fakeRuntime{}
	// No structured runtime registered for "claude".
	rt := NewRoutingRuntime(term, map[string]Runtime{}, func(string) (string, string) {
		return "claude", TransportStructured
	})
	if err := rt.SendPrompt(context.Background(), "x", "hi"); err != nil {
		t.Fatal(err)
	}
	if len(term.prompts) != 1 {
		t.Fatalf("expected fallback to terminal, got %v", term.prompts)
	}
}

// TestClaudeStructuredRuntime_Live exercises the real `claude` CLI end to end.
// It is gated on CLAVER_LIVE_CLAUDE because it needs an authenticated CLI and
// makes real model calls; it is skipped in CI and on unauthenticated hosts.
func TestClaudeStructuredRuntime_Live(t *testing.T) {
	if os.Getenv("CLAVER_LIVE_CLAUDE") == "" {
		t.Skip("set CLAVER_LIVE_CLAUDE=1 to run the live claude smoke test")
	}
	rt := NewClaudeStructuredRuntime("", "", nil)
	coll := &eventCollector{}
	spec := RuntimeSpec{
		SessionID:     "live1",
		Agent:         "claude",
		RunMode:       "manual",
		Transport:     TransportStructured,
		WorkDir:       t.TempDir(),
		Emit:          coll.emit,
		EmitEphemeral: coll.ephemeral,
	}
	if err := rt.Start(context.Background(), spec); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = rt.Stop(context.Background(), "live1") })

	if err := rt.SendPrompt(context.Background(), "live1", "Reply with exactly the word: pong"); err != nil {
		t.Fatalf("send prompt: %v", err)
	}
	coll.waitForType(t, EvTurn, 90*time.Second)
	if len(coll.byType(EvMessage)) == 0 {
		t.Fatal("no assistant message before turn completed")
	}
}
