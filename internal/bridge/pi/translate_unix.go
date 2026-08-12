//go:build !windows

// Pi event -> agent.AgentEvent translation table.
//
// The translator is purely functional from the caller's point of
// view: it takes one parsed event envelope and returns the
// AgentEvent stream to feed into the session's events channel. It
// never touches the read loop, the process, or the pending registry.
// This makes it trivial to test against a JSON fixture per the F-32
// acceptance matrix §9.2.
//
// Translation rules are documented in docs/feat/F-32-pi-rpc-bridge.md
// §2.3, as revised by docs/feat/F-52-pi-stream-aggregation.md.
// Anything not explicitly listed there is dropped (logged at debug)
// so unknown upstream events do not terminate the session.
//
// F-52 — stream aggregation. Pi streams assistant text at *token*
// granularity (`text_delta`). Before F-52 each delta became its own
// EventAgentText, which the gateway turned into an OutReply and the Feishu
// adapter into a separate 💬 log entry — one sentence exploded into
// twenty bubbles and twenty card PATCHes. The translator now buffers
// deltas and emits at semantic-block boundaries instead:
//
//	no tools   : 0 × EventAgentText,  1 × EventAgentResult
//	with tools : 1 × EventAgentText per narration segment preceding a
//	             tool call, then 1 × EventAgentResult for the conclusion
//
// The flush point is `tool_execution_start` (see flushPendingTextLocked).
// That is what keeps "one reply per turn" and "show progress mid-turn"
// from fighting each other, and it is why the final EventAgentResult never
// duplicates text the user has already seen.

package pi

import (
	"encoding/json"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/cnlangzi/nightme/internal/agent"
)

// thinkingPrefix marks an EventAgentText as a reasoning block rather than
// a reply. gateway.Translate strips it and routes the payload to
// OutThinking; the claudecode bridge uses the same sentinel, so the
// channel renderers need no per-bridge branching.
const thinkingPrefix = "[思考] "

// emptyReplyFallback is the EventAgentResult text used when a turn settles
// without any un-flushed assistant text — e.g. the agent ended on a
// tool call and said nothing afterwards.
//
// It is NOT cosmetic. gateway.Translate drops an EventAgentResult whose
// Text is empty and IsError is false (internal/gateway/translate.go),
// and the runtime reads Usage off the *translated* OutboundMessage
// (cmd/nightme/run.go), so an empty-text result silently takes the
// turn's token counts down with it. Guaranteeing a non-empty Text in
// the bridge keeps usage flowing without touching the shared gateway
// layer. cc-connect (MsgEmptyResponse) and openclaw-lark
// (EMPTY_REPLY_FALLBACK_TEXT = 'Done.') both do the same thing.
const emptyReplyFallback = "Done."

// pendingTool records a tool_call that has emitted tool_execution_start
// but not yet the matching tool_execution_end. The bridge uses it to
// re-attach Name + raw Args on the end event — Pi's wire does not echo
// args on end, so without start->end correlation the renderer would
// fall back to "🔧 tool" and lose the type-aware summary.
type pendingTool struct {
	Name string
	Args string // raw JSON from tool_execution_start.args
}

// turnState is every piece of mutable state scoped to a single turn
// (prompt -> ... -> agent_settled). It is replaced wholesale on
// agent_settled and on /new, which is the whole point of grouping it:
// resetting a turn is one assignment, and a future per-turn field
// cannot be forgotten by a reset path.
//
// Concurrency: all fields are read and written under translator.turnMu.
// See the comment on that field for who contends.
type turnState struct {
	// textBuf accumulates text_delta payloads per contentIndex.
	// Keyed rather than a single Builder because Pi's wire allows
	// several content blocks to interleave on one message; in
	// practice models emit them in order, but keying costs nothing
	// and removes an ordering assumption. Entries are created on
	// first delta and drained by closeTextBlockLocked.
	textBuf map[int]*strings.Builder

	// thinkBuf accumulates thinking_delta payloads. Reasoning has a
	// single stream (no contentIndex fan-out in practice) and is
	// flushed on thinking_end rather than at the tool boundary —
	// reasoning is a different kind of thing from the reply and must
	// not bleed into pendingText.
	thinkBuf strings.Builder

	// pendingText holds text blocks that have been completed
	// (text_end) but not yet delivered. Drained either by
	// flushPendingTextLocked at a tool boundary or by the final
	// EventAgentResult on agent_settled. Blocks are joined with "\n".
	pendingText string

	// lastMessageText is the text composed from the most recent
	// assistant message_end's content[] — Pi's own authoritative
	// rendering of that message. Overwritten (not appended) per
	// message: only the final assistant message of the turn matters.
	// Used solely as a fallback when the delta stream produced
	// nothing, which happens when Pi replays a message instead of
	// streaming it. Gated by textDelivered — see there.
	lastMessageText string

	// textDelivered records that at least one EventAgentText carrying
	// *reply* text has already gone out this turn (reasoning does not
	// count; it is a different surface).
	//
	// It exists to disarm the lastMessageText fallback. Pi emits
	// message_end(assistant) with content[] = [text, toolCall] BEFORE
	// the matching tool_execution_start, so a turn that ends on a tool
	// call leaves lastMessageText holding text we already flushed at
	// that tool boundary. Falling back to it would re-deliver the
	// paragraph the user just read — as a 📝 result card this time.
	//
	// A boolean rather than clearing lastMessageText at flush time
	// because the two events can arrive in either order; the flag is
	// order-independent, a clear is not.
	textDelivered bool

	// active records that this turn observed something worth
	// reporting (reply text, reasoning, an assistant message, or a
	// tool call). A settled turn that saw none of those produces no
	// EventAgentResult at all — see finishTurnLocked.
	active bool

	// stopReason is the most recent assistant message_end's
	// stopReason. Surfaced as AgentResultEvent.Subtype; "error" also
	// sets .
	stopReason string

	// lastUsage is the usage block of the most recent assistant
	// message_end.
	//
	// OVERWRITTEN, never summed. Each Pi API call reports an input
	// side (input + cacheRead + cacheWrite) that already contains
	// the entire conversation history, so it is a *snapshot* of
	// current context occupancy. Summing the snapshots of a
	// multi-call turn would overstate it by roughly the call count.
	// cc-connect reaches the same conclusion from the other
	// direction — its handleAgentEnd walks agent_end.messages[]
	// backwards and takes the last assistant usage.
	lastUsage *agent.UsageInfo

	// pendingTools keys in-flight tool calls (toolCallId) to their
	// start-time metadata so the matching tool_execution_end can
	// carry Name + Args into AgentToolEndEvent. Populated in
	// tool_execution_start, drained in tool_execution_end, and
	// discarded wholesale when the turn resets — a tool that never
	// reported an end (Pi aborted mid-call) must not leak into the
	// next turn and mis-attribute a later tool's args.
	pendingTools map[string]pendingTool
}

func newTurnState() *turnState {
	return &turnState{
		textBuf:      make(map[int]*strings.Builder),
		pendingTools: make(map[string]pendingTool),
	}
}

// translator carries the per-session translation state: the session
// fingerprint used to attribute EventAgentReady, the sticky connectedSent flag,
// and the current turn's accumulation buffers.
type translator struct {
	agentName string
	workspace string
	branch    string

	// connectedSent is set after the first EventAgentReady has been emitted
	// for the session, so subsequent get_state responses or model
	// changes do not re-fire the receipt header. Guarded by the
	// session's translatorMu (NOT turnMu) — see below.
	connectedSent bool

	// contextWindow is the API-reported model context-window size
	// in tokens, captured from get_state.data.model.contextWindow
	// (F-54). Bridge-local: decodeMessageUsage receives it as a
	// parameter, computes ContextWindowPct, and the value itself
	// never crosses the UsageInfo struct boundary.
	//
	// Refreshed on every emitConnected call. emitConnected fires
	// once at boot AND once on every /new (session.New resets
	// connectedSent=false and re-runs the boot handshake — see
	// session.go newSession). emitConnected unconditionally
	// resets the field to 0 before the conditional Store, so a
	// missing ContextWindow in the new get_state response
	// (catalog miss, RPC hiccup) does NOT silently inherit the
	// previous session's window — that would corrupt every
	// subsequent pct for the new session (F-54 §3).
	//
	// Stored via atomic.LoadInt64/StoreInt64 so the concurrent
	// decodeMessageUsage read (on the readPump goroutine, under
	// turnMu) sees a coherent value without coupling turnMu and
	// translatorMu (emitConnected runs under translatorMu).
	contextWindow atomic.Int64

	// turnMu serialises all access to turn and suppressing.
	// translate() runs on the readPump goroutine; session.New()
	// (driven by /new) resets the turn from a different goroutine.
	// Without the mutex that is a concurrent map read/write on
	// textBuf / pendingTools, which panics the daemon.
	//
	// turnMu is intentionally separate from the session's
	// translatorMu (which guards connectedSent + emitConnected). The two
	// protect disjoint state and are never held together — New()
	// takes them sequentially, not nested — so there is no lock
	// ordering hazard. Keep it that way: taking translatorMu around
	// translate() would serialise every wire event against /new.
	turnMu sync.Mutex
	turn   *turnState

	// suppressing drops every translated event while a /new reset is
	// in flight. Set by beginReset, cleared by endReset.
	//
	// Resetting the turn state alone is not enough. session.New()
	// resets, then issues a get_state RPC with a 10s deadline before
	// it can deliver the new EventAgentReady — and readPump keeps
	// translating the whole time. Wire events still in the pipe from
	// the turn the user just abandoned would land in the *fresh*
	// turnState: an old message_end would stamp its usage onto the
	// new session (corrupting the context-occupancy number on the
	// bridge's per-turn snapshot), and an old agent_settled would
	// ship the abandoned reply as the new session's result card.
	//
	// Dropping is unconditionally correct in this window: no prompt
	// has been sent to the new session yet, so nothing arriving here
	// can belong to it. Command responses (new_session / get_state)
	// travel the response path, and extension_ui_request +
	// malformed-frame errors are handled in readPump before
	// translate() is reached — none of them are affected.
	suppressing bool
}

