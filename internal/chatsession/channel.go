package chatsession

import "context"

// Channel is the outbound channel surface that ChatSession depends on.
//
// Defined here in chatsession (rather than reusing internal/channel.Channel)
// to avoid an import cycle: internal/channel/feishu and internal/gateway
// both depend on internal/chatsession, so chatsession cannot import them.
//
// The concrete *channel.Channel satisfies this interface via Go's
// structural typing once a thin wrapper translates gateway.OutboundMessage
// to chatsession.OutboundMessage — see cmd/nightme/channel_adapter.go.
//
// Set once at ChatSession construction (Manager.GetOrCreate resolves via
// the channelResolver wired at runtime startup); read-only thereafter.
type Channel interface {
	// Send posts a text reply.
	Send(ctx context.Context, msg OutboundMessage) error
	// SendCard posts an interactive card; returns the bot-side msg id.
	SendCard(ctx context.Context, msg OutboundMessage) (msgID string, err error)
	// Patch edits an existing bot message in place (F-46). The
	// implementation should treat msg.PatchBotMsgID as the target
	// message id and update its body in place.
	Patch(ctx context.Context, msg OutboundMessage) error
}

// OutboundMessage is the message payload Channel accepts. Mirrors the
// relevant subset of gateway.OutboundMessage — the runtime shim
// translates between them. Defined here to keep chatsession self-contained.
type OutboundMessage struct {
	ChatID  string
	Text    string
	ReplyTo string
	Card    *Card
	// F-46: when PatchBotMsgID is set the Channel patches the
	// existing bot message in place instead of sending new content.
	PatchBotMsgID     string
	PatchChosenEmoji  string
	PatchResult       string
	CardTitle         string
	CardBody          string
	CardChoices       []CardChoice
	CardRequestID     string
	ChosenChoiceEmoji string
}

// Card is the interactive card payload.
type Card struct {
	Kind        string
	Title       string
	Body        string
	Choices     []CardChoice
	RequestID   string
	Disabled    bool
	ChosenEmoji string
}

// CardChoice is one button on a decision card.
type CardChoice struct {
	Emoji  string
	Label  string
	Action string
}