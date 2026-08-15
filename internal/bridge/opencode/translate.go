
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
	"sort"
	"strings"
	"sync"
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

	// turnHadContent tracks whether ANY AgentEvent conveying
	// agent work (text / tool start / tool end / reasoning) has
	// been delivered during the current turn. Reset on each new
	// turn via ResetTurn(). When the terminal event fires and
	// this is still false, we tag Done.Reason = "empty" so the
	// runtime can surface a "(empty response)" hint to the
	// user — distinguishing "model produced nothing" from a
	// genuine settle. Mirrors cc-connect's relay.go:5161 fallback.
	//
	// Guarded by turnMu together with the per-turn buffering
	// state (turn *turnState). Written from the SSE reader
	// goroutine (markContent / handleEvent) and the SendBlocks
	// call path (ResetTurn); read from the terminal-event
	// branches and from handlePart / handleTextStreamLocked.
	turnMu                sync.Mutex
	turnHadContent        bool
	turnTerminalEmitted   bool

	// turnHadStep tracks whether a session.next.step.started event
	// fired during the current turn. The 1.18 step event payload
	// doesn't carry tool callIDs (we can't reconstruct what ran),
	// but its presence proves the model actually took a turn — so a
	// turn with step.started + step.ended but no payload-bearing
	// events is NOT "empty" (tools likely ran via the 1.18
	// session.next.tool.* event family we don't yet consume).
	// Combined with turnHadContent to refine the Done.Reason
	// choice on terminal events.
	turnHadStep bool // guarded by turnMu; see turnHadContent

	// turn is the per-turn buffering state for streamed
	// `session.next.text.*` events. Added in the opencode-stream-buffer
	// fix to (a) match pi's "token-level buffer + flush at boundary"
	// behaviour and (b) strip inline <think>...</think> blocks the
	// same way the pi bridge does. Guarded by turnMu; see think_tags.go
	// for the splitter rationale.
	turn *turnState

	// availableCommands caches the latest list of slash commands
	// opencode advertises via SSE (stage 8.2). The runtime shim
	// can read this via the agent's AvailableBuiltinCommands()
	// delegate to know which "/foo" inputs came from opencode
	// itself rather than the runtime registry. Note: we don't
	// (yet) execute these commands — opencode's HTTP API doesn't
	// expose a /command endpoint, so they currently fall through
	// to the agent as plain text prompts (same path as the
	// cc-connect behavior). This list is purely informational.
	//
	// Map of command-name → raw JSON (for future expansion when
	// opencode adds HTTP-side command dispatch).
	availableCommands map[string]json.RawMessage
}

type toolEntry struct {
	name   string
	args   string
	output string // opencode 1.18.18 may send the final output on tool.success
}

// turnState is the per-turn buffering state for the streamed
// `session.next.text.*` path (opencode 1.18's token-level text
// bus). Mirrors pi's `turnState.textBuf` + `thinkHoldings`
// pattern; the keying dimension is opencode's PartID instead of
// pi's contentIndex because opencode identifies text parts by a
// stable per-part UUID rather than a positional index.
//
// All fields are guarded by translator.turnMu — textBuf,
// pendingText, thinkHoldings, and activeTextBlock are written
// from the SSE reader goroutine (handleEvent), and ResetTurn
// clears the lot wholesale on each new turn.
//
// Lifecycle:
//
//	ResetTurn()                       → all fields zeroed
//	session.next.text.started         → activeTextBlock = partID,
//	                                    make textBuf[partID]
//	session.next.text.delta           → splitThinking over delta + held;
//	                                    Kept → textBuf[partID], Thinking →
//	                                    [思考] emit, Held → thinkHoldings[partID]
//	session.next.text.ended           → closeTextBlockLocked drops the
//	                                    part's accumulated text into
//	                                    pendingText (still NO deliver)
//	tool pending / step.ended / idle   → flushPendingTextLocked emits
//	                                    one EventAgentText{Text: pendingText}
//	                                    and resets pendingText
type turnState struct {
	// textBuf maps PartID → accumulated reply text for that text
	// part. Created on session.next.text.started, written on each
	// .delta, drained into pendingText on .ended. We buffer
	// per-part because opencode can interleave multiple text parts
	// in a single turn (rare but legal on the wire — e.g. a
	// mid-message correction the model emits as a fresh text
	// part). Keying by PartID keeps each part's tail from
	// bleeding into the next.
	textBuf map[string]*strings.Builder

	// thinkHoldings buffers the trailing partial of a
	// <think>...</think> block that straddled two
	// session.next.text.delta events. Keyed by PartID because
	// each opencode text part has its own delta stream — the
	// Held value is meaningless across parts. Populated by
	// splitThinking when it sees an opening tag with no matching
	// close in the current delta; cleared when the matching
	// </think> arrives (the next delta is prepended with it).
	//
	// Reset wholesale with the rest of the turn — a half-open
	// think block that never closes (model aborted mid-reasoning,
	// partial reply) must not bleed into the next turn's first
	// text block.
	thinkHoldings map[string]string

	// pendingText is the joined reply text assembled from all
	// text parts that have ended so far in the current turn.
	// Drained by flushPendingTextLocked at the tool boundary or
	// at the turn's terminal event into ONE
	// EventAgentText{Text: pendingText}. Empty between flushes —
	// the bridge emits nothing on the reply surface until a
	// flush happens, which keeps the chat client from rendering
	// each token as it streams (the opencode-stream-buffer fix:
	// before this, every `session.next.text.delta` was delivered
	// as its own EventAgentText, causing per-word refreshes).
	pendingText strings.Builder

	// activeTextBlock is the PartID of the most-recently-started
	// text block whose `.ended` has not yet arrived. Drops to ""
	// after flush — guards against a stray .delta that arrives
	// after .ended (misordered; opencode 1.18 sometimes emits
	// one out-of-order .delta on resubscribe) from poisoning
	// the next block's buffer.
	activeTextBlock string
}

// newTurnState allocates an empty turnState. Used by both
// newTranslator (one-shot at handshake) and ResetTurn (per-turn
// re-init on each new prompt). Allocating fresh maps at
// ResetTurn rather than clearing in-place avoids carrying stale
// Held/thinkHoldings across turns (the move-logic-atomically
// rule: a reset means a fresh struct, not a partial wipe).
func newTurnState() *turnState {
	return &turnState{
		textBuf:      make(map[string]*strings.Builder),
		thinkHoldings: make(map[string]string),
	}
}

// newTranslator builds a translator. deliver is the live Agent's
// deliver helper; the bridge's session lifecycle calls deliver on
// every produced event.
func newTranslator(deliver func(agent.AgentEvent) agent.AgentEvent, agentName, workspace, branch, sessionID, model string) *translator {
	return &translator{
		deliver:           deliver,
		agentName:         agentName,
		workspace:         workspace,
		branch:            branch,
		sessionID:         sessionID,
		model:             model,
		pendingTools:      make(map[string]toolEntry),
		availableCommands: make(map[string]json.RawMessage),
		turn:              newTurnState(),
	}
}

// ResetTurn clears per-turn state. Call before each Prompt
// submission so the next terminal event can detect a (genuinely)
// empty response. Per-turn buffering is also discarded wholesale
// — see the comment on newTurnState for the rationale.
func (t *translator) ResetTurn() {
	t.turnMu.Lock()
	t.turnHadContent = false
	t.turnHadStep = false
	t.turnTerminalEmitted = false
	t.turn = newTurnState()
	t.turnMu.Unlock()
}

// markContent flips turnHadContent on. Called from the branches that
// deliver agent work events (text, tool start/end, reasoning).
func (t *translator) markContent() {
	t.turnMu.Lock()
	t.turnHadContent = true
	t.turnMu.Unlock()
}

// turnHadAny reports whether ANY content was delivered during the
// current turn (text/tool/reasoning) or a step.started event fired.
// Used by the terminal-event branches to choose Done.Reason.
func (t *translator) turnHadAny() (content, step bool) {
	t.turnMu.Lock()
	defer t.turnMu.Unlock()
	return t.turnHadContent, t.turnHadStep
}

// tryEmitTurnDone is the single funnel for EventAgentDone so the
// various terminal-event branches (session.next.step.ended,
// session.idle, session.next.idle, session.next.step.failed, and
// the opencode 1.18.18 fallback via session.next.tool.success /
// tool.failed) all converge on a consistent wire shape.
//
// Idempotent per turn: only the FIRST caller emits EventAgentDone
// for this prompt. Subsequent callers (a stray session.idle that
// follows a tool.success, for example) are no-ops. This is the
// regression guard for the "double-Done" symptom on the 1.18.18
// protocol path, which only emits tool lifecycle events and never
// the canonical session.idle / session.next.step.ended signal —
// without the per-turn guard, every tool completion would feed a
// fresh Done to the runtime readpump and tear down / re-arm the
// busy guard mid-turn.
//
// The reason argument is the canonical reason ("settled" / "failed"
// / "empty"); if reason == "settled" AND neither content nor step
// fired this turn, we downgrade to "empty" so the runtime can
// surface an empty-response hint — same rule as the historical
// terminal branches.
//
// err is optional; when non-nil it is forwarded on AgentEvent.Err so
// the chat client can show the failure reason.
func (t *translator) tryEmitTurnDone(reason string, exitCode int, err error) {
	t.turnMu.Lock()
	if t.turnTerminalEmitted {
		t.turnMu.Unlock()
		return
	}
	t.turnTerminalEmitted = true
	hadContent := t.turnHadContent
	hadStep := t.turnHadStep
	usage := t.lastUsage
	t.turnMu.Unlock()

	effective := reason
	if reason == "settled" && !hadContent && !hadStep {
		effective = "empty"
	}

	ev := agent.AgentEvent{
		Kind:      agent.EventAgentDone,
		SessionID: t.sessionID,
		Model:     t.model,
		AgentName: t.agentName,
		Workspace: t.workspace,
		Branch:    t.branch,
		Done: &agent.AgentDoneEvent{
			Reason:   effective,
			ExitCode: exitCode,
			Usage:    usage,
		},
	}
	if err != nil {
		ev.Err = err
	}
	t.deliver(ev)
}

