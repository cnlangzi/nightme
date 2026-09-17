package discord

// Component mirrors the Message Components V1 wire shape Discord
// accepts on CreateMessage / EditMessage and inside modal envelopes.
// V1 is the default — no IS_COMPONENTS_V2 flag is needed and content +
// embeds + components coexist on a single message.
//
// Component types (Discord docs /docs/components/reference):
//
//	1  ActionRow   — container for up to 5 Buttons or 1 Select.
//	2  Button      — interactive element; non-link / non-premium
//	                buttons MUST carry a CustomID.
//	3  StringSelect — V1 dropdown (unused in nightme).
//	4  TextInput   — only valid inside a modal.
//
// Button styles:
//
//	1  Primary   (blurple)
//	2  Secondary (grey)
//	3  Success   (green)
//	4  Danger    (red)
//	5  Link      (URL; no interaction event is emitted)
type Component struct {
	Type        ComponentType   `json:"type"`
	Style       int             `json:"style,omitempty"`
	Label       string          `json:"label,omitempty"`
	CustomID    string          `json:"custom_id,omitempty"`
	URL         string          `json:"url,omitempty"`
	Disabled    bool            `json:"disabled,omitempty"`
	Emoji       *ComponentEmoji `json:"emoji,omitempty"`
	Placeholder string          `json:"placeholder,omitempty"`
	MinLength   int             `json:"min_length,omitempty"`
	MaxLength   int             `json:"max_length,omitempty"`
	Required    bool            `json:"required,omitempty"`
	Value       string          `json:"value,omitempty"`
	Components  []Component     `json:"components,omitempty"`
}

// ComponentType is the discriminator on Component. The constants
// below are the only values nightme emits.
type ComponentType int

const (
	ComponentActionRow ComponentType = 1
	ComponentButton    ComponentType = 2
	ComponentTextInput ComponentType = 4
)

// ComponentEmoji is the optional emoji decoration on a Button.
// Only Name is populated — Discord resolves custom server emojis by
// id (which we never have); unicode emoji names (e.g. "🎉") are
// rendered directly. Kept in the model for forward compatibility
// with future "decorated choice" affordances.
type ComponentEmoji struct {
	Name string `json:"name,omitempty"`
	ID   string `json:"id,omitempty"`
}

// Button styles per Discord docs. Defined here so the choice-card
// builder doesn't sprinkle numeric literals through send.go.
const (
	ButtonStylePrimary   = 1
	ButtonStyleSecondary = 2
	ButtonStyleSuccess   = 3
	ButtonStyleDanger    = 4
	ButtonStyleLink      = 5
)

// ModalPayload is the body of an interaction response of Type=9
// (MODAL). Title is 1-45 chars; CustomID is 1-100 chars (the
// interaction correlate that lands on MODAL_SUBMIT).
type ModalPayload struct {
	Title      string      `json:"title"`
	CustomID   string      `json:"custom_id"`
	Components []Component `json:"components"`
}

// InteractionData is the `data` field on an incoming Interaction
// and the body of an outgoing InteractionResponse.
//
// On incoming MESSAGE_COMPONENT (Type=3) and MODAL_SUBMIT (Type=5):
//
//   - CustomID is the button / modal identifier nightme encoded at
//     send time ("c:<short>:<idx>" or "i:<short>").
//   - Components is populated for MODAL_SUBMIT — the user-supplied
//     text values land in the leaf TextInput's Value field.
//   - Value is the single-text-input shortcut (unused; MODAL_SUBMIT
//     uses Components instead per Discord's wire shape).
//
// On outgoing (AcknowledgeInteraction), only Type=7 (UPDATE_MESSAGE)
// leaves Data nil — Discord keeps the original message visible
// until a follow-up REST edit. Type=9 (MODAL) populates Data with
// a *ModalPayload (see AcknowledgeInteraction callers).
type InteractionData struct {
	CustomID   string      `json:"custom_id,omitempty"`
	Components []Component `json:"components,omitempty"`
	Modal      *Modal      `json:"-"`
}

// Modal is the polymorphic carrier for InteractionResponse.Data on
// Type=9. Kept as a separate type from ModalPayload so future
// components-only response types can coexist without renaming.
type Modal = ModalPayload

// InteractionResponse is the body of POST /interactions/{id}/{token}/callback.
//
// Type values (Discord /docs/interactions/receiving-and-responding):
//
//	1  PONG                                    — PING only.
//	4  CHANNEL_MESSAGE_WITH_SOURCE             — new visible reply.
//	5  DEFERRED_CHANNEL_MESSAGE_WITH_SOURCE    — ACK + visible loading.
//	6  DEFERRED_UPDATE_MESSAGE                 — ACK, no visible loading.
//	7  UPDATE_MESSAGE                          — edit the message the
//	                                            component was attached
//	                                            to in place. nightme
//	                                            uses this for choice
//	                                            clicks: ACK first, edit
//	                                            later via REST.
//	9  MODAL                                   — pop up a modal.
//
// The token expires 3 seconds after the interaction event is
// delivered. Callers must use a fresh context.WithTimeout(
// context.Background(), 2500*time.Millisecond) so a cancelled
// gateway ctx can't strand the ACK.
type InteractionResponse struct {
	Type int              `json:"type"`
	Data *InteractionData `json:"data,omitempty"`
}

// Interaction is the d-payload of op=0 t=INTERACTION_CREATE.
// ChannelID / MessageID identify the message the interaction was
// attached to (for UPDATE_MESSAGE the bot edits this exact
// message); User identifies the clicker.
type Interaction struct {
	ID            Snowflake        `json:"id"`
	ApplicationID Snowflake        `json:"application_id"`
	Type          int              `json:"type"`
	Token         string           `json:"token"`
	ChannelID     Snowflake        `json:"channel_id"`
	GuildID       Snowflake        `json:"guild_id,omitempty"`
	User          User             `json:"user,omitempty"`
	Member        *Member          `json:"member,omitempty"`
	Data          *InteractionData `json:"data,omitempty"`
	Message       *Message         `json:"message,omitempty"`
}

// Interaction types per Discord docs. Defined here so the
// interaction handler doesn't carry numeric literals.
const (
	InteractionTypePing             = 1
	InteractionTypeMessageComponent = 3
	InteractionTypeModalSubmit      = 5
)
