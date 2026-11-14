// impersonate.go — the Feishu adapter speaking for the personal
// agent.
//
// What the agent says is a real message of the chat: it is posted on
// the platform like any other message and comes back into the daemon
// through Incoming(), so the receipt, the reaction and the
// interactive prompts anchor to it the way they anchor to a person's
// message.
//
// Feishu does not deliver the bot's own messages back to the bot, so
// the inbound that carries the message is synthesized here.

package feishu

import (
	"context"
	"errors"
	"fmt"
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
	if chatID == "" {
		return "", errors.New("feishu: chat_id is required")
	}
	display := opts.Display
	if display == "" {
		display = text
	}
	msgID, err := a.SendMessageText(ctx, chatID, display, opts.ReplyTo, false)
	if err != nil {
		return "", err
	}

	owner := opts.Owner
	if owner == "" {
		owner = a.fetchBotOpenID(ctx)
	}
	inbound := channel.Message{
		ChatID:     chatID,
		UserID:     owner,
		Text:       text,
		MessageID:  msgID,
		ReplyTo:    opts.ReplyTo,
		Time:       time.Now(),
		HasMention: true,
		Origin:     messages.OriginMe,
		RunID:      opts.RunID,
	}
	a.logInbound(inbound)
	if err := a.publishImpersonated(ctx, inbound); err != nil {
		return msgID, fmt.Errorf("feishu: the message did not reach the daemon: %w", err)
	}
	return msgID, nil
}

// Ask puts a question in the chat and returns the id of the message
// it became.
//
// The message is visible and the operator can answer it, but the
// daemon does not take it as a turn of its own: the question belongs
// to the run that asked it.
func (a *Adapter) Ask(ctx context.Context, chatID, text string, opts channel.SpeakOptions) (string, error) {
	display := opts.Display
	if display == "" {
		display = text
	}
	return a.SendMessageText(ctx, chatID, display, opts.ReplyTo, false)
}

// Act clicks a button of an interactive prompt.
//
// The card is patched to show the choice, and the click enters the
// daemon as an ordinary card action — the same path a person's
// click takes.
func (a *Adapter) Act(ctx context.Context, chatID string, action messages.ActionPayload) error {
	if action.RequestID == "" {
		return errors.New("feishu: the action has no request id")
	}
	botMsgID := a.msgIDForRequestID(action.RequestID)
	if botMsgID == "" {
		botMsgID = a.lastOptMsgID(chatID)
	}
	if botMsgID == "" {
		return fmt.Errorf("feishu: no card is waiting for %q", action.RequestID)
	}

	option, _, _, err := a.applyOptClick(botMsgID, action.RequestID, action.Option, false, "")
	if err != nil {
		return fmt.Errorf("feishu: the card could not be answered: %w", err)
	}
	if option == "" {
		return fmt.Errorf("feishu: the card has no option %q", action.Option)
	}

	inbound := channel.Message{
		ChatID:     chatID,
		UserID:     a.fetchBotOpenID(ctx),
		Text:       "",
		MessageID:  botMsgID,
		Time:       time.Now(),
		HasMention: true,
		Origin:     messages.OriginMe,
		Action: &messages.ActionPayload{
			RequestID: action.RequestID,
			Option:    option,
			Form:      action.Form,
		},
	}
	return a.publishImpersonated(ctx, inbound)
}

// publishImpersonated pushes a synthesized inbound message onto the
// adapter's inbound queue.
//
// The queue is closed when the adapter stops; the push gives up then
// rather than blocking forever.
func (a *Adapter) publishImpersonated(ctx context.Context, msg channel.Message) error {
	a.mu.RLock()
	if a.stopped || a.incoming == nil {
		a.mu.RUnlock()
		return errors.New("feishu: the adapter is stopped")
	}
	done := a.done
	in := a.incoming
	a.mu.RUnlock()

	a.publishMu.Lock()
	defer a.publishMu.Unlock()
	a.mu.RLock()
	if a.stopped || a.incoming == nil {
		a.mu.RUnlock()
		return errors.New("feishu: the adapter is stopped")
	}
	a.mu.RUnlock()

	select {
	case in <- msg:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return errors.New("feishu: the adapter is stopped")
	}
}
