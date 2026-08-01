package claudecode

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/cnlangzi/nightme/internal/agent"
)

// streamEvent is the on-the-wire JSON shape emitted by Claude Code in
// --output-format stream-json mode. Only the fields we consume are
// modeled; unknown fields are ignored by the decoder (json.Unmarshal is
// permissive by default for extra keys).
//
// Schema source: experimentally verified against Claude Code 2.1.220
// (npm/native install) and cross-referenced with cc-connect's
// internal/agent/claudecode/session.go. The schema is not officially
// documented by Anthropic; we treat unknown fields as forward-compatible.
type streamEvent struct {
	Type      string        `json:"type"`
	Subtype   string        `json:"subtype,omitempty"`
	Message   *assistantMsg `json:"message,omitempty"`
	SessionID string        `json:"session_id,omitempty"`

	// result fields
	Result     string `json:"result,omitempty"`
	DurationMs int64  `json:"duration_ms,omitempty"`
	IsError    bool   `json:"is_error,omitempty"`

	// permissive extras
	Raw json.RawMessage `json:"-"`
}

type assistantMsg struct {
	ID      string         `json:"id,omitempty"`
	Role    string         `json:"role,omitempty"`
	Model   string         `json:"model,omitempty"`
	Content []contentBlock `json:"content,omitempty"`
}

