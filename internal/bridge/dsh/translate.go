// translate.go — F-52 state machine: SessionEvent → agent.AgentEvent.
//
// dsh web's mux stream pushes 42+ SessionEvent variants via
// serverFrame{method:"session/event"}. We decode the 11 we care
// about and emit nightme's AgentEvent vocabulary. F-52 granularity
// contract applies: one EventAgentText is one complete semantic
// block (assistant/message boundary or tool_execution_start flush
// point), NOT a streaming delta. Mirror pi/translate.go §2 design
// because dsh's `assistant/chunk → assistant/message → turn/end`
// sequence is identical.
//
// Required guards (F-52 / F-32 / F-49):
//   - textDelivered : 防止 tool-结尾 turn 把已 flush 段当 result 重放
//   - active        : 空 turn 不出 "Done." 卡片(避免乱入 result)
//   - resetWindow   : /new 中断时丢弃半截 event(目前 dsh 用 Reset→
//                     ErrRestartRequired,所以本 guard 暂时只用于
//                     turn 边界清空,跨 session 持久化由 server 管)
//
// ContentImage / ContentFile on tool inputs degrade to bracketed
// annotations on text args (same fallback as print-mode / pi /
// claudecode). See session.go contentBlocksToDTO for the inverse
// direction.
package dsh

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/cnlangzi/nightme/internal/agent"
)

// translator holds per-turn buffering state. One per driver
// instance; reset at every turn boundary (turn/start). Locked
// because handleMuxFrame may be called from the read pump while
// runtime calls SendBlocks etc. (currently SendBlocks doesn't touch
// the translator, but the lock is cheap and future-proof).
type translator struct {
	agentName string
	workspace string

	mu sync.Mutex

	// F-52 buffer state — mirrors pi/translate.go. textBuf is a
	// slice (not map[int]) because contentIndex is small in practice
	// (0..N where N is the number of text streams in the message,
	// typically 1); a slice avoids the map[int]*Builder allocation
	// overhead and the value-vs-pointer dance.
	textBuf     []strings.Builder
	thinkBuf    strings.Builder
	pendingText string
	lastText    string
	lastUsage   *agent.UsageInfo

	// pendingTools backfills Name + Args onto tool/result events
	// (the result event only carries result + isError, not the
	// tool name or args — dsh follows the same wire shape as pi).
	// Cleared at every turn/start to prevent stale entries from a
	// cancelled previous turn colliding with a recycled toolCallID
	// in the next turn (dsh's runtime does reuse numeric ids).
	pendingTools map[string]pendingTool

	// textDelivered: did we already flush `pendingText` for this
	// turn? If yes, fall back to lastText only when the *whole*
	// turn produced no fresh text. Avoids re-emitting segments
	// the user already saw in the rolling log.
	textDelivered bool

	// active: at least one of (chunk / message / tool_call / tool_
	// result / assistant message_end) was observed for this turn.
	// When turn/end fires with active==false we emit EventDone
	// without EventResult to avoid the "Done." phantom card.
	//
	// Set PER-TYPE (not unconditionally) so unknown / future event
	// types (which fall through to the default branch and only log)
	// don't flip active=true and synthesize a phantom Done card.
	active bool
}

// pendingTool records Name + Args from tool/call so we can
// backfill onto the matching tool/result.
type pendingTool struct {
	Name string
	Args string
}

// newTranslator constructs a translator for the given agent +
// workspace. agentName / workspace are stamped onto every event
// (the runtime uses these for header rendering).
func newTranslator(agentName, workspace string) *translator {
	return &translator{
		agentName:    agentName,
		workspace:    workspace,
		textBuf:      []strings.Builder{}, // lazy-grown by contentIndex
		pendingTools: map[string]pendingTool{},
	}
}

// maxTextStreams caps the assistant/chunk contentIndex to prevent
// a malicious or buggy dsh web from triggering OOM via a single
// huge JSON int (which would grow textBuf slice to ~96 bytes × N).
// 16 streams is comfortably more than any legitimate dsh message
// produces (real-world: typically 1).
const maxTextStreams = 16

