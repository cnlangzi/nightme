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
// execution result with delivery state) are physically deleted in
// F-53 §6.3. MessageDone has since been re-introduced under a
// narrower contract (see "MessageDone is the F-53 §8 follow-up"
// below) as the explicit F-53 §8 follow-up for synchronous
// dispatch paths. MessageFailed remains absent — see "What
// MessageDone does NOT mean" in the MessageDone section.
//
// Terminal execution result is carried by `chatsession.Prompt.
// EndReason`. See docs/feat/F-53-message-prompt-lifecycle.md §3 原则 1
// / §6.3 for the original rationale.
//
// MessageState is the abstract-layer vocabulary; Channels consume
// it via the runtime's MessageStateBus subscriber (see
// cmd/nightme/run.go) which builds
// `OutboundMessage{Kind: OutMessageState, MessageState: ...}` and
// stamps F-48 StatusBar on MessageSubmitted. Channels render
// via their own platform primitives (Feishu reaction emoji, Slack
// shortcode, Web DOM diff, etc.). Channel authors are NOT required
// to render every value — Channels choose what subset to surface.
//
// Note (F-54): the legacy gateway.OnMessageState path is still
// present on the gateway interface for v1.3 test compatibility,
// but production wiring must NOT call it (would cause duplicate
// MessageState events per transition).
//
// MessageDone is the explicit F-53 §8 follow-up: it restores a
// completion indicator on the user message for synchronous
// dispatch paths (slash command, shell dispatch) that have no
// Prompt lifecycle. Semantics are deliberately narrow —
// "the dispatcher has finished interacting with this user message;
// no further MessageState transitions will arrive for this user
// message id". This is orthogonal to chatsession.PromptDone, which
// marks the receipt card (not the user message). MessageDone
// carries NO success / failure information; outcome is conveyed
// by the reply text itself (the ❌ prefix is the existing
// convention for failure replies).
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
	// Prompt ends (no fan-out; see
	// docs/feat/F-53-message-prompt-lifecycle.md §5.1).
	MessageSubmitted

	// MessageDropped: the message was explicitly cleared before
	// it could be submitted. Triggered by `ChatSession.
	// MarkDropped`, which is called from:
	//   - BufferClear (which `/close` and `/new` invoke to drop
	//     the queued batch)
	//
	// NOT triggered by SendBlocks failure — a failed send leaves
	// the message in `MessageQueued` and the next
	// `flushPending` retries it.
	MessageDropped

	// MessageDone: the dispatcher has finished its interaction
	// with this user message. Currently emitted by the framework
	// commander layer (internal/command/commander.go Dispatch)
	// immediately after the matched SlashCommandFactory.Handle
	// returns, regardless of success / failure. Symmetric with
	// the framework's pre-Handle MessageQueued emission so every
	// slash command gets a ⏳ → ✅ pair on the user message
	// without per-command wiring. See
	// docs/feat/slash-command-reactions.md for the full design
	// and rationale.
	MessageDone
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
	case MessageDone:
		return "done"
	}
	return "unknown"
}
