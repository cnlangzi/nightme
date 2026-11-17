// card.go — the Feishu card.
//
// A card is one message of the chat, PATCHed in place as the run
// moves. The card of a session is the session's own: the run posts it
// and the chat patches it, so a session owns one card and the chat
// never posts a second one for it.

package feishu

import (
	"context"
	"strings"

	"github.com/cnlangzi/nightme/internal/channel"
)

// Compile-time check: the adapter can publish a card.
var _ channel.CardPublisher = (*Adapter)(nil)

// postedCardKey names a card this adapter posted.
//
// A message id is unique inside its chat, not across the chats, so
// the chat is part of the key.
type postedCardKey struct {
	chatID string
	msgID  string
}

// PostCard publishes the card in the chat.
//
// The card is posted as a reply to the message the spec names, so it
// hangs where the operator expects it. A card this adapter posted is
// a card it can adopt: the events that carry its id as their anchor
// are rendered onto it instead of onto a second card.
func (a *Adapter) PostCard(ctx context.Context, chatID string, spec channel.CardSpec) (string, error) {
	content, err := runCardJSON(spec)
	if err != nil {
		return "", err
	}
	msgID, err := a.sendCardContent(ctx, chatID, content, spec.ReplyTo, false)
	if err != nil {
		return "", err
	}
	a.rememberPostedCard(chatID, msgID)
	return msgID, nil
}

// AdoptCard tells the adapter that the message with the given id is a
// card of its own.
//
// The events that carry such an anchor are rendered onto that card
// instead of onto a new one. A daemon that comes back has forgotten
// the cards it posted, and the owner of the card tells it again.
func (a *Adapter) AdoptCard(chatID, cardID string) {
	a.rememberPostedCard(chatID, cardID)
}

// rememberPostedCard records a card this adapter posted.
//
// The card is the anchor of the session it belongs to: the receipt
// for that anchor adopts the card rather than sending a new one.
func (a *Adapter) rememberPostedCard(chatID, msgID string) {
	if msgID == "" {
		return
	}
	a.postedCards.Add(postedCardKey{chatID: chatID, msgID: msgID}, true)
}

// adoptableCard returns the id of the card the anchor names, when the
// anchor is a card this adapter posted.
//
// An empty result means the anchor is an ordinary message: the
// receipt for it is created and sent as it is.
func (a *Adapter) adoptableCard(chatID, anchor string) string {
	if anchor == "" {
		return ""
	}
	if a.postedCards.Contains(postedCardKey{chatID: chatID, msgID: anchor}) {
		return anchor
	}
	return ""
}

// runCardJSON renders the card as a Feishu card 2.0 envelope.
//
// The body is rendered one line per element: Feishu's markdown
// element treats a newline as a soft break, so a body with newlines
// in a single element runs together on the card.
//
// A card with no title has no header: the header of a session card
// would be replaced by the session's own header when the card is
// patched, and a card that changes its shape reads as two cards.
//
// The body keeps inside Card 2.0's cap of five markdown tables: the
// tables past the fifth lose their table shape, so the card is still
// created and still PATCHed.
func runCardJSON(spec channel.CardSpec) (string, error) {
	lines := strings.Split(spec.Body, "\n")
	elements := make([]map[string]any, 0, 8)
	for _, line := range limitCardTables(lines) {
		if strings.TrimSpace(line) == "" {
			elements = append(elements, map[string]any{"tag": "hr"})
			continue
		}
		elements = append(elements, map[string]any{
			"tag":     "markdown",
			"content": SanitizeCardMarkdown(line),
		})
	}
	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{"width_mode": "fill"},
		"body":   map[string]any{"elements": elements},
	}
	if title := strings.TrimSpace(spec.Title); title != "" {
		card["header"] = map[string]any{
			"title":    map[string]any{"tag": "plain_text", "content": title},
			"template": "blue",
		}
	}
	raw, err := encodeCardJSON(card)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// limitCardTables keeps a card body inside Feishu Card 2.0's hard cap
// of five markdown tables.
//
// The lines are the body of the card in render order, and the body is
// one part of the card: the tables of the body are counted from the
// first one. See limitCardTableLines for what the cap does to the
// tables past it.
func limitCardTables(lines []string) []string {
	out, _ := limitCardTableLines(lines, 0)
	return out
}

// limitCardTableLines keeps the tables of one part of a card inside
// Feishu Card 2.0's hard cap of five markdown tables.
//
// The lines are the markdown of the part in render order. used is the
// number of tables the parts before this one hold: the cap is a
// property of the card and not of one part, so a card that holds six
// tables flattens the sixth whatever part it lives in. The returned
// count is the number of tables this part holds.
//
// The tables past the fifth lose their table shape: their rows are
// rendered as plain text and their delimiter row is dropped, so what is
// left is not a table any more and the card is still created and still
// PATCHed — Feishu refuses a card that carries more than five tables.
func limitCardTableLines(lines []string, used int) ([]string, int) {
	groups := tableGroups(lines)
	out := make([]string, 0, len(lines))
	for i, line := range lines {
		if groups[i] == 0 || used+groups[i] <= resultCardTableLimit {
			out = append(out, line)
			continue
		}
		// The delimiter row is the table's syntax and not its text:
		// dropping it is what keeps the rows around it from reading
		// as a table again.
		if tableDelimiterRow(line) {
			continue
		}
		out = append(out, plainTableRow(line))
	}
	return out, used + countTableGroups(groups)
}

// plainTableRow renders a row of a markdown table as plain text.
//
// The border of the row is the table's syntax and not its text: what
// is left is the cells of the row.
func plainTableRow(line string) string {
	trimmed := strings.TrimSpace(line)
	trimmed = strings.TrimPrefix(trimmed, "|")
	trimmed = strings.TrimSuffix(trimmed, "|")
	return strings.TrimSpace(trimmed)
}