// handleMuxFrame is the mux-pump entry. It unmarshals the payload
// and dispatches by method. All paths run under the translator's
// mutex.
func (d *driver) handleMuxFrame(method, rpcID string, payload json.RawMessage) {
	switch method {
	case "session/subscribed":
		// Baseline frame on stream open. Just log it; the
		// SessionID is already known via session.create.
		var sub muxSessionSubscribed
		if err := json.Unmarshal(payload, &sub); err != nil {
			dLog("dsh: subscribed decode: %v", err)
			return
		}
		dLog("dsh: subscribed", "session_id", sub.SessionID, "last_seq", sub.LastSeq)
	case "session/event":
		var ev muxSessionEvent
		if err := json.Unmarshal(payload, &ev); err != nil {
			dLog("dsh: session/event decode: %v", err)
			return
		}
		d.translate.handleSessionEvent(ev, d.deliver)
	case "session/projection":
		// Projection snapshots (title/tasks/sessionListMetadata).
		// The runtime doesn't currently consume these; we just
		// log so a debug session can see them.
		dLog("dsh: session/projection: %d bytes", len(payload))
	case "approval/requested":
		var ar muxApprovalRequested
		if err := json.Unmarshal(payload, &ar); err != nil {
			dLog("dsh: approval/requested decode: %v", err)
			return
		}
		d.handleApprovalRequested(ar)
	case "approval/resolved":
		var ar muxApprovalResolved
		_ = json.Unmarshal(payload, &ar)
		dLog("dsh: approval/resolved audit", "approval_id", ar.ApprovalID, "outcome", ar.Outcome)
	case "question/requested":
		var qr muxQuestionRequested
		if err := json.Unmarshal(payload, &qr); err != nil {
			dLog("dsh: question/requested decode: %v", err)
			return
		}
		// Same routing-key constraint as approval/requested above:
		// /api/respond is keyed on the server-frame rpcId, NOT the
		// payload's SessionID+":q". We pass rpcID as the key and let
		// handleQuestionRequested manage the display logic.
		d.handleQuestionRequested(rpcID, qr)
	case "question/resolved":
		dLog("dsh: question/resolved audit")
	case "session/queue":
		// Future: surface queued/steering items in a QueueDock UI
		// (F-38 follow-up). For now, debug log only.
		dLog("dsh: session/queue: %d bytes", len(payload))
	case "session/jobs":
		dLog("dsh: session/jobs: %d bytes", len(payload))
	case "approval/asked":
		var aa approvalAskedEvent
		if err := json.Unmarshal(payload, &aa); err != nil {
			dLog("dsh: approval/asked decode: %v", err)
			return
		}
		d.handleInlineApproval(aa.ToolCallID, aa.ToolName, aa.Action, aa.Options)

	default:
		dLog("dsh: mux unknown method=%q len=%d", method, len(payload))
	}
}

// handleHostFrame is the host-pump entry. Host events are
// lifecycle-shaped (session/created, session/destroyed,
// agent/status). We currently log all of them; future PR may
// surface create/destroy to the runtime.
func (d *driver) handleHostFrame(method, rpcID string, payload json.RawMessage) {
	dLog("dsh: host method=%q len=%d", method, len(payload))
	// No-op for now. Host events don't drive AgentEvent semantics;
	// they describe container-level state we'd surface as
	// diagnostic info only.
}

