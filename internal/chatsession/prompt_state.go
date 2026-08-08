// Package chatsession — PromptState (F-53, moved from feishu in F-07).
//
// `PromptState` is the abstract lifecycle state of a `Prompt` —
// independent of any single channel's visual rendering. Each
// channel renders the (state transition) however it wants: Feishu
// adds a reaction on the receipt card, Slack might add a status
// emoji, Web UI might re-color a card header. The state itself
// is universal — it lives in this package alongside `Prompt`.
//
// Stage transitions (Phase 0):
//
//	PromptRunning  ─happens when→  PromptDone
//	  (set at construction;   (set by ChatSession.endPrompt on
//	   endPrompt is no-op)    EventAgentDone / EventAgentError)
//
// The phase-0 invariant is that `PromptRunning` is the
// "interesting" state — there's no `PromptPending` (the v1.3
// pre-creation state) and `PromptDone` is the only terminal.
//
// Why home is here, not in `feishu` (F-07 reversal):
//
//	F-53 originally moved `agent.PromptState` (4-value) into
//	`feishu` as a 2-value private enum on the reasoning that
//	it was a channel-rendering detail. But `PromptState` is
//	actually a property of the `Prompt` object — the channel
//	only consumes it for rendering. Putting it in `chatsession`
//	keeps the type next to `Prompt` so future channels (Slack,
//	Web, ...) can adopt the same vocabulary without a duplicate
//	type, and so the abstract state lives next to the abstract
//	object it describes.
package chatsession

// PromptState is the (currently) 2-value lifecycle enum of a
// `Prompt` — see type doc above.
type PromptState int

const (
	// PromptRunning: the `Prompt` has been created and is
	// currently active. Set at construction (the hook that
	// commits the Prompt also sets `Prompt.AckedAt` and installs
	// it on `AgentSession.currentPrompt`).
	PromptRunning PromptState = iota

	// PromptDone: the `Prompt` has finished — either cleanly
	// (EventAgentDone) or with an error (EventAgentError). Wire-up is
	// via `ChatSession.endPrompt(reason)`; the runtime
	// translates that to a per-channel render via the
	// `PromptEndBus` callback.
	//
	// Although the value is reserved, Phase 0 only emits
	// PromptRunning → (no transition) for the happy path;
	// the readpump calls `endPrompt` on EventAgentDone/Error and
	// that mutates the receipt's promptState to PromptDone.
	PromptDone
)

// String renders a PromptState for logs / diagnostics.
func (s PromptState) String() string {
	switch s {
	case PromptRunning:
		return "running"
	case PromptDone:
		return "done"
	}
	return "unknown"
}