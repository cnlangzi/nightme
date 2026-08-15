// Package feishu — F-44 simplified MessageReceipt with F-44 revert
// for OutReply folding.
//
// Post-F-44 the receipt card carries two sections:
//
//	💬 chunk 1   ⬅ F-44 revert: OutReply folds into the rolling log
//	💬 chunk 2   ⬅   (each entry split into 1+ div elements)
//	💬 chunk 3
//	📋 Tasks     ⬅ F-38: agent task checklist (F-44 simplified
//	  - [ ] Subject       surface — no header / footer / evicted
//	  - [x] Subject       marker, just the two sections above)
//
// F-25 → F-42 had OutReply / OutResult / OutInit / OutUsage fold
// into the same card. F-44 first-pass reversed that: OutReply /
// OutResult each go to their own top-level Create message; OutInit
// / OutUsage are silently dropped until the footer PR. F-44 revert
// (this file) restores OutReply folding into the rolling log because
// the top-level Create surface produced a hard-to-scan stream of N
// standalone bubbles for any long reply. (F-49: OutCompaction kind
// deleted — the runtime consumes EventAgentCompaction directly via
// AgentSession.RecordCompaction() and produces no OutboundMessage,
// so the receipt path never sees this kind.)
// OutResult / OutTask* stay on top-level Create (final-answer +
// checklist don't have the same "many small chunks" visual
// problem).
//
// Card limits + bail-out: when an OutReply chunk would push the
// card past 50 elements / 30 KB envelope, AppendEntry returns
// ErrReceiptOverflow and the caller sends that chunk as a fresh
// top-level Create (ReplyInChat — always visible in main chat,
// escapes the parent-thread drawer). The receipt itself stays
// anchored to the user message (ReplyInBoth) so the visual thread
// to the user input is preserved in the normal case.
package feishu

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/messages"
)

// receiptBot is the minimal Feishu surface the task receipt depends on.
// *Adapter satisfies this via its existing methods. Tests pass a mock
// that records the calls. Keeping the field typed as an interface
// (instead of *Adapter) makes the receipt unit-testable without spinning
// up a real lark client.
//
// F-44: only SendCard + PatchMessage remain in active use — first-send
// then in-place PATCH for the task checklist (see docs/channel/feishu.md
// §5). AddReaction / UpdateMessage / SendMessageText are kept on the
// interface for backward compatibility with mocks in existing tests but
// are no longer called from production code paths.
type receiptBot interface {
	AddReaction(ctx context.Context, msgID, emoji string) (string, error)
	UpdateMessage(ctx context.Context, messageID, text string) error
	SendMessageText(ctx context.Context, chatID, text, rootID string, replyInThread bool) (string, error)
	// SendCardForReceipt posts a new interactive card and returns its
	// message ID. Used on the FIRST render of a receipt (no cardMsgID
	// yet). v1.3.x (§13.10): rootID is the user message id to thread
	// the cold-start card to. Renamed from SendCard in F-46 to
	// disambiguate from the channel.Channel.SendCard interface
	// method that takes a messages.OutboundMessage.
	SendCardForReceipt(ctx context.Context, chatID, cardJSON, rootID string, replyInThread bool) (string, error)
	// PatchMessage replaces the body of an existing message in place
	// (Feishu PATCH /im/v1/messages/{id}). Used on every render after
	// the first.
	PatchMessage(ctx context.Context, messageID, cardJSON string) error
	// AddReaction / UpdateMessage / SendMessageText are retained on
	// the interface for backward compatibility with existing mock
	// implementations in tests, but the F-44 task-only receipt no
	// longer calls them in production paths. They remain on the
	// interface so test mocks don't have to be rewritten every time
	// the interface contracts evolves; production callers should
	// prefer the two methods above (SendCard / PatchMessage).
}

