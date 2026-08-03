package agent

// MessageState is the lifecycle stage of one inbound user message
// within nightme's processing pipeline. It answers "where is this
// message in the system right now?" so channels can render a
// user-visible progress indicator.
//
// Owner: ChatSession (per-userMsg). Trigger: ChatSession
// lifecycle events (received / forwarded / done / error). See
// SPEC §2.5 for full semantics and the rationale for keeping
// MessageState independent from any Channel's receipt object.
//
// Scope: only produced for plain user messages, NOT slash
// commands. See docs/feat/F-31-message-state.md.
//
// History: this type used to live in its own package
// `internal/receipt/` alongside a now-removed Receipt FSM and
// ReceiptState enum (v1.2 Gateway-owned design). v1.3 collapses
// the package to just this one enum; we moved it into `agent`
// (the package every other layer already imports) so there is
// no new dependency edge and no import cycle.
type MessageState int

const (
	// StateReceived: ChatSession has accepted the message but
	// not yet dispatched it to an AgentSession. Triggered on
	// ChatSession.GetOrCreate.
	StateReceived MessageState = iota

	// StateForwarded: the message has been dispatched to an
	// AgentSession (lazy spawn succeeded; blocks enqueued or
	// sent to PTY stdin). Triggered on successful
	// LookupActiveAgentSession.
	StateForwarded

	// StateDone: the AgentSession has finished processing this
	// message (EventDone arrived on the readPump). Triggered by
	// ChatSession.runReadPump on EventDone.
	StateDone

	// StateError: the AgentSession reported an error for this
	// message (EventError arrived). Triggered by
	// ChatSession.runReadPump on EventError.
	StateError
)

// String renders MessageState as a short human label, primarily
// for log lines and test diagnostics.
func (s MessageState) String() string {
	switch s {
	case StateReceived:
		return "received"
	case StateForwarded:
		return "forwarded"
	case StateDone:
		return "done"
	case StateError:
		return "error"
	}
	return "unknown"
}