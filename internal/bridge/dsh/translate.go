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
	// map[int]*strings.Builder (NOT a slice of strings.Builder) for
	// a critical reason: strings.Builder has a noCopy / copyCheck
	// guard (Go 1.20+) that panics the moment you call Grow/Write/
	// WriteString on a Builder that was value-copied. A slice of
	// values would silently break the moment `append` grew the
	// underlying array — every existing Builder would be
	// value-copied, its internal `addr` would still point to the
	// OLD array, and the next Grow/WriteString on any previously-
	// touched index would panic. We hit this in production
	// (2026-08-15) after a long dsh turn grew textBuf past its
	// initial capacity — see translate.go assistant/chunk case.
	//
	// A map of pointers keeps each Builder at a stable address for
	// its entire lifetime, so Grow/WriteString are always safe.
	// contentIndex stays small in practice (0..N where N is the
	// number of concurrent text streams, typically 1) so the
	// map[int] overhead is negligible — pi/translate.go has used
	// this exact shape since the F-52 state machine was ported.
	textBuf     map[int]*strings.Builder
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
		textBuf:      map[int]*strings.Builder{}, // lazy-grown by contentIndex
		pendingTools: map[string]pendingTool{},
	}
}

// maxTextStreams caps the assistant/chunk contentIndex to prevent
// a malicious or buggy dsh web from triggering OOM via a single
// huge JSON int (which would grow textBuf slice to ~96 bytes × N).
// 16 streams is comfortably more than any legitimate dsh message
// produces (real-world: typically 1).
const maxTextStreams = 16

// handleMuxFrame moved to handle_mux.go in F-DSH-CHAT-001.
// The mux-pump entry now lives alongside session/projection routing
// so the dispatcher + projection channels can be reasoned about
// together. See handle_mux.go for the routing table.

// handleHostFrame moved to handle_mux.go in F-DSH-CHAT-001.

// handleSessionEvent removed in F-DSH-CHAT-001 (I1 fix).
//
// The previous implementation was a 200-line switch statement that
// duplicated the dispatcher logic in dispatch.go. Production path
// goes through driver.dispatcher.dispatch; the only caller was the
// textBuf grow-and-reuse regression test, which has been refactored
// to construct a dispatcher (see translate_regression_test.go's
// `makeDispatcherForTest` helper).
//
// If you need the old switch back for debugging, `git log` the
// removed block — it's preserved in commit history.

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