// divTextCharLimit caps the text length of a single `div` element in the
// receipt card body. This is the Feishu hard limit on `div.text` — the
// splitter uses this as maxRunes so no div ever exceeds the server's
// accepted size.
//
// F-37 design: receipt entries may produce multiple `div` elements when
// split, but each individual div stays under this limit. F-44 keeps this
// constant because buildTaskChecklistChunks still produces multiple divs
// for long task lists.
const divTextCharLimit = 1000

// MessageReceipt is the per-user-message rolling-log + task-checklist
// display (F-25 → F-40 + F-38 + F-44 simplification + F-44 revert
// for OutReply folding). One receipt owns ONE Feishu card message
// (the cardMsgID) which carries two sections: the rolling-log
// OutReply entries (multi-div when long) and the **📋 Tasks**
// checklist. OutResult / OutTask* have their own top-level Create
// messages and don't fold into the card (F-44 follow-up).
//
// F-44 fields:
//   - entries: rolling-log OutReply text chunks. Each entry is split
//     into 1+ div elements by splitMarkdownForDivs (the per-entry
//     budget respects Feishu's div.text 1000 char hard cap). New
//     chunks arrive via AppendEntry; the per-card 50-element / 30 KB
//     envelope cap is checked BEFORE the PATCH is issued, and a
//     would-overflow entry is rejected with ErrReceiptOverflow so
//     the caller can send it as a fresh top-level Create (F-40
//     bail-out, F-44 follow-up styling).
//   - tasks: latest snapshot, replaced wholesale on each SetTaskList.
//   - cardMsgID / replyMsgID / initializing / lastBody / lastBodyPatch:
//     SendCard → PatchMessage bookkeeping (unchanged from F-42).
//   - bot / logger / mu / chatID / userMsgID: infrastructure.
//
// F-44 deleted fields (still removed; their writers all gone):
//   - agentName / workspace / branch / inputTokens / outputTokens:
//     OutInit / OutUsage silent drop until footer PR
//   - evicted: FIFO eviction was for OutReply entries (F-44 revert
//     reinstates entries but the hard eviction policy stays gone —
//     overflow now bails out to fresh messages instead of FIFO
//     truncating the receipt).
type MessageReceipt struct {
	chatID     string
	userMsgID  string
	replyMsgID string
	bot        receiptBot
	logger     *slog.Logger

	mu          sync.Mutex
	promptState chatsession.PromptState // F-53: feishu-local 2-value enum (was agent.PromptState 4-value)

	// entries is the rolling-log of OutReply text chunks, oldest
	// first. Each entry is rendered as a separate markdown element
	// (or split into multiple markdown elements by
	// splitMarkdownForDivs when the entry text exceeds
	// divTextCharLimit). Append-only; no FIFO eviction (F-44 revert
	// dropped that — overflow now bails out per-entry to a fresh
	// top-level Create instead of silently dropping old entries).
	entries []LogEntry

	// tasks is the latest Claude task snapshot (F-38) for this turn.
	// The bridge always sends the full snapshot, so we copy it verbatim
	// on every event. The slice is rendered as a single markdown
	// element list (split by divTextCharLimit).
	tasks []agent.AgentTaskItem

	// footerLines (F-45) is the rendered StatusBar footer
	// split into one entry per output line. Each entry maps to
	// one <plain_text> element inside the bottom-of-card <div>
	// — Feishu plain_text does NOT honour \n inside a single
	// element, so the receipt must store the multi-line form
	// directly (rather than a single string with embedded \n).
	// nil / empty slice = no footer (silent drop, pre-EventAgentReady,
	// or no StatusBar on the wire).
	footerLines []string

	// cardMsgID is the Feishu message id of the rolling-log card
	// once it has been created. Empty before the first render. After
	// the first SendCard it is set; subsequent renders PatchMessage
	// against this id rather than posting new messages.
	cardMsgID string

	// initializing is true between the moment ensureReceiptForReply
	// / ensureReceiptForTask registers this receipt in
	// receiptsByUserMsgID and the moment its first SendCard returns
	// with a real cardMsgID. During that brief window the
	// placeholder is visible to other goroutines (receiptFor /
	// ensureReceiptForReply return it), but its cardMsgID is still
	// empty — so a renderLocked driven by a concurrent AppendEntry
	// / SetTaskList would otherwise issue a second SendCard
	// (orphan). renderLocked checks this flag and short-circuits
	// while true, dropping the render so the ensure helper's own
	// SendCard is the only card in chat. The flag is cleared under
	// r.mu immediately after the SendCard return value is stored.
	// F-42 review finding #4/#5.
	initializing bool

	// Feishu limits message updates to roughly five per second.
	// Skip duplicate bodies and pace real PATCH requests.
	lastBody      string
	lastBodyPatch time.Time

	// heartbeat (F-63) carries the per-turn progress snapshot
	// sourced from runtime.HeartbeatTracker via OutboundMessage.
	// ThinkCount / ToolCount are monotonic counters; LastBeatAt
	// is refreshed by any OutboundKind (the "agent is alive"
	// signal). The header rendered by buildReceiptCard pulls
	// from this field — pre-F-63 the placeholder header was
	// only "⌨️ Working..." and disappeared on the first entry.
	heartbeat messages.HeartbeatSnapshot

	// heartbeatMinInterval is the minimum gap between two
	// heartbeat-driven PATCHes. ApplyHeartbeat consults this
	// against lastBodyPatch (renderLocked's own throttle state)
	// so any successful PATCH — entry / task / heartbeat —
	// bumps the window and prevents double-PATCHes when
	// entries and heartbeat both want to update within the same
	// 100ms. Zero disables the throttle (tests that want to
	// exercise ApplyHeartbeat back-to-back without time.Sleep
	// set it to 0).
	heartbeatMinInterval time.Duration
}

