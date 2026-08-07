package agent

// MessageState is the wire + lifecycle stage of one inbound user
// message within nightme's processing pipeline. F-53 shrinks the
// v1.3 4-value enum to 3 values:
//
//   - MessageQueued:    was MessageReceived
//   - MessageSubmitted: was MessageForwarded
//   - MessageDropped:   new (Phase 0; explicit clear only)
//
// The previous MessageDone / MessageFailed values (which conflated
// execution result with delivery state) are physically deleted —
// terminal execution result is now carried by `chatsession.Prompt.
// EndReason`. See docs/feat/message_lifecycle.md §3 原则 1 / §6.3.
//
// MessageState is the abstract-layer vocabulary; Channels consume
// it via the existing wire (Gateway.OnMessageState →
// `OutboundMessage{Kind: OutMessageState, MessageState: ...}`) and
// render via their own platform primitives (Feishu reaction emoji,
// Slack shortcode, Web DOM diff, etc.). Channel authors are NOT
// required to render every value — Channels choose what subset to
// surface.
type MessageState int

const (
	// MessageQueued: ChatSession has accepted the message and
	// queued it for submission. Triggered on `QueueUserMessage`
	// entry (currently on the runtime dispatcher in
	// cmd/nightme/run.go newMessageDispatcher, BEFORE the
	// AS-spawn attempt — spawn failure is reported via OutReply,
	// not as a MessageState transition).
	MessageQueued MessageState = iota

	// MessageSubmitted: SendBlocks returned nil; the message
	// (and any batched siblings) have been formally delivered to
	// the AgentSession. Triggered inside
	// ChatSession.defaultPromptHookLocked after successful
	// submission. Terminal from a delivery-pipeline perspective
	// — `Message.Stage` does NOT change when the corresponding
	// Prompt ends (no fan-out; see docs/feat/message_lifecycle.md
	// §5.1).
	MessageSubmitted

	// MessageDropped: the message was explicitly cleared before
	// it could be submitted. Triggered by `ChatSession.
	// MarkDropped`, which is called from:
	//   - BufferClear (which `/kill` and `/new` invoke to drop
	//     the queued batch)
	//
	// NOT triggered by SendBlocks failure — a failed send leaves
	// the message in `MessageQueued` and the next
	// `flushPending` retries it.
	MessageDropped
)

// String renders MessageState as a short human label, primarily
// for log lines and test diagnostics.
func (s MessageState) String() string {
	switch s {
	case MessageQueued:
		return "queued"
	case MessageSubmitted:
		return "submitted"
	case MessageDropped:
		return "dropped"
	}
	return "unknown"
}