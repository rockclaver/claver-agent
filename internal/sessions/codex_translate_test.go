package sessions

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func translateCodexFixture(t *testing.T, name string) []translated {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("fixtures", "codex", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var all []translated
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		evs, err := translateCodexLine(line)
		if err != nil {
			t.Fatalf("translate %s line %q: %v", name, line, err)
		}
		all = append(all, evs...)
	}
	return all
}

func TestTranslateCodex_Basic(t *testing.T) {
	evs := translateCodexFixture(t, "basic.jsonl")
	want := []string{EvTurn, EvMessageDelta, EvMessageDelta, EvMessage, EvUsage, EvTurn}
	if !eqStrings(types(evs), want) {
		t.Fatalf("types = %v want %v", types(evs), want)
	}
	if s := evs[0].Payload.(Turn); s.State != TurnStarted {
		t.Fatalf("first turn = %+v", s)
	}
	d := evs[1].Payload.(MessageDelta)
	if d.Text != "Hello, " || d.Role != "assistant" || !evs[1].Ephemeral {
		t.Fatalf("delta = %+v ephemeral=%v", d, evs[1].Ephemeral)
	}
	m := evs[3].Payload.(Message)
	if m.Role != "assistant" || m.Text != "Hello, world" || len(m.Blocks) != 1 {
		t.Fatalf("message = %+v", m)
	}
	if evs[3].Ephemeral {
		t.Fatal("finalized message must not be ephemeral")
	}
	u := evs[4].Payload.(Usage)
	if u.Input != 1200 || u.Output != 15 || u.Cache != 100 {
		t.Fatalf("usage = %+v", u)
	}
	if s := evs[5].Payload.(Turn); s.State != TurnComplete || s.StopReason != "completed" {
		t.Fatalf("final turn = %+v", s)
	}
}

func TestTranslateCodex_CommandExecution(t *testing.T) {
	evs := translateCodexFixture(t, "command.jsonl")
	want := []string{EvToolCall, EvToolCall}
	if !eqStrings(types(evs), want) {
		t.Fatalf("types = %v want %v", types(evs), want)
	}
	started := evs[0].Payload.(ToolCall)
	if started.CallID != "it-cmd-1" || started.Name != "shell" || started.Status != ToolStarted {
		t.Fatalf("started = %+v", started)
	}
	var args struct {
		Command string `json:"command"`
		Cwd     string `json:"cwd"`
	}
	if err := json.Unmarshal(started.Args, &args); err != nil || args.Command != "ls -la" || args.Cwd != "/work" {
		t.Fatalf("args = %v %+v", err, args)
	}
	done := evs[1].Payload.(ToolCall)
	if done.CallID != "it-cmd-1" || done.Status != ToolCompleted || done.Result != "total 0" {
		t.Fatalf("done = %+v", done)
	}
}

func TestTranslateCodex_FileChange(t *testing.T) {
	evs := translateCodexFixture(t, "filechange.jsonl")
	want := []string{EvToolCall, EvDiff, EvToolCall}
	if !eqStrings(types(evs), want) {
		t.Fatalf("types = %v want %v", types(evs), want)
	}
	tc := evs[0].Payload.(ToolCall)
	if tc.CallID != "it-fc-1" || tc.Name != "apply_patch" || tc.Status != ToolStarted {
		t.Fatalf("tool = %+v", tc)
	}
	d := evs[1].Payload.(Diff)
	if d.CallID != "it-fc-1" || d.Path != "server.go" || !bytes.Contains([]byte(d.Patch), []byte("envPort()")) {
		t.Fatalf("diff = %+v", d)
	}
	if done := evs[2].Payload.(ToolCall); done.Status != ToolCompleted {
		t.Fatalf("done status = %q", done.Status)
	}
}

func TestTranslateCodex_Reasoning(t *testing.T) {
	evs := translateCodexFixture(t, "reasoning.jsonl")
	want := []string{EvReasoning}
	if !eqStrings(types(evs), want) {
		t.Fatalf("types = %v want %v", types(evs), want)
	}
	if r := evs[0].Payload.(Reasoning); r.Text != "Let me reason about this." {
		t.Fatalf("reasoning = %+v", r)
	}
}

