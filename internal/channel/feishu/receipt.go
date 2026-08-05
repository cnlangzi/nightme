// Package feishu — F-44 simplified MessageReceipt.
//
// Post-F-44 the receipt card is task-only:
//
//	📋 Tasks
//	  - [ ] Subject
//	  - [x] Subject
//	  - [ ] Subject (ActiveForm)
//
// F-25 → F-42 had OutReply / OutResult / OutInit / OutUsage / OutCompaction
// fold into the same card (rolling-log + 元数据). F-44 reverses that:
// OutReply / OutResult each go to their own ReplyInThreadAndChat message;
// OutInit / OutUsage are silently dropped until the footer PR; the
// remaining card surface is the F-38 task checklist.
//
// The receipt state machine shrinks to one role: hold the latest task
// snapshot and PATCH it in place when SetTaskList fires. renderLocked fires
// only on SetTaskList (no more Append / SetExecuting / SetCompleted from
// the receipt side — terminal signals travel via OutMessageState →
// AddReaction on the user message).
package feishu

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
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
	// SendCard posts a new interactive card and returns its message ID.
	// Used on the FIRST render of a receipt (no cardMsgID yet).
	// v1.3.x (§13.10): rootID is the user message id to thread
	// the cold-start card to.
	SendCard(ctx context.Context, chatID, cardJSON, rootID string, replyInThread bool) (string, error)
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

// MessageReceipt is the per-user-message task-checklist display (F-38 +
// F-44). One receipt owns ONE Feishu card message (the cardMsgID) which
// carries only the **📋 Tasks** markdown section. OutReply / OutResult
// each have their own ReplyInThreadAndChat messages, anchored to the same
// userMsgID for visual coherence.
//
// F-44 fields:
//   - promptState: reserved for footer PR (see §6.2 in F-44 doc). Set on
//     first SetTaskList (PromptPending → PromptRunning). Not transitioned
//     further (terminal signals travel via OutMessageState).
//   - tasks: latest snapshot, replaced wholesale on each SetTaskList.
//   - cardMsgID / replyMsgID / initializing / lastBody / lastBodyPatch:
//     SendCard → PatchMessage bookkeeping (unchanged from F-42).
//   - bot / logger / mu / chatID / userMsgID: infrastructure.
//
// F-44 deleted fields (no longer needed; their writers all gone):
//   - entries []LogEntry: OutReply / OutError no longer fold
//   - agentName / workspace / branch / inputTokens / outputTokens:
//     OutInit / OutUsage silent drop until footer PR
//   - evicted: FIFO eviction was for OutReply entries
type MessageReceipt struct {
	chatID     string
	userMsgID  string
	replyMsgID string
	bot        receiptBot
	logger     *slog.Logger

	mu          sync.Mutex
	promptState agent.PromptState

	// tasks is the latest Claude task snapshot (F-38) for this turn.
	// The bridge always sends the full snapshot, so we copy it verbatim
	// on every event. The slice is rendered as a single markdown
	// element list (split by divTextCharLimit) — the only thing in the
	// card body post-F-44.
	tasks []agent.TaskItem

	// cardMsgID is the Feishu message id of the task-checklist card
	// once it has been created. Empty before the first render. After
	// the first SendCard it is set; subsequent renders PatchMessage
	// against this id rather than posting new messages.
	cardMsgID string

	// initializing is true between the moment ensureReceiptForTask
	// registers this receipt in receiptsByUserMsgID and the moment its
	// first SendCard returns with a real cardMsgID. During that brief
	// window the placeholder is visible to other goroutines
	// (receiptFor / ensureReceiptForTask return it), but its cardMsgID
	// is still empty — so a renderLocked driven by a concurrent
	// SetTaskList would otherwise issue a second SendCard (orphan).
	// renderLocked checks this flag and short-circuits while true,
	// dropping the render so the ensure helper's own SendCard is the
	// only card in chat. The flag is cleared under r.mu immediately
	// after the SendCard return value is stored. F-42 review
	// finding #4/#5.
	initializing bool

	// Feishu limits message updates to roughly five per second.
	// Skip duplicate bodies and pace real PATCH requests.
	lastBody      string
	lastBodyPatch time.Time
}

