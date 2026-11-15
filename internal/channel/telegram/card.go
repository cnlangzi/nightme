// card.go — the Telegram run card.
//
// A run card is the overview of a workflow run: what the run is
// doing, what it decided last, and the buttons that steer it. It is
// one message of the chat, edited in place as the run moves.
//
// The buttons carry the text a click produces, so a click travels the
// same path a typed command takes.

package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/messages"
)

// Compile-time check: the adapter can publish a card.
var _ channel.CardPublisher = (*Adapter)(nil)

// meActionKind marks a card button that runs a /me subcommand.
const meActionKind = "m"

// meActionPrefix is the callback-data prefix of a run-card button.
const meActionPrefix = meActionKind + ":"

// PostCard publishes the run card in the chat.
func (a *Adapter) PostCard(ctx context.Context, chatID string, spec channel.CardSpec) (string, error) {
	rawChatID, topicID, ok := splitSessionID(chatID)
	if !ok {
		return "", fmt.Errorf("telegram: chat id %q is not a telegram chat id", chatID)
	}
	result, err := a.sendRichBlocks(ctx, rawChatID, topicID, 0, renderRunCard(spec), runCardKeyboard(spec))
	if err != nil {
		return "", err
	}
	return strconv.Itoa(result.MessageID), nil
}

// UpdateCard replaces the card's content.
func (a *Adapter) UpdateCard(ctx context.Context, chatID, cardID string, spec channel.CardSpec) error {
	rawChatID, _, ok := splitSessionID(chatID)
	if !ok {
		return fmt.Errorf("telegram: chat id %q is not a telegram chat id", chatID)
	}
	id, err := strconv.Atoi(cardID)
	if err != nil {
		return fmt.Errorf("telegram: %q is not a telegram message id", cardID)
	}
	return a.editRichBlocks(ctx, rawChatID, id, renderRunCard(spec), runCardKeyboard(spec))
}

// renderRunCard renders the card as rich-message blocks: the title as
// a heading, the body one paragraph per line.
//
// One block per line keeps every line of the overview on a line of
// its own, the way the card reads on the other channels.
func renderRunCard(spec channel.CardSpec) string {
	blocks := make([]map[string]any, 0, 10)
	if spec.Title != "" {
		blocks = append(blocks, map[string]any{
			"type": "heading",
			"text": escapeInline(spec.Title),
			"size": 2,
		})
	}
	for _, line := range strings.Split(spec.Body, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		blocks = append(blocks, map[string]any{
			"type": "paragraph",
			"text": escapeInline(line),
		})
	}
	if len(blocks) == 0 {
		blocks = append(blocks, map[string]any{
			"type": "paragraph",
			"text": "",
		})
	}
	b, _ := json.Marshal(blocks)
	return string(b)
}

// runCardKeyboard renders the card's buttons as an inline keyboard.
//
// The buttons are laid out two to a row, the way the choice keyboard
// lays them out: a run card can carry a button per option, and a row
// of ten is unreadable.
func runCardKeyboard(spec channel.CardSpec) map[string]any {
	if len(spec.Buttons) == 0 {
		return nil
	}
	rows := make([]any, 0, (len(spec.Buttons)+1)/2)
	row := make([]any, 0, 2)
	for _, b := range spec.Buttons {
		if len(row) == 2 {
			rows = append(rows, row)
			row = make([]any, 0, 2)
		}
		row = append(row, map[string]any{
			"text":          escapeInline(b.Label),
			"callback_data": meActionPrefix + b.Action,
		})
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	return map[string]any{"inline_keyboard": rows}
}

// handleRunCardClick turns a run-card button click into the command
// it stands for.
//
// The command enters the daemon as an ordinary inbound message, so it
// is dispatched by the same code that dispatches a typed command.
func (a *Adapter) handleRunCardClick(ctx context.Context, callback *CallbackQuery, action string) {
	if callback == nil || callback.Message == nil {
		if callback != nil {
			a.answerCallback(ctx, callback.ID, "Unsupported action", true)
		}
		return
	}
	chatID := a.sessionChatID(
		strconv.FormatInt(callback.Message.Chat.ID, 10),
		callback.Message.MessageThreadID,
	)
	synthetic := messages.InboundMessage{
		ChatID:     chatID,
		Text:       "/me " + strings.TrimSpace(action),
		HasMention: true,
		Time:       time.Now(),
	}
	// The click is published without blocking: a full queue must not
	// stall the poll loop.
	if !a.publish(synthetic) {
		a.answerCallback(ctx, callback.ID, "The chat is busy, try again", true)
		return
	}
	a.answerCallback(ctx, callback.ID, "Recorded", false)
}
