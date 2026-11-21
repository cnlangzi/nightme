// Tests for the receipt card's markdown-table cap.
//
// Feishu Card 2.0 holds five markdown tables per card and refuses a
// card that carries more. The rolling-log entries of the receipt carry
// the agent's reply, so a reply that holds more than five tables used
// to make the receipt card fail to be created — the turn showed
// nothing at all.

package feishu

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// receiptTables counts the markdown tables a receipt card holds.
//
// Feishu renders the tables of a markdown element on their own, so the
// tables of the card are the tables of its elements.
func receiptTables(t *testing.T, raw string) int {
	t.Helper()
	var card struct {
		Body struct {
			Elements []struct {
				Tag     string `json:"tag"`
				Content string `json:"content"`
			} `json:"elements"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(raw), &card); err != nil {
		t.Fatalf("the card could not be decoded: %v", err)
	}
	n := 0
	for _, el := range card.Body.Elements {
		if el.Tag != "markdown" {
			continue
		}
		n += countMarkdownTables(el.Content)
	}
	return n
}

// receiptTableEntries builds one rolling-log entry per table: the
// header row, the separator row and one body row.
func receiptTableEntries(n int) []LogEntry {
	entries := make([]LogEntry, 0, n)
	for i := 0; i < n; i++ {
		entries = append(entries, LogEntry{
			Text: fmt.Sprintf("| c%d | h%d |\n| --- | --- |\n| x%d | y%d |", i, i, i, i),
		})
	}
	return entries
}

// TestBuildReceiptCard_KeepsInsideTheTableCap — Feishu refuses a card
// that carries more than five tables. The tables past the fifth lose
// their table shape: the card is still created and still PATCHed.
func TestBuildReceiptCard_KeepsInsideTheTableCap(t *testing.T) {
	body, _, err := buildReceiptCard(receiptTableEntries(7), nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("buildReceiptCard: %v", err)
	}
	if n := receiptTables(t, body); n > resultCardTableLimit {
		t.Fatalf("the card holds %d tables, want at most %d", n, resultCardTableLimit)
	}
	if n := receiptTables(t, body); n != resultCardTableLimit {
		t.Fatalf("the card holds %d tables, want the first %d kept", n, resultCardTableLimit)
	}
}

// TestBuildReceiptCard_KeepsTheRowsOfTheTablesPastTheCap — the text of
// a table that lost its shape stays in the card: only the border of
// the row is dropped, the cells of the row remain readable as plain
// lines.
func TestBuildReceiptCard_KeepsTheRowsOfTheTablesPastTheCap(t *testing.T) {
	body, _, err := buildReceiptCard(receiptTableEntries(7), nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("buildReceiptCard: %v", err)
	}
	for _, i := range []int{5, 6} {
		if !strings.Contains(body, fmt.Sprintf("c%d | h%d", i, i)) {
			t.Errorf("the header of table %d is missing from the card:\n%s", i, body)
		}
		if !strings.Contains(body, fmt.Sprintf("x%d | y%d", i, i)) {
			t.Errorf("the body of table %d is missing from the card:\n%s", i, body)
		}
		if strings.Contains(body, fmt.Sprintf("| c%d | h%d |", i, i)) {
			t.Errorf("the table %d kept its border:\n%s", i, body)
		}
	}
}

// TestBuildReceiptCard_KeepsTheTablesOfTheFirstFive — a receipt that
// stays inside the cap is unchanged: the tables it holds are tables.
func TestBuildReceiptCard_KeepsTheTablesOfTheFirstFive(t *testing.T) {
	body, _, err := buildReceiptCard(receiptTableEntries(5), nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("buildReceiptCard: %v", err)
	}
	if n := receiptTables(t, body); n != 5 {
		t.Fatalf("the card holds %d tables, want 5", n)
	}
	if !strings.Contains(body, "| c4 | h4 |") {
		t.Errorf("the fifth table lost its shape:\n%s", body)
	}
}

// TestBuildReceiptCard_KeepsInsideTheTableCap_SingleEntry — the tables
// of one entry count against the same cap: a reply that carries seven
// tables in one chunk is capped the same way.
func TestBuildReceiptCard_KeepsInsideTheTableCap_SingleEntry(t *testing.T) {
	var text strings.Builder
	for i := 0; i < 7; i++ {
		text.WriteString(fmt.Sprintf("| c%d | h%d |\n| --- | --- |\n| x%d | y%d |\n\n", i, i, i, i))
	}
	body, _, err := buildReceiptCard([]LogEntry{{Text: text.String()}}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("buildReceiptCard: %v", err)
	}
	if n := receiptTables(t, body); n != resultCardTableLimit {
		t.Fatalf("the card holds %d tables, want %d", n, resultCardTableLimit)
	}
	if !strings.Contains(body, "c6 | h6") {
		t.Errorf("the seventh table lost its text:\n%s", body)
	}
}

// TestBuildReceiptCard_DropsTheDelimiterOfTheTablesPastTheCap — a
// markdown parser reads a row of cells followed by a delimiter row as a
// table, so the flattened tables have to lose their delimiter row: what
// is left are plain lines.
func TestBuildReceiptCard_DropsTheDelimiterOfTheTablesPastTheCap(t *testing.T) {
	body, _, err := buildReceiptCard(receiptTableEntries(7), nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("buildReceiptCard: %v", err)
	}
	if n := strings.Count(body, "| --- | --- |"); n != resultCardTableLimit {
		t.Fatalf("the card holds %d delimiter rows, want the %d of the tables it keeps", n, resultCardTableLimit)
	}
}

// TestBuildReceiptCard_KeepsTheCodeSamples — a fenced code block is
// code, not a table: its pipe rows are neither counted against the cap
// nor rewritten, so the sample the agent showed keeps its pipes.
func TestBuildReceiptCard_KeepsTheCodeSamples(t *testing.T) {
	entries := receiptTableEntries(5)
	entries = append(entries, LogEntry{Text: "```markdown\n| keep | me |\n| --- | --- |\n```"})
	body, _, err := buildReceiptCard(entries, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("buildReceiptCard: %v", err)
	}
	if !strings.Contains(body, "| keep | me |") {
		t.Errorf("the code sample lost its pipes:\n%s", body)
	}
	if !strings.Contains(body, "| --- | --- |") {
		t.Errorf("the code sample lost its delimiter row:\n%s", body)
	}
	if n := receiptTables(t, body); n != resultCardTableLimit {
		t.Fatalf("the card holds %d tables, want %d", n, resultCardTableLimit)
	}
}
