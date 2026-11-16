// card.go — the Feishu run card.
//
// A run card is the overview of a workflow run: what the run is
// doing, what it decided last, and the buttons that steer it. It is
// one message of the chat, PATCHed in place as the run moves.
//
// The buttons carry the text a click produces, so a click travels the
// same path a typed command takes.

package feishu

import (
	"context"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/channel"

	larkcallback "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
)

// Compile-time check: the adapter can publish a card.
var _ channel.CardPublisher = (*Adapter)(nil)

// meActionPrefix marks a card button that runs a /me subcommand.
const meActionPrefix = "me:"

// PostCard publishes the run card in the chat.
func (a *Adapter) PostCard(ctx context.Context, chatID string, spec channel.CardSpec) (string, error) {
	content, err := runCardJSON(spec)
	if err != nil {
		return "", err
	}
	return a.sendCardContent(ctx, chatID, content, "", false)
}

// UpdateCard replaces the card's content.
func (a *Adapter) UpdateCard(ctx context.Context, chatID, cardID string, spec channel.CardSpec) error {
	content, err := runCardJSON(spec)
	if err != nil {
		return err
	}
	return a.PatchMessage(ctx, cardID, content)
}

// runCardJSON renders the card as a Feishu card 2.0 envelope.
//
// The body is rendered one line per element: Feishu's markdown
// element treats a newline as a soft break, so a body with newlines
// in a single element runs together on the card.
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
	if len(spec.Buttons) > 0 {
		buttons := make([]map[string]any, 0, len(spec.Buttons))
		for _, b := range spec.Buttons {
			buttons = append(buttons, map[string]any{
				"tag":  "button",
				"text": map[string]any{"tag": "plain_text", "content": b.Label},
				"type": "default",
				"value": map[string]string{
					"action": meActionPrefix + b.Action,
				},
			})
		}
		elements = append(elements, buildColumnSet(buttons))
	}

	title := spec.Title
	if title == "" {
		title = "Run"
	}
	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{"width_mode": "fill"},
		"header": map[string]any{
			"title":    map[string]any{"tag": "plain_text", "content": title},
			"template": "blue",
		},
		"body": map[string]any{"elements": elements},
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
// The lines are the body of the card in render order. A table is a
// run of consecutive lines that are pipe-delimited rows. The tables
// past the fifth lose their table shape: their rows are rendered as
// plain text, so the card is still created and still PATCHed —
// Feishu refuses a card that carries more than five tables.
func limitCardTables(lines []string) []string {
	out := make([]string, len(lines))
	tables := 0
	inTable := false
	for i, line := range lines {
		if tableRow(line) {
			if !inTable {
				tables++
				inTable = true
			}
		} else {
			inTable = false
		}
		if inTable && tables > resultCardTableLimit {
			out[i] = plainTableRow(line)
			continue
		}
		out[i] = line
	}
	return out
}

// plainTableRow renders a row of a markdown table as plain text.
//
// The pipes are escaped: the row keeps its shape and stops being a
// table.
func plainTableRow(line string) string {
	return strings.ReplaceAll(line, "|", `\|`)
}

// handleMeCardAction turns a run-card button click into the command
// it stands for.
//
// The command enters the daemon as an ordinary inbound message, so it
// is dispatched by the same code that dispatches a typed command.
func (a *Adapter) handleMeCardAction(ctx context.Context, event *larkcallback.CardActionTriggerEvent, command string) (*larkcallback.CardActionTriggerResponse, error) {
	chatID := SessionChatID(cardActionSource{event: event})
	if chatID == "" {
		return &larkcallback.CardActionTriggerResponse{}, nil
	}
	synthetic := channel.Message{
		ChatID:     chatID,
		Text:       "/me " + strings.TrimSpace(command),
		HasMention: true,
		Time:       time.Now(),
	}
	select {
	case a.incoming <- synthetic:
		return &larkcallback.CardActionTriggerResponse{
			Toast: &larkcallback.Toast{Type: "info", Content: "Recorded"},
		}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		a.logger.Warn("feishu: run card action inbound full, dropping",
			"chat_id", chatID, "command", command)
	}
	// The click did not reach the daemon: the toast must not claim it
	// was recorded.
	return &larkcallback.CardActionTriggerResponse{
		Toast: &larkcallback.Toast{Type: "warning", Content: "The chat is busy, try again"},
	}, nil
}
