//go:build !windows

package claudecode

import (
	"strings"
	"testing"
)

// --- detectAskInText tests ---

func TestDetectAskInText_MarkdownTable(t *testing.T) {
	input := `I need to add caching. Which database should I use? Please pick one:

| Option | Description |
|--------|-------------|
| **PostgreSQL** | Production |
| **SQLite** | Dev only |
| **MongoDB** | Document |`

	q := detectAskInText(input)
	if q == nil {
		t.Fatal("expected Question, got nil")
	}
	if !strings.Contains(q.Question, "database") {
		t.Errorf("question = %q, want to contain 'database'", q.Question)
	}
	if len(q.Options) != 3 {
		t.Errorf("options = %d, want 3", len(q.Options))
	}
	if q.Options[0].Label != "PostgreSQL" {
		t.Errorf("first option = %q, want 'PostgreSQL'", q.Options[0].Label)
	}
	if q.Options[1].Label != "SQLite" {
		t.Errorf("second option = %q, want 'SQLite'", q.Options[1].Label)
	}
}

func TestDetectAskInText_NumberedList(t *testing.T) {
	input := `I have two approaches for migration. Which should I use? Please pick one:

1. Blue-green deployment - zero downtime
2. Rolling update - gradual rollout`

	q := detectAskInText(input)
	if q == nil {
		t.Fatal("expected Question, got nil")
	}
	if !strings.Contains(q.Question, "Which") {
		t.Errorf("question = %q, want to contain 'Which'", q.Question)
	}
	if len(q.Options) != 2 {
		t.Errorf("options = %d, want 2", len(q.Options))
	}
	if q.Options[0].Label != "Blue-green deployment" {
		t.Errorf("first option = %q", q.Options[0].Label)
	}
	if q.Options[0].Description != "zero downtime" {
		t.Errorf("first desc = %q", q.Options[0].Description)
	}
}

func TestDetectAskInText_NoAskKeyword_ReturnsNil(t *testing.T) {
	input := `Let me explain how this works.

| Option | Description |
|--------|-------------|
| Foo | Bar |
| Baz | Qux |`

	if q := detectAskInText(input); q != nil {
		t.Errorf("expected nil, got %+v", q)
	}
}

func TestDetectAskInText_OnlyOneOption_ReturnsNil(t *testing.T) {
	// F-24 spec: < 2 options is rejected by Claude Code itself, so
	// nightme should also reject to avoid rendering a broken card.
	input := `Please pick one:

1. Only option - nothing else`

	if q := detectAskInText(input); q != nil {
		t.Errorf("expected nil for single option, got %+v", q)
	}
}

func TestDetectAskInText_NoQuestionMark_ReturnsNil(t *testing.T) {
	input := `Please pick one.

| Option | Description |
|--------|-------------|
| A | first |
| B | second |`

	if q := detectAskInText(input); q != nil {
		t.Errorf("expected nil for no question mark, got %+v", q)
	}
}

func TestDetectAskInText_EmptyInput(t *testing.T) {
	if q := detectAskInText(""); q != nil {
		t.Errorf("expected nil for empty input")
	}
	if q := detectAskInText("   \n\n  "); q != nil {
		t.Errorf("expected nil for whitespace-only input")
	}
}

func TestDetectAskInText_HeaderCleaned(t *testing.T) {
	input := `Which database should I use? Please pick one.

| Option | Description |
|--------|-------------|
| Postgres | A |
| MySQL | B |`

	q := detectAskInText(input)
	if q == nil {
		t.Fatal("got nil")
	}
	// Header should strip "Which " prefix and end at the '?'.
	if q.Header == "" {
		t.Error("Header is empty")
	}
	if strings.HasPrefix(q.Header, "Which ") {
		t.Errorf("Header = %q, should not start with 'Which '", q.Header)
	}
}

// --- Header-from-question helpers ---

func TestHeaderFromQuestion_StripsPrefix(t *testing.T) {
	cases := map[string]string{
		"Which database should I use?":     "database should I use",
		"What is your preferred approach?": "is your preferred approach",
		"Please select the auth method?":   "the auth method",
		"Choose the deployment target?":    "the deployment target",
		"Which?":                           "Which",
	}
	for input, want := range cases {
		got := headerFromQuestion(input)
		if got != want {
			t.Errorf("headerFromQuestion(%q) = %q, want %q", input, got, want)
		}
	}
}

// --- Strip emphasis helper ---

func TestStripMarkdownEmphasis(t *testing.T) {
	cases := map[string]string{
		"**PostgreSQL**":             "PostgreSQL",
		"*SQLite*":                   "SQLite",
		"`code`":                     "code",
		"**Postgres (Recommended)**": "Postgres (Recommended)",
		"no emphasis":                "no emphasis",
	}
	for input, want := range cases {
		got := stripMarkdownEmphasis(input)
		if got != want {
			t.Errorf("stripMarkdownEmphasis(%q) = %q, want %q", input, got, want)
		}
	}
}

// --- isAllDigits ---

func TestIsAllDigits(t *testing.T) {
	cases := map[string]bool{
		"":    false,
		"0":   true,
		"123": true,
		"12a": false,
		" 1 ": false,
		"-1":  false,
		"1.0": false,
	}
	for input, want := range cases {
		if got := isAllDigits(input); got != want {
			t.Errorf("isAllDigits(%q) = %v, want %v", input, got, want)
		}
	}
}
