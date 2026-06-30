package sessions

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// claude_translate.go converts the `claude` CLI's stream-json output (one NDJSON
// object per line) into normalized events (normalize.go). It is a pure function
// so it can be table-tested against recorded fixtures (fixtures/claude/*.ndjson).
// Wire shapes are grounded in a real `claude` 2.1.139 capture (system/init,
// assistant, result) and the official @anthropic-ai/claude-agent-sdk source for
// content blocks and the can_use_tool control_request.

// --- Claude stream-json wire structs (only the fields we consume) ---

type claudeInnerMessage struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content"` // string or []claudeContentBlock
	Usage      *claudeWireUsage `json:"usage"`
	StopReason string           `json:"stop_reason"`
}

type claudeContentBlock struct {
	Type string `json:"type"`
	// text
	Text string `json:"text"`
	// thinking
	Thinking string `json:"thinking"`
	// tool_use
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	// tool_result
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"` // string or []block
	IsError   bool            `json:"is_error"`
}

type claudeWireUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// translateClaudeLine parses one NDJSON line and returns the normalized events
// it produces. Unknown/irrelevant lines (system, hooks, control_response,
// transcript_mirror) yield no events. A malformed line returns an error; the
// runtime logs and skips it rather than tearing down the session.
func translateClaudeLine(line []byte) ([]translated, error) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil, nil
	}
	var env struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		return nil, err
	}
	switch env.Type {
	case "assistant":
		return translateClaudeAssistant(line)
	case "user":
		return translateClaudeUser(line)
	case "result":
		return translateClaudeResult(line)
	case "stream_event":
		return translateClaudeStreamEvent(line)
	case "control_request":
		return translateClaudeControlRequest(line)
	default:
		return nil, nil
	}
}

func translateClaudeAssistant(line []byte) ([]translated, error) {
	var msg struct {
		Message claudeInnerMessage `json:"message"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return nil, err
	}
	blocks, _ := decodeContentBlocks(msg.Message.Content)

	var reasonings, tools []translated
	var text bytes.Buffer
	var textBlocks []MessageBlock
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text == "" {
				continue
			}
			if text.Len() > 0 {
				text.WriteByte('\n')
			}
			text.WriteString(b.Text)
			textBlocks = append(textBlocks, MessageBlock{Kind: "text", Text: b.Text})
		case "thinking":
			if b.Thinking != "" {
				reasonings = append(reasonings, translated{Type: EvReasoning, Payload: Reasoning{Text: b.Thinking}})
			}
		case "tool_use":
			tools = append(tools, translated{Type: EvToolCall, Payload: ToolCall{
				CallID: b.ID,
				Name:   b.Name,
				Args:   b.Input,
				Status: ToolStarted,
			}})
		}
	}
	// Stable UI order within one assistant message: reasoning, then the text
	// block, then any tool calls it requested.
	out := reasonings
	if text.Len() > 0 {
		out = append(out, translated{Type: EvMessage, Payload: Message{
			Role:   "assistant",
			Text:   text.String(),
			Blocks: textBlocks,
		}})
	}
	out = append(out, tools...)
	return out, nil
}

func translateClaudeUser(line []byte) ([]translated, error) {
	var msg struct {
		Message claudeInnerMessage `json:"message"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return nil, err
	}
	blocks, _ := decodeContentBlocks(msg.Message.Content)

	var out []translated
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		status := ToolCompleted
		if b.IsError {
			status = ToolFailed
		}
		out = append(out, translated{Type: EvToolCall, Payload: ToolCall{
			CallID: b.ToolUseID,
			Status: status,
			Result: contentToText(b.Content),
		}})
	}
	return out, nil
}

func translateClaudeResult(line []byte) ([]translated, error) {
	var r struct {
		Subtype    string           `json:"subtype"`
		IsError    bool             `json:"is_error"`
		Result     string           `json:"result"`
		StopReason string           `json:"stop_reason"`
		TotalCost  float64          `json:"total_cost_usd"`
		Usage      *claudeWireUsage `json:"usage"`
	}
	if err := json.Unmarshal(line, &r); err != nil {
		return nil, err
	}
	var out []translated
	if r.Usage != nil {
		out = append(out, translated{Type: EvUsage, Payload: Usage{
			Input:   r.Usage.InputTokens,
			Output:  r.Usage.OutputTokens,
			Cache:   r.Usage.CacheReadInputTokens + r.Usage.CacheCreationInputTokens,
			CostUSD: r.TotalCost,
		}})
	}
	if r.IsError {
		out = append(out, translated{Type: EvError, Payload: ErrorEvent{
			Message: r.Result,
			Fatal:   false,
		}})
	}
	reason := r.StopReason
	if reason == "" {
		reason = r.Subtype
	}
	out = append(out, translated{Type: EvTurn, Payload: Turn{State: TurnComplete, StopReason: reason}})
	return out, nil
}