// NewMessageReceiptForReply constructs a fresh rolling-log + task
// receipt. F-44 callers (ensureReceiptForTask) use it for the
// task-checklist path; F-44 revert callers (ensureReceiptForReply)
// use it for the OutReply rolling-log path. The first entry is
// installed before this constructor is called (by the ensure
// helper); the constructor itself just wires infrastructure.
//
// bot is the adapter (or any receiptBot implementation) used by
// renderLocked to ship card bodies (SendCard on first render,
// PatchMessage on subsequent). F-44 callers pass `a`; passing nil
// will panic on the first render.
//
// The replyMsgID parameter is the Feishu message id of any
// pre-existing reply card (legacy F-44 path that no longer applies
// post-F-44-revert — kept for the constructor signature so the
// task-only caller can still pass a placeholder). Pass "" for
// fresh receipts (the typical F-44 revert case); the first
// SendCard in renderLocked populates cardMsgID.
func NewMessageReceiptForReply(chatID, userMsgID, replyMsgID string, bot receiptBot) *MessageReceipt {
	return &MessageReceipt{
		chatID:       chatID,
		userMsgID:    userMsgID,
		replyMsgID:   replyMsgID,
		cardMsgID:    replyMsgID,
		bot:          bot,
		logger:       slog.Default(),
		// F-63: 2s heartbeat throttle. Dense thinking streams can
		// fire 10+ OutHeartbeat events per second; we coalesce them
		// into one PATCH every 2s to stay well under Feishu's
		// ~5 QPS message-update limit. Zero would disable the
		// throttle (tests opt in).
		heartbeatMinInterval: 2 * time.Second,
		// F-53: initial state is chatsession.PromptRunning (was PromptPending
		// in v1.3). The "Prompt is born running" rule from
		// docs/feat/message_lifecycle.md §4.2 means we never need
		// a Pending→Running transition on first SetTaskList —
		// SetTaskListWithFooter's transition guard is removed
		// below.
		promptState: chatsession.PromptRunning,
	}
}

