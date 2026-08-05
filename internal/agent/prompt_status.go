package agent

// PromptStatus is the lifecycle stage of one inbound prompt that
// ChatSession has dispatched to an AgentSession. It answers
// "where is this prompt in the agent run?" so channels can render
// the appropriate card-header / thread-marker / DOM state.
//
// Owner: each channel's receipt object (per userMsgID). Trigger:
// receipt.Append on agent.EventDone / EventError. See SPEC §2.5
// for the split between MessageState (delivery) and PromptStatus
// (execution).
//
// Scope: only produced for plain user messages, NOT slash
// commands. Slash commands have their own OutCommandReply path.
//
// IMPORTANT: PromptStatus is a SHARED VOCABULARY, not a wire event.
// Unlike MessageState (which ChatSession broadcasts via
// OnMessageState → OutboundMessage{Kind: OutMessageState}),
// PromptStatus transitions are channel-internal: each channel's
// receipt observes agent.Event* directly and updates its own
// PromptStatus. Channels MUST NOT introduce a new
// OutboundMessage{Kind: OutPromptStatus} wire event — that would
// duplicate agent.EventDone/EventError.
//
// History: this type was previously feishu.ReceiptState
// (v1.3.x F-42 era). The receipt FSM was strictly feishu-private
// because it controlled only the card body. v1.3.x lifts it to
// agent so future channels (Slack / Web / ...) can adopt the
// same vocabulary without re-inventing it. The 4 state values
// match feishu's prior semantics verbatim.
type PromptStatus int

const (
	// PromptPending: the prompt has been queued for an
	// AgentSession but no agent event has arrived yet. Triggered
	// on receipt construction (cold-start) or on first
	// ensureReceiptFor* call.
	PromptPending PromptStatus = iota

	// PromptRunning: the AgentSession is processing the prompt
	// (first agent event arrived on receipt.Append). Triggered
	// on first non-empty entry append.
	PromptRunning

	// PromptSucceeded: the AgentSession emitted EventDone for
	// this prompt. Terminal.
	PromptSucceeded

	// PromptFailed: the AgentSession emitted EventError for this
	// prompt. Terminal.
	PromptFailed
)

// String renders PromptStatus as a short human label, primarily
// for log lines and test diagnostics.
func (s PromptStatus) String() string {
	switch s {
	case PromptPending:
		return "pending"
	case PromptRunning:
		return "running"
	case PromptSucceeded:
		return "succeeded"
	case PromptFailed:
		return "failed"
	}
	return "unknown"
}