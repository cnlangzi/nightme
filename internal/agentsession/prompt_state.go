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
package agentsession

// PromptState is the (currently) 2-value lifecycle enum of a
// `Prompt` — see type doc above.
type PromptState int

const (
	// PromptRunning: the `Prompt` has been created and is
	// currently active. Set at construction (the hook that
	// commits the Prompt also sets `Prompt.AckedAt` and installs
	// it on `AgentSession.currentPrompt`).
	PromptRunning PromptState = iota

	// PromptDone: the `Prompt` has finished — cleanly (EventAgentDone
	// on the readpump). Wire-up is via `ChatSession.endPrompt(reason)`;
	// the runtime translates that to a per-channel render via the
	// `PromptEndBus` callback.
	//
	// Phase 0 only emits PromptRunning → (PromptDone or PromptError)
	// from the readpump's EventAgentDone / EventAgentError branches.
	PromptDone

	// PromptError: the `Prompt` has finished with a non-clean reason
	// (EventAgentError / PromptEndProcessDied / PromptEndStalledKilled /
	// PromptEndUserKilled / PromptEndUserStopped). Phase 0 emits this
	// from the same readpump sites as PromptDone; channels distinguish
	// it to paint the cross (❌) reaction instead of the check (✅).
	// See `internal/agent/prompt_end_reason.go` for the full reason
	// vocabulary this verdict collapses.
	PromptError
)

// String renders a PromptState for logs / diagnostics.
func (s PromptState) String() string {
	switch s {
	case PromptRunning:
		return "running"
	case PromptDone:
		return "done"
	case PromptError:
		return "error"
	}
	return "unknown"
}