func newTranslator(agentName, workspace, branch string) *translator {
	return &translator{
		agentName: agentName,
		workspace: workspace,
		branch:    branch,
		turn:      newTurnState(),
	}
}

// beginReset discards the current turn and opens the suppression
// window. Called by session.New() as soon as pi has acknowledged
// new_session; every subsequent wire event is dropped until
// endReset. See translator.suppressing for why the window is needed
// rather than a bare turn reset.
//
// MUST be paired with endReset on every exit path — a leaked
// suppression flag mutes the session permanently. session.New()
// defers the call.
func (t *translator) beginReset() {
	t.turnMu.Lock()
	defer t.turnMu.Unlock()
	t.turn = newTurnState()
	t.suppressing = true
}

// endReset closes the suppression window opened by beginReset.
//
// The turn state is not re-created here: translate() bails out
// before touching it while suppressing, so nothing accumulated
// during the window and the state is still the pristine one
// beginReset installed.
func (t *translator) endReset() {
	t.turnMu.Lock()
	defer t.turnMu.Unlock()
	t.suppressing = false
}

// translate consumes one Pi event and returns zero or more
// AgentEvent values. The returned slice is empty for events that
// should be dropped silently. A non-nil error indicates a malformed
// payload that the caller should treat as fatal for the session
// (the bridge closes the events channel and emits EventAgentError in
// that case).
//
// The whole body runs under turnMu. That is safe because translation
// is pure CPU — no I/O, no channel sends, no calls back into the
// session — so the critical section is bounded by a JSON decode.
// Delivery happens in readPump *after* translate returns.
func (t *translator) translate(raw []byte, logger *slog.Logger) ([]agent.AgentEvent, error) {
	if logger == nil {
		logger = slog.Default()
	}
	var env eventEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}

	t.turnMu.Lock()
	defer t.turnMu.Unlock()

	// A /new reset is in flight — everything on the wire belongs to
	// the turn the user just abandoned. Drop it before it can touch
	// the fresh turn state. See translator.suppressing.
	if t.suppressing {
		logger.Debug("pi event dropped (reset in flight)",
			slog.String("type", env.Type),
		)
		return nil, nil
	}

	switch env.Type {

	case "agent_start", "agent_end", "turn_start", "turn_end",
		"message_start", "auto_retry_start", "auto_retry_end",
		"summarization_retry_scheduled",
		"summarization_retry_attempt_start",
		"summarization_retry_finished",
		"queue_update", "bash_execution_update":
		// Lifecycle / queue / streaming events that the F-32 MVP
		// does not surface. Logged for debug; never terminate the
		// session.
		//
		// agent_end stays on this list deliberately: it carries a
		// willRetry flag and can fire several times within one turn,
		// so it is NOT a terminal marker (F-32 §2.3). cc-connect
		// emits its EventAgentResult here and accepts the duplicates; we
		// use agent_settled instead and get exactly one.
		logger.Debug("pi event ignored",
			slog.String("type", env.Type),
			slog.String("raw", string(raw)),
		)
		return nil, nil

	case "agent_settled":
		// Turn-end marker. The session stays open; subsequent
		// prompts are still accepted.
		//
		// This is the only place a turn's EventAgentResult is produced —
		// exactly one per turn, carrying the final text and the
		// usage snapshot. EventAgentDone follows it because the runtime's
		// readpump flips to Idle and flushes the queued prompts on
		// EventAgentDone; emitting the result afterwards would race the
		// next turn.
		out := t.finishTurnLocked()
		out = append(out, agent.AgentEvent{
			Kind: agent.EventAgentDone,
			Done: &agent.AgentDoneEvent{ExitCode: 0, Reason: "settled"},
		})
		t.turn = newTurnState()
		return out, nil

	case "message_update":
		return t.translateMessageUpdateLocked(raw, logger)

	case "message_end":
		return t.translateMessageEndLocked(raw, logger)

	case "tool_execution_start":
		var ev toolExecutionStart
		if err := json.Unmarshal(raw, &ev); err != nil {
			return nil, err
		}
		// Flush the narration that preceded this tool call so the
		// user sees what the agent is doing while it runs. Clearing
		// pendingText here is also what guarantees the turn's final
		// EventAgentResult carries only the *last* segment — the text
		// delivered now is never repeated in the result (F-52).
		t.turn.active = true
		out := t.flushPendingTextLocked()
		// Empty toolCallId is rejected at the wire level by Pi, but
		// belt-and-braces: storing under key "" would let two
		// malformed events collapse onto each other and a later
		// empty-id end would inherit the wrong Name/Args. Record
		// only when the id is non-empty; the matching end falls
		// back to wire ToolName. EventAgentToolStart still fires with
		// ID=="" so consumers see the start; the orphan-end
		// fallback path covers the result line.
		if ev.ToolCallID != "" {
			t.turn.pendingTools[ev.ToolCallID] = pendingTool{
				Name: ev.ToolName,
				Args: stringOrEmpty(ev.Args),
			}
		} else {
			logger.Warn("pi tool_execution_start with empty toolCallId — pending correlation skipped",
				slog.String("raw", string(raw)))
		}
		return append(out, agent.AgentEvent{
			Kind: agent.EventAgentToolStart,
			ToolStart: &agent.AgentToolStartEvent{
				ID:   ev.ToolCallID,
				Name: ev.ToolName,
				Args: stringOrEmpty(ev.Args),
			},
		}), nil

	case "tool_execution_update":
		// MVP does not surface partial tool results. Logged for
		// debug so users with --verbose can still see streaming
		// progress in the bridge log.
		logger.Debug("pi tool_execution_update ignored",
			slog.String("raw", string(raw)),
		)
		return nil, nil

	case "tool_execution_end":
		var ev toolExecutionEnd
		if err := json.Unmarshal(raw, &ev); err != nil {
			return nil, err
		}
		// Re-attach Name + Args recorded from tool_execution_start.
		// Fall back to the wire's own toolName when no start event
		// was observed (orphan end — e.g. tool started before the
		// bridge attached, or end arriving before start under a
		// reordered pump). Args stays empty in the fallback path.
		name := ev.ToolName
		args := ""
		if ev.ToolCallID != "" {
			if pending, ok := t.turn.pendingTools[ev.ToolCallID]; ok {
				if name == "" {
					name = pending.Name
				}
				args = pending.Args
			}
			delete(t.turn.pendingTools, ev.ToolCallID)
		} else {
			logger.Warn("pi tool_execution_end with empty toolCallId — orphan path, wire ToolName only",
				slog.String("raw", string(raw)))
		}
		return []agent.AgentEvent{{
			Kind: agent.EventAgentToolEnd,
			ToolEnd: &agent.AgentToolEndEvent{
				ID:     ev.ToolCallID,
				Name:   name,
				Args:   args,
				Output: stringOrEmpty(ev.Result),
			},
			Err: toolErrorIf(ev.IsError),
		}}, nil

	case "compaction_start":
		// F-49: bridge abstracts the protocol difference. Pi emits a
		// start+end pair per compaction cycle; only the end is
		// surfaced to the runtime as one EventAgentCompaction, so a single
		// Pi cycle bumps the AgentSession counter exactly once.
		// Reasons live in protocol.go's compactionStart.Reason but
		// are intentionally not propagated (no Subtype field after
		// F-49). See docs/feat/F-49-compaction-counter.md §1.3.
		return nil, nil

	case "compaction_end":
		var ev compactionEnd
		if err := json.Unmarshal(raw, &ev); err != nil {
			return nil, err
		}
		// Compaction can fire mid-turn; the turn's accumulation
		// buffers are deliberately left untouched so the reply being
		// composed survives the cycle.
		//
		// Compaction tracking removed (F-49 abandoned): bridges no
		// longer emit a dedicated event. Runtime is a pure pass-through.
		return nil, nil

	case "extension_ui_request":
		// The F-32 MVP does not forward extension UI to the
		// channel. The caller (session.readPump) handles the
		// auto-cancelled response separately so the translator
		// can stay pure.
		logger.Warn("pi extension_ui_request ignored (F-32 MVP)",
			slog.String("raw", string(raw)),
		)
		return nil, nil

	case "extension_error":
		var ev extensionError
		if err := json.Unmarshal(raw, &ev); err != nil {
			return nil, err
		}
		logger.Warn("pi extension_error",
			slog.String("path", ev.ExtensionPath),
			slog.String("event", ev.Event),
			slog.String("error", ev.Error),
		)
		return nil, nil

	case "state_update":
		// Defensive: the official pi-coding-agent RPC spec (docs/rpc.md)
		// does NOT list state_update as a server-emitted event. The
		// F-34 reset path in pi/session.go New() does NOT rely on this
		// case — it issues new_session + get_state synchronously and
		// pushes the resulting EventAgentReady through deliver().
		//
		// We keep this case so a future pi version that DOES emit
		// state_update (informally) still surfaces the new sessionId
		// to the runtime. Bypasses connectedSent for the same reason as
		// the canonical path.
		var ev stateUpdate
		if err := json.Unmarshal(raw, &ev); err != nil {
			return nil, err
		}
		if ev.SessionID == "" {
			logger.Debug("pi state_update ignored (no sessionId)",
				slog.String("raw", string(raw)))
			return nil, nil
		}
		return []agent.AgentEvent{{
		Kind:      agent.EventAgentReady,
		SessionID: ev.SessionID,
		Model:     modelDisplay(ev.ModelID, ev.ModelName),
		AgentName: t.agentName,
		Workspace: t.workspace,
		Branch:    t.branch,
	}}, nil

	default:
		// Unknown event. Drop silently with a debug log so a
		// future Pi version can add events without breaking the
		// bridge.
		logger.Debug("pi event unknown",
			slog.String("type", env.Type),
			slog.String("raw", string(raw)),
		)
		return nil, nil
	}
}