// NewMessageReceiptForReply wraps an already-posted reply message (the
// caller is responsible for sending the initial text). The returned
// receipt is registered in the adapter's indexes so subsequent SetTaskList
// calls update the same card. Use this when the adapter owns the full
// lifecycle.
//
// bot is the adapter (or any receiptBot implementation) used by
// renderLocked to ship card bodies (SendCard on first render,
// PatchMessage on subsequent). F-44 callers pass `a`; passing nil will
// panic on the first SetTaskList.
//
// F-44: this constructor now serves ONLY the task-checklist path
// (ensureReceiptForTask). The ensureReceiptForReply / OutReply fold path
// was deleted.
func NewMessageReceiptForReply(chatID, userMsgID, replyMsgID string, bot receiptBot) *MessageReceipt {
	return &MessageReceipt{
		chatID:       chatID,
		userMsgID:    userMsgID,
		replyMsgID:   replyMsgID,
		cardMsgID:    replyMsgID,
		bot:          bot,
		logger:       slog.Default(),
		promptState: agent.PromptPending,
	}
}

// SetTaskList replaces the per-turn task checklist (F-38) with a fresh
// snapshot from the bridge. The slice is copied so subsequent caller
// mutations to the underlying array cannot affect the receipt. Empty
// lists (len(Items)==0) are accepted and clear the checklist.
//
// F-44: SetTaskList is now the ONLY production writer to a receipt. It
// also promotes the receipt from PromptPending to PromptRunning on
// first call (so future footer PR can recover the prompt state header
// without extra plumbing). Late SetTaskList after a successful PATCH
// still PATCHes the card with the new snapshot.
func (r *MessageReceipt) SetTaskList(ctx context.Context, list *agent.TaskListEvent) error {
	if r == nil {
		return nil
	}
	if list == nil {
		return errors.New("feishu receipt: SetTaskList called with nil TaskListEvent")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	items := list.Items
	if len(items) == 0 {
		r.tasks = nil
	} else {
		copyItems := make([]agent.TaskItem, len(items))
		copy(copyItems, items)
		r.tasks = copyItems
	}
	if r.promptState == agent.PromptPending {
		r.promptState = agent.PromptRunning
	}
	return r.renderLocked(ctx)
}

// PromptState returns the current prompt execution state. Useful for
// tests + diagnostics. Returns the agent.PromptState value stored on
// this receipt; callers should compare against agent.PromptPending /
// PromptRunning / PromptSucceeded / PromptFailed.
//
// F-44: SetTaskList promotes Pending → Running on first call. The
// receipt does not transition to Succeeded / Failed (terminal signals
// travel via OutMessageState → AddReaction, not via receipt state).
func (r *MessageReceipt) PromptState() agent.PromptState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.promptState
}

// State returns the current state. Useful for tests + diagnostics.
//
// Deprecated: this getter preserves the v1.3.x F-42 era signature for
// backward compatibility in test fixtures. New code should call
// PromptState() (which returns the agent.PromptState enum directly).
// Existing call sites will be migrated in a follow-up.
func (r *MessageReceipt) State() agent.PromptState {
	return r.PromptState()
}

// renderLocked pushes the current task-checklist snapshot to Feishu.
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
	body, err := buildReceiptCard(r)
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
		msgID, sendErr := r.bot.SendCard(ctx, r.chatID, body, r.userMsgID, false)
		if sendErr != nil {
			r.logger.Warn("feishu receipt: create card failed",
				"err", sendErr, "state", r.promptState)
			return fmt.Errorf("feishu receipt: create card: %w", sendErr)
		}
		r.cardMsgID = msgID
		r.replyMsgID = msgID // keep alias in sync
		r.lastBody = body
		r.lastBodyPatch = time.Now()
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
		return fmt.Errorf("feishu receipt: patch card: %w", err)
	}
	r.lastBody = body
	r.lastBodyPatch = time.Now()
	return nil
}