// Package chatsession — Prompt (F-53).
//
// `Prompt` is the per-submission domain object: one batch of
// `[]*Message` (1..N) merged and forwarded to an AgentSession via
// a single `SendBlocks` call. It replaces the old ad-hoc scalar
// `currentTurnUserMsgID` and ephemeral flush tuples with a
// first-class entity.
//
// Lifecycle:
//   - Construction: `flushPending` builds a candidate Prompt
//     (MessageIDs, Blocks, ChatSessionID, AgentSessionID,
//     LastMessageID) BEFORE calling the hook. The hook (default
//     `defaultPromptHookLocked`) is responsible for either:
//       * `SendBlocks` returns nil → commit Prompt (assign ID, set
//         CreatedAt/AckedAt, install on `AgentSession.currentPrompt`,
//         flip all messages' Stage to Submitted, wire emit).
//       * `SendBlocks` returns error → discard Prompt, messages
//         stay Queued for the next flush.
//   - Active: `AgentSession.currentPrompt` is the in-flight Prompt
//     for this AgentSession. `runReadPump` reads
//     `as.currentPrompt.LastMessageID` for the EventHandler anchor.
//   - End: `endPrompt(reason)` sets EndedAt + EndReason and clears
//     `AgentSession.currentPrompt`. Messages stay attached to
//     the Prompt via `Prompt.Messages` for the lifetime of the
//     prompt's readpump subscribers; once cleared, no consumer
//     holds a reference and they become eligible for GC.
//
// Storage: `*Prompt` lives on `AgentSession.currentPrompt`, NOT on
// ChatSession. The write is performed under `ChatSession.mu` (see
// `defaultPromptHookLocked`); read access from `runReadPump` is also
// under `ChatSession.mu`. This avoids introducing a second lock on
// AgentSession at the cost of a small coupling — accepted as the
// simpler path for Phase 0 (see docs/feat/message_lifecycle.md §4.2
// "存储归属").
package agentsession

import (
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// Prompt represents one merged submission to an AgentSession.
//
// One Prompt corresponds to exactly one SendBlocks call on the
// owning AgentSession. Multiple Messages can be merged into one
// Prompt (1..N), but a single Message never belongs to more than
// one Prompt (Message.PromptID is set at Submitted and is immutable).
type Prompt struct {
	// ID is the unique identifier for this Prompt, formatted as
	// `<AgentSessionID>-p<seq>` (e.g. `as_3-p7`). Sequence comes
	// from `AgentSession.promptCounter`, monotonic per AS.
	ID string

	// ChatSessionID is the owning ChatSession.
	ChatSessionID string

	// AgentSessionID is the submission target — snapshotted at
	// Prompt creation. Does not follow later agent switches (/use);
	// if the user /use's mid-Prompt, the old AS still holds this
	// Prompt as `currentPrompt` until its `endPrompt` fires
	// (ProcessDied in a future PR; currently left dangling —
	// see docs/feat/message_lifecycle.md §8 out-of-scope).
	AgentSessionID string

	// Messages is the ordered list of Message values merged into
	// this Prompt, in the same order they were pulled from
	// the queue. Populated at construction from the drained
	// queue. Empty slice is invalid (Prompt requires at least
	// one message).
	//
	// Carrying the full values — instead of just IDs — lets
	// the prompt-end path (ChatSession.writebackMessageState)
	// and the AS-side readpump reach per-message data without
	// needing a per-chat ID→Message index. The Messages are
	// immutable; no consumer of this Prompt mutates them.
	Messages []Message

	// LastMessageID is the ID of the last entry in Messages —
	// the anchor for the EventHandler (placeholder card mounts
	// to this message). `runReadPump` reads it from
	// `as.currentPrompt`. Multi-message turns anchor on the
	// LAST message id, matching the pre-Phase-1 behavior.
	LastMessageID string

	// Blocks is the merged ContentBlock slice that was actually
	// sent via SendBlocks. Preserved verbatim for diagnostics.
	Blocks []agent.ContentBlock

	// CreatedAt is when the candidate Prompt was first assembled
	// (before SendBlocks). Set in `flushPending`.
	CreatedAt time.Time

	// AckedAt is when `as.SendBlocks` returned nil — the
	// authoritative "submission succeeded" timestamp. Set in
	// `defaultPromptHookLocked` on success.
	AckedAt time.Time

	// LastProgressAt is the most recent observed-progress time
	// (touched by `runReadPump` on every AgentEvent). Reserved for
	// future stall-detection work (L2 in the original WIP design).
	// Phase 0 maintains the field but does not act on it.
	LastProgressAt time.Time

	// EndedAt is when `endPrompt` was called. Zero value while
	// still running.
	EndedAt time.Time

	// EndReason describes why the Prompt ended. Zero value
	// (`PromptEndClean` is iota=0, so technically non-zero too —
	// callers should test `EndedAt.IsZero()` for "still running").
	EndReason PromptEndReason
}

// PromptEndReason is the WHY of a Prompt ending. Independent of the
// execution state (`Prompt` itself has no State field — Phase 0
// derives "still running" from `EndedAt.IsZero()`, see
// docs/feat/message_lifecycle.md §4.2).
type PromptEndReason int

const (
	// PromptEndClean: agent emitted EventAgentDone normally.
	PromptEndClean PromptEndReason = iota

	// PromptEndError: agent emitted EventAgentError (unrecoverable
	// per-event error reported by the bridge).
	PromptEndError

	// PromptEndProcessDied: AS process exited without producing
	// EventAgentDone/EventAgentError. PHASE 0 DOES NOT TRIGGER THIS — the
	// readPump's `!ok` branch currently returns without calling
	// endPrompt. Reserved for the "Prompt 投递稳定性优化" PR
	// (see docs/feat/message_lifecycle.md §8).
	PromptEndProcessDied

	// PromptEndStalledKilled: endPrompt called by the stall
	// watchdog (L2). Phase 0 does not implement stall detection;
	// reserved.
	PromptEndStalledKilled

	// PromptEndUserKilled: endPrompt called by `/close` slash
	// command before process exit. Phase 0 does not distinguish
	// user-initiated kills from ProcessDied; reserved.
	PromptEndUserKilled
)

// String renders a PromptEndReason for logs / diagnostics.
func (r PromptEndReason) String() string {
	switch r {
	case PromptEndClean:
		return "clean"
	case PromptEndError:
		return "error"
	case PromptEndProcessDied:
		return "process-died"
	case PromptEndStalledKilled:
		return "stalled-killed"
	case PromptEndUserKilled:
		return "user-killed"
	}
	return "unknown"
}

// PromptHook is the flush-time callback installed on InputBuffer.
// It receives a fully-built candidate Prompt (MessageIDs, Blocks,
// IDs all populated) and is responsible for:
//
//  1. Calling `as.SendBlocks` (or any other transport-level send).
//  2. On nil error: commit Prompt — install on
//     `AgentSession.currentPrompt`, stamp `AckedAt`, flip all
//     referenced Messages' Stage to Submitted, emit
//     `MessageSubmitted` on the wire.
//  3. On non-nil error: leave the candidate Prompt unused; messages
//     stay Queued for the next `flushPending` to retry.
//
// The hook runs WITHOUT `InputBuffer.mu` held (InputBuffer releases
// its lock before invoking; see `InputBuffer.flushPending`). The
// hook itself is responsible for any locking it needs (typically
// `ChatSession.mu` for the message-stage / currentPrompt writes).
type PromptHook func(p *Prompt) error