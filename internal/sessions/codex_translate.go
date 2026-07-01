package sessions

import (
	"bytes"
	"encoding/json"
	"strings"
)

// codex_translate.go converts the `codex app-server` JSON-RPC 2.0 stream (one
// object per line over stdio) into the same normalized events (normalize.go)
// the Claude runtime emits, so the Flutter transcript renders Codex unchanged.
// It is a pure function table-tested against recorded fixtures
// (fixtures/codex/*.jsonl). Wire shapes are source-verified from
// `codex app-server generate-json-schema` (codex-cli 0.142.5); the committed
// schema and a drift check live in codex_schema_test.go.

// Server->client notification methods (no id) and approval request methods
// (carry an id we must answer). Only the subset the normalized model needs is
// handled; every other method is intentionally ignored.
const (
	codexMethodTurnStarted   = "turn/started"
	codexMethodTurnCompleted = "turn/completed"
	codexMethodTokenUsage    = "thread/tokenUsage/updated"
	codexMethodPlanUpdated   = "turn/plan/updated"
	codexMethodAgentMsgDelta = "item/agentMessage/delta"
	codexMethodItemStarted   = "item/started"
	codexMethodItemCompleted = "item/completed"
	codexMethodError         = "error"
	codexMethodCmdApproval   = "item/commandExecution/requestApproval"
	codexMethodFileApproval  = "item/fileChange/requestApproval"
	codexMethodExecApproval  = "execCommandApproval" // legacy ReviewDecision form
	codexMethodPatchApproval = "applyPatchApproval"  // legacy ReviewDecision form
)

// codexApprovalMethods is the set of server->client requests this runtime
// answers as a normalized approval. Anything else with an id is declined by the
// conn so the app-server is never left waiting.
var codexApprovalMethods = map[string]bool{
	codexMethodCmdApproval:   true,
	codexMethodFileApproval:  true,
	codexMethodExecApproval:  true,
	codexMethodPatchApproval: true,
}

// --- codex app-server wire structs (only the fields we consume) ---

type codexThreadItem struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	// agentMessage
	Text string `json:"text"`
	// reasoning
	Content json.RawMessage `json:"content"` // []string
	Summary []string        `json:"summary"`
	// commandExecution
	Command          string `json:"command"`
	Cwd              string `json:"cwd"`
	AggregatedOutput string `json:"aggregatedOutput"`
	// fileChange
	Changes []codexFileChange `json:"changes"`
	// mcpToolCall
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	// shared lifecycle / error
	Status  string `json:"status"`
	Message string `json:"message"`
}

type codexFileChange struct {
	Path string `json:"path"`
	Diff string `json:"diff"`
}

type codexTokenBreakdown struct {
	CachedInputTokens int `json:"cachedInputTokens"`
	InputTokens       int `json:"inputTokens"`
	OutputTokens      int `json:"outputTokens"`
}

// translateCodexLine parses one JSON-RPC line and returns the normalized events
// it produces. Responses to our own client requests (no method) and unhandled
// notifications yield none; a malformed line returns an error the runtime logs
// and skips.
func translateCodexLine(line []byte) ([]translated, error) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil, nil
	}
	var env struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		ID     json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		return nil, err
	}
	if env.Method == "" {
		return nil, nil // response to a client request; routed by the conn, not translated
	}
	switch env.Method {
	case codexMethodTurnStarted:
		return []translated{{Type: EvTurn, Payload: Turn{State: TurnStarted}}}, nil
	case codexMethodTurnCompleted:
		return translateCodexTurnCompleted(env.Params)
	case codexMethodTokenUsage:
		return translateCodexTokenUsage(env.Params)
	case codexMethodPlanUpdated:
		return translateCodexPlan(env.Params)
	case codexMethodAgentMsgDelta:
		return translateCodexAgentDelta(env.Params)
	case codexMethodItemStarted:
		return translateCodexItem(env.Params, false)
	case codexMethodItemCompleted:
		return translateCodexItem(env.Params, true)
	case codexMethodError:
		return translateCodexError(env.Params)
	case codexMethodCmdApproval, codexMethodExecApproval:
		return translateCodexCommandApproval(env.ID, env.Params)
	case codexMethodFileApproval, codexMethodPatchApproval:
		return translateCodexFileApproval(env.ID, env.Params)
	default:
		return nil, nil
	}
}