// AvailableBuiltinCommands returns the slash command names opencode
// has advertised via the latest available_commands_update event,
// sorted alphabetically. The returned slice is a copy — callers
// may mutate it without affecting translator state.
//
// Returns nil when no commands have been advertised yet (e.g. the
// event hasn't fired, or the underlying agent isn't running
// opencode 1.18+).
func (t *translator) AvailableBuiltinCommands() []string {
	if len(t.availableCommands) == 0 {
		return nil
	}
	names := make([]string, 0, len(t.availableCommands))
	for name := range t.availableCommands {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// IsBuiltinCommand returns true if name (without leading "/") is
// in the opencode-advertised command list. Used by the runtime
// shim to mark inputs as "this is an opencode builtin" before
// forwarding as a prompt.
func (t *translator) IsBuiltinCommand(name string) bool {
	if t == nil || len(t.availableCommands) == 0 {
		return false
	}
	name = strings.TrimPrefix(name, "/")
	_, ok := t.availableCommands[name]
	return ok
}

// sseNoiseEvents is the allowlist of known-noise event types the
// opencode server emits that we neither act on nor need to log at
// info level on every occurrence. Each is verified harmless during
// the stage 8 e2e runs:
//   - server.connected: subscription confirmation
//   - plugin.added:    per-plugin boot chatter (one per loaded plugin)
//   - catalog.updated: provider/model catalog refresh
//   - reference.updated, integration.updated: editor integrations
//
// New event types SHOULD be added here only after confirming they
// carry no payload we care about. Anything that does carry payload
// gets an explicit case above the allowlist check.
var sseNoiseEvents = map[string]struct{}{
	"server.connected":      {},
	"plugin.added":          {},
	"catalog.updated":       {},
	"reference.updated":     {},
	"integration.updated":   {},
	"message.updated":       {},
	"message.removed":       {},
}

// handleEvent is the entry point invoked by decodeSSE for each parsed
// event. Returns nil so the stream stays alive; SSE-level errors are
// the caller's problem.
func (t *translator) handleEvent(ev SessionEvent) error {
	switch ev.Type {
	case "":
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
	//
	// Buffering (opencode-stream-buffer): the previous
	// implementation called t.deliver(EventAgentText{Text: delta})
	// on every .delta event, which produced one chat-side render
	// per token — the symptom "每个词一直刷新" reproduced on
	// production. The fix mirrors pi's "token-level buffer + flush
	// at boundary" pattern (commit 892bef3 + internal/bridge/pi/
	// translate.go turnState.textBuf + closeTextBlockLocked):
	//
	//   .started → start a new per-PartID textBuf
	//   .delta   → split-thinking strip + write into textBuf[partID]
	//              (no deliver — see closeTextBlockLocked)
	//   .ended   → move textBuf[partID] into pendingText
	//              (still no deliver — see flushPendingTextLocked)
	// flushPendingTextLocked is called at the tool boundary and at
	// the turn's terminal events; it emits ONE EventAgentText with
	// the joined reply text. The chat client therefore renders once
	// per part-cluster, not once per token.
	//
	// session.next.text.started / text.ended mark the boundaries
	// of a single text block; the actual delta is on
	// session.next.text.delta. For models that return the entire
	// text in one shot (no streaming), the text may arrive via
	// text.ended rather than as a series of deltas — we treat
	// that as a fallback path (closeTextBlockLocked handles both).
	case "session.next.text.started", "session.next.text.delta", "session.next.text.ended":
		t.handleTextStreamEvent(ev)
	case "session.next.prompt.admitted", "session.next.prompted":
		// opencode 1.18 emits these as the prompt lifecycle markers
		// on the global bus:
		//   prompt.admitted: the prompt was queued for processing
		//   prompted:        the agent has started working on it
		// Neither carries a payload we currently consume (no
		// promptID, no queue position); we log at debug so future
		// turn-tracking work has a breadcrumb trail. The actual
		// turn-end signal is session.next.step.ended (handled
		// below).
		oLog("sse: session.next.prompt", "type", ev.Type)
	case "session.next.step.started":
		// opencode 1.18 fired step.started as a per-step lifecycle
		// marker. We log only — the actual tool streaming now
		// goes through session.next.text.* events. Mark turnHadStep
		// so the terminal event knows the model took a turn even
		// when no payload-bearing events fire (the 1.18 step event
		// payload doesn't include tool callIDs so we can't tell what
		// ran, just that something did).
		t.turnMu.Lock()
		t.turnHadStep = true
		t.turnMu.Unlock()
		oLog("sse: session.next.step", "type", ev.Type)
	case "session.next.step.ended":
		// TERMINAL signal for opencode 1.18+. The first
		// session.next.step.ended after a session.next.step.started
		// (or session.next.prompted) marks the end of the turn.
		// We emit EventAgentDone so the runtime readpump clears
		// the busy guard. Subsequent events from the same session
		// (compaction, more turns) start a new turn cycle.
		//
		// Tool lifecycle correlation (callID etc.) is not yet
		// wired because the opencode 1.18 step event payload
		// doesn't include callID; we log only and rely on the
		// per-session text delta for the channel footer.
		//
		// Reason is "settled" when content arrived during the
		// turn OR a step.started fired (proving the model did
		// work even if the 1.18 protocol hid the details).
		// Only mark "empty" when neither content events NOR
		// step events arrived — that path means the prompt was
		// admitted but the model produced nothing (auth/quota/
		// hang before first token).
		// opencode-stream-buffer (P0): drain any text parts whose
		// .ended never arrived (resubscribe mid-part, connection
		// drop, single-shot models) into pendingText, then flush
		// pendingText. Without the closeAll step, the reply text
		// stuck in textBuf would never reach the chat client.
		t.turnMu.Lock()
		t.closeAllTextBlocksLocked()
		flushEvents := t.flushPendingTextLocked()
		t.turnMu.Unlock()
		for _, ev := range flushEvents {
			t.deliver(ev)
		}

		// Also surface any unclosed <think> that survived to the
		// terminal (partial reply, model aborted mid-reasoning)
		// as a single [思考] EventAgentText so it never leaks
		// into the next turn's first text block.
		t.turnMu.Lock()
		leaked := t.flushLeftoverThinkLocked()
		t.turnMu.Unlock()
		for _, ev := range leaked {
			t.deliver(ev)
		}

		// Idempotent per turn: tryEmitTurnDone swallows a second
		// Done if a fallback terminal (tool.success, see below) already
		// fired. Same regression guard as session.idle.
		t.tryEmitTurnDone("settled", 0, nil)
	case "session.idle", "session.next.idle":
		// Older opencode releases (≤ 1.17) emit the per-turn
		// terminal signal as session.idle. opencode 1.18+ switched
		// to session.next.step.ended (handled above) — but we
		// keep the case as a forward-compat hook so a future
		// release reintroducing session.next.idle works.
		//
		// Same closeAll + flush + Done rules as
		// session.next.step.ended above.
		t.turnMu.Lock()
		t.closeAllTextBlocksLocked()
		flushEvents := t.flushPendingTextLocked()
		t.turnMu.Unlock()
		for _, ev := range flushEvents {
			t.deliver(ev)
		}
		t.turnMu.Lock()
		leaked := t.flushLeftoverThinkLocked()
		t.turnMu.Unlock()
		for _, ev := range leaked {
			t.deliver(ev)
		}

		t.tryEmitTurnDone("settled", 0, nil)
	case "session.next.step.failed":
		// opencode 1.18 emits this when the model step (LLM call)
		// failed — auth/network/quota. We treat it as a terminal
		// event for the turn so the runtime readpump clears the
		// busy guard and the next prompt can proceed. The error
		// details are surfaced via EventAgentDone{Reason: "failed"}.
		var p struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(ev.properties(), &p)
		t.tryEmitTurnDone("failed", 1, errorOrNil(p.Error))

	// ─── session.next.tool.* (opencode 1.18.18 fallback path) ───
	//
	// On the opencode 1.18.18 protocol observed in production
	// (cleanly running gpt-5 / claude providers), the bus never
	// carries session.next.text.* or message.part.updated at all —
	// only the tool lifecycle event family is published. Without
	// these handlers the chat client sees Working... → DONE with no
	// content (the "啥都没反应就 Done 了" symptom), because the
	// translator's only turn-end sources were step.ended /
	// session.idle / next.idle / session.next.step.failed, all of
	// which 1.18.18 omits.
	//
	// Lifecycle on the bus:
	//
	//	input.started → input.delta* → input.ended   (build args string)
	//	tool.called                              (emit EventAgentToolStart)
	//	tool.success | tool.failed               (emit EventAgentToolEnd + tryEmitTurnDone)
	//
	// The input.* family and tool.called both carry callID/tool but
	// may arrive in any order across releases — we tolerate either
	// ordering. tool.success and tool.failed are the only two
	// turn-terminal events on this protocol path; they funnel through
	// tryEmitTurnDone so we don't double-fire Done if a stray
	// session.idle still arrives later.
	case "session.next.tool.input.started":
		t.handleToolInputStarted(ev)
	case "session.next.tool.input.delta":
		t.handleToolInputDelta(ev)
	case "session.next.tool.input.ended":
		t.handleToolInputEnded(ev)
	case "session.next.tool.called":
		t.handleToolCalled(ev)
	case "session.next.tool.progress":
		t.handleToolProgress(ev)
	case "session.next.tool.success":
		t.handleToolSucceeded(ev)
	case "session.next.tool.failed":
		t.handleToolFailed(ev)
	case "session.next.context.updated":
		t.handleContextUpdated(ev)

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
		// opencode advertises its built-in slash commands via this
		// event (typically right after /api/event subscription).
		// Each command has a shape like
		//   {"name":"clear", "description":"...", "alias":"c"}
		// We only need the name today (for the runtime shim's
		// "is this an opencode builtin?" check). The full payload
		// is kept in availableCommands[name] for future use when
		// opencode ships HTTP-side command dispatch.
		var p struct {
			AvailableCommands []json.RawMessage `json:"availableCommands"`
		}
		if err := json.Unmarshal(ev.properties(), &p); err != nil {
			return nil
		}
		// Reset and re-populate so deletions on the server side
		// (e.g. a plugin disabling a command) are reflected.
		t.availableCommands = make(map[string]json.RawMessage, len(p.AvailableCommands))
		for _, raw := range p.AvailableCommands {
			var meta struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(raw, &meta); err != nil || meta.Name == "" {
				continue
			}
			t.availableCommands[meta.Name] = raw
		}
		oLog("sse: available_commands_update", "count", len(t.availableCommands))
	case "permission.asked":
		var p PermissionAsked
		if err := json.Unmarshal(ev.properties(), &p); err == nil {
			t.handlePermission(p)
		}
	default:
		// Allowlist of known-noise events: drop silently. The
		// sseNoiseEvents map is the single place to add new
		// "we know about this but don't care" event types.
		if _, ok := sseNoiseEvents[ev.Type]; ok {
			return nil
		}
		// Truly unknown event → debug log, do not kill the stream.
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
		t.markContent()
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
		t.markContent()
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
		// opencode-stream-buffer: tool boundary flush. The
		// previous comment in handlePart correctly noted that
		// the message.part.updated path emits ONE EventAgentText
		// per part — but the 1.18 streaming path (above) takes
		// the buffered route and never reaches this branch.
		// This is the fallback: if the wire serves a non-streamed
		// turn via message.part.updated text/type, the buffer
		// won't have any pendingText and the chat client renders
		// a single block per part exactly as before. No change
		// needed here — the prior implementation was already
		// correct for this code path.
		t.markContent()
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
		// opencode-stream-buffer: tool boundary flush. The
		// buffer may still hold a closed-but-not-yet-delivered
		// reply segment (e.g. "Let me check the files…" before
		// the model calls a tool). pi's closeTextBlockLocked
		// flushes the same way at this point — without it, the
		// pre-tool text never reaches OutReply until the turn's
		// terminal event, which arrives after the tool runs and
		// re-orders the chat visually.
		t.turnMu.Lock()
		flushEvents := t.flushPendingTextLocked()
		t.turnMu.Unlock()
		for _, ev := range flushEvents {
			t.deliver(ev)
		}
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

// ─── session.next.tool.* handlers (opencode 1.18.18 fallback) ────
//
// The global event bus on opencode 1.18.18 only publishes the
// session.next.tool.* family on each tool turn. Without these
// handlers the chat client sees the busy spinner until the
// watchdog kills the session — the visible symptom was DONE | LzBook
// immediately with no rendered content. See the case-branch
// comments in handleEvent for the lifecycle and rationale.
//
// All handlers are defensive: any single event arriving without a
// matching callID (or with empty fields) is logged and skipped
// rather than fabricated. The pendingTools map mirrors the one
// used by the message.part.updated path; both paths write to the
// same map so a session that mixes the two protocols (1.17's
// part.updated + 1.18's tool.*) still correlates correctly.

// sessionNextToolEvent is the union shape for tool.called /
// tool.success / tool.failed: callID + tool name + optional
// finalized input/output/error fields.
//
// Wire-format note (opencode 1.18.18 SSE observed in production):
//
//	input.ended       -> {callID, text:JSONString}
//	tool.called       -> {callID, tool, input:object, provider.executed}
//	tool.success      -> {callID, structured:object, content:LLMToolContent[], ...}
//	tool.failed       -> {callID, error, ...}
//
// The previous version only knew about `input` / `output` / `tool`
// - all three come across as different field names per event
// family. The fields below mirror the live wire so each handler
// can pick whichever name its event actually carries.
type sessionNextToolEvent struct {
	CallID    string          `json:"callID"`
	Tool      string          `json:"tool,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Text      string          `json:"text,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	Structured json.RawMessage `json:"structured,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// sessionNextToolInputEvent is the streaming-input shape used by
// input.started / input.delta. Two field-name quirks from the
// opencode 1.18.18 wire (reverse-engineered from SSE probes):
//
//	input.started -> {callID, name:"bash"|"read"|...}
//	input.delta   -> {callID, delta:"partial JSON"}
//
// The previous shape only knew `tool` - which is the wrong key
// for input.started. That bug silently dropped the tool name on
// every streaming-input cycle, leaving the bridge with empty
// tool receipts whenever the model streamed args token-by-token.
type sessionNextToolInputEvent struct {
	CallID string `json:"callID"`
	Name   string `json:"name,omitempty"`
	Tool   string `json:"tool,omitempty"`
	Delta  string `json:"delta,omitempty"`
	sessionNextToolEvent // embed for tool/input context (rarely populated here)
}

// handleToolInputStarted arms the per-callID input buffer. The
// translator's pendingTools entry is created here too (if not
// already present) so subsequent input.delta / tool.called events
// find a stable slot to write into.
//
// Wire note: opencode 1.18.18 ships the tool name as `name`, NOT
// `tool` (which is only populated on tool.called / tool.success).
// Falling back to embed.Tool keeps legacy/future events working.
func (t *translator) handleToolInputStarted(ev SessionEvent) {
	var p sessionNextToolInputEvent
	_ = json.Unmarshal(ev.properties(), &p)
	if p.CallID == "" {
		oLog("sse: tool.input.started missing callID")
		return
	}
	name := p.Name
	if name == "" {
		name = p.Tool
	}
	t.turnMu.Lock()
	defer t.turnMu.Unlock()
	entry, ok := t.pendingTools[p.CallID]
	if !ok {
		entry = toolEntry{}
	}
	if entry.name == "" && name != "" {
		entry.name = normalizeToolName(name)
	}
	t.pendingTools[p.CallID] = entry
}

func (t *translator) handleToolInputDelta(ev SessionEvent) {
	var p sessionNextToolInputEvent
	_ = json.Unmarshal(ev.properties(), &p)
	if p.CallID == "" {
		return
	}
	t.turnMu.Lock()
	defer t.turnMu.Unlock()
	entry, ok := t.pendingTools[p.CallID]
	if !ok {
		entry = toolEntry{}
	}
	entry.args = entry.args + p.Delta
	t.pendingTools[p.CallID] = entry
}

// handleToolInputEnded snaps the per-callID input buffer to the
// authoritative final JSON the server emits on input.ended.
//
// Wire note (opencode 1.18.18): input.ended ships the finalized
// args under the `text` key as a JSON STRING, NOT under `input`
// as an object. The previous implementation looked for `input`,
// so p.Input was always nil and the streamed delta buffer was
// never replaced with the canonical block. We also pick up the
// tool name from the embedded event when the streaming-input
// path never sent input.started first (input.ended carries no
// `name`, so the field is best-effort here).
func (t *translator) handleToolInputEnded(ev SessionEvent) {
	var p sessionNextToolEvent
	_ = json.Unmarshal(ev.properties(), &p)
	if p.CallID == "" {
		// Without a callID we can't correlate — drop. Should not
		// happen in practice on 1.18.18 but the bridge never
		// trusts the wire.
		return
	}
	t.turnMu.Lock()
	defer t.turnMu.Unlock()
	entry, ok := t.pendingTools[p.CallID]
	if !ok {
		entry = toolEntry{}
	}
	// input.ended carries the FINAL input as a JSON string under
	// `text` (opencode 1.18+); fall back to `input` (raw) and
	// the streamed delta buffer if neither is present.
	switch {
	case p.Text != "":
		entry.args = p.Text
	case len(p.Input) > 0 && string(p.Input) != "null":
		entry.args = string(p.Input)
	}
	if entry.name == "" && p.Name != "" {
		entry.name = normalizeToolName(p.Name)
	} else if entry.name == "" && p.Tool != "" {
		entry.name = normalizeToolName(p.Tool)
	}
	t.pendingTools[p.CallID] = entry
}

// handleToolCalled emits EventAgentToolStart. We flush any
// pre-tool buffered reply text FIRST so the chat client renders
// the model's "Let me check…" before the tool receipt (the
// same flushing discipline as handleToolPart "pending").
func (t *translator) handleToolCalled(ev SessionEvent) {
	var p sessionNextToolEvent
	_ = json.Unmarshal(ev.properties(), &p)
	if p.CallID == "" {
		// Orphan .called (no matching .input.started + .input.ended).
		// Not an error; some opencode paths emit .called directly with
		// input finalized inline. Synthesize a callID so the chat
		// client still sees a named tool receipt.
		p.CallID = fmt.Sprintf("orphan_%d", time.Now().UnixNano())
	}
	t.markContent()

	// Tool-boundary flush: deliver any pre-tool reply text before
	// emitting Start (mirrors handleToolPart 'pending' ordering).
	t.turnMu.Lock()
	flushEvents := t.flushPendingTextLocked()
	t.turnMu.Unlock()
	for _, fev := range flushEvents {
		t.deliver(fev)
	}

	t.turnMu.Lock()
	entry, ok := t.pendingTools[p.CallID]
	if !ok {
		entry = toolEntry{name: normalizeToolName(p.Tool)}
	}
	if entry.name == "" {
		entry.name = normalizeToolName(p.Tool)
	}
	// Final inline input beats the streamed args buffer.
	if len(p.Input) > 0 && string(p.Input) != "null" {
		entry.args = string(p.Input)
	}
	t.pendingTools[p.CallID] = entry
	t.turnMu.Unlock()

	t.deliver(agent.AgentEvent{
		Kind:      agent.EventAgentToolStart,
		SessionID: t.sessionID,
		Model:     t.model,
		AgentName: t.agentName,
		Workspace: t.workspace,
		Branch:    t.branch,
		ToolStart: &agent.AgentToolStartEvent{
			ID:   p.CallID,
			Name: entry.name,
			Args: entry.args,
		},
	})
}

// handleToolSucceeded closes a tool call and emits the per-turn
// Done if no other turn-end signal has fired yet. This is the
// terminal signal for the opencode 1.18.18 protocol path.
//
// Wire note (opencode 1.18.18 — reverse-engineered from SSE
// probes against the real binary):
//
//	tool.success.data = {
//	  callID, structured:{uri,name,content,encoding,mime},
//	  content:[ {type:"text",text:"..."}|{type:"file",uri,mime,name} ],
//	  outputPaths:[...], provider:{executed, metadata},
//	}
//
// The previous shape only read `output` - which the wire doesn't
// carry. The actual text payload lives at:
//
//  1. structured.content  (a plain string the read tool returns,
//     eg "PONG-from-file\n" for file reads)
//  2. content[].text      (for tools that return LLMToolContent
//     arrays with `text` entries - e.g. bash stdout chunks)
//
// We probe both and concatenate, preferring structured.content
// because it's always present on the file/read-shaped tools we
// see most often. Empty structured.content and empty content[] -
// i.e. tool.success with no rendered output - still emit
// EventAgentToolEnd so the receipt line lands, but with an empty
// Output so the chat client shows "no output" rather than the
// previous "running…" stuck state.
func (t *translator) handleToolSucceeded(ev SessionEvent) {
	var p sessionNextToolEvent
	_ = json.Unmarshal(ev.properties(), &p)
	if p.CallID == "" {
		oLog("sse: tool.success missing callID")
		return
	}
	t.markContent()

	t.turnMu.Lock()
	entry, ok := t.pendingTools[p.CallID]
	if !ok {
		entry = toolEntry{name: normalizeToolName(p.Name)}
	}
	// Wire note: tool.success carries the tool name under `tool`,
	// not `name` (the streaming-input family uses `name`). Fall
	// back so an event family that ships only `name` still
	// surfaces a label.
	if entry.name == "" && p.Tool != "" {
		entry.name = normalizeToolName(p.Tool)
	}
	if entry.name == "" && p.Name != "" {
		entry.name = normalizeToolName(p.Name)
	}
	// Promote any partial output that handleToolProgress stashed
	// earlier; only fill from structured/content if no progress
	// event pre-populated the field (success always wins over
	// progress because it is the authoritative final payload).
	if entry.output == "" {
		entry.output = extractToolOutput(p.Structured, p.Content)
	}
	delete(t.pendingTools, p.CallID)
	t.turnMu.Unlock()

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
			Output: entry.output,
		},
	})

	// Terminal signal for the 1.18.18 protocol path. tryEmitTurnDone
	// is idempotent so a late session.idle / session.next.idle that
	// follows a tool.success is a no-op.
	t.tryEmitTurnDone("settled", 0, nil)
}

// toolOutputMaxBytes caps the text we surface for a single tool
// invocation. A 100-MiB read of a giant log file shouldn't blow
// up the chat transcript; the renderer applies its own truncation
// on top of this, but pre-clipping here keeps the in-memory
// AgentEvent sane for long-running agents.
const toolOutputMaxBytes = 64 * 1024

// extractToolOutput pulls a human-readable summary out of a
// tool.success payload. It tries two shapes:
//
//	structured.content  (file-shaped tools: read, list, glob)
//	content[].text      (LLMToolContent arrays: bash, generic)
//
// extractToolOutput returns the user-visible tool output text
// (with file refs formatted) and is hard-capped at
// toolOutputMaxBytes so a runaway read doesn't blow up the chat
// transcript. Returns the empty string when the payload is
// genuinely empty.
func extractToolOutput(structured, content json.RawMessage) string {
	// structured.content (most common for file tools).
	if len(structured) > 0 && string(structured) != "null" {
		var s struct {
			Content string `json:"content"`
			Name    string `json:"name"`
			Mime    string `json:"mime"`
		}
		if err := json.Unmarshal(structured, &s); err == nil && s.Content != "" {
			return truncateToolOutput(s.Content)
		}
	}
	// content: LLMToolContent[].
	if len(content) > 0 && string(content) != "null" {
		var items []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Name string `json:"name"`
			Mime string `json:"mime"`
			URI  string `json:"uri"`
		}
		if err := json.Unmarshal(content, &items); err == nil {
			var b strings.Builder
			for _, it := range items {
				switch it.Type {
				case "text":
					if it.Text != "" {
						b.WriteString(it.Text)
						b.WriteByte('\n')
					}
				case "file":
					// File refs: surface "<name> (<mime>)". Long
					// file:// URIs are clipped to the basename so
					// the chat receipt doesn't get spammed.
					label := it.Name
					if label == "" {
						label = uriBasename(it.URI)
					}
					if label != "" {
						if it.Mime != "" {
							fmt.Fprintf(&b, "%s (%s)\n", label, it.Mime)
						} else {
							fmt.Fprintf(&b, "%s\n", label)
						}
					}
				}
			}
			return truncateToolOutput(strings.TrimRight(b.String(), "\n"))
		}
	}
	return ""
}

// uriBasename pulls the last path segment off a file:// URI
// (or any URI with a slash). Returns "" when the URI has no
// usable path component after trimming scheme / query / fragment.
func uriBasename(uri string) string {
	if uri == "" {
		return ""
	}
	// Trim scheme prefix so we don't split on `://`.
	p := uri
	if i := strings.Index(p, "://"); i >= 0 {
		p = p[i+3:]
	}
	// Strip query / fragment so they don't get mistaken for path.
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	// Take the segment after the LAST slash. Trim a trailing
	// slash so "file:///path/?q=1" -> "" -> "" -> "" -> "" (no
	// segment) returns "" instead of "path/" trimmed to "path"
	// for an input the user never meant as a file.
	for len(p) > 0 && p[len(p)-1] == '/' {
		p = p[:len(p)-1]
	}
	if i := strings.LastIndex(p, "/"); i >= 0 {
		p = p[i+1:]
	}
	return p
}

// truncateToolOutput clips s at toolOutputMaxBytes and appends
// a tail marker so the chat client knows the payload was
// truncated (rather than the original ending at that byte).
func truncateToolOutput(s string) string {
	if len(s) <= toolOutputMaxBytes {
		return s
	}
	return s[:toolOutputMaxBytes] + "\n…[truncated " +
		fmt.Sprintf("%d", len(s)-toolOutputMaxBytes) + " bytes]"
}

// handleToolFailed closes a tool call with an error and emits a
// turn-end Done with Reason:"failed" if no other terminal event
// fired first. Mirrors session.next.step.failed's reason choice.
//
// Wire note: tool.failed.data.error is sometimes a bare string,
// sometimes an object like {name, data, message}. We extract the
// user-visible message in both shapes so the chat client always
// sees a readable error rather than "{}".
func (t *translator) handleToolFailed(ev SessionEvent) {
	var p sessionNextToolEvent
	_ = json.Unmarshal(ev.properties(), &p)
	if p.CallID == "" {
		oLog("sse: tool.failed missing callID")
		return
	}
	t.markContent()

	errMsg := extractToolError(p.Error)

	t.turnMu.Lock()
	entry, ok := t.pendingTools[p.CallID]
	if !ok {
		entry = toolEntry{name: normalizeToolName(p.Tool)}
	}
	if entry.name == "" && p.Name != "" {
		entry.name = normalizeToolName(p.Name)
	}
	delete(t.pendingTools, p.CallID)
	t.turnMu.Unlock()

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
			Output: entry.output,
		},
		Err: errorOrNil(errMsg),
	})

	t.tryEmitTurnDone("failed", 1, errorOrNil(errMsg))
}

