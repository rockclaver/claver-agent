package sessions

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadClaudeTranscriptUsage_SumsAssistantTurns(t *testing.T) {
	root := t.TempDir()
	projDir := filepath.Join(root, "-Users-me-proj")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	uuid := "11111111-2222-3333-4444-555555555555"
	lines := "" +
		`{"type":"user","message":{}}` + "\n" +
		`{"type":"assistant","message":{"usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":5,"cache_creation_input_tokens":10}}}` + "\n" +
		`{"type":"assistant","message":{"usage":{"input_tokens":200,"output_tokens":30,"cache_read_input_tokens":7}}}` + "\n" +
		`not json` + "\n"
	if err := os.WriteFile(filepath.Join(projDir, uuid+".jsonl"), []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}

	u, ok := readClaudeTranscriptUsage(root, uuid)
	if !ok {
		t.Fatal("expected transcript to be found")
	}
	// input = (100+10) + (200+0); output = 20+30; cache = 5+7
	if u.input != 310 || u.output != 50 || u.cache != 12 {
		t.Fatalf("usage = %d/%d/%d want 310/50/12", u.input, u.output, u.cache)
	}
	if u.total() != 360 {
		t.Fatalf("total = %d want 360", u.total())
	}
}

func TestReadClaudeTranscriptUsage_MissingTranscript(t *testing.T) {
	if _, ok := readClaudeTranscriptUsage(t.TempDir(), "nope"); ok {
		t.Fatal("expected ok=false when transcript is absent")
	}
	if _, ok := readClaudeTranscriptUsage("", "x"); ok {
		t.Fatal("expected ok=false with empty projects dir")
	}
}