// translateCodexItem maps an item/started or item/completed notification to the
// normalized events for its item type. Messages and reasoning are emitted only
// when finalized (completed); tool-like items emit a tool_call on both start and
// completion so the UI shows the running -> done lifecycle, and a file change
// emits its proposed diff(s) on start so they are visible before any approval.
func translateCodexItem(params json.RawMessage, completed bool) ([]translated, error) {
	var p struct {
		Item codexThreadItem `json:"item"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	it := p.Item
	switch it.Type {
	case "agentMessage":
		if !completed || it.Text == "" {
			return nil, nil // streamed live via item/agentMessage/delta until finalized
		}
		return []translated{{Type: EvMessage, Payload: Message{
			Role:   "assistant",
			Text:   it.Text,
			Blocks: []MessageBlock{{Kind: "text", Text: it.Text}},
		}}}, nil
	case "reasoning":
		if !completed {
			return nil, nil
		}
		text := codexReasoningText(it)
		if text == "" {
			return nil, nil
		}
		return []translated{{Type: EvReasoning, Payload: Reasoning{Text: text}}}, nil
	case "commandExecution":
		tc := ToolCall{CallID: it.ID, Name: "shell", Args: codexCommandArgs(it), Status: codexToolStatus(it.Status)}
		if completed {
			tc.Result = it.AggregatedOutput
		}
		return []translated{{Type: EvToolCall, Payload: tc}}, nil
	case "fileChange":
		out := []translated{{Type: EvToolCall, Payload: ToolCall{
			CallID: it.ID, Name: "apply_patch", Args: codexFileChangeArgs(it), Status: codexToolStatus(it.Status),
		}}}
		if !completed {
			for _, ch := range it.Changes {
				out = append(out, translated{Type: EvDiff, Payload: Diff{CallID: it.ID, Path: ch.Path, Patch: ch.Diff}})
			}
		}
		return out, nil
	case "mcpToolCall":
		tc := ToolCall{CallID: it.ID, Name: codexMcpName(it), Args: it.Arguments, Status: codexToolStatus(it.Status)}
		return []translated{{Type: EvToolCall, Payload: tc}}, nil
	case "error":
		if !completed {
			return nil, nil
		}
		return []translated{{Type: EvError, Payload: ErrorEvent{Message: it.Message, Fatal: false}}}, nil
	default:
		return nil, nil
	}
}

func translateCodexTurnCompleted(params json.RawMessage) ([]translated, error) {
	var p struct {
		Turn struct {
			Status string `json:"status"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	return []translated{{Type: EvTurn, Payload: Turn{State: TurnComplete, StopReason: p.Turn.Status}}}, nil
}

// translateCodexTokenUsage emits a usage event from the turn's own breakdown
// (`last`). The transcript chip accumulates usage events, so emitting the
// per-turn delta keeps the cumulative total correct across turns.
func translateCodexTokenUsage(params json.RawMessage) ([]translated, error) {
	var p struct {
		TokenUsage struct {
			Last codexTokenBreakdown `json:"last"`
		} `json:"tokenUsage"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	b := p.TokenUsage.Last
	if b.InputTokens == 0 && b.OutputTokens == 0 && b.CachedInputTokens == 0 {
		return nil, nil
	}
	return []translated{{Type: EvUsage, Payload: Usage{
		Input: b.InputTokens, Output: b.OutputTokens, Cache: b.CachedInputTokens,
	}}}, nil
}

// translateCodexPlan maps a Codex update_plan to a normalized plan. Codex plans
// are display-only: Gating is always false (there is no Codex plan-mode gate).
func translateCodexPlan(params json.RawMessage) ([]translated, error) {
	var p struct {
		Plan []struct {
			Step   string `json:"step"`
			Status string `json:"status"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	items := make([]PlanItem, 0, len(p.Plan))
	for _, s := range p.Plan {
		step := strings.TrimSpace(s.Step)
		if step == "" {
			continue
		}
		items = append(items, PlanItem{Title: step, Status: codexPlanStatus(s.Status)})
	}
	if len(items) == 0 {
		return nil, nil
	}
	return []translated{{Type: EvPlan, Payload: Plan{Items: items, Gating: false}}}, nil
}

func translateCodexAgentDelta(params json.RawMessage) ([]translated, error) {
	var p struct {
		Delta string `json:"delta"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	if p.Delta == "" {
		return nil, nil
	}
	return []translated{{Type: EvMessageDelta, Payload: MessageDelta{Role: "assistant", Text: p.Delta}, Ephemeral: true}}, nil
}

// translateCodexError maps an error notification to a non-fatal error event: a
// turn error does not end the thread, which can still accept another prompt.
func translateCodexError(params json.RawMessage) ([]translated, error) {
	var p struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	msg := p.Error.Message
	if msg == "" {
		msg = "codex error"
	}
	return []translated{{Type: EvError, Payload: ErrorEvent{Message: msg, Fatal: false}}}, nil
}

func translateCodexCommandApproval(id, params json.RawMessage) ([]translated, error) {
	var p struct {
		Command json.RawMessage `json:"command"` // string or []string
		Cwd     json.RawMessage `json:"cwd"`
		Reason  string          `json:"reason"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	cmd := codexCommandText(p.Command)
	summary := cmd
	if summary == "" {
		summary = "Run a command"
	}
	detail, _ := json.Marshal(map[string]any{"command": cmd, "cwd": codexStringOrRaw(p.Cwd)})
	return []translated{{Type: EvApprovalRequest, Payload: ApprovalRequest{
		RequestID: codexRequestID(id),
		Kind:      ApprovalCommand,
		Summary:   summary,
		Detail:    string(detail),
		Options:   []string{DecisionAllow, DecisionAllowAlways, DecisionDeny},
	}}}, nil
}

func translateCodexFileApproval(id, params json.RawMessage) ([]translated, error) {
	var p struct {
		FileChanges json.RawMessage `json:"fileChanges"` // legacy applyPatchApproval only
		Reason      string          `json:"reason"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	// The v2 item/fileChange/requestApproval references the item by id; its diff
	// was already emitted by the preceding item/started(fileChange). The legacy
	// applyPatchApproval carries the changes inline.
	detail := codexPatchDetail(p.FileChanges, p.Reason)
	return []translated{{Type: EvApprovalRequest, Payload: ApprovalRequest{
		RequestID: codexRequestID(id),
		Kind:      ApprovalEdit,
		Summary:   "Apply file changes",
		Detail:    detail,
		Options:   []string{DecisionAllow, DecisionAllowAlways, DecisionDeny},
	}}}, nil
}

// --- small helpers ---

func codexToolStatus(s string) string {
	switch s {
	case "completed":
		return ToolCompleted
	case "failed", "declined":
		return ToolFailed
	default: // inProgress and anything unknown render as a running card
		return ToolStarted
	}
}

func codexPlanStatus(s string) string {
	switch s {
	case "inProgress":
		return "in_progress"
	case "completed":
		return "completed"
	default:
		return "pending"
	}
}

func codexReasoningText(it codexThreadItem) string {
	var parts []string
	_ = json.Unmarshal(it.Content, &parts)
	if len(parts) == 0 {
		parts = it.Summary
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func codexCommandArgs(it codexThreadItem) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"command": it.Command, "cwd": it.Cwd})
	return b
}

func codexFileChangeArgs(it codexThreadItem) json.RawMessage {
	paths := make([]string, 0, len(it.Changes))
	for _, ch := range it.Changes {
		paths = append(paths, ch.Path)
	}
	b, _ := json.Marshal(map[string]any{"paths": paths})
	return b
}

func codexMcpName(it codexThreadItem) string {
	if it.Server != "" && it.Tool != "" {
		return it.Server + "." + it.Tool
	}
	if it.Tool != "" {
		return it.Tool
	}
	return "mcp_tool"
}

// codexCommandText renders a command that is either a JSON string or a JSON
// array of argv strings into a single display string.
func codexCommandText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var argv []string
	if json.Unmarshal(raw, &argv) == nil {
		return strings.Join(argv, " ")
	}
	return strings.TrimSpace(string(raw))
}

func codexStringOrRaw(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}

// codexPatchDetail builds the ApprovalSheet detail for a file-change approval.
// When the legacy applyPatchApproval carries inline changes, it synthesizes a
// {diff} payload the sheet renders as a unified diff; otherwise it falls back to
// the reason (the proposed diff is already shown inline in the transcript).
func codexPatchDetail(fileChanges json.RawMessage, reason string) string {
	var changes map[string]struct {
		Diff string `json:"diff"`
	}
	if len(fileChanges) > 0 && json.Unmarshal(fileChanges, &changes) == nil && len(changes) > 0 {
		var b strings.Builder
		for path, ch := range changes {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString("@@ " + path + " @@\n")
			b.WriteString(ch.Diff)
		}
		out, _ := json.Marshal(map[string]any{"diff": b.String()})
		return string(out)
	}
	if reason != "" {
		out, _ := json.Marshal(map[string]any{"reason": reason})
		return string(out)
	}
	return "{}"
}

// codexRequestID renders a JSON-RPC id (string or number) as the stable request
// id string carried on the normalized approval and echoed back on the response.
func codexRequestID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return strings.Trim(strings.TrimSpace(string(raw)), `"`)
}
