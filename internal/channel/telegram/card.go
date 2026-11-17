// card.go — the Telegram card.
//
// A card is one message of the chat, edited in place as the run
// moves. The card of a session is the session's own: the run posts it
// and the chat edits it, so a session owns one message and the chat
// never posts a second one for it.

package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/cnlangzi/nightme/internal/channel"
)

// Compile-time check: the adapter can publish a card.
var _ channel.CardPublisher = (*Adapter)(nil)

// postedCardsMax bounds the cache of the cards this adapter posted.
const postedCardsMax = 2000

// postedCardKey names a card this adapter posted.
//
// A message id is unique inside its chat, not across the chats, so
// the chat is part of the key.
type postedCardKey struct {
	chatID    string
	messageID int
}

// PostCard publishes the card in the chat.
//
// The card is posted as a reply to the message the spec names, so it
// hangs where the operator expects it. A card this adapter posted is
// a card it can adopt: the events that carry its id as their anchor
// are rendered onto it instead of onto a second message.
func (a *Adapter) PostCard(ctx context.Context, chatID string, spec channel.CardSpec) (string, error) {
	rawChatID, topicID, ok := splitSessionID(chatID)
	if !ok {
		return "", fmt.Errorf("telegram: chat id %q is not a telegram chat id", chatID)
	}
	replyTo := 0
	if id, err := strconv.Atoi(spec.ReplyTo); err == nil && id > 0 {
		replyTo = id
	}
	result, err := a.sendRichBlocks(ctx, rawChatID, topicID, replyTo, renderRunCard(spec), nil)
	if err != nil {
		return "", err
	}
	a.rememberPostedCard(rawChatID, result.MessageID)
	return strconv.Itoa(result.MessageID), nil
}

// AdoptCard tells the adapter that the message with the given id is a
// card of its own.
//
// The events that carry such an anchor are rendered onto that card
// instead of onto a new one. A daemon that comes back has forgotten
// the cards it posted, and the owner of the card tells it again.
func (a *Adapter) AdoptCard(chatID, cardID string) {
	rawChatID, _, ok := splitSessionID(chatID)
	if !ok {
		return
	}
	id, err := strconv.Atoi(cardID)
	if err != nil {
		return
	}
	a.rememberPostedCard(rawChatID, id)
}

// rememberPostedCard records a card this adapter posted.
//
// The card is the anchor of the session it belongs to: the rich turn
// for that anchor adopts the card rather than sending a new message.
func (a *Adapter) rememberPostedCard(chatID string, messageID int) {
	if messageID == 0 {
		return
	}
	a.mu.Lock()
	if len(a.postedCards) >= postedCardsMax {
		// The cache is a convenience: it tells the adapter which
		// anchors are cards of its own. Forgetting one costs a second
		// card for that session, nothing more.
		clear(a.postedCards)
	}
	a.postedCards[postedCardKey{chatID: chatID, messageID: messageID}] = true
	a.mu.Unlock()
}

// adoptableCard reports whether the anchor names a card this adapter
// posted.
//
// Such an anchor is a session's own card: the rich turn starts on it
// instead of cold-creating a message of its own.
func (a *Adapter) adoptableCard(chatID string, anchor int) bool {
	if anchor == 0 {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.postedCards[postedCardKey{chatID: chatID, messageID: anchor}]
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
