// SSE event → agent.AgentEvent translator.
//
// The opencode server pushes a discriminated union of events on the
// SSE stream. Each event has a `type` field and a `properties` blob.
// We dispatch on `type` and only act on the ones nightme's runtime
// understands; everything else is logged at debug level and dropped.
//
// Lifecycle:
//
//	event "session.idle"             → EventAgentDone{Reason:"settled"}
//	event "session.error"            → EventAgentError
//	event "session.compacted"        → EventAgentReady (footer update)
//	event "message.part.updated"     → part → text / tool / reasoning
//	event "permission.asked"         → EventAgentPermission
//
// F-52 granularity contract: text / reasoning Parts are already part
// boundaries on the opencode side, so we emit one EventAgentText per
// part without buffering. This is closer to claudecode's behaviour
// (one per content block) than to pi's (token-level buffer + flush at
// text_end).
package opencode

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// translator turns raw SSE events into agent.AgentEvent values. It is
// internal state used by the agent's lifecycle goroutine. The
// translator owns no goroutines itself; it is driven by decodeSSE
// callbacks.
type translator struct {
	deliver func(agent.AgentEvent) agent.AgentEvent

	// Static context stamped on every event.
	agentName string
	workspace string
	branch    string

	// Static session identity carved at handshake.
	sessionID string
	model     string

	// pendingTools correlates ToolStart → ToolEnd by callID. We do
	// not key this on the internal part.id because opencode re-uses
	// the same callID across the running → completed states.
	pendingTools map[string]toolEntry

	// lastUsage is the most recent /usage_update payload. We
	// stash it on the translator so the next session.idle can
	// forward it on Done.Usage without re-emitting the wire event.
	lastUsage *agent.UsageInfo
}

type toolEntry struct {
	name string
	args string
}

// newTranslator builds a translator. deliver is the live Agent's
// deliver helper; the bridge's session lifecycle calls deliver on
// every produced event.
func newTranslator(deliver func(agent.AgentEvent) agent.AgentEvent, agentName, workspace, branch, sessionID, model string) *translator {
	return &translator{
		deliver:      deliver,
		agentName:    agentName,
		workspace:    workspace,
		branch:       branch,
		sessionID:    sessionID,
		model:        model,
		pendingTools: make(map[string]toolEntry),
	}
}