// handleToolProgress handles session.next.tool.progress events
// that opencode 1.18+ emits for long-running tools (eg bash
// commands that stream stdout before completion). The payload
// mirrors tool.success but arrives BEFORE the success so the chat
// client can show partial output.
//
// Wire format:
//
//	{callID, structured:object, content:LLMToolContent[]}
//
// We don't have a partial-output event in the AgentEvent schema,
// so we coalesce: stash the partial text in the pendingTools
// entry, then promote it into entry.output when tool.success
// arrives. If the tool fails before producing a success event,
// the partial still surfaces via the fail path.
func (t *translator) handleToolProgress(ev SessionEvent) {
	var p sessionNextToolEvent
	_ = json.Unmarshal(ev.properties(), &p)
	if p.CallID == "" {
		return
	}
	out := extractToolOutput(p.Structured, p.Content)
	if out == "" {
		return
	}
	t.turnMu.Lock()
	defer t.turnMu.Unlock()
	entry, ok := t.pendingTools[p.CallID]
	if !ok {
		entry = toolEntry{}
	}
	if entry.name == "" {
		if p.Name != "" {
			entry.name = normalizeToolName(p.Name)
		} else if p.Tool != "" {
			entry.name = normalizeToolName(p.Tool)
		}
	}
	entry.output = out
	t.pendingTools[p.CallID] = entry
}