func translateClaudeStreamEvent(line []byte) ([]translated, error) {
	var se struct {
		Event struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		} `json:"event"`
	}
	if err := json.Unmarshal(line, &se); err != nil {
		return nil, err
	}
	if se.Event.Type == "content_block_delta" && se.Event.Delta.Type == "text_delta" && se.Event.Delta.Text != "" {
		return []translated{{
			Type:      EvMessageDelta,
			Payload:   MessageDelta{Role: "assistant", Text: se.Event.Delta.Text},
			Ephemeral: true,
		}}, nil
	}
	return nil, nil
}

func translateClaudeControlRequest(line []byte) ([]translated, error) {
	var cr struct {
		RequestID string `json:"request_id"`
		Request   struct {
			Subtype     string          `json:"subtype"`
			ToolName    string          `json:"tool_name"`
			Input       json.RawMessage `json:"input"`
			Title       string          `json:"title"`
			Description string          `json:"description"`
		} `json:"request"`
	}
	if err := json.Unmarshal(line, &cr); err != nil {
		return nil, err
	}
	if cr.Request.Subtype != "can_use_tool" {
		return nil, nil
	}
	summary := cr.Request.Title
	if summary == "" {
		summary = cr.Request.ToolName
	}
	detail := cr.Request.Description
	if detail == "" {
		detail = string(cr.Request.Input)
	}
	var out []translated
	if cr.Request.ToolName == "ExitPlanMode" {
		out = append(out, translated{
			Type:    EvPlan,
			Payload: Plan{Items: parsePlanItems(cr.Request.Input), Gating: true},
		})
	}
	out = append(out, translated{
		Type: EvApprovalRequest,
		Payload: ApprovalRequest{
			RequestID: cr.RequestID,
			Kind:      approvalKindForTool(cr.Request.ToolName),
			Summary:   summary,
			Detail:    detail,
			Options:   []string{DecisionAllow, DecisionAllowAlways, DecisionDeny},
		},
	})
	return out, nil
}

// approvalKindForTool maps a Claude tool name to a normalized approval kind.
func approvalKindForTool(tool string) string {
	switch tool {
	case "Edit", "Write", "NotebookEdit", "MultiEdit":
		return ApprovalEdit
	case "ExitPlanMode", "EnterPlanMode":
		return ApprovalPlan
	default:
		return ApprovalCommand
	}
}

// parsePlanItems normalizes an ExitPlanMode tool input ({"plan": "..."}) into
// display plan items: one per non-empty markdown line with leading list/heading
// markers stripped. Falls back to a single item carrying the whole text.
func parsePlanItems(input json.RawMessage) []PlanItem {
	var in struct {
		Plan string `json:"plan"`
	}
	_ = json.Unmarshal(input, &in)
	text := strings.TrimSpace(in.Plan)
	if text == "" {
		return nil
	}
	var items []PlanItem
	for _, raw := range strings.Split(text, "\n") {
		line := stripListMarker(strings.TrimSpace(raw))
		if line == "" {
			continue
		}
		items = append(items, PlanItem{Title: line, Status: "pending"})
	}
	if len(items) == 0 {
		return []PlanItem{{Title: text, Status: "pending"}}
	}
	return items
}

// stripListMarker removes a leading markdown heading, bullet, or ordered-list
// marker from one line.
func stripListMarker(line string) string {
	h := 0
	for h < len(line) && line[h] == '#' {
		h++
	}
	if h > 0 && h < len(line) && line[h] == ' ' {
		return strings.TrimSpace(line[h+1:])
	}
	for _, p := range []string{"- ", "* ", "+ "} {
		if strings.HasPrefix(line, p) {
			return strings.TrimSpace(line[len(p):])
		}
	}
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i > 0 && i < len(line) && line[i] == '.' {
		return strings.TrimSpace(line[i+1:])
	}
	return line
}

// decodeContentBlocks parses a message's content field, which is either a JSON
// string (rendered as a single text block) or an array of typed blocks.
func decodeContentBlocks(raw json.RawMessage) ([]claudeContentBlock, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, false
	}
	if raw[0] == '[' {
		var blocks []claudeContentBlock
		if err := json.Unmarshal(raw, &blocks); err != nil {
			return nil, false
		}
		return blocks, true
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, false
		}
		return []claudeContentBlock{{Type: "text", Text: s}}, true
	}
	return nil, false
}

// contentToText flattens a tool_result content field (string or array of blocks)
// into plain text for display.
func contentToText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
		return ""
	}
	if raw[0] == '[' {
		var blocks []claudeContentBlock
		if err := json.Unmarshal(raw, &blocks); err != nil {
			return ""
		}
		var b bytes.Buffer
		for _, blk := range blocks {
			if blk.Type == "text" && blk.Text != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(blk.Text)
			}
		}
		return b.String()
	}
	// Some tool results report a bare number/bool; render it literally.
	if raw[0] == 't' || raw[0] == 'f' || (raw[0] >= '0' && raw[0] <= '9') || raw[0] == '-' {
		if _, err := strconv.Atoi(string(raw)); err == nil {
			return string(raw)
		}
	}
	return ""
}