// contentBlock is the union shape for assistant message content. The
// Type field discriminates; only fields relevant to that type are set.
type contentBlock struct {
	Type string `json:"type"`

	// text / thinking
	Text     string `json:"text,omitempty"`
	Thinking string `json:"thinking,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result (in user-role message)
	ToolUseID string          `json:"tool_use_id,omitempty"`
	// Content is the tool's result payload. In Claude Code's
	// stream-json schema this can be a plain JSON string OR a
	// nested array of content blocks (multi-modal). We accept it
	// as RawMessage and stringify at emit time so the renderer
	// can surface a single-line summary in the rolling log.
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// pumpStream reads one JSON event per line from r and translates each
// into an agent.AgentEvent on events. It returns when r returns io.EOF
// or a permanent read error.
//
// Malformed lines are logged and skipped (do NOT abort the session —
// Claude Code may emit non-JSON noise during startup banner / hooks).
//
// On normal EOF, a final EventDone is emitted with the captured exit
// code (-1 if not yet observed). The events channel is then closed.
//
// Permissions (AskUserQuestion) are routed through askHandler if non-nil;
// otherwise they fall through to EventPermission with a default set of
// options. See ask.go for the dual-path (tool_use + text fallback)
// detection logic.
func pumpStream(r io.Reader, events chan<- agent.AgentEvent, askHandler askHandlerFunc, logger *slog.Logger) {
	scanner := bufio.NewScanner(r)
	// Allow long lines (Claude Code may emit large content blocks).
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var ev streamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			if logger != nil {
				logger.Warn("claudecode: invalid json event",
					"err", err,
					"line", truncateForLog(string(line), 200))
			}
			continue
		}

		translate(ev, events, askHandler, logger)
	}

	if err := scanner.Err(); err != nil {
		if logger != nil {
			logger.Warn("claudecode: stream read error", "err", err)
		}
		events <- agent.AgentEvent{
			Kind:  agent.EventError,
			Error: &agent.ErrorEvent{Err: fmt.Errorf("claudecode stream: %w", err)},
		}
	}
}

// translate converts one streamEvent into AgentEvent(s) on events.
// Returns nothing — events that don't map to AgentEvent (e.g. unknown
// type) are silently dropped (logged at debug).
func translate(ev streamEvent, events chan<- agent.AgentEvent, askHandler askHandlerFunc, logger *slog.Logger) {
	switch ev.Type {
	case "system":
		// system/init is informational; we surface it via EventText
		// so the channel can echo a "session initialized" indicator.
		// Other subtypes (e.g. status, hook) are ignored.
		if ev.Subtype == "init" {
			msg := fmt.Sprintf("session initialized (model: %s)", extractModel(ev))
			events <- agent.AgentEvent{Kind: agent.EventText, Text: msg}
		}

	case "assistant":
		if ev.Message == nil {
			return
		}
		for _, block := range ev.Message.Content {
			switch block.Type {
			case "text":
				if block.Text == "" {
					continue
				}
				// TEXT-FALLBACK (F-24 §5.3): when AskUserQuestion
				// isn't exposed as a tool_use, Claude Code falls back
				// to rendering a markdown question with "please pick
				// one". Detect that pattern and emit EventPermission
				// instead of EventText so the channel renders a
				// proper interactive card.
				if q := detectAskInText(block.Text); q != nil && askHandler != nil {
					emitAskFromText(*q, events, logger)
				} else {
					events <- agent.AgentEvent{
						Kind: agent.EventText,
						Text: block.Text,
					}
				}

			case "thinking":
				// Thinking blocks are surfaced as EventText with a
				// "[思考] " prefix so the channel layer (Feishu
				// Renderer) can render them as a 💭 entry in the
				// per-message activity log. The prefix lets the
				// renderer tell thinking from a final reply and
				// from a system-init blurb ("session initialized
				// (model: …)") which already carries its own
				// context. Empty blocks (Claude Code can emit a
				// zero-length thinking delta) are skipped.
				if strings.TrimSpace(block.Thinking) == "" {
					continue
				}
				events <- agent.AgentEvent{
					Kind: agent.EventText,
					Text: "[思考] " + block.Thinking,
				}

			case "tool_use":
				handleToolUse(block, events, askHandler, logger)

			default:
				if logger != nil {
					logger.Debug("claudecode: unknown assistant block type",
						"type", block.Type)
				}
			}
		}

	case "user":
		// user-role messages in stream-json carry tool_result blocks
		// (echoed back so the model sees its own tool calls).
		if ev.Message == nil {
			return
		}
		for _, block := range ev.Message.Content {
			if block.Type != "tool_result" {
				continue
			}
			events <- agent.AgentEvent{
				Kind: agent.EventToolEnd,
				ToolEnd: &agent.ToolEndEvent{
					Name:   block.Name,
					Output: stringifyToolResult(block.Content),
				},
			}
		}

	case "result":
		events <- agent.AgentEvent{
			Kind: agent.EventDone,
			Done: &agent.DoneEvent{ExitCode: 0},
		}

	default:
		if logger != nil {
			logger.Debug("claudecode: unknown event type", "type", ev.Type)
		}
	}
}

// handleToolUse routes a tool_use block. AskUserQuestion is intercepted
// via askHandler; other tools are emitted as EventToolStart.
func handleToolUse(block contentBlock, events chan<- agent.AgentEvent, askHandler askHandlerFunc, logger *slog.Logger) {
	if block.Name == "AskUserQuestion" && askHandler != nil {
		askHandler(block, events, logger)
		return
	}

	// Default: forward as a normal tool start. The channel can render
	// it as "🔧 Read(/tmp/foo.py)" etc.
	inputStr := string(block.Input)
	if len(inputStr) > 500 {
		inputStr = inputStr[:500] + "…"
	}
	events <- agent.AgentEvent{
		Kind: agent.EventToolStart,
		ToolStart: &agent.ToolStartEvent{
			ID:   block.ID,
			Name: block.Name,
			Args: inputStr,
		},
	}
}

// extractModel pulls the model name out of a system/init event. The
// field is not consistently located across Claude Code versions; we
// try the most likely spots and fall back to "".
func extractModel(ev streamEvent) string {
	if ev.Message != nil && ev.Message.Model != "" {
		return ev.Message.Model
	}
	// Some versions put model inside the raw event payload.
	var probe struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(ev.Raw, &probe)
	return probe.Model
}

// stringifyToolResult flattens a tool_result's content payload to a
// single-line string for the rolling-log display. Claude Code emits
// the content as either a JSON string (the common case) or a JSON
// array of content blocks (multi-modal). We accept both shapes and
// return a single-line summary that the renderer truncates to its
// own per-entry budget.
//
// The function is best-effort: malformed payloads return a JSON dump
// rather than an empty string, so the user at least sees that
// *something* came back.
func stringifyToolResult(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Case 1: content is a plain JSON string.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	// Case 2: content is an array of content blocks. Flatten
	// each block's text (or a short representation of non-text
	// blocks) with a newline separator.
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var out strings.Builder
		for i, b := range blocks {
			if i > 0 {
				out.WriteString(" | ")
			}
			switch b.Type {
			case "text":
				out.WriteString(b.Text)
			default:
				out.WriteString("[" + b.Type + "]")
			}
		}
		return out.String()
	}
	// Case 3: neither shape — return a compact JSON dump. The
	// renderer will truncate; the user sees something useful.
	return string(raw)
}

// truncateForLog caps a string for log lines so a runaway line does
// not blow up the log file.
func truncateForLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// strings.HasPrefix shim so we don't import strings just for one call.
// (kept as a small helper to avoid unused import when this file is
// trimmed down in future refactors)
var _ = strings.HasPrefix
