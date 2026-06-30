package sessions

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func translateFixture(t *testing.T, name string) []translated {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("fixtures", "claude", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var all []translated
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		evs, err := translateClaudeLine(line)
		if err != nil {
			t.Fatalf("translate %s line %q: %v", name, line, err)
		}
		all = append(all, evs...)
	}
	return all
}

func types(evs []translated) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}
	return out
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestTranslateClaude_Basic(t *testing.T) {
	evs := translateFixture(t, "basic.ndjson")
	want := []string{EvMessage, EvUsage, EvTurn}
	if !eqStrings(types(evs), want) {
		t.Fatalf("types = %v want %v", types(evs), want)
	}
	msg := evs[0].Payload.(Message)
	if msg.Role != "assistant" || msg.Text != "Hello, world" || len(msg.Blocks) != 1 {
		t.Fatalf("message = %+v", msg)
	}
	if evs[0].Ephemeral {
		t.Fatal("finalized message must not be ephemeral")
	}
	u := evs[1].Payload.(Usage)
	if u.Input != 1200 || u.Output != 15 || u.Cache != 100 || u.CostUSD != 0.0123 {
		t.Fatalf("usage = %+v", u)
	}
	turn := evs[2].Payload.(Turn)
	if turn.State != TurnComplete || turn.StopReason != "end_turn" {
		t.Fatalf("turn = %+v", turn)
	}
}

func TestTranslateClaude_ToolCall(t *testing.T) {
	evs := translateFixture(t, "tool.ndjson")
	want := []string{EvMessage, EvToolCall, EvToolCall, EvMessage, EvUsage, EvTurn}
	if !eqStrings(types(evs), want) {
		t.Fatalf("types = %v want %v", types(evs), want)
	}
	started := evs[1].Payload.(ToolCall)
	if started.CallID != "toolu_1" || started.Name != "Bash" || started.Status != ToolStarted {
		t.Fatalf("started = %+v", started)
	}
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(started.Args, &args); err != nil || args.Command != "ls -la" {
		t.Fatalf("args = %v %+v", err, args)
	}
	done := evs[2].Payload.(ToolCall)
	if done.CallID != "toolu_1" || done.Status != ToolCompleted || done.Result != "total 0" {
		t.Fatalf("done = %+v", done)
	}
	// Aggregate usage from the final result line.
	u := evs[4].Payload.(Usage)
	if u.Input != 3100 || u.Output != 60 {
		t.Fatalf("usage = %+v", u)
	}
}

func TestTranslateClaude_Permission(t *testing.T) {
	evs := translateFixture(t, "permission.ndjson")
	want := []string{EvApprovalRequest, EvApprovalRequest}
	if !eqStrings(types(evs), want) {
		t.Fatalf("types = %v want %v", types(evs), want)
	}
	bash := evs[0].Payload.(ApprovalRequest)
	if bash.RequestID != "req-bash-1" || bash.Kind != ApprovalCommand {
		t.Fatalf("bash approval = %+v", bash)
	}
	if len(bash.Options) != 3 || bash.Options[0] != DecisionAllow {
		t.Fatalf("options = %v", bash.Options)
	}
	if bash.Summary != "Run a shell command" {
		t.Fatalf("summary = %q", bash.Summary)
	}
	edit := evs[1].Payload.(ApprovalRequest)
	if edit.RequestID != "req-edit-1" || edit.Kind != ApprovalEdit {
		t.Fatalf("edit approval = %+v", edit)
	}
}

func TestTranslateClaude_ExitPlanMode(t *testing.T) {
	evs := translateFixture(t, "plan.ndjson")
	want := []string{EvPlan, EvApprovalRequest}
	if !eqStrings(types(evs), want) {
		t.Fatalf("types = %v want %v", types(evs), want)
	}
	plan := evs[0].Payload.(Plan)
	if !plan.Gating {
		t.Fatalf("ExitPlanMode plan must gate: %+v", plan)
	}
	wantItems := []string{"Steps", "Read the code", "Write the fix", "Run tests"}
	if len(plan.Items) != len(wantItems) {
		t.Fatalf("items = %+v want %v", plan.Items, wantItems)
	}
	for i, it := range plan.Items {
		if it.Title != wantItems[i] || it.Status != "pending" {
			t.Fatalf("item %d = %+v", i, it)
		}
	}
	ap := evs[1].Payload.(ApprovalRequest)
	if ap.Kind != ApprovalPlan || ap.RequestID != "req-plan-1" {
		t.Fatalf("approval = %+v", ap)
	}
}

func TestTranslateClaude_Partial(t *testing.T) {
	evs := translateFixture(t, "partial.ndjson")
	want := []string{EvMessageDelta, EvMessageDelta}
	if !eqStrings(types(evs), want) {
		t.Fatalf("types = %v want %v", types(evs), want)
	}
	for _, e := range evs {
		if !e.Ephemeral {
			t.Fatalf("delta must be ephemeral: %+v", e)
		}
	}
	if d := evs[0].Payload.(MessageDelta); d.Text != "Hel" || d.Role != "assistant" {
		t.Fatalf("delta = %+v", d)
	}
}

func TestTranslateClaude_Reasoning(t *testing.T) {
	evs := translateFixture(t, "reasoning.ndjson")
	want := []string{EvReasoning, EvMessage}
	if !eqStrings(types(evs), want) {
		t.Fatalf("types = %v want %v", types(evs), want)
	}
	if r := evs[0].Payload.(Reasoning); r.Text != "Let me reason about this." {
		t.Fatalf("reasoning = %+v", r)
	}
	if m := evs[1].Payload.(Message); m.Text != "Done." {
		t.Fatalf("message = %+v", m)
	}
}

func TestTranslateClaude_IgnoresAndErrors(t *testing.T) {
	// System/init and control_response produce no normalized events.
	for _, line := range []string{
		`{"type":"system","subtype":"init","session_id":"s"}`,
		`{"type":"control_response","response":{"subtype":"success","request_id":"r"}}`,
		`{"type":"control_cancel_request","request_id":"r"}`,
		``,
	} {
		evs, err := translateClaudeLine([]byte(line))
		if err != nil {
			t.Fatalf("line %q err %v", line, err)
		}
		if len(evs) != 0 {
			t.Fatalf("line %q yielded %v", line, types(evs))
		}
	}
	// Malformed JSON surfaces an error for the runtime to log and skip.
	if _, err := translateClaudeLine([]byte("{not json")); err == nil {
		t.Fatal("expected error for malformed line")
	}
}
