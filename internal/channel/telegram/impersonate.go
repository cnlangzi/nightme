// impersonate.go — the Telegram adapter speaking for the personal
// agent.
//
// What the agent says is a real message of the chat: it is posted on
// the platform like any other message and comes back into the daemon
// through Incoming(), so the receipt, the reaction and the
// interactive prompts anchor to it the way they anchor to a person's
// message.
//
// Telegram echoes the bot's own messages back through getUpdates, and
// the adapter drops them. The inbound that carries the message is
// therefore synthesized here, from the message the platform accepted.

package telegram

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/messages"
)

// Compile-time check: the adapter can speak for a program.
var _ channel.Impersonator = (*Adapter)(nil)

// Speak posts text into the chat and returns the id of the message
// it became.
//
// The chat shows the display text; the daemon is given the text
// itself, so a run can mark what it says without the mark reaching
// the agent.
func (a *Adapter) Speak(ctx context.Context, chatID, text string, opts channel.SpeakOptions) (string, error) {
	rawChatID, threadID, ok := splitSessionID(chatID)
	if !ok {
		return "", fmt.Errorf("telegram: chat id %q is not a telegram chat id", chatID)
	}
	display := opts.Display
	if display == "" {
		display = text
	}
	replyTo := 0
	if opts.ReplyTo != "" {
		n, err := strconv.Atoi(opts.ReplyTo)
		if err != nil {
			return "", fmt.Errorf("telegram: %q is not a telegram message id", opts.ReplyTo)
		}
		replyTo = n
	}

	result, err := a.sendTelegramMessage(ctx, rawChatID, threadID, replyTo, escapeInline(display), nil)
	if err != nil {
		return "", err
	}

	native := &Message{
		MessageID:       result.MessageID,
		MessageThreadID: threadID,
		IsTopicMessage:  threadID > 0,
		Date:            time.Now().Unix(),
		Chat:            a.nativeChat(rawChatID, threadID),
		From:            &User{ID: ownerID(opts.Owner)},
		Text:            text,
	}
	if replyTo > 0 {
		native.ReplyToMessage = &Message{MessageID: replyTo}
	}
	a.ingest(ctx, native, messages.OriginMe, opts.RunID)
	return strconv.Itoa(result.MessageID), nil
}

// Act clicks a button of an interactive prompt.
//
// The prompt is marked settled and the card shows the choice; the
// click enters the daemon as an ordinary card action — the same path
// a person's click takes.
func (a *Adapter) Act(ctx context.Context, chatID string, action messages.ActionPayload) error {
	if action.RequestID == "" {
		return errors.New("telegram: the action has no request id")
	}
	state, ok := a.state.choiceByRequestID(action.RequestID)
	if !ok {
		return fmt.Errorf("telegram: no prompt is waiting for %q", action.RequestID)
	}
	if state.Settled {
		return fmt.Errorf("telegram: the prompt %q was already answered", action.RequestID)
	}
	option, ok := findChoiceOption(state, action.Option)
	if !ok {
		return fmt.Errorf("telegram: the prompt %q has no option %q", action.RequestID, action.Option)
	}

	a.markSettled(state, option.ID)
	a.showChoiceMade(ctx, state, option)

	a.mu.Lock()
	botID := a.botID
	a.mu.Unlock()
	select {
	case a.incoming <- messages.InboundMessage{
		ChatID:     state.ChatID,
		UserID:     strconv.FormatInt(botID, 10),
		MessageID:  strconv.Itoa(state.MessageID),
		HasMention: true,
		Origin:     messages.OriginMe,
		Action: &messages.ActionPayload{
			RequestID: action.RequestID,
			Option:    option.ID,
			Form:      action.Form,
		},
	}:
	case <-a.ctxDone():
		return errors.New("telegram: the adapter is stopped")
	}
	return nil
}

// nativeChat renders the chat a synthesized message belongs to.
//
// The chat kind is taken from the stored topic state when the chat
// has one: the placeholder path needs it to decide whether the rich
// turn is created eagerly.
func (a *Adapter) nativeChat(rawChatID string, threadID int) Chat {
	chat := Chat{ID: rawChatIDNumber(rawChatID)}
	state, ok := a.state.topic(rawChatID, threadID)
	if !ok {
		return chat
	}
	if state.ChatKind == ChatKindPrivate {
		chat.Type = "private"
	} else {
		chat.Type = "group"
	}
	return chat
}

// showChoiceMade edits the prompt so the chat shows which option was
// picked.
func (a *Adapter) showChoiceMade(ctx context.Context, state *ChoiceState, option messages.ChoiceOption) {
	text := renderChoice(state)
	if text != "" {
		text += "\n\n"
	}
	label := option.Label
	if label == "" {
		label = option.ID
	}
	text += "<b>✓ " + escapeInline(label) + "</b>"
	keyboard := map[string]any{"inline_keyboard": []any{}}
	if err := a.editRichFromHTML(ctx, rawChatIDFromSession(state.ChatID), state.MessageID, text, keyboard); err != nil && a.logger != nil {
		a.logger.Warn("telegram: the answered prompt could not be edited",
			"request_id", state.RequestID, "err", err)
	}
}

// findChoiceOption returns the option the action names.
func findChoiceOption(state *ChoiceState, optionID string) (messages.ChoiceOption, bool) {
	if state == nil || state.Choice == nil {
		return messages.ChoiceOption{}, false
	}
	options := state.Choice.Options
	if len(state.Choice.Questions) > 0 {
		if state.Step >= len(state.Choice.Questions) {
			return messages.ChoiceOption{}, false
		}
		options = state.Choice.Questions[state.Step].Options
	}
	for _, option := range options {
		if option.ID == optionID || option.Label == optionID {
			return option, true
		}
	}
	return messages.ChoiceOption{}, false
}

// ownerID parses the id of the user a message is attributed to.
//
// A zero id means the message belongs to the bot itself.
func ownerID(owner string) int64 {
	if owner == "" {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(owner), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// rawChatIDNumber parses the numeric part of a raw chat id.
func rawChatIDNumber(rawChatID string) int64 {
	n, err := strconv.ParseInt(rawChatID, 10, 64)
	if err != nil {
		return 0
	}
	return n
}
