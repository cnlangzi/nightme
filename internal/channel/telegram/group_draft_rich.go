package telegram

import (
	"encoding/json"
	"fmt"
)

// buildParagraphBlocksForEntries serializes a windowed entries buffer
// into the rich-message blocks JSON envelope Telegram's sendRichMessage
// and editMessageText(rich_message=...) accept. Each entry becomes
// one `{type:"paragraph", text:body}` block — no markdown walking,
// no inline-entity expansion. The entry body is already plain text
// (emoji-prefixed lines like "💭 considering X" / "● Read(...)" /
// "⎿ 📄 Read → 47 lines") emitted by summarizeTool / formatTool /
// the "💭 " prefix at adapter.go, so escaping <, >, & is unnecessary.
//
// Returns the wire form `{"blocks":[…]}` ready for the
// `rich_message` parameter.
func buildParagraphBlocksForEntries(entries []richTurnEntry) (string, error) {
	blocks := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		blocks = append(blocks, map[string]any{
			"type": "paragraph",
			"text": e.body,
		})
	}
	wrapped := map[string]any{"blocks": blocks}
	data, err := json.Marshal(wrapped)
	if err != nil {
		return "", fmt.Errorf("telegram: encode rich blocks: %w", err)
	}
	return string(data), nil
}
