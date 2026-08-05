package agent

// MessageState is the lifecycle stage of one inbound user message
// within nightme's delivery pipeline. It answers "where is this
// message in the system right now?" so channels can render a
// user-visible reaction emoji on the user message itself
// (⏳ received → 🔄 forwarded → ✅ done / ❌ failed).
//
// Owner: ChatSession (per-userMsg). Trigger: ChatSession
// lifecycle events (received / forwarded / done / failed). See
// SPEC §2.5 for full semantics and the rationale for keeping
// MessageState independent from any Channel's receipt object.
//
// IMPORTANT: MessageState describes the DELIVERY pipeline (did the
// message reach ChatSession? did it reach AgentSession? did the
// agent turn terminate?), NOT the prompt execution lifecycle.
// The execution view is PromptState (see prompt_state.go) —
// each channel owns its own PromptState on its receipt object,
// updated by receipt.Append on agent.EventDone / EventError.
// The two FSMs answer different questions and are kept
// independent on purpose.
//
// Scope: only produced for plain user messages, NOT slash
// commands. See docs/feat/F-31-message-state.md.
//
// Channel rendering: every Channel implements its own
// MessageState → native-rendering mapping (Feishu AddReaction,
// Slack emoji shortcode, Web DOM state). The abstract enum is
// the contract; the concrete rendering is the Channel's
// responsibility (SPEC §2.4 / §2.5).
//
// History: this type used to live in its own package
// `internal/receipt/` alongside a now-removed Receipt FSM and
// ReceiptState enum (v1.2 Gateway-owned design). v1.3 collapsed
// the package to just this one enum; we moved it into `agent`
// (the package every other layer already imports) so there is
// no new dependency edge and no import cycle. The v1.3.x rename
// drops the `State` prefix in favour of `Message` to make the
// `agent.MessageReceived` form read as a complete phrase and to
// distinguish from PromptState (which lives in the same
// package).
type MessageState int

const (
	// MessageReceived: ChatSession has accepted the message but
	// not yet dispatched it to an AgentSession. Triggered on
	// ChatSession.GetOrCreate.
	MessageReceived MessageState = iota

	// MessageForwarded: the message has been dispatched to an
	// AgentSession (lazy spawn succeeded; blocks enqueued or
	// sent to PTY stdin). Triggered on successful
	// LookupActiveAgentSession.
	MessageForwarded

	// MessageDone: the AgentSession has finished processing this
	// message (EventDone arrived on the readPump). Terminal.
	// Triggered by ChatSession.runReadPump on EventDone.
	MessageDone

	// MessageFailed: the AgentSession reported an error for this
	// message (EventError arrived). Terminal. Triggered by
	// ChatSession.runReadPump on EventError.
	MessageFailed
)

// String renders MessageState as a short human label, primarily
// for log lines and test diagnostics.
func (s MessageState) String() string {
	switch s {
	case MessageReceived:
		return "received"
	case MessageForwarded:
		return "forwarded"
	case MessageDone:
		return "done"
	case MessageFailed:
		return "failed"
	}
	return "unknown"
}