package sessions

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

type codexConnHarness struct {
	conn   *codexConn
	stdout *io.PipeWriter
	out    *lineCapture
	coll   *eventCollector
}

func newCodexConnHarness(t *testing.T) *codexConnHarness {
	t.Helper()
	stdoutR, stdoutW := io.Pipe()
	out := newLineCapture()
	coll := &eventCollector{}
	conn := newCodexConn(structuredSink{sessionID: "s1", emit: coll.emit, ephemeral: coll.ephemeral}, out)
	go conn.run(stdoutR)
	t.Cleanup(func() { _ = stdoutW.Close() })
	return &codexConnHarness{conn: conn, stdout: stdoutW, out: out, coll: coll}
}

func (h *codexConnHarness) feed(t *testing.T, line string) {
	t.Helper()
	if _, err := io.WriteString(h.stdout, line+"\n"); err != nil {
		t.Fatalf("feed: %v", err)
	}
}

func TestCodexConn_TranslatesNotifications(t *testing.T) {
	h := newCodexConnHarness(t)
	h.feed(t, `{"method":"turn/started","params":{"threadId":"th","turn":{"id":"tn","status":"inProgress"}}}`)
	h.feed(t, `{"method":"item/completed","params":{"item":{"type":"agentMessage","id":"m1","text":"hello"},"threadId":"th","turnId":"tn"}}`)
	h.feed(t, `{"method":"turn/completed","params":{"threadId":"th","turn":{"id":"tn","status":"completed"}}}`)
	msg := h.coll.waitForType(t, EvMessage, time.Second)
	var m Message
	if err := json.Unmarshal([]byte(msg.ev.Data), &m); err != nil || m.Text != "hello" {
		t.Fatalf("message = %v %q", err, msg.ev.Data)
	}
	h.coll.waitForType(t, EvTurn, time.Second)
}

func TestCodexConn_SendPromptOpensTurn(t *testing.T) {
	h := newCodexConnHarness(t)
	h.conn.mu.Lock()
	h.conn.threadID = "th-1"
	h.conn.mu.Unlock()

	done := make(chan error, 1)
	go func() { done <- h.conn.sendPrompt(context.Background(), "do it") }()

	frame := h.out.read(t)
	if frame["method"] != "turn/start" {
		t.Fatalf("method = %v", frame["method"])
	}
	params, _ := frame["params"].(map[string]any)
	if params["threadId"] != "th-1" {
		t.Fatalf("threadId = %v", params["threadId"])
	}
	input, _ := params["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input = %v", params["input"])
	}
	first, _ := input[0].(map[string]any)
	if first["type"] != "text" || first["text"] != "do it" {
		t.Fatalf("input[0] = %v", input[0])
	}
	// Unblock sendPrompt with a matching response.
	id := int(frame["id"].(float64))
	h.feed(t, fmt.Sprintf(`{"id":%d,"result":{"turn":{"id":"tn-9"}}}`, id))
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("sendPrompt: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sendPrompt did not return")
	}
	h.conn.mu.Lock()
	turn := h.conn.turnID
	h.conn.mu.Unlock()
	if turn != "tn-9" {
		t.Fatalf("turnID = %q", turn)
	}
}

func TestCodexConn_ApprovalResponse(t *testing.T) {
	h := newCodexConnHarness(t)
	h.feed(t, `{"id":"req-1","method":"item/commandExecution/requestApproval","params":{"itemId":"i1","threadId":"th","turnId":"tn","command":"ls","commandActions":[],"cwd":"/w","reason":"r","startedAtMs":1}}`)
	ev := h.coll.waitForType(t, EvApprovalRequest, time.Second)
	var ap ApprovalRequest
	if err := json.Unmarshal([]byte(ev.ev.Data), &ap); err != nil || ap.RequestID != "req-1" {
		t.Fatalf("approval = %v %q", err, ev.ev.Data)
	}
	if err := h.conn.approve("req-1", DecisionAllow); err != nil {
		t.Fatalf("approve: %v", err)
	}
	resp := h.out.read(t)
	if resp["id"] != "req-1" {
		t.Fatalf("resp id = %v", resp["id"])
	}
	result, _ := resp["result"].(map[string]any)
	if result["decision"] != "accept" {
		t.Fatalf("decision = %v", result["decision"])
	}
	// A second approve for the same id is unknown (consumed once).
	if err := h.conn.approve("req-1", DecisionAllow); err == nil {
		t.Fatal("expected error approving an already-answered request")
	}
}

func TestCodexConn_AllowAlwaysAndDenyDecisions(t *testing.T) {
	h := newCodexConnHarness(t)
	// v2 file approval -> acceptForSession on allow_always.
	h.feed(t, `{"id":7,"method":"item/fileChange/requestApproval","params":{"itemId":"i2","threadId":"th","turnId":"tn","reason":"r","startedAtMs":1}}`)
	h.coll.waitForType(t, EvApprovalRequest, time.Second)
	if err := h.conn.approve("7", DecisionAllowAlways); err != nil {
		t.Fatalf("approve: %v", err)
	}
	resp := h.out.read(t)
	if result, _ := resp["result"].(map[string]any); result["decision"] != "acceptForSession" {
		t.Fatalf("decision = %v", resp["result"])
	}
	// legacy exec approval -> denied on deny.
	h.feed(t, `{"id":"e1","method":"execCommandApproval","params":{"callId":"c1","conversationId":"th","command":["rm","-rf","/"],"cwd":"/w","parsedCmd":[]}}`)
	h.coll.waitForType(t, EvApprovalRequest, time.Second)
	if err := h.conn.approve("e1", DecisionDeny); err != nil {
		t.Fatalf("approve: %v", err)
	}
	resp = h.out.read(t)
	if result, _ := resp["result"].(map[string]any); result["decision"] != "denied" {
		t.Fatalf("legacy decision = %v", resp["result"])
	}
}

func TestCodexConn_DeclinesUnsupportedRequest(t *testing.T) {
	h := newCodexConnHarness(t)
	h.feed(t, `{"id":99,"method":"account/chatgptAuthTokens/refresh","params":{}}`)
	resp := h.out.read(t)
	if int(resp["id"].(float64)) != 99 {
		t.Fatalf("resp id = %v", resp["id"])
	}
	if resp["error"] == nil {
		t.Fatalf("expected an error response, got %v", resp)
	}
	if len(h.coll.byType(EvApprovalRequest)) != 0 {
		t.Fatal("unsupported request must not emit an approval")
	}
}

func TestCodexConn_Interrupt(t *testing.T) {
	h := newCodexConnHarness(t)
	h.conn.mu.Lock()
	h.conn.threadID = "th"
	h.conn.turnID = "tn"
	h.conn.mu.Unlock()
	go func() { _ = h.conn.interrupt(context.Background()) }()
	frame := h.out.read(t)
	if frame["method"] != "turn/interrupt" {
		t.Fatalf("method = %v", frame["method"])
	}
	params, _ := frame["params"].(map[string]any)
	if params["threadId"] != "th" || params["turnId"] != "tn" {
		t.Fatalf("params = %v", params)
	}
	id := int(frame["id"].(float64))
	h.feed(t, fmt.Sprintf(`{"id":%d,"result":{}}`, id))
}