// translateMessageUpdateLocked dispatches message_update events to the
// per-delta handler. The nested assistantMessageEvent.type field is
// the discriminator; the surrounding eventEnvelope is discarded.
//
// Caller must hold turnMu.
func (t *translator) translateMessageUpdateLocked(raw []byte, logger *slog.Logger) ([]agent.AgentEvent, error) {
	var ev messageUpdateEnvelope
	if err := json.Unmarshal(raw, &ev); err != nil {
		return nil, err
	}
	var delta assistantMessageEvent
	if err := json.Unmarshal(ev.AssistantMessageEvent, &delta); err != nil {
		return nil, err
	}

	switch delta.Type {
	case "text_start":
		// A new block opens on this index. Drop any stale partial
		// so a missed text_end cannot bleed the previous block's
		// tail into this one.
		delete(t.turn.textBuf, delta.ContentIndex)
		return nil, nil

	case "text_delta":
		// Accumulate only. See the F-52 note at the top of the file
		// for why this no longer emits.
		if delta.Delta == "" {
			return nil, nil
		}
		b := t.turn.textBuf[delta.ContentIndex]
		if b == nil {
			b = &strings.Builder{}
			t.turn.textBuf[delta.ContentIndex] = b
		}
		b.WriteString(delta.Delta)
		t.turn.active = true
		return nil, nil

	case "text_end":
		// Block complete — move it into pendingText, awaiting either
		// a tool boundary or the turn's EventAgentResult.
		t.closeTextBlockLocked(delta.ContentIndex)
		return nil, nil

	case "thinking_start":
		t.turn.thinkBuf.Reset()
		return nil, nil

	case "thinking_delta":
		t.turn.thinkBuf.WriteString(delta.Delta)
		t.turn.active = true
		return nil, nil

	case "thinking_end":
		// Reasoning flushes on its own boundary rather than at the
		// tool boundary: it is a distinct surface (💭 vs 💬) and must
		// never end up inside the reply's EventAgentResult.
		text := strings.TrimSpace(t.turn.thinkBuf.String())
		t.turn.thinkBuf.Reset()
		if text == "" {
			return nil, nil
		}
		return []agent.AgentEvent{{
			Kind: agent.EventAgentText,
			Text: thinkingPrefix + text,
		}}, nil

	case "toolcall_start", "toolcall_delta", "toolcall_end":
		// toolcall_* precedes tool_execution_* in Pi's event stream.
		// We deliberately do NOT emit here: tool_execution_start
		// carries the canonical name and args, and emitting twice
		// would surface a phantom "starting" line in the receipt
		// between two genuine tool invocations.
		return nil, nil

	case "start", "done", "error":
		// Lifecycle markers on the assistant message; no event.
		return nil, nil

	default:
		logger.Debug("pi message_update delta unknown",
			slog.String("type", delta.Type),
			slog.String("raw", string(raw)),
		)
		return nil, nil
	}
}

