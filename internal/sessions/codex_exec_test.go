package sessions

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func translateCodexExecFixture(t *testing.T, name string) []translated {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("fixtures", "codex-exec", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var all []translated
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		evs, err := translateCodexExecLine(line)
		if err != nil {
			t.Fatalf("translate %s line %q: %v", name, line, err)
		}
		all = append(all, evs...)
	}
	return all
}

func TestTranslateCodexExec_Basic(t *testing.T) {
	evs := translateCodexExecFixture(t, "basic.jsonl")
	want := []string{EvTurn, EvMessage, EvUsage, EvTurn}
	if !eqStrings(types(evs), want) {
		t.Fatalf("types = %v want %v", types(evs), want)
	}
	if s := evs[0].Payload.(Turn); s.State != TurnStarted {
		t.Fatalf("first turn = %+v", s)
	}
	if m := evs[1].Payload.(Message); m.Text != "Hello from exec" || m.Role != "assistant" {
		t.Fatalf("message = %+v", m)
	}
	u := evs[2].Payload.(Usage)
	if u.Input != 1200 || u.Output != 15 || u.Cache != 100 {
		t.Fatalf("usage = %+v", u)
	}
	if s := evs[3].Payload.(Turn); s.State != TurnComplete {
		t.Fatalf("final turn = %+v", s)
	}
}

func TestTranslateCodexExec_CommandExecution(t *testing.T) {
	evs := translateCodexExecFixture(t, "command.jsonl")
	if !eqStrings(types(evs), []string{EvToolCall, EvToolCall}) {
		t.Fatalf("types = %v", types(evs))
	}
	started := evs[0].Payload.(ToolCall)
	if started.CallID != "item_1" || started.Name != "shell" || started.Status != ToolStarted {
		t.Fatalf("started = %+v", started)
	}
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(started.Args, &args); err != nil || args.Command != "go test ./..." {
		t.Fatalf("args = %v %+v", err, args)
	}
	if done := evs[1].Payload.(ToolCall); done.Status != ToolCompleted || done.Result != "ok\n" {
		t.Fatalf("done = %+v", done)
	}
}

func TestTranslateCodexExec_FileChangeHasNoDiff(t *testing.T) {
	evs := translateCodexExecFixture(t, "filechange.jsonl")
	// The exec stream omits unified diffs, so a file change is one tool call and
	// never a diff event.
	if !eqStrings(types(evs), []string{EvToolCall}) {
		t.Fatalf("types = %v want a single tool_call", types(evs))
	}
	tc := evs[0].Payload.(ToolCall)
	if tc.Name != "apply_patch" || tc.Status != ToolCompleted {
		t.Fatalf("tool = %+v", tc)
	}
	var args struct {
		Paths []string `json:"paths"`
	}
	if err := json.Unmarshal(tc.Args, &args); err != nil || len(args.Paths) != 2 || args.Paths[0] != "main.go" {
		t.Fatalf("paths = %v %+v", err, args)
	}
}

func TestTranslateCodexExec_TodoListAsPlan(t *testing.T) {
	evs := translateCodexExecFixture(t, "todo.jsonl")
	want := []string{EvPlan, EvPlan, EvPlan}
	if !eqStrings(types(evs), want) {
		t.Fatalf("types = %v want %v", types(evs), want)
	}
	last := evs[2].Payload.(Plan)
	if last.Gating {
		t.Fatal("exec todo plans must be non-gating")
	}
	if len(last.Items) != 2 || last.Items[0].Status != "completed" || last.Items[1].Status != "completed" {
		t.Fatalf("final plan = %+v", last.Items)
	}
	mid := evs[1].Payload.(Plan)
	if mid.Items[0].Status != "completed" || mid.Items[1].Status != "pending" {
		t.Fatalf("mid plan = %+v", mid.Items)
	}
}

func TestTranslateCodexExec_Errors(t *testing.T) {
	evs := translateCodexExecFixture(t, "error.jsonl")
	// error item -> error; turn.failed -> error + turn(complete); top-level error -> error.
	want := []string{EvError, EvError, EvTurn, EvError}
	if !eqStrings(types(evs), want) {
		t.Fatalf("types = %v want %v", types(evs), want)
	}
	if e := evs[0].Payload.(ErrorEvent); e.Fatal {
		t.Fatal("exec error items must be non-fatal")
	}
	if s := evs[2].Payload.(Turn); s.State != TurnComplete || s.StopReason != "failed" {
		t.Fatalf("turn = %+v", s)
	}
}

func TestTranslateCodexExec_IgnoresThreadStartedAndMalformed(t *testing.T) {
	if evs, err := translateCodexExecLine([]byte(`{"type":"thread.started","thread_id":"t"}`)); err != nil || len(evs) != 0 {
		t.Fatalf("thread.started: evs=%v err=%v", types(evs), err)
	}
	if _, err := translateCodexExecLine([]byte("{bad")); err == nil {
		t.Fatal("expected error for malformed line")
	}
}
