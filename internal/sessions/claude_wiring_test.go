package sessions

import (
	"context"
	"io"
	"testing"
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