// handleContextUpdated logs session.next.context.updated events.
// opencode 1.18 fires these between model steps whenever the
// prompt-side context changes - typically after a tool call
// returns and the server injects a fresh skills/providers/
// permissions snapshot. The payload's `text` blob is the new
// serialized prompt prefix; we don't surface it (the chat client
// doesn't need to see the system prompt churn), but we log the
// delta so future debug captures can correlate turn boundaries
// with prompt-side transitions.
//
// Wire format:
//
//	{timestamp, sessionID, messageID, text}
func (t *translator) handleContextUpdated(ev SessionEvent) {
	var p struct {
		SessionID string `json:"sessionID"`
		MessageID string `json:"messageID"`
		Text      string `json:"text"`
	}
	_ = json.Unmarshal(ev.properties(), &p)
	oLog("sse: session.next.context.updated",
		"sessionID", p.SessionID,
		"messageID", p.MessageID,
		"text_len", len(p.Text),
	)
}

// extractToolError flattens tool.failed.data.error into a
// human-readable string. It handles both shapes observed on the
// wire:
//
//	"opencode: permission denied for /foo"
//	{"name":"PermissionDeniedError","data":{"message":"..."}}
//
// Object-form priority: .data.message > .message > the name
// (we never return bare JSON so the chat client doesn't have
// to render `{}` for an empty error). Returns "" when the
// payload is genuinely empty so the caller can decide whether
// to surface "(unknown error)" or skip.
func extractToolError(raw string) string {
	if raw == "" {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err == nil {
		if data, ok := obj["data"].(map[string]any); ok {
			if m, _ := data["message"].(string); m != "" {
				return m
			}
		}
		if m, _ := obj["message"].(string); m != "" {
			return m
		}
	}
	return raw
}

// ─── session.next.text.* stream handling ──────────────────────────
//
// handleTextStreamEvent is the single entry point for opencode 1.18's
// per-session text bus. It dispatches on the event sub-type and
// routes through:
//
//   handleTextStreamStarted → mark active block, allocate textBuf
//   handleTextStreamDelta   → splitThinking strip + textBuf write
//   handleTextStreamEnded   → closeTextBlockLocked → pendingText
//
// Flush is centralized in flushPendingTextLocked, called from the
// tool-boundary hooks (handleToolPart at the pending transition)
// and the terminal events (session.next.step.ended /
// session.idle). See F-OPENCODE-opencode-bridge.md §4.6 for the
// protocol-side rationale.
//
// The full lock is held across these methods — they're called
// only from the SSE reader goroutine, so contention with
// ResetTurn (called from the Prompt goroutine) is bounded to the
// critical sections that the lock guards.

// handleTextStreamEvent decodes the text event's wire payload and
// dispatches on the .type suffix. Empty bodies on .started / .ended
// are tolerated (the model may emit boundary markers with no
// payload; the actual content arrives on .delta or the subsequent
// .ended snapshot).
func (t *translator) handleTextStreamEvent(ev SessionEvent) {
	var p struct {
		PartID string `json:"partID"`
		Text   string `json:"text"`
		Delta  string `json:"delta"`
	}
	_ = json.Unmarshal(ev.properties(), &p)

	payload := p.Text
	if payload == "" {
		payload = p.Delta
	}

	switch ev.Type {
	case "session.next.text.started":
		t.handleTextStreamStarted(p.PartID)
	case "session.next.text.delta":
		t.handleTextStreamDelta(p.PartID, payload)
	case "session.next.text.ended":
		// Pass the snapshot text too — some models emit the
		// entire body on .ended without any .delta. In that
		// case payload == p.Text and we want it buffered.
		t.handleTextStreamEnded(p.PartID, payload)
	}
}

// handleTextStreamStarted opens a new per-part textBuf. Called from
// handleTextStreamEvent under turnMu.
func (t *translator) handleTextStreamStarted(partID string) {
	if partID == "" {
		// Without a PartID we can't key the buffer safely; fall
		// back to a literal so the bridge still surfaces the
		// content (one EventAgentText per cluster, not per token).
		partID = "_"
	}
	t.turnMu.Lock()
	defer t.turnMu.Unlock()
	if t.turn == nil {
		t.turn = newTurnState()
	}
	t.turn.activeTextBlock = partID
	if _, ok := t.turn.textBuf[partID]; !ok {
		t.turn.textBuf[partID] = &strings.Builder{}
	}
}

// handleTextStreamDelta buffers one delta (or the snapshot of a
// single-shot model) into textBuf[partID] after stripping inline
// <think>...</think> blocks. Extracted reasoning surfaces as its
// own [思考] EventAgentText (same channel as the structured
// `reasoning` Part type). The [思考] emit counts as content for
// the empty-turn detector (markContent) but the buffered reply
// text is what flushPendingTextLocked surfaces at the terminal
// event, so the chat client sees one render per part-cluster.
//
// Called from handleTextStreamEvent under turnMu.
func (t *translator) handleTextStreamDelta(partID, payload string) {
	if payload == "" {
		return
	}
	t.turnMu.Lock()
	defer t.turnMu.Unlock()
	if t.turn == nil {
		t.turn = newTurnState()
	}

	pid := partID
	if pid == "" {
		pid = t.turn.activeTextBlock
		if pid == "" {
			pid = "_"
		}
	}

	// Prepend the trailing partial of an unclosed <think> that
	// straddled the previous delta, so splitThinking sees the
	// whole block (the Held protocol from think_tags.go).
	combined := t.turn.thinkHoldings[pid] + payload
	t.turn.thinkHoldings[pid] = ""

	split := splitThinking(combined)
	if split.Kept != "" {
		buf, ok := t.turn.textBuf[pid]
		if !ok {
			buf = &strings.Builder{}
			t.turn.textBuf[pid] = buf
		}
		buf.WriteString(split.Kept)
		t.turnHadContent = true
		t.turn.activeTextBlock = pid
	}
	if split.Held != "" {
		t.turn.thinkHoldings[pid] = split.Held
	}
	if thinking := strings.TrimSpace(split.Thinking); thinking != "" {
		t.turnHadContent = true
		t.deliver(agent.AgentEvent{
			Kind:      agent.EventAgentText,
			SessionID: t.sessionID,
			Model:     t.model,
			AgentName: t.agentName,
			Workspace: t.workspace,
			Branch:    t.branch,
			Text:      "[思考] " + thinking,
		})
	}
}

// handleTextStreamEnded moves a closed part's textBuf into
// pendingText. Reply text is NOT yet delivered —
// flushPendingTextLocked picks it up at the tool boundary or the
// turn's terminal event, so the chat client sees a single render
// per part-cluster rather than one per token. If `payload` is
// non-empty (single-shot model: entire text arrived on .ended),
// it is buffered through the same splitThinking path the .delta
// branch uses.
//
// Called from handleTextStreamEvent under turnMu.
func (t *translator) handleTextStreamEnded(partID, payload string) {
	t.turnMu.Lock()
	defer t.turnMu.Unlock()
	if t.turn == nil {
		t.turn = newTurnState()
	}
	pid := partID
	if pid == "" {
		pid = t.turn.activeTextBlock
	}
	if pid == "" {
		// No active block — drop on the floor rather than
		// fabricating a key. This is the safety net for
		// reordered `.ended` with no preceding `.started`,
		// which opencode 1.18 sometimes emits on resubscribe.
		return
	}

	// Single-shot model: delta carries the full text. Run it
	// through splitThinking so inline think blocks don't leak.
	combined := t.turn.thinkHoldings[pid] + payload
	t.turn.thinkHoldings[pid] = ""
	if combined != "" {
		split := splitThinking(combined)
		if split.Kept != "" {
			buf, ok := t.turn.textBuf[pid]
			if !ok {
				buf = &strings.Builder{}
				t.turn.textBuf[pid] = buf
			}
			buf.WriteString(split.Kept)
			t.turnHadContent = true
		}
		// A closed block typically has no Held — emit any
		// in-place Thinking the same way the .delta branch does.
		if thinking := strings.TrimSpace(split.Thinking); thinking != "" {
			t.turnHadContent = true
			t.deliver(agent.AgentEvent{
				Kind:      agent.EventAgentText,
				SessionID: t.sessionID,
				Model:     t.model,
				AgentName: t.agentName,
				Workspace: t.workspace,
				Branch:    t.branch,
				Text:      "[思考] " + thinking,
			})
		}
	}

	t.closeTextBlockLocked(pid)
	if t.turn.activeTextBlock == pid {
		t.turn.activeTextBlock = ""
	}
}

// closeTextBlockLocked moves a single part's textBuf into
// pendingText and frees the bucket. Caller MUST hold turnMu.
//
// Why a separate pass: the .started → many .delta → .ended span
// typically lasts many SSE frames; we keep the per-part bucket
// around until .ended so the next part starts on a clean slate.
// Whether to surface the accumulated text as EventAgentText or
// keep holding it for a tool-boundary flush is the
// flushPendingTextLocked caller's call.
func (t *translator) closeTextBlockLocked(partID string) {
	buf, ok := t.turn.textBuf[partID]
	if !ok {
		return
	}
	text := buf.String()
	delete(t.turn.textBuf, partID)
	if text == "" {
		return
	}
	if t.turn.pendingText.Len() > 0 {
		t.turn.pendingText.WriteByte('\n')
	}
	t.turn.pendingText.WriteString(text)
	t.turnHadContent = true
}

// closeAllTextBlocksLocked drains every remaining textBuf entry
// into pendingText. Called at turn-terminal time so any text
// part whose `.ended` was not observed (opencode 1.18 fires
// .delta without a closing .ended when the connection drops
// mid-stream, the bridge resubscribes mid-part, or the model
// single-shots the reply on .started itself without ever
// emitting a separate .ended) still reaches the chat surface.
//
// Caller MUST hold turnMu. Reset parts and activeTextBlock so
// the next turn starts clean.
func (t *translator) closeAllTextBlocksLocked() {
	if t.turn == nil || len(t.turn.textBuf) == 0 {
		return
	}
	// Drain in deterministic order by sorting the partIDs so
	// tests don't flake on Go's randomized map iteration.
	ids := make([]string, 0, len(t.turn.textBuf))
	for id := range t.turn.textBuf {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		t.closeTextBlockLocked(id)
	}
	t.turn.activeTextBlock = ""
}

// flushPendingTextLocked emits ONE EventAgentText with the joined
// reply text from all closed parts in the current turn. Caller
// MUST hold turnMu; the returned slice is empty when there is
// nothing to emit.
//
// Called from:
//
//   - handleToolPart on the first pending transition of a tool
//     call (tool-boundary flush; pi does the same)
//   - the terminal-event branches (session.next.step.ended /
//     session.idle) when no tool was called but the buffer still
//     holds reply text the chat client must see before Done
//
// Resetting the pendingText after a successful flush means the
// next call returns an empty slice, so the terminal-event path
// can be safely entered twice without duplicating the reply.
func (t *translator) flushPendingTextLocked() []agent.AgentEvent {
	if t.turn == nil || t.turn.pendingText.Len() == 0 {
		return nil
	}
	text := t.turn.pendingText.String()
	t.turn.pendingText.Reset()
	if text == "" {
		return nil
	}
	t.turnHadContent = true
	return []agent.AgentEvent{{
		Kind:      agent.EventAgentText,
		SessionID: t.sessionID,
		Model:     t.model,
		AgentName: t.agentName,
		Workspace: t.workspace,
		Branch:    t.branch,
		Text:      text,
	}}
}

// flushLeftoverThinkLocked emits any partial <think> block whose
// close tag never arrived (model aborted mid-reasoning, partial
// reply, etc.) as a single [思考] EventAgentText. Belt-and-braces
// against think-tag leakage at the turn boundary: the Held text
// is the raw unclosed tag plus its inner text, and leaving it in
// turnState across turns would silently smuggle the partial into
// the next turn's first text block.
//
// Called from the terminal-event branches AFTER
// flushPendingTextLocked. Caller MUST hold turnMu; the returned
// slice is empty when there are no leftovers. Walks thinkHoldings
// and clears each entry on read — there's at most one held
// partial per PartID and at most one PartID's worth of leak per
// turn in practice.
func (t *translator) flushLeftoverThinkLocked() []agent.AgentEvent {
	if t.turn == nil || len(t.turn.thinkHoldings) == 0 {
		return nil
	}
	var out []agent.AgentEvent
	for _, held := range t.turn.thinkHoldings {
		if held == "" {
			continue
		}
		// Strip the open tag we re-attached during Held, leaving
		// the inner text as the reasoning payload. Future
		// readers can still spot an unclosed block by the
		// absence of the [思考] close marker, but that's the
		// model half-truncated its own reasoning — better to
		// surface it than to silently drop it.
		inner := strings.TrimPrefix(held, thinkOpenTag)
		out = append(out, agent.AgentEvent{
			Kind:      agent.EventAgentText,
			SessionID: t.sessionID,
			Model:     t.model,
			AgentName: t.agentName,
			Workspace: t.workspace,
			Branch:    t.branch,
			Text:      "[思考] " + inner,
		})
		t.turnHadContent = true
	}
	t.turn.thinkHoldings = make(map[string]string)
	return out
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

// errorOrNil wraps an empty string as nil so EventAgentError.Err
// is a clean error instead of a stub with empty message. The
// runtime uses `ev.Err != nil` to decide whether to render an
// error icon, so we MUST keep the contract that empty-string
// errors don't render.
func errorOrNil(s string) error {
	if s == "" {
		return nil
	}
	return fmt.Errorf("%s", s)
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