// SetTaskList replaces the per-turn task checklist (F-38) with a fresh
// snapshot from the bridge. The slice is copied so subsequent caller
// mutations to the underlying array cannot affect the receipt. Empty
// lists (len(Items)==0) are accepted and clear the checklist.
//
// F-44: SetTaskList is now the ONLY production writer to a receipt. It
// also promotes the receipt from PromptPending to chatsession.PromptRunning on
// first call (so future footer PR can recover the prompt state header
// without extra plumbing). Late SetTaskList after a successful PATCH
// still PATCHes the card with the new snapshot.
func (r *MessageReceipt) SetTaskList(ctx context.Context, list *agent.AgentTaskListEvent) error {
	return r.SetTaskListWithFooter(ctx, list, nil)
}

// SetTaskListWithFooter (F-45) replaces the per-turn task
// checklist AND stamps the rendered StatusBar footer at the
// bottom of the receipt card. footerLines may be nil / empty
// when no StatusBar is stamped (silent drop); the receipt
// preserves the previously-stamped footer in that case —
// symmetric with AppendEntryWithFooter's preserve-on-empty
// semantics, so a transient nil/zero StatusBar between
// turns doesn't wipe a previously-rendered footer.
func (r *MessageReceipt) SetTaskListWithFooter(ctx context.Context, list *agent.AgentTaskListEvent, footerLines []string) error {
	if r == nil {
		return nil
	}
	if list == nil {
		return errors.New("feishu receipt: SetTaskList called with nil AgentTaskListEvent")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	items := list.Items
	if len(items) == 0 {
		r.tasks = nil
	} else {
		copyItems := make([]agent.AgentTaskItem, len(items))
		copy(copyItems, items)
		r.tasks = copyItems
	}
	if len(footerLines) > 0 {
		r.footerLines = footerLines
	}
	// F-53: Pending→Running transition removed. Initial state is
	// already chatsession.PromptRunning (set at construction). Receipt never
	// transitions to chatsession.PromptDone in Phase 0 — that value is reserved
	// for a future UX PR that surfaces terminal state on the card
	// (see docs/feat/message_lifecycle.md §7).
	return r.renderLocked(ctx)
}

// chatsession.PromptState returns the current prompt execution state. Useful for
// tests + diagnostics. Returns the local chatsession.PromptState value stored on
// this receipt; callers should compare against chatsession.PromptRunning /
// chatsession.PromptDone.
//
// F-53: SetTaskList no longer promotes Pending → Running (there is
// no Pending state — receipts start at chatsession.PromptRunning). Receipts do
// not transition to chatsession.PromptDone in Phase 0.
func (r *MessageReceipt) PromptState() chatsession.PromptState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.promptState
}

// State returns the current state. Useful for tests + diagnostics.
func (r *MessageReceipt) State() chatsession.PromptState {
	return r.PromptState()
}

// Tasks returns a snapshot of the receipt's current task checklist.
// Holds r.mu; safe to call from any goroutine. The returned slice
// is the snapshot taken under the lock — SetTaskListWithFooter
// always installs a freshly-allocated slice (or nil for an empty
// checklist), so the snapshot is safe to iterate without further
// synchronization (the underlying array is not mutated after the
// lock is released; ranging over a nil slice is a no-op).
//
// Used by the adapter's OutReply overflow handler (fix-reply-placehold-card)
// to build the body for the rollover placeholder card without
// racing against a concurrent SetTaskList from the bridge event
// pump.
//
// Nil-safe: returns nil when called on a nil receiver, matching
// the guard pattern used by AppendEntryWithFooter / RolloverTo.
func (r *MessageReceipt) Tasks() []agent.AgentTaskItem {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tasks
}