const codexStub = `#!/bin/sh
# fake 'codex app-server': minimal JSON-RPC handshake + one scripted turn.
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      id=` + "`" + `printf '%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p'` + "`" + `
      printf '{"id":%s,"result":{}}\n' "$id"
      ;;
    *'"method":"thread/start"'*)
      id=` + "`" + `printf '%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p'` + "`" + `
      printf '{"id":%s,"result":{"thread":{"id":"th-stub"}}}\n' "$id"
      ;;
    *'"method":"turn/start"'*)
      id=` + "`" + `printf '%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p'` + "`" + `
      printf '{"id":%s,"result":{"turn":{"id":"tn-stub"}}}\n' "$id"
      printf '%s\n' '{"method":"item/completed","params":{"item":{"type":"agentMessage","id":"m1","text":"hi there"},"threadId":"th-stub","turnId":"tn-stub"}}'
      printf '%s\n' '{"method":"thread/tokenUsage/updated","params":{"threadId":"th-stub","turnId":"tn-stub","tokenUsage":{"last":{"cachedInputTokens":0,"inputTokens":3,"outputTokens":2,"reasoningOutputTokens":0,"totalTokens":5}}}}'
      printf '%s\n' '{"method":"turn/completed","params":{"threadId":"th-stub","turn":{"id":"tn-stub","status":"completed"}}}'
      ;;
  esac
done
`

func TestCodexStructuredRuntime_StubEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub uses a POSIX shell")
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "codex")
	if err := os.WriteFile(stub, []byte(codexStub), 0o755); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(dir, "work")

	coll := &eventCollector{}
	rt := NewCodexStructuredRuntime(dir, "", nil)
	spec := RuntimeSpec{
		SessionID:     "s1",
		Agent:         "codex",
		RunMode:       "manual",
		Transport:     TransportStructured,
		WorkDir:       work,
		Emit:          coll.emit,
		EmitEphemeral: coll.ephemeral,
	}
	if err := rt.Start(context.Background(), spec); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = rt.Stop(context.Background(), "s1") })

	if !rt.Alive(context.Background(), "s1") {
		t.Fatal("expected session alive after start")
	}
	if err := rt.SendPrompt(context.Background(), "s1", "hi"); err != nil {
		t.Fatalf("send prompt: %v", err)
	}

	msg := coll.waitForType(t, EvMessage, 3*time.Second)
	var m Message
	if err := json.Unmarshal([]byte(msg.ev.Data), &m); err != nil || m.Text != "hi there" {
		t.Fatalf("message = %v %q", err, msg.ev.Data)
	}
	u := coll.waitForType(t, EvUsage, 3*time.Second)
	var usage Usage
	if err := json.Unmarshal([]byte(u.ev.Data), &usage); err != nil || usage.Input != 3 || usage.Output != 2 {
		t.Fatalf("usage = %v %q", err, u.ev.Data)
	}
	coll.waitForType(t, EvTurn, 3*time.Second)

	if err := rt.Stop(context.Background(), "s1"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if rt.Alive(context.Background(), "s1") {
		t.Fatal("expected session not alive after stop")
	}
}

// TestCodexStructuredRuntime_Live exercises the real `codex app-server` end to
// end. Gated on CLAVER_LIVE_CODEX because it needs an authenticated CLI and
// makes real model calls; skipped in CI and on unauthenticated hosts.
func TestCodexStructuredRuntime_Live(t *testing.T) {
	if os.Getenv("CLAVER_LIVE_CODEX") == "" {
		t.Skip("set CLAVER_LIVE_CODEX=1 to run the live codex smoke test")
	}
	rt := NewCodexStructuredRuntime("", "", nil)
	coll := &eventCollector{}
	spec := RuntimeSpec{
		SessionID:     "live-codex",
		Agent:         "codex",
		RunMode:       "manual",
		Transport:     TransportStructured,
		WorkDir:       t.TempDir(),
		Emit:          coll.emit,
		EmitEphemeral: coll.ephemeral,
	}
	if err := rt.Start(context.Background(), spec); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = rt.Stop(context.Background(), "live-codex") })

	if err := rt.SendPrompt(context.Background(), "live-codex", "Reply with exactly the word: pong"); err != nil {
		t.Fatalf("send prompt: %v", err)
	}
	coll.waitForType(t, EvTurn, 120*time.Second)
	if len(coll.byType(EvMessage)) == 0 {
		t.Fatal("no assistant message before turn completed")
	}
}
