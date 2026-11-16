// Tests for the Feishu run card.

package feishu

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/channel"
)

// cardElements returns the elements of a rendered card.
func cardElements(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var card struct {
		Body struct {
			Elements []map[string]any `json:"elements"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(raw), &card); err != nil {
		t.Fatalf("the card could not be decoded: %v", err)
	}
	return card.Body.Elements
}

// tableElements counts the elements of a card that are a table.
func tableElements(elements []map[string]any) int {
	n := 0
	for _, el := range elements {
		content, _ := el["content"].(string)
		if tableRow(content) {
			n++
		}
	}
	return n
}

// TestRunCardJSON_KeepsInsideTheTableCap — Feishu refuses a card
// that carries more than five tables. The body of a run card is free
// text, so the tables past the fifth lose their table shape: the card
// is still created and still PATCHed.
func TestRunCardJSON_KeepsInsideTheTableCap(t *testing.T) {
	var body strings.Builder
	for i := 0; i < 6; i++ {
		body.WriteString("| a | b |\n")
		body.WriteString("text\n")
	}

	raw, err := runCardJSON(channel.CardSpec{Title: "run", Body: body.String()})
	if err != nil {
		t.Fatalf("the card could not be rendered: %v", err)
	}
	elements := cardElements(t, raw)
	if n := tableElements(elements); n > resultCardTableLimit {
		t.Fatalf("the card carries %d tables, want at most %d", n, resultCardTableLimit)
	}
	if n := tableElements(elements); n != resultCardTableLimit {
		t.Fatalf("the card carries %d tables, want the first %d kept", n, resultCardTableLimit)
	}
	// The table past the fifth is plain text: it kept its rows and
	// lost its shape.
	plain := 0
	for _, el := range elements {
		content, _ := el["content"].(string)
		if strings.Contains(content, `\|`) {
			plain++
		}
	}
	if plain != 1 {
		t.Fatalf("%d elements were rendered as plain text, want the sixth table", plain)
	}
}

// TestRunCardJSON_KeepsTheTablesOfTheFirstFive — the tables a card
// can hold are kept as they are.
func TestRunCardJSON_KeepsTheTablesOfTheFirstFive(t *testing.T) {
	var body strings.Builder
	for i := 0; i < 5; i++ {
		body.WriteString("| a | b |\n")
		body.WriteString("text\n")
	}

	raw, err := runCardJSON(channel.CardSpec{Title: "run", Body: body.String()})
	if err != nil {
		t.Fatalf("the card could not be rendered: %v", err)
	}
	if n := tableElements(cardElements(t, raw)); n != 5 {
		t.Fatalf("the card carries %d tables, want 5", n)
	}
}