// SetPromptState (F-53 follow-up) transitions the receipt's
// `promptState` to `state` and, when transitioning for the first
// time to that state, adds the corresponding reaction on the
// receipt's own card (NOT the user message — see
// `mapStateToFeishuEmoji` for that surface).
//
// Reaction mapping (driven by `mapPromptStateToFeishuEmoji`):
//
//	chatsession.PromptRunning → OnIt  (🔄)  — added the first time the
//	                              receipt renders (cold-start card)
//	chatsession.PromptDone    → DONE  (✅)  — added when ChatSession.endPrompt
//	                              fires (EventAgentDone / EventAgentError)
//
// Idempotent: calling SetPromptState with the state already set
// is a no-op (no duplicate reaction, no extra API call).
//
// Called from two sites:
//
//  1. `renderLocked` after the first successful
//     `SendCardForReceipt` (chatsession.PromptRunning) — adds 🔄 once.
//  2. The adapter's prompt-end wiring (chatsession.PromptDone) when
//     ChatSession.endPrompt fires — adds ✅ once.
//
// If `cardMsgID` is empty (receipt not yet rendered, or first
// render failed), the reaction is silently skipped — the
// `cardMsgID` set just before this call ensures the cold-start
// path always has a card to react on.
//
// Best-effort: AddReaction failures are logged at the adapter
// level (via the runtime's reaction handler) and do not propagate.
// The receipt's `promptState` is updated regardless — the FSM is
// the source of truth, the reaction is visual decoration.
func (r *MessageReceipt) SetPromptState(ctx context.Context, state chatsession.PromptState) {
	r.mu.Lock()
	prev := r.promptState
	cardMsgID := r.cardMsgID
	r.promptState = state
	r.mu.Unlock()

	if prev == state || cardMsgID == "" {
		return
	}

	emoji := mapPromptStateToFeishuEmoji(state)
	if emoji == "" {
		return
	}
	if _, err := r.bot.AddReaction(ctx, cardMsgID, emoji); err != nil {
		// Best-effort: log and move on. We don't want a transient
		// Feishu reaction-API failure to derail the receipt FSM.
		r.logger.Warn("feishu receipt: add prompt-state reaction failed",
			"err", err, "state", state, "emoji", emoji, "card_msg_id", cardMsgID)
	}
}

// AppendEntry appends a new rolling-log entry (typically an OutReply
// text chunk from the agent's stream-json) and re-renders the card.
//
// Pre-render overflow check: the would-be post-append card body is
// built first; if its element count exceeds receiptMaxElements (50)
// or its byte size exceeds resultCardEnvelopeBudget (28 KB), the
// method returns ErrReceiptOverflow WITHOUT issuing a PATCH. The
// caller is expected to catch this and send the entry as a fresh
// top-level Create (F-40 bail-out, F-44 follow-up styling) so the
// entry still reaches the user, just not folded into the card.
//
// Why bail out (not truncate / not FIFO-evict): truncation drops
// the tail of the user's reply mid-word; FIFO eviction silently
// hides old entries the user already saw. Bail-out gives the user
// every entry in order, just in a different visual surface for
// the overflow case (top-level bubble, always visible in main chat).
//
// The first entry of a receipt is installed by ensureReceiptForReply
// before this method is called; the receipt's entries slice is
// always non-empty when AppendEntry runs in production (callers
// that want a no-op on an empty receipt should short-circuit at
// the call site).
func (r *MessageReceipt) AppendEntry(ctx context.Context, entry LogEntry) error {
	return r.AppendEntryWithFooter(ctx, entry, nil)
}