// handleEvent is the entry point invoked by decodeSSE for each parsed
// event. Returns nil so the stream stays alive; SSE-level errors are
// the caller's problem.
func (t *translator) handleEvent(ev SessionEvent) error {
	switch ev.Type {
	case "":
		return nil
	// "message.updated" / "message.removed" are session-wide signals
	// we don't render; ignore.
	case "message.updated", "message.removed":
		return nil
	case "message.part.updated":
		var p struct {
			Part Part `json:"part"`
		}
		if err := json.Unmarshal(ev.properties(), &p); err != nil {
			return nil
		}
		t.handlePart(p.Part)

	// opencode 1.18+ streams text via session.next.text.delta on
	// the global event bus. The per-session message.part.updated
	// path still works for older releases. We accept both so the
	// bridge renders text on whatever system the user is running.
	case "session.next.text.delta":
		var p struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(ev.properties(), &p); err == nil && p.Text != "" {
			t.deliver(agent.AgentEvent{
				Kind:      agent.EventAgentText,
				SessionID: t.sessionID,
				Model:     t.model,
				AgentName: t.agentName,
				Workspace: t.workspace,
				Branch:    t.branch,
				Text:      p.Text,
			})
		}
	case "session.next.step.started", "session.next.step.ended":
		// Tool lifecycle in the new event taxonomy. The actual
		// callID / tool name are not in the test fixtures yet —
		// once we have a real sample we can correlate by callID.
		// For now we log only.
		oLog("sse: session.next.step", "type", ev.Type)
	case "session.idle", "session.next.idle":
		// Carry forward the last-seen usage, if any, so the
		// channel footer can render totals. The Done.Usage field
		// is the canonical source for runtime accumulation per
		// F-49 §1.2.
		//
		// opencode 1.18+ renamed the event from `session.idle` to
		// `session.next.idle` as part of the session.next.*
		// taxonomy rewrite. We accept both for forward
		// compatibility with older releases.
		usage := t.lastUsage
		t.deliver(agent.AgentEvent{
			Kind:      agent.EventAgentDone,
			SessionID: t.sessionID,
			Model:     t.model,
			AgentName: t.agentName,
			Workspace: t.workspace,
			Branch:    t.branch,
			Done: &agent.AgentDoneEvent{
				Reason:  "settled",
				ExitCode: 0,
				Usage:   usage,
			},
		})
	case "session.error":
		var p struct {
			Error json.RawMessage `json:"error"`
		}
		_ = json.Unmarshal(ev.properties(), &p)
		t.deliver(agent.AgentEvent{
			Kind:      agent.EventAgentError,
			SessionID: t.sessionID,
			Model:     t.model,
			AgentName: t.agentName,
			Workspace: t.workspace,
			Branch:    t.branch,
			Err:       fmt.Errorf("opencode session error: %s", string(p.Error)),
		})
	case "session.compacted":
		// Compaction doesn't have a dedicated AgentEvent in the
		// current schema. Synthesize a fresh EventAgentReady so the
		// runtime can refresh SessionID/Model tokens. The SessionID
		// is unchanged; only the token counts reset (handled by the
		// next /prompt response).
		t.deliver(agent.AgentEvent{
			Kind:      agent.EventAgentReady,
			SessionID: t.sessionID,
			Model:     t.model,
			AgentName: t.agentName,
			Workspace: t.workspace,
			Branch:    t.branch,
		})
	case "usage_update":
		var p UsageUpdate
		if err := json.Unmarshal(ev.properties(), &p); err == nil {
			t.lastUsage = p.toUsageInfo()
		}
	case "current_mode_update":
		var p struct {
			CurrentModeID string `json:"currentModeId"`
		}
		if err := json.Unmarshal(ev.properties(), &p); err == nil && p.CurrentModeID != "" {
			// Cache mode on the translator; not surfaced to the
			// runtime yet because the agent package does not have a
			// dedicated mode event. Re-emit EventAgentReady so the
			// channel can refresh its header.
			t.deliver(agent.AgentEvent{
				Kind:      agent.EventAgentReady,
				SessionID: t.sessionID,
				Model:     t.model,
				AgentName: t.agentName,
				Workspace: t.workspace,
				Branch:    t.branch,
			})
		}
	case "available_commands_update":
		// Stage 2 stores the latest slash command list in the
		// translator so future integrations can route unknown
		// /commands to opencode. Currently we just log it.
		var p struct {
			AvailableCommands []json.RawMessage `json:"availableCommands"`
		}
		if err := json.Unmarshal(ev.properties(), &p); err == nil {
			oLog("sse: available_commands_update", "count", len(p.AvailableCommands))
		}
	case "permission.asked":
		var p PermissionAsked
		if err := json.Unmarshal(ev.properties(), &p); err == nil {
			t.handlePermission(p)
		}
	default:
		// Unknown event → debug log, do not kill the stream.
		oLog("sse: unknown event type", "type", ev.Type)
	}
	return nil
}

// ─── part types ──────────────────────────────────────────────────

// Part is the union of message part types. We only model the fields
// we read; the rest is left as raw JSON for future translation.
type Part struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	MessageID string `json:"messageID"`
	Type      string `json:"type"`

	// text / reasoning
	Text string `json:"text,omitempty"`

	// tool
	Tool   string          `json:"tool,omitempty"`
	CallID string          `json:"callID,omitempty"`
	State  json.RawMessage `json:"state,omitempty"`

	// text / reasoning / file
	Synthetic bool `json:"synthetic,omitempty"`
	Ignored   bool `json:"ignored,omitempty"`

	// file
	MIME     string `json:"mime,omitempty"`
	Filename string `json:"filename,omitempty"`
	URL      string `json:"url,omitempty"`
}

// handlePart dispatches a single Part update to the right AgentEvent.
//
// Opencode pushes a fresh `message.part.updated` for every state
// transition: a tool gets pending → running → completed in three
// separate updates. We correlate by callID and emit Start / Update /
// End accordingly.
func (t *translator) handlePart(p Part) {
	if p.Synthetic || p.Ignored {
		// Skip session summaries / hidden parts that are bookkeeping
		// noise (e.g. tool permission rationale).
		return
	}

	switch p.Type {
	case "text":
		if p.Text == "" {
			return
		}
		t.deliver(agent.AgentEvent{
			Kind:      agent.EventAgentText,
			SessionID: t.sessionID,
			Model:     t.model,
			AgentName: t.agentName,
			Workspace: t.workspace,
			Branch:    t.branch,
			Text:      p.Text,
		})
	case "reasoning":
		if p.Text == "" {
			return
		}
		t.deliver(agent.AgentEvent{
			Kind:      agent.EventAgentText,
			SessionID: t.sessionID,
			Model:     t.model,
			AgentName: t.agentName,
			Workspace: t.workspace,
			Branch:    t.branch,
			Text:      "[思考] " + p.Text,
		})
	case "tool":
		t.handleToolPart(p)
	case "agent", "subtask", "step-start", "step-finish", "snapshot",
		"patch", "retry", "compaction":
		// Internal flow markers — log only when in debug mode.
		oLog("sse: ignored part type", "type", p.Type)
	default:
		oLog("sse: unknown part type", "type", p.Type)
	}
}

