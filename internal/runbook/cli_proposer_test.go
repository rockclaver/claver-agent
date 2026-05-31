package runbook

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rockclaver/claver/agent/internal/aiproposal"
)

func TestCLIProposer_ParsesClaudeEnvelope(t *testing.T) {
	// Real claude -p --output-format json wraps the assistant message in a
	// {"result": "<assistant text>", ...} envelope. Verify we unwrap it.
	body := `{"summary":"restart x","risk":"low","steps":[{"kind":"infra.service.action","params":{"name":"x.service","action":"restart"},"description":"d"}]}`
	envelope := `{"type":"result","result":"` + escapeJSON(body) + `"}`
	p := CLIProposer{
		Agent: "claude",
		Exec: func(_ context.Context, _ string, _ []string, _ []string, _ string) ([]byte, error) {
			return []byte(envelope), nil
		},
	}
	got, err := p.Propose(context.Background(), sampleAlert(), Grounding{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary != "restart x" || got.Risk != RiskLow || len(got.Steps) != 1 {
		t.Fatalf("bad parse: %+v", got)
	}
	if got.Steps[0].Kind != aiproposal.KindServiceAction {
		t.Fatalf("step kind=%q", got.Steps[0].Kind)
	}
}

func TestCLIProposer_ParsesDirectJSON(t *testing.T) {
	// codex exec returns raw assistant text, no envelope.
	body := `{"summary":"manual review","risk":"medium","steps":[]}`
	p := CLIProposer{
		Agent: "codex",
		Exec: func(_ context.Context, _ string, _ []string, _ []string, _ string) ([]byte, error) {
			return []byte(body), nil
		},
	}
	got, err := p.Propose(context.Background(), sampleAlert(), Grounding{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary != "manual review" || len(got.Steps) != 0 {
		t.Fatalf("bad parse: %+v", got)
	}
}

func TestCLIProposer_ParsesJSONInsideCodeFence(t *testing.T) {
	body := "Here is the plan:\n```json\n{\"summary\":\"x\",\"risk\":\"low\",\"steps\":[]}\n```\nthat is all"
	p := CLIProposer{
		Agent: "codex",
		Exec: func(_ context.Context, _ string, _ []string, _ []string, _ string) ([]byte, error) {
			return []byte(body), nil
		},
	}
	got, err := p.Propose(context.Background(), sampleAlert(), Grounding{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary != "x" {
		t.Fatalf("bad parse: %+v", got)
	}
}

func TestCLIProposer_ExecFailureReturnsError(t *testing.T) {
	p := CLIProposer{
		Exec: func(_ context.Context, _ string, _ []string, _ []string, _ string) ([]byte, error) {
			return []byte("auth failed"), errors.New("exit 1")
		},
	}
	if _, err := p.Propose(context.Background(), sampleAlert(), Grounding{}); err == nil {
		t.Fatal("expected error")
	}
}

func TestCLIProposer_UnparseableOutputReturnsError(t *testing.T) {
	p := CLIProposer{
		Exec: func(_ context.Context, _ string, _ []string, _ []string, _ string) ([]byte, error) {
			return []byte("I cannot help with that."), nil
		},
	}
	if _, err := p.Propose(context.Background(), sampleAlert(), Grounding{}); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestCLIProposer_TimeoutEnforced(t *testing.T) {
	p := CLIProposer{
		Timeout: 5 * time.Millisecond,
		Exec: func(ctx context.Context, _ string, _ []string, _ []string, _ string) ([]byte, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Second):
				return []byte("{}"), nil
			}
		},
	}
	start := time.Now()
	if _, err := p.Propose(context.Background(), sampleAlert(), Grounding{}); err == nil {
		t.Fatal("expected timeout error")
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatalf("timeout not respected: took %v", time.Since(start))
	}
}

func TestCLIProposer_PromptContainsAlertAndGrounding(t *testing.T) {
	var capturedPrompt string
	p := CLIProposer{
		Exec: func(_ context.Context, _ string, _ []string, _ []string, stdin string) ([]byte, error) {
			capturedPrompt = stdin
			return []byte(`{"summary":"ok","risk":"low","steps":[]}`), nil
		},
	}
	if _, err := p.Propose(context.Background(), sampleAlert(), Grounding{Metrics: "MX"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(capturedPrompt, "disk_usage") {
		t.Fatal("prompt missing alert rule")
	}
	if !strings.Contains(capturedPrompt, "MX") {
		t.Fatal("prompt missing grounding metrics")
	}
	if !strings.Contains(capturedPrompt, "infra.service.action") {
		t.Fatal("prompt missing kind whitelist")
	}
}

// escapeJSON is a tiny helper to embed a JSON string inside another JSON
// string literal in tests.
func escapeJSON(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}