// ApplyHeartbeat (F-63) updates the receipt's per-turn heartbeat
// snapshot and triggers a PATCH when the counter actually changed.
// Called from Adapter.Send's OutHeartbeat branch — the runtime
// handler emits one OutHeartbeat per countable outbound event
// (OutThinking / OutToolStart) BEFORE the policy chain (see
// F-63 §3.2), so even when /think off / /tools off drops the
// original event, this method still sees the increment and the
// receipt header reflects the agent's real activity.
//
// Idempotency: applying the same snapshot twice produces only one
// PATCH — the changed check is on ThinkCount/ToolCount, not on
// LastBeatAt (LastBeatAt is updated by the snapshot write but
// does not gate the render).
//
// Throttle: heartbeatMinInterval caps the PATCH rate. We
// compare against r.lastBodyPatch (the SAME field renderLocked
// updates on every successful PATCH — entry / task / heartbeat).
// This means any successful PATCH bumps the window, preventing
// double-PATCHes when entries and heartbeat both want to
// update within the same window. The 2s ceiling applies only
// when entries/tasks are silent; in a busy turn the entries
// PATCHes themselves naturally throttle heartbeats.
//
// Locking: holds r.mu through renderLocked (matching the
// existing AppendEntryWithFooter pattern). renderLocked's
// "Locked" suffix is the contract — it accesses r.entries,
// r.tasks, r.heartbeat, r.cardMsgID, r.lastBody, r.lastBodyPatch
// without acquiring the lock itself. The lock duration is
// bounded by Feishu PATCH latency (~50-200ms in practice);
// contention is between the same receipt's per-turn goroutines,
// never across chats. renderLocked has its own same-body
// short-circuit and 300ms minimum PATCH interval, so holding
// the lock while it runs is not a regression vs the prior
// code that unlocked first.
func (r *MessageReceipt) ApplyHeartbeat(ctx context.Context, snap messages.HeartbeatSnapshot) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	prev := r.heartbeat
	r.heartbeat = snap
	changed := snap.ThinkCount != prev.ThinkCount || snap.ToolCount != prev.ToolCount
	if !changed {
		return
	}
	if r.heartbeatMinInterval > 0 &&
		!r.lastBodyPatch.IsZero() &&
		time.Since(r.lastBodyPatch) < r.heartbeatMinInterval {
		return
	}
	if err := r.renderLocked(ctx); err != nil {
		r.logger.Warn("feishu receipt: heartbeat render failed",
			"err", err, "card_msg_id", r.cardMsgID)
	}
	// renderLocked already updates r.lastBodyPatch on success —
	// the throttle window advances automatically.
}

// AppendEntryWithFooter (F-45) appends a rolling-log entry AND
// stamps the rendered StatusBar footer for this turn.
// footerLines may be nil / empty (no StatusBar stamped this
// turn); the receipt keeps the previously-stored footer in that
// case so the rendered card's footer doesn't visually regress
// between turns.
func (r *MessageReceipt) AppendEntryWithFooter(ctx context.Context, entry LogEntry, footerLines []string) error {
	if r == nil {
		return nil
	}
	if entry.Text == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// Build the would-be card body first so we can check the
	// element / envelope budget BEFORE issuing any PATCH. The
	// receipt's own lastBody is unchanged in the overflow path,
	// so a subsequent successful append is a no-op (buildReceiptCard
	// returns the same body, renderLocked skips the PATCH).
	//
	// buildReceiptCard takes (entries, tasks, footerLines) directly
	// — no struct copy — so we don't bypass the r.mu lock
	// semantics. The proposed slice is a fresh slice that we only
	// commit to r.entries AFTER the overflow check passes.
	proposed := append(r.entries, entry)
	if len(footerLines) > 0 {
		r.footerLines = footerLines
	}
	body, elementCount, err := buildReceiptCard(proposed, r.tasks, r.footerLines, &r.heartbeat)
	if err != nil {
		return fmt.Errorf("feishu receipt: build card for overflow check: %w", err)
	}
	// receiptBodyStats takes the elementCount from buildReceiptCard
	// (single source of truth — see receiptBodyStats doc) and
	// just measures the body bytes. Off-by-one drift between
	// this overflow check and the actual PATCH body is
	// structurally impossible now.
	elementCount, bodyBytes := receiptBodyStats(proposed, r.tasks, r.footerLines, body, elementCount)
	if wouldReceiptOverflow(elementCount, bodyBytes) {
		return ErrReceiptOverflow
	}
	// Commit the entry + render.
	r.entries = proposed
	return r.renderLocked(ctx)
}