func TestTranslateCodex_Plan(t *testing.T) {
	evs := translateCodexFixture(t, "plan.jsonl")
	want := []string{EvPlan}
	if !eqStrings(types(evs), want) {
		t.Fatalf("types = %v want %v", types(evs), want)
	}
	plan := evs[0].Payload.(Plan)
	if plan.Gating {
		t.Fatal("codex plans must be display-only (non-gating)")
	}
	wantItems := []struct{ title, status string }{
		{"Read the code", "completed"},
		{"Write the fix", "in_progress"},
		{"Run tests", "pending"},
	}
	if len(plan.Items) != len(wantItems) {
		t.Fatalf("items = %+v", plan.Items)
	}
	for i, w := range wantItems {
		if plan.Items[i].Title != w.title || plan.Items[i].Status != w.status {
			t.Fatalf("item %d = %+v want %v", i, plan.Items[i], w)
		}
	}
}

func TestTranslateCodex_CommandApproval(t *testing.T) {
	evs := translateCodexFixture(t, "approval_command.jsonl")
	want := []string{EvApprovalRequest}
	if !eqStrings(types(evs), want) {
		t.Fatalf("types = %v want %v", types(evs), want)
	}
	a := evs[0].Payload.(ApprovalRequest)
	if a.RequestID != "req-cmd-7" || a.Kind != ApprovalCommand {
		t.Fatalf("approval = %+v", a)
	}
	if a.Summary != "rm -rf build" {
		t.Fatalf("summary = %q", a.Summary)
	}
	if len(a.Options) != 3 || a.Options[0] != DecisionAllow {
		t.Fatalf("options = %v", a.Options)
	}
	var detail struct {
		Command string `json:"command"`
		Cwd     string `json:"cwd"`
	}
	if err := json.Unmarshal([]byte(a.Detail), &detail); err != nil || detail.Command != "rm -rf build" || detail.Cwd != "/work" {
		t.Fatalf("detail = %v %+v", err, detail)
	}
}

func TestTranslateCodex_PatchApproval(t *testing.T) {
	evs := translateCodexFixture(t, "approval_patch.jsonl")
	want := []string{EvApprovalRequest}
	if !eqStrings(types(evs), want) {
		t.Fatalf("types = %v want %v", types(evs), want)
	}
	a := evs[0].Payload.(ApprovalRequest)
	// Numeric JSON-RPC ids round-trip as their decimal string.
	if a.RequestID != "42" || a.Kind != ApprovalEdit {
		t.Fatalf("approval = %+v", a)
	}
}

func TestTranslateCodex_LegacyPatchApproval(t *testing.T) {
	evs := translateCodexFixture(t, "approval_patch_legacy.jsonl")
	if !eqStrings(types(evs), []string{EvApprovalRequest}) {
		t.Fatalf("types = %v", types(evs))
	}
	a := evs[0].Payload.(ApprovalRequest)
	if a.RequestID != "req-patch-9" || a.Kind != ApprovalEdit {
		t.Fatalf("approval = %+v", a)
	}
	// The inline fileChanges are synthesized into a {diff} the sheet renders.
	var detail struct {
		Diff string `json:"diff"`
	}
	if err := json.Unmarshal([]byte(a.Detail), &detail); err != nil || !bytes.Contains([]byte(detail.Diff), []byte("main.go")) {
		t.Fatalf("detail = %v %q", err, a.Detail)
	}
}

func TestTranslateCodex_Error(t *testing.T) {
	evs := translateCodexFixture(t, "error.jsonl")
	if !eqStrings(types(evs), []string{EvError}) {
		t.Fatalf("types = %v", types(evs))
	}
	e := evs[0].Payload.(ErrorEvent)
	if e.Message != "Reconnecting... 2/5" || e.Fatal {
		t.Fatalf("error = %+v", e)
	}
}

func TestTranslateCodex_IgnoresAndErrors(t *testing.T) {
	// Lifecycle/control/unknown-item frames produce no normalized events.
	if evs := translateCodexFixture(t, "ignore.jsonl"); len(evs) != 0 {
		t.Fatalf("ignore.jsonl yielded %v", types(evs))
	}
	// A bare response (no method) is routed by the conn, not translated.
	if evs, err := translateCodexLine([]byte(`{"id":7,"result":{"ok":true}}`)); err != nil || len(evs) != 0 {
		t.Fatalf("response line: evs=%v err=%v", types(evs), err)
	}
	// Malformed JSON surfaces an error for the runtime to log and skip.
	if _, err := translateCodexLine([]byte("{not json")); err == nil {
		t.Fatal("expected error for malformed line")
	}
}
