package sessions

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// codexSchemaPath is the committed `codex app-server generate-json-schema`
// output, pinned to the supported codex-cli version. codex_translate.go and
// codex_runtime.go are written against this exact protocol surface.
const codexSchemaPath = "fixtures/codex/app_server_schema.json"

// codexSchemaSurface is the set of method names, decision values, item types,
// and policy/sandbox literals the codex translator and runtime depend on. A
// codex bump that renames or drops any of these is a breaking protocol change.
func codexSchemaSurface() []string {
	return []string{
		// server -> client notifications we translate
		codexMethodTurnStarted, codexMethodTurnCompleted, codexMethodTokenUsage,
		codexMethodPlanUpdated, codexMethodAgentMsgDelta, codexMethodItemStarted, codexMethodItemCompleted,
		// server -> client approval requests we answer
		codexMethodCmdApproval, codexMethodFileApproval, codexMethodExecApproval, codexMethodPatchApproval,
		// client -> server requests we send
		"thread/start", "thread/resume", "thread/fork", "turn/start", "turn/interrupt", "initialize",
		// decision values we send back
		"acceptForSession", "approved_for_session",
		// item types we switch on
		`"agentMessage"`, `"reasoning"`, `"commandExecution"`, `"fileChange"`, `"mcpToolCall"`,
		// approval policy + sandbox values we set on thread/start
		"on-request", "workspace-write", "danger-full-access",
	}
}

// codexSchemaGaps returns the subset of codexSchemaSurface() the given schema no
// longer declares. An empty result means the runtime's assumptions still hold.
func codexSchemaGaps(schema []byte) []string {
	s := string(schema)
	var gaps []string
	for _, needle := range codexSchemaSurface() {
		if !strings.Contains(s, needle) {
			gaps = append(gaps, needle)
		}
	}
	return gaps
}

// TestCodexSchema_DeclaresTranslatedSurface asserts the committed app-server
// schema still declares every method, decision value, and item type the
// runtime depends on. It runs in CI without codex installed, so a codex bump
// that regenerates the schema and renames/drops part of our surface fails here.
func TestCodexSchema_DeclaresTranslatedSurface(t *testing.T) {
	data, err := os.ReadFile(codexSchemaPath)
	if err != nil {
		t.Fatalf("read committed schema: %v", err)
	}
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("committed schema is not valid JSON: %v", err)
	}
	if gaps := codexSchemaGaps(data); len(gaps) != 0 {
		t.Errorf("committed codex schema no longer declares %v; the runtime's assumptions have drifted", gaps)
	}
}

// TestCodexSchema_DriftIsDetected proves the surface check above actually fails
// when a depended-on method disappears, so a future codex bump that renames it
// cannot slip through CI as a silent no-op.
func TestCodexSchema_DriftIsDetected(t *testing.T) {
	data, err := os.ReadFile(codexSchemaPath)
	if err != nil {
		t.Fatalf("read committed schema: %v", err)
	}
	// Simulate a bump that renamed turn/start -> turn/begin.
	mutated := bytes.ReplaceAll(data, []byte("turn/start"), []byte("turn/begin"))
	if bytes.Equal(mutated, data) {
		t.Fatal("expected committed schema to contain turn/start")
	}
	gaps := codexSchemaGaps(mutated)
	found := false
	for _, g := range gaps {
		if g == "turn/start" {
			found = true
		}
	}
	if !found {
		t.Fatalf("drift check did not flag the renamed turn/start method; gaps=%v", gaps)
	}
}

// TestCodexSchema_NoDrift regenerates the schema with the installed codex and
// byte-compares it to the committed file. It is skipped when codex is absent
// (e.g. CI), where the committed artifact and the surface check above stand in.
func TestCodexSchema_NoDrift(t *testing.T) {
	bin, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex not installed; skipping schema regeneration diff")
	}
	dir := t.TempDir()
	if out, err := exec.Command(bin, "app-server", "generate-json-schema", "--out", dir).CombinedOutput(); err != nil {
		t.Skipf("codex schema generation unavailable: %v (%s)", err, out)
	}
	fresh, err := os.ReadFile(filepath.Join(dir, "codex_app_server_protocol.schemas.json"))
	if err != nil {
		t.Fatalf("read regenerated schema: %v", err)
	}
	committed, err := os.ReadFile(codexSchemaPath)
	if err != nil {
		t.Fatalf("read committed schema: %v", err)
	}
	if !bytes.Equal(fresh, committed) {
		t.Fatalf("codex app-server schema drifted from %s.\nRegenerate: codex app-server generate-json-schema --out <tmp> && cp <tmp>/codex_app_server_protocol.schemas.json %s\nThen re-verify the codex translator against the new surface.", codexSchemaPath, codexSchemaPath)
	}
}