// RolloverTo (fix-reply-placehold-card) transitions the receipt to
// a freshly-created overflow placeholder card when the original
// receipt card exceeded its 50-element / 30 KB envelope. After
// RolloverTo returns, every subsequent AppendEntry / SetTaskList
// PATCHes the new card instead of the original — so a stream of N
// overflow chunks collapses into a single new placeholder card
// followed by N-1 PATCHes, instead of N standalone bubbles in main
// chat.
//
// Why a method, not just field assignment: cardMsgID, replyMsgID,
// entries, footerLines, lastBody, and lastBodyPatch all need to
// move together. Doing it under r.mu keeps a concurrent
// renderLocked / SetPromptState from observing a half-migrated
// state. lastBody + lastBodyPatch also need to be reset so the
// first PATCH onto the new card isn't suppressed by the
// duplicate-body short-circuit or delayed by the 300ms rate
// limiter measured against the OLD card's last PATCH.
//
// tasks are intentionally preserved across the rollover — the
// checklist is a global view across the whole turn; freezing it on
// the old card and restarting on the new one would visually orphan
// in-flight tasks.
//
// Caller contract:
//
//  1. SendCardForReceipt(chatID, body, "", false) must already
//     have succeeded and returned msgID. RolloverTo does NOT send
//     any network traffic itself; the caller owns the send.
//  2. firstEntry should match the entry whose overflow triggered
//     the rollover, so the new card starts with the same visible
//     content the old postOrphanReplyCard fallback would have
//     rendered.
//
// The original card remains visible in chat but the receipt no
// longer tracks it — future AppendEntry calls go to msgID instead.
// OnPromptEnded's SetPromptState therefore lands on the LAST
// overflow card, which is the surface the user is reading.
func (r *MessageReceipt) RolloverTo(msgID string, firstEntry LogEntry, footerLines []string) {
	if r == nil || msgID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cardMsgID = msgID
	r.replyMsgID = msgID
	r.entries = []LogEntry{firstEntry}
	r.footerLines = footerLines
	// Reset render-state cache so the first PATCH on the new card
	// is not skipped by the duplicate-body short-circuit
	// (`body == r.lastBody` in renderLocked) and is not delayed by
	// the 300ms rate limiter measured against the old card's last
	// PATCH time.
	r.lastBody = ""
	r.lastBodyPatch = time.Time{}
	// tasks intentionally preserved — see method doc.
}