// handleSessionEvent dispatches one session/event mux frame to the
// translator. Mirrors pi/translate.go's F-52 logic with dsh's
// event vocabulary.
func (t *translator) handleSessionEvent(ev muxSessionEvent, deliver func(agent.AgentEvent)) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Decode just the envelope type first; per-Type payloads are
	// unmarshaled inside each case.
	var env sessionEventEnvelope
	if err := json.Unmarshal(ev.Event, &env); err != nil {
		dLog("dsh: session.event envelope decode: %v", err)
		return
	}
	t.active = true

	switch env.Type {
	case "assistant/chunk":
		var ch assistantChunkEvent
		if err := json.Unmarshal(ev.Event, &ch); err != nil {
			return
		}
		t.active = true
		// textBuf is lazy-grown: most turns have only 1 stream
		// (contentIndex 0) so we avoid allocating a backing array
		// until we see the first chunk. NB: we MUST append a
		// zero-value Builder and then Grow it in place — copying
		// a non-zero Builder by value triggers the runtime
		// "strings: illegal use of non-zero Builder copied by value"
		// check (Go 1.20+).
		//
		// contentIndex is capped at maxTextStreams to prevent a
		// malicious / buggy dsh web from triggering OOM via a
		// single huge JSON int. 16 streams is comfortably more
		// than any legitimate dsh message needs.
		if ch.ContentIndex < 0 || ch.ContentIndex >= maxTextStreams {
			dLog("dsh: assistant/chunk contentIndex out of range",
				"content_index", ch.ContentIndex,
				"max", maxTextStreams)
			return
		}
		for len(t.textBuf) <= ch.ContentIndex {
			t.textBuf = append(t.textBuf, strings.Builder{})
		}
		t.textBuf[ch.ContentIndex].Grow(256) // typical chunk size hint
		t.textBuf[ch.ContentIndex].WriteString(ch.Delta)

	case "assistant/message":
		var am assistantMessageEvent
		if err := json.Unmarshal(ev.Event, &am); err != nil {
			return
		}
		t.active = true
		// Mirror pi F-52: at assistant/message boundary, take
		// content[].text into pendingText. We don't emit EventText
		// here — the tool-boundary flush below is the canonical
		// emit point so OutResult (turn/end) carries the LAST
		// paragraph and never duplicates already-shown chunks.
		t.pendingText = pickText(am.Content)
		t.lastText = t.pendingText

	case "tool/call":
		t.active = true
		var tc toolCallEvent
		if err := json.Unmarshal(ev.Event, &tc); err != nil {
			return
		}
		// Flush pendingText at tool-boundary (F-52 — tool start is
		// the moment the user expects to see the preceding prose).
		if t.pendingText != "" {
			text := t.pendingText
			deliver(agent.AgentEvent{
				Kind: agent.EventAgentText,
				Text: text,
			})
			t.textDelivered = true
			t.pendingText = ""
		}
		// Stash Name + Args so tool/result can backfill.
		t.pendingTools[tc.ToolCallID] = pendingTool{
			Name: tc.ToolName,
			Args: tc.Args,
		}
		deliver(agent.AgentEvent{
			Kind: agent.EventAgentToolStart,
			ToolStart: &agent.AgentToolStartEvent{
				ID:   tc.ToolCallID,
				Name: tc.ToolName,
				Args: tc.Args,
			},
		})

	case "tool/result":
		var tr toolResultEvent
		if err := json.Unmarshal(ev.Event, &tr); err != nil {
			return
		}
		t.active = true
		pt, hasPending := t.pendingTools[tr.ToolCallID]
		if hasPending {
			delete(t.pendingTools, tr.ToolCallID)
		}
		name := pt.Name
		args := pt.Args
		if !hasPending {
			// Orphan result (no matching call). Use fallback
			// name so the renderer doesn't print "tool" — better
			// than silently dropping args.
			name = ""
		}
		var errOut error
		if tr.IsError {
			errOut = fmt.Errorf("dsh: tool %s failed: %s", name, tr.Result)
		}
		ev := agent.AgentEvent{
			Kind: agent.EventAgentToolEnd,
			ToolEnd: &agent.AgentToolEndEvent{
				ID:     tr.ToolCallID,
				Name:   name,
				Args:   args,
				Output: tr.Result,
			},
		}
		if errOut != nil {
			ev.Err = errOut
		}
		deliver(ev)

	case "turn/start":
		var ts turnStartEvent
		_ = json.Unmarshal(ev.Event, &ts)
		t.active = true
		// New turn — clear per-turn buffers AND pendingTools.
		// pendingTools is cleared because:
		//   1. Tools are turn-scoped in dsh (a tool called in
		//      turn N has no relation to a tool in turn N+1).
		//   2. dsh's runtime MAY recycle numeric toolCallIDs across
		//      turns. Without clearing, a stale pendingTool from
		//      turn N would corrupt the tool/result event of a
		//      recycled id in turn N+1.
		t.textBuf = t.textBuf[:0]
		t.thinkBuf.Reset()
		t.pendingText = ""
		t.lastText = ""
		t.lastUsage = nil
		t.textDelivered = false
		t.pendingTools = map[string]pendingTool{}

	case "turn/end":
		var te turnEndEvent
		_ = json.Unmarshal(ev.Event, &te)
		// F-52 guard #1: only synthesize EventResult if the turn
		// was actually active. fire-and-forget settlements
		// (e.g. out-of-band compactions) don't get a phantom
		// "Done." card.
		if t.active {
			text := ""
			// F-52 guard #2: prefer pendingText (the part the
			// user hasn't seen yet). Fall back to lastText ONLY
			// when textDelivered==false (no flush happened this
			// turn). Avoids re-emitting a paragraph the user
			// already saw in the rolling log.
			if t.pendingText != "" {
				text = t.pendingText
			} else if !t.textDelivered && t.lastText != "" {
				text = t.lastText
			}
			if text == "" {
				// Empty result + active turn → dsh's "empty
				// reply fallback" (F-52 §2.4.3): produce a
				// non-empty string so gateway.Translate doesn't
				// discard the EventResult along with its Usage.
				text = "Done."
			}
			usage := t.lastUsage
			deliver(agent.AgentEvent{
				Kind: agent.EventAgentResult,
				Result: &agent.AgentResultEvent{
					Text:       text,
					DurationMs: 0, // not on the wire; runtime can compute
					Subtype:    stopReasonToSubtype(te.StopReason),
					Usage:      usage,
				},
			})
		}
		deliver(agent.AgentEvent{
			Kind: agent.EventAgentDone,
			Done: &agent.AgentDoneEvent{
				ExitCode: 0,
				Reason:   "settled",
				Usage:    t.lastUsage,
			},
		})
		// Reset per-turn state for the next turn.
		t.active = false
		t.textDelivered = false
		t.pendingText = ""
		t.lastText = ""
		t.lastUsage = nil

	case "compaction/end":
		var ce compactionEndEvent
		_ = json.Unmarshal(ev.Event, &ce)
		// F-49 compaction event. There's no EventAgentCompaction
		// kind in the agent package today (pi/claudecode also
		// don't emit it) — adding it is a cross-bridge change
		// that lands separately. Until then, debug log so a
		// curious operator can see compaction happen.
		if !ce.Aborted {
			dLog("dsh: compaction completed", "reason", ce.Reason)
		}

	case "todo/write":
		var tw todoWriteEvent
		if err := json.Unmarshal(ev.Event, &tw); err != nil {
			return
		}
		// F-38 TaskList snapshot. We always emit Create (not
		// Update) so the runtime replaces the checklist wholesale
		// rather than diff-applying — last-write-wins.
		items := make([]agent.AgentTaskItem, 0, len(tw.Items))
		for _, it := range tw.Items {
			items = append(items, agent.AgentTaskItem{
				Subject: it.Content,
				Status:  todoStatusToTaskStatus(it.Status),
			})
		}
		deliver(agent.AgentEvent{
			Kind: agent.EventAgentTaskCreate,
			TaskList: &agent.AgentTaskListEvent{Items: items},
		})

	case "approval/asked":
		// approval/asked via the session/event channel is
		// handled at the dispatch layer (handleMuxFrame) where
		// the driver reference is in scope — translator doesn't
		// know about the driver.

	default:
		dLog("dsh: session.event unknown type=%q", env.Type)
	}
}

// pickText walks content[] in order, concatenating text blocks
// verbatim. Non-text blocks (image / file) are skipped — the
// bridge already degraded them to bracketed annotations at
// contentBlocksToDTO time.
func pickText(content []contentBlockDTO) string {
	var sb strings.Builder
	for _, b := range content {
		if b.Type == "text" && b.Text != "" {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

// stopReasonToSubtype maps dsh's stopReason vocabulary onto the
// bridge's Subtype string (used for error categorization in /gtw
// commit-style calls). Mirrors the codex "completed" / "failed"
// convention — finer-grained subtyping can land later.
func stopReasonToSubtype(sr string) string {
	if sr == "" || sr == "stop" || sr == "end_turn" {
		return "completed"
	}
	return "failed"
}

// todoStatusToTaskStatus maps dsh's todo status vocabulary onto
// nightme's AgentTaskStatus enum.
func todoStatusToTaskStatus(s string) agent.AgentTaskStatus {
	switch s {
	case "completed":
		return agent.TaskCompleted
	case "in_progress":
		return agent.TaskInProgress
	default:
		return agent.TaskPending
	}
}