// translateMessageEndLocked dispatches message_end events by role.
// Neither role emits an event any more: assistant messages record the
// turn's final text / stopReason / usage for agent_settled to deliver,
// and toolResult is already covered by tool_execution_end.
//
// A turn can contain several assistant messages (text -> toolCall ->
// toolResult -> next assistant message). Emitting per message, as the
// pre-F-52 code did, produced one EventAgentResult per message rather than
// one per turn.
//
// Caller must hold turnMu.
func (t *translator) translateMessageEndLocked(raw []byte, logger *slog.Logger) ([]agent.AgentEvent, error) {
	var env struct {
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	if len(env.Message) == 0 {
		return nil, nil
	}

	// Probe the role with a tiny decode so we can route on it
	// without committing to the heavier struct for the wrong
	// variant.
	var probe struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(env.Message, &probe); err != nil {
		return nil, err
	}

	switch probe.Role {
	case "assistant":
		var msg assistantMessage
		if err := json.Unmarshal(env.Message, &msg); err != nil {
			return nil, err
		}
		t.recordAssistantMessageLocked(msg)
		return nil, nil
	case "toolResult":
		// Already covered by tool_execution_end.
		return nil, nil
	default:
		logger.Debug("pi message_end role unknown",
			slog.String("role", probe.Role),
			slog.String("raw", string(raw)),
		)
		return nil, nil
	}
}

// recordAssistantMessageLocked folds one finalized assistant message
// into the turn state. Nothing is emitted; agent_settled delivers.
//
// Caller must hold turnMu.
func (t *translator) recordAssistantMessageLocked(msg assistantMessage) {
	// Pi's own rendering of this message's text. Kept only as a
	// fallback for when the delta stream produced nothing (replayed
	// or non-streamed message) — the streamed buffers are the
	// primary source because they are what the tool-boundary flush
	// operates on, and mixing the two could double-deliver.
	var text string
	for _, b := range msg.Content {
		if b.Type == "text" && b.Text != "" {
			if text != "" {
				text += "\n"
			}
			text += b.Text
		}
	}
	// Overwrite, not append: only the turn's final assistant message
	// is a candidate for the result text. An earlier message's text
	// has already been flushed at the tool boundary that followed it.
	t.turn.lastMessageText = text
	t.turn.stopReason = msg.StopReason
	t.turn.active = true

	// Usage is a context-occupancy snapshot, so the latest one wins.
	// See turnState.lastUsage for why summing would be wrong.
	// Empty usage blocks are common (synthetic messages); leaving the
	// previous snapshot in place is better than clobbering it with
	// zeroes.
	if u := decodeMessageUsage(msg.Usage, int(t.contextWindow.Load())); u != nil {
		t.turn.lastUsage = u
	}
}

// closeTextBlockLocked drains the accumulated deltas for one content
// index into pendingText. No-op when the index has no buffer.
//
// Caller must hold turnMu.
func (t *translator) closeTextBlockLocked(idx int) {
	b := t.turn.textBuf[idx]
	if b == nil {
		return
	}
	delete(t.turn.textBuf, idx)
	text := b.String()
	if text == "" {
		return
	}
	if t.turn.pendingText != "" {
		t.turn.pendingText += "\n"
	}
	t.turn.pendingText += text
}

// closeOpenBlocksLocked drains every still-open text block, in
// ascending content-index order so the result is deterministic
// regardless of Go's map iteration order.
//
// Normally a no-op: Pi closes each block with text_end. It matters on
// the abort / error paths, where a turn can settle with deltas still
// buffered — without this the tail of the reply would be silently lost.
//
// Caller must hold turnMu.
func (t *translator) closeOpenBlocksLocked() {
	if len(t.turn.textBuf) == 0 {
		return
	}
	idxs := make([]int, 0, len(t.turn.textBuf))
	for i := range t.turn.textBuf {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	for _, i := range idxs {
		t.closeTextBlockLocked(i)
	}
}

// flushPendingTextLocked delivers the accumulated narration as a
// single EventAgentText and clears it. Returns nil (not an empty event)
// when there is nothing pending, so callers can append unconditionally.
//
// Caller must hold turnMu.
func (t *translator) flushPendingTextLocked() []agent.AgentEvent {
	t.closeOpenBlocksLocked()
	text := strings.TrimSpace(t.turn.pendingText)
	t.turn.pendingText = ""
	if text == "" {
		return nil
	}
	// Only a real delivery arms the flag — a no-op flush (nothing
	// buffered) must leave the lastMessageText fallback available for
	// turns where Pi never streamed in the first place.
	t.turn.textDelivered = true
	return []agent.AgentEvent{{Kind: agent.EventAgentText, Text: text}}
}

// finishTurnLocked builds the turn's single EventAgentResult, or returns
// nil when the turn observed nothing worth reporting.
//
// Text resolution order:
//  1. pendingText — what we accumulated since the last flush. This is
//     the normal path and is exactly the segment the user has NOT
//     seen yet.
//  2. lastMessageText — Pi's own composition, but ONLY when no reply
//     text has been delivered yet this turn. See turnState.textDelivered
//     for why the guard is load-bearing.
//  3. emptyReplyFallback — never emit an empty Text; see the const.
//
// Caller must hold turnMu.
func (t *translator) finishTurnLocked() []agent.AgentEvent {
	t.closeOpenBlocksLocked()

	// An untouched turn means agent_settled fired without an
	// accompanying run (Pi settles out-of-band paths such as a
	// fire-and-forget compaction the same way). Emitting a result
	// there would put a spurious "Done." card in the user's chat.
	if !t.turn.active {
		return nil
	}

	text := strings.TrimSpace(t.turn.pendingText)
	if text == "" && !t.turn.textDelivered {
		text = strings.TrimSpace(t.turn.lastMessageText)
	}
	if text == "" {
		text = emptyReplyFallback
	}
	t.turn.pendingText = ""

	return []agent.AgentEvent{{
		Kind: agent.EventAgentResult,
		Result: &agent.AgentResultEvent{
			Text:    text,
			Subtype: t.turn.stopReason,
			Usage:   t.turn.lastUsage,
		},
	}}
}

// decodeMessageUsage converts Pi's per-message usage block into the
// bridge-neutral agent.UsageInfo. Returns nil when the block is
// absent or reports nothing — the documented "no usage this message"
// signal, which keeps the previous snapshot in place.
//
// Each breakdown field (input / output / cacheRead / cacheWrite) is
// checked independently. Pi's `totalTokens` is reported separately
// from the breakdown on the wire, so it can legitimately be 0 while
// the per-field totals are non-zero (synthetic messages, early
// releases, schema variants). Gating on `Total` alone would silently
// drop those turns' usage — breaking the F-52 promise that "usage
// 100% flows through". Mirrors the symmetric check in
// internal/bridge/claudecode/stream.go:decodeUsage.
// decodeMessageUsage translates a single pi message_usage payload
// (cost + input/output/cache counters) into the canonical
// agent.UsageInfo. Single-shot per-turn snapshot — runtime
// does NOT aggregate across turns.
//
// ctxWindow is the API-reported model context-window size in
// tokens, sourced from translator.contextWindow (F-54) which is
// filled from get_state.data.model.contextWindow. Doc 1
// context-window-pct formula is applied when both numerator and
// denominator are positive; otherwise the field stays at zero
// and the channel footer omits the "X%" segment. See
// docs/feat/F-45-session-footer.md §1.5 /
// docs/feat/F-54-pi-contextwindow-from-get-state.md §2.2.
func decodeMessageUsage(u *messageUsage, ctxWindow int) *agent.UsageInfo {
	if u == nil {
		return nil
	}
	costZero := u.Cost == nil || u.Cost.Total == 0
	if u.Input == 0 && u.Output == 0 && u.CacheRead == 0 && u.CacheWrite == 0 && costZero {
		return nil
	}
	out := &agent.UsageInfo{
		InputTokens:          u.Input,
		OutputTokens:         u.Output,
		CacheReadInputTokens: u.CacheRead,
		// Pi does not separate cache_creation from cache_write in
		// its user-facing usage block. Map cacheWrite to
		// CacheCreationInputTokens for parity with claudecode.
		CacheCreationInputTokens: u.CacheWrite,
	}
	if u.Cost != nil {
		out.CostUSD = u.Cost.Total
	}
	if ctxWindow > 0 {
		used := u.Input + u.Output + u.CacheRead + u.CacheWrite
		if used > 0 {
			out.ContextWindowPct = float64(used) / float64(ctxWindow) * 100
		}
		// F-55: forward the window alongside X% so the footer can
		// render `X% (window)`. Single render-side consumer; the
		// runtime does not recompute / catalog / clamp based on
		// this value — see docs/feat/F-55-footer-show-context-window.md.
		out.ContextWindow = ctxWindow
	}
	return out
}

// emitConnected is invoked from session.go after the get_state handshake
// succeeds. It is independent of the per-event translate path
// because EventAgentReady is driven by a response, not an event.
func (t *translator) emitConnected(result *getStateResult) []agent.AgentEvent {
	if t.connectedSent {
		return nil
	}
	t.connectedSent = true
	// F-54 §3: unconditional reset BEFORE the conditional Store.
	// Without this, /new's second emitConnected would silently
	// inherit the previous session's window when the new model's
	// get_state returns ContextWindow=0 (catalog miss, RPC hiccup,
	// future set_model) — every subsequent pct would be computed
	// against the WRONG model's window. With this, the same-model
	// /new case behaves identically to the old code (reset → Store
	// the same value), and the model-switch /new case correctly
	// clears the stale value.
	t.contextWindow.Store(0)
	modelID := ""
	modelName := ""
	if result != nil && result.Model != nil {
		modelID = result.Model.ID
		modelName = result.Model.Name
		// F-54: cache the API-reported context-window on the
		// translator so subsequent decodeMessageUsage calls can
		// compute per-turn ContextWindowPct. Bridge-local state;
		// only the percentage crosses the UsageInfo boundary.
		// Stored via atomic.StoreInt64 so the concurrent
		// decodeMessageUsage read (under turnMu) sees a coherent
		// value without coupling the two mutexes.
		if result.Model.ContextWindow > 0 {
			t.contextWindow.Store(int64(result.Model.ContextWindow))
		}
	}
	sessionID := ""
	if result != nil {
		sessionID = result.SessionID
	}
	return []agent.AgentEvent{{
		Kind:      agent.EventAgentReady,
		SessionID: sessionID,
		Model:     modelDisplay(modelID, modelName),
		AgentName: t.agentName,
		Workspace: t.workspace,
		Branch:    t.branch,
	}}
}

// stringOrEmpty renders a json.RawMessage as a string for the
// AgentToolStartEvent.Args / AgentToolEndEvent.Output fields, which both
// carry a free-form text payload in the agent package.
func stringOrEmpty(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	return string(raw)
}

// toolErrorIf turns a boolean isError flag into a non-nil error
// suitable for GoneToolEndErr. We do not include the tool's
// stdout text in the error; channels render Err string when
// non-nil and ignore Output.
func toolErrorIf(isError bool) error {
	if !isError {
		return nil
	}
	return &piToolError{}
}

// piToolError is a sentinel error type so the renderer can detect
// "tool failed" without string-comparing the wrapped cause.
type piToolError struct{}

func (e *piToolError) Error() string { return "pi: tool execution failed" }

// modelDisplay composes a human-readable model label for EventAgentReady
// from Pi's id and name. We prefer "name (id)" when both are
// present so the receipt shows the friendly name and the receipt
// log line keeps the id for grep.
func modelDisplay(id, name string) string {
	if id == "" && name == "" {
		return ""
	}
	if name == "" {
		return id
	}
	if id == "" {
		return name
	}
	return name + " (" + id + ")"
}