// handleToolPart emits the tool lifecycle. The state field of opencode
// is itself a discriminated union (ToolState = pending | running |
// completed | error); we decode only the `status` field.
func (t *translator) handleToolPart(p Part) {
	var state struct {
		Status string          `json:"status"`
		Input  json.RawMessage `json:"input"`
		Output json.RawMessage `json:"output"`
		Error  json.RawMessage `json:"error"`
	}
	_ = json.Unmarshal(p.State, &state)

	// Normalize the tool name so the channel footer can render
	// Claude-style titles (Bash, Read, Write) instead of the
	// opencode-internal slugs (bash, read, write). We capitalize
	// the first letter — sufficient for the most common tools.
	name := normalizeToolName(p.Tool)

	switch state.Status {
	case "pending":
		args := stringOrEmpty(state.Input)
		t.pendingTools[p.CallID] = toolEntry{
			name: name,
			args: args,
		}
		t.deliver(agent.AgentEvent{
			Kind:      agent.EventAgentToolStart,
			SessionID: t.sessionID,
			Model:     t.model,
			AgentName: t.agentName,
			Workspace: t.workspace,
			Branch:    t.branch,
			ToolStart: &agent.AgentToolStartEvent{
				ID:   p.CallID,
				Name: name,
				Args: args,
			},
		})
	case "running":
		// opencode does not currently emit a per-update output
		// delta for running tools; the partial stdout is folded
		// into the `completed` state's `output` field. We emit
		// an Update event so the channel layer can render a
		// "running" indicator if it wants.
		entry, ok := t.pendingTools[p.CallID]
		if !ok {
			entry = toolEntry{name: name}
		}
		args := entry.args
		if args == "" {
			args = stringOrEmpty(state.Input)
		}
		t.deliver(agent.AgentEvent{
			Kind:      agent.EventAgentToolStart,
			SessionID: t.sessionID,
			Model:     t.model,
			AgentName: t.agentName,
			Workspace: t.workspace,
			Branch:    t.branch,
			ToolStart: &agent.AgentToolStartEvent{
				ID:   p.CallID,
				Name: name,
				Args: args,
			},
		})
	case "completed":
		entry, ok := t.pendingTools[p.CallID]
		if !ok {
			entry = toolEntry{name: name}
		}
		delete(t.pendingTools, p.CallID)
		t.deliver(agent.AgentEvent{
			Kind:      agent.EventAgentToolEnd,
			SessionID: t.sessionID,
			Model:     t.model,
			AgentName: t.agentName,
			Workspace: t.workspace,
			Branch:    t.branch,
			ToolEnd: &agent.AgentToolEndEvent{
				ID:     p.CallID,
				Name:   entry.name,
				Args:   entry.args,
				Output: stringOrEmpty(state.Output),
			},
		})
	case "error":
		entry, ok := t.pendingTools[p.CallID]
		if !ok {
			entry = toolEntry{name: name}
		}
		delete(t.pendingTools, p.CallID)
		t.deliver(agent.AgentEvent{
			Kind:      agent.EventAgentToolEnd,
			SessionID: t.sessionID,
			Model:     t.model,
			AgentName: t.agentName,
			Workspace: t.workspace,
			Branch:    t.branch,
			ToolEnd: &agent.AgentToolEndEvent{
				ID:     p.CallID,
				Name:   entry.name,
				Args:   entry.args,
				Output: stringOrEmpty(state.Output),
			},
			Err: fmt.Errorf("opencode tool error: %s", stringOrEmpty(state.Error)),
		})
	default:
		oLog("sse: unknown tool state", "state", state.Status, "callID", p.CallID)
	}
}

