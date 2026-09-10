package agent

// PromptEndReason is the WHY of a Prompt terminating. Universal
// vocabulary consumed by every rendering layer (Feishu / Telegram /
// Slack / future Web) to paint the terminal marker — ❌ for any
// non-clean reason, ✅ only when the agent emitted EventAgentDone
// normally. Lives in `agent` (alongside MessageState, ContentBlock,
// BridgeDiagnostic) so the leaf `channel.Channel` interface can
// reference it without importing `agentsession`. `agentsession`
// already talk in `agent.PromptEndReason`.
//
// Independent of the execution stage (`Prompt` itself has no State
// field — see docs/feat/message_lifecycle.md §4.2).
type PromptEndReason int

const (
	// PromptEndClean: agent emitted EventAgentDone normally.
	PromptEndClean PromptEndReason = iota

	// PromptEndError: agent emitted EventAgentError (unrecoverable
	// per-event error reported by the bridge).
	PromptEndError

	// PromptEndProcessDied: AS process exited without producing
	// EventAgentDone/EventAgentError. PHASE 0 DOES NOT TRIGGER THIS —
	// the readPump's `!ok` branch currently returns without calling
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

	// PromptEndUserStopped: endPrompt called by /stop slash
	// command. The bridge process MAY continue running (per-
	// bridge Stop semantics — see internal/command/stop), but
	// the in-flight Prompt is over from the user's POV and
	// IsReady must flip true synchronously so the next TryFlush
	// can land, without waiting for the bridge protocol to emit
	// a terminal event.
	PromptEndUserStopped
)

// IsError reports whether the reason is a non-clean termination —
// any reason other than PromptEndClean. Channels / renderers use
// this to pick the ❌ terminal marker instead of ✅. PromptEndUser*
// reasons are user-initiated and still return true here (they
// surface as ❌ so the user can distinguish a hand-cancelled turn
// from a clean completion); keep the boolean symmetric with the
// "did this turn succeed without user intervention" question, not
// the "is anything wrong" question — those are different and the
// former is the one channels need.
func (r PromptEndReason) IsError() bool {
	return r != PromptEndClean
}

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
	case PromptEndUserStopped:
		return "user-stopped"
	}
	return "unknown"
}