// renderLocked pushes the current rolling-log + task-checklist
// snapshot to Feishu. Caller must hold r.mu.
// Caller must hold r.mu.
//
// F-44 simplified semantics:
//   - If r.cardMsgID == "" → first render. Call SendCard to create the
//     interactive card; capture the message id.
//   - Else → call PatchMessage to replace the body of the existing card
//     in place. The whole card is replaced; the server doesn't accept
//     diffs.
//
// The card body is built by buildReceiptCard (in adapter.go) which now
// renders ONLY the task-checklist section. Failure modes:
//   - SendCard / PatchMessage failure: logged + returned to the caller.
//     The card surface may be stale until the next render; we do NOT
//     auto-create a new card on PATCH failure (avoids duplicate
//     surfaces).
func (r *MessageReceipt) renderLocked(ctx context.Context) error {
	// F-42 review finding #4/#5: while ensureReceiptForTask is
	// mid-SendCard, this placeholder is visible to concurrent
	// goroutines but cardMsgID is still empty. If we proceeded to the
	// `cardMsgID == ""` SendCard branch, a concurrent SetTaskList would
	// create a SECOND card in chat (orphan). The ensure helper's own
	// SendCard is the only legitimate card; any render triggered from
	// elsewhere during this window is dropped — the data it added stays
	// in the buffer, and the next renderLocked call AFTER the ensure
	// helper finishes will pick it up and PATCH it into the canonical
	// card. Data is preserved; visual churn is avoided.
	if r.initializing {
		return nil
	}

	// Build the card body. buildReceiptCard is in adapter.go.
	body, _, err := buildReceiptCard(r.entries, r.tasks, r.footerLines, &r.heartbeat)
	if err != nil {
		r.logger.Warn("feishu receipt: build card failed",
			"err", err, "state", r.promptState)
		return fmt.Errorf("feishu receipt: build card: %w", err)
	}

	if r.cardMsgID == "" {
		// First send: create the card. Capture the message id for
		// subsequent PATCH calls.
		//
		// v1.3.x (§13.10): pass userMsgID as rootID so the cold-start
		// card is rendered as a reply to the user's message. Once the
		// card exists, PatchMessage preserves the thread across
		// subsequent in-place edits.
		//
		// F-37: replyInThread=false — the cold-start card IS the
		// pinned main-chat answer; thread-only would leave the main
		// chat empty until the receipt PATCHes happen.
		msgID, sendErr := r.bot.SendCardForReceipt(ctx, r.chatID, body, r.userMsgID, false)
		if sendErr != nil {
			r.logger.Warn("feishu receipt: create card failed",
				"err", sendErr, "state", r.promptState)
			return fmt.Errorf("feishu receipt: create card: %w", sendErr)
		}
		r.cardMsgID = msgID
		r.replyMsgID = msgID // keep alias in sync
		r.lastBody = body
		r.lastBodyPatch = time.Now()
		// F-53 follow-up: add the 🔄 "Running" reaction on the
		// newly-created card. Idempotent — SetPromptState skips
		// when state is already chatsession.PromptRunning.
		r.SetPromptState(ctx, chatsession.PromptRunning)
		return nil
	}

	// Skip PATCHes when the body is identical to the previous render
	// (common when rapid events don't actually change the card) and
	// pace the real PATCHes below Feishu's message-update rate limit.
	if body == r.lastBody {
		return nil
	}
	const minPatchInterval = 300 * time.Millisecond
	if wait := minPatchInterval - time.Since(r.lastBodyPatch); wait > 0 {
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}

	// Subsequent: PATCH the existing card in place. The whole body
	// is replaced; the server doesn't accept diffs.
	if patchErr := r.bot.PatchMessage(ctx, r.cardMsgID, body); patchErr != nil {
		r.logger.Warn("feishu receipt: patch card failed",
			"err", patchErr, "state", r.promptState, "card_msg_id", r.cardMsgID)
		// F-46 follow-up: was wrapping the outer `err` (already
		// returned from buildReceiptCard above), producing a
		// "%!w(<nil>)" message and making errors.Is(err,
		// ErrReceiptOverflow) return false in callers. Wrap
		// patchErr so the upstream AppendEntryWithFooter caller
		// can correctly route the overflow bail-out.
		return fmt.Errorf("feishu receipt: patch card: %w", patchErr)
	}
	r.lastBody = body
	r.lastBodyPatch = time.Now()
	return nil
}
// receiptBodyStats returns the byte size of a card body and the
// authoritative element count that buildReceiptCard produced
// for it. Used by AppendEntryWithFooter's pre-render overflow
// check.
//
// Element count is sourced from buildReceiptCard's own return
// value — the previous (pre-F-63.1) duplication between this
// function and buildReceiptCard was the source of an off-by-one
// bug where the F-63 heartbeat header wasn't counted here. By
// passing buildReceiptCard's count through, the two stay
// locked and any future section added to the card builder
// automatically participates in the overflow guard.
//
// Kept as a function (rather than inlining at the call site)
// so the overflow check reads as one operation: "size + count
// for THIS would-be body". The (entries, tasks, footerLines)
// arguments are unused but kept on the signature for symmetry
// with the original API — call sites that already have these
// in scope don't need to refactor.
//
// Note: a future change could swap this for `json.Unmarshal` +
// `len(parsed.Body.Elements)` to remove the need to thread
// elementCount through buildReceiptCard. The trade-off is the
// parse cost (~5µs per AppendEntry call) — not worth it until
// the AppendEntry hot path becomes a bottleneck.
func receiptBodyStats(entries []LogEntry, tasks []agent.AgentTaskItem, footerLines []string, body string, elementCount int) (int, int) {
	_ = entries
	_ = tasks
	_ = footerLines
	return elementCount, len(body)
}