// normalizeToolName maps an opencode tool slug to the Claude-style
// Title-Case name the channel footer / receipts expect. We do not
// have a full tool catalog here; a handful of common tools get
// explicit maps, the rest fall through to a capitalised first
// letter ("read" → "Read", "todowrite" → "Todowrite"). When opencode
// names a tool the channel adapter does not recognize, the user
// sees a less polished but still functional name.
func normalizeToolName(raw string) string {
	if raw == "" {
		return ""
	}
	// Already canonical (mixed case ≥ 2 chars) — leave alone.
	if strings.ToUpper(raw[:1]) == raw[:1] && len(raw) > 1 && raw[1:] != strings.ToUpper(raw[1:]) {
		return raw
	}
	// Known aliases.
	switch strings.ToLower(raw) {
	case "bash":
		return "Bash"
	case "read":
		return "Read"
	case "write":
		return "Write"
	case "edit":
		return "Edit"
	case "glob":
		return "Glob"
	case "grep":
		return "Grep"
	case "task":
		return "Task"
	case "webfetch":
		return "WebFetch"
	case "websearch":
		return "WebSearch"
	case "todowrite":
		return "TodoWrite"
	case "todoread":
		return "TodoRead"
	}
	// Default: capitalise the first letter.
	if raw == "" {
		return raw
	}
	return strings.ToUpper(raw[:1]) + raw[1:]
}

// ─── usage tracking ──────────────────────────────────────────────

// UsageUpdate is the payload of the `usage_update` SSE event. The
// shape mirrors the OpenAPI schema; we only read the fields we
// render. Tokens: in + out + cache create + cache read all default
// to 0 if missing. CostUSD is optional; 0 means "unknown".
type UsageUpdate struct {
	Used  int64 `json:"used"`
	Size  int64 `json:"size"`
	Cost  *struct {
		Amount   float64 `json:"amount"`
		Currency string  `json:"currency"`
	} `json:"cost,omitempty"`
	// Tokens is the API-reported split used by opencode. When
	// present we use it verbatim; otherwise we split Used/4
	// heuristically (F-49 §1.6 last-resort fallback).
	Tokens *struct {
		Input  int `json:"input"`
		Output int `json:"output"`
		Cache  *struct {
			Read     int `json:"read"`
			Creation int `json:"write"`
		} `json:"cache,omitempty"`
	} `json:"tokens,omitempty"`
}

// toUsageInfo converts the wire payload into the runtime's
// UsageInfo. The translation mirrors the codex/pi pattern: cache
// tokens are reported separately; context window is filled in if
// the server sent `size` so the channel footer can render the
// denominator.
func (u UsageUpdate) toUsageInfo() *agent.UsageInfo {
	info := &agent.UsageInfo{}
	if u.Tokens != nil {
		info.InputTokens = u.Tokens.Input
		info.OutputTokens = u.Tokens.Output
		if u.Tokens.Cache != nil {
			info.CacheReadInputTokens = u.Tokens.Cache.Read
			info.CacheCreationInputTokens = u.Tokens.Cache.Creation
		}
	}
	if u.Cost != nil {
		info.CostUSD = u.Cost.Amount
	}
	if u.Size > 0 {
		// Size is the API-reported context window. The bridge
		// forwards it verbatim; the channel footer renders it
		// alongside the percentage.
		info.ContextWindow = int(u.Size)
	}
	return info
}


// PermissionAsked is the payload of the `permission.asked` SSE event.
type PermissionAsked struct {
	SessionID  string          `json:"sessionID"`
	ID         string          `json:"id"`
	Permission string          `json:"permission"`
	Patterns   []string        `json:"patterns"`
	Metadata   json.RawMessage `json:"metadata"`
	Always     []string        `json:"always"`
	Tool       json.RawMessage `json:"tool"`
}

func (t *translator) handlePermission(p PermissionAsked) {
	// We surface the available options as the optionId strings the
	// server itself accepts ("once" / "always" / "reject"), so the
	// channel layer can pass the user's choice back verbatim via
	// SendPermission.
	options := []string{"once", "always", "reject"}

	// Carve a short description from metadata or the patterns list.
	desc := stringOrEmpty(p.Metadata)
	if desc == "" && len(p.Patterns) > 0 {
		desc = p.Patterns[0]
	}

	t.deliver(agent.AgentEvent{
		Kind:      agent.EventAgentPermission,
		SessionID: t.sessionID,
		Model:     t.model,
		AgentName: t.agentName,
		Workspace: t.workspace,
		Branch:    t.branch,
		Permission: &agent.AgentPermissionRequest{
			Tool:   p.Permission,
			Action: desc,
			Options: options,
			ResponseCh: nil, // populated by session.go to wire the reply channel
		},
	})
}

// ─── helpers ─────────────────────────────────────────────────────

// stringOrEmpty returns the raw JSON as a string if it is a quoted
// string, or the empty string otherwise. Used for metadata / output
// fields where we want a quick debug surface but not full decode.
func stringOrEmpty(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// Not a plain string — return a compact JSON rendering.
	return string(raw)
}

// humanDuration formats a time.Duration as a short human-readable
// string for log fields. Not used yet but kept for future latency
// logging.
func humanDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.2fs", d.Seconds())
}
