package discord

import (
	"strconv"
	"testing"

	"github.com/cnlangzi/nightme/internal/messages"
)

// TestBuildChoiceComponents_RowsAndStyles verifies the V1 layout
// invariant: ≤5 buttons per row, ≤5 rows total, Primary on the
// first option only, Secondary on the rest.
func TestBuildChoiceComponents_RowsAndStyles(t *testing.T) {
	state := &choiceState{
		RequestID: "req-1",
		Choice: &messages.Choice{
			RequestID: "req-1",
			Options:   messages.ChoiceOptionsFromLabels([]string{"yes", "no", "later"}),
		},
	}
	a := newTestAdapter(&fakeREST{})
	rows := a.buildChoiceComponents(state)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.Type != ComponentActionRow {
		t.Errorf("row type = %d, want ActionRow", row.Type)
	}
	if len(row.Components) != 3 {
		t.Errorf("button count = %d, want 3", len(row.Components))
	}
	if row.Components[0].Style != ButtonStylePrimary {
		t.Errorf("first style = %d, want Primary", row.Components[0].Style)
	}
	for i := 1; i < len(row.Components); i++ {
		if row.Components[i].Style != ButtonStyleSecondary {
			t.Errorf("option %d style = %d, want Secondary", i, row.Components[i].Style)
		}
	}
}

// TestBuildChoiceComponents_LargeOptionSetWrapsIntoFivePerRow
// verifies the 5-buttons-per-row limit with a 7-option list.
func TestBuildChoiceComponents_LargeOptionSetWrapsIntoFivePerRow(t *testing.T) {
	state := &choiceState{
		RequestID: "req-1",
		Choice: &messages.Choice{
			RequestID: "req-1",
			Options:   messages.ChoiceOptionsFromLabels([]string{"a", "b", "c", "d", "e", "f", "g"}),
		},
	}
	a := newTestAdapter(&fakeREST{})
	rows := a.buildChoiceComponents(state)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if len(rows[0].Components) != 5 {
		t.Errorf("first row = %d, want 5", len(rows[0].Components))
	}
	if len(rows[1].Components) != 2 {
		t.Errorf("second row = %d, want 2", len(rows[1].Components))
	}
}

// TestBuildChoiceComponents_ExcessiveOptionsCappedForRowBudget
// verifies the >20-options case is capped to 4 rows of 5 so the
// appended "Type your answer" row keeps the total ≤5 (Discord's
// hard limit). The review pointed this out as a 400-rejection risk.
func TestBuildChoiceComponents_ExcessiveOptionsCappedForRowBudget(t *testing.T) {
	opts := make([]string, 30)
	for i := range opts {
		opts[i] = "opt-" + strconv.Itoa(i)
	}
	state := &choiceState{
		RequestID: "req-1",
		Choice: &messages.Choice{
			RequestID: "req-1",
			Questions: []messages.ChoiceQuestion{{
				ID:      "q1",
				Options: messages.ChoiceOptionsFromLabels(opts),
			}},
		},
	}
	a := newTestAdapter(&fakeREST{})
	rows := a.buildChoiceComponents(state)
	// 4 option rows + 1 input row = 5 rows. Discord rejects >5.
	if len(rows) != 5 {
		t.Errorf("rows = %d, want 5 (4 option + 1 input)", len(rows))
	}
	// Total option buttons = 20 (cap).
	totalButtons := 0
	for _, row := range rows[:len(rows)-1] {
		totalButtons += len(row.Components)
	}
	if totalButtons != 20 {
		t.Errorf("option buttons = %d, want 20 (cap)", totalButtons)
	}
}

// TestBuildChoiceComponents_InputButtonForQuestionsOnly verifies
// the "Type your answer" row is appended iff the choice has at
// least one question.
func TestBuildChoiceComponents_InputButtonForQuestionsOnly(t *testing.T) {
	withQ := &choiceState{
		RequestID: "req-q",
		Choice: &messages.Choice{
			RequestID: "req-q",
			Questions: []messages.ChoiceQuestion{{
				ID: "q1", Question: "what?", Options: messages.ChoiceOptionsFromLabels([]string{"a", "b"}),
			}},
		},
	}
	a := newTestAdapter(&fakeREST{})
	rows := a.buildChoiceComponents(withQ)
	// 1 row of 2 options + 1 row with "Type your answer" button = 2 rows.
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (options + input)", len(rows))
	}
	last := rows[len(rows)-1].Components
	if len(last) != 1 || last[0].Label != "Type your answer" {
		t.Errorf("input button missing; last = %+v", last)
	}

	withoutQ := &choiceState{
		RequestID: "req-p",
		Choice: &messages.Choice{
			RequestID: "req-p",
			Options:   messages.ChoiceOptionsFromLabels([]string{"yes", "no"}),
		},
	}
	rows2 := a.buildChoiceComponents(withoutQ)
	if len(rows2) != 1 {
		t.Errorf("rows = %d, want 1 (no input row for non-question)", len(rows2))
	}
}

// TestShortID_Truncation verifies the "8 + 8 with hyphen" shape
// (mirrors telegram/topic.go:243).
func TestShortID_Truncation(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"short", "short"},
		{"0123456789abcdef", "0123456789abcdef"}, // exactly 16 → unchanged
		{"0123456789abcdefABCDEF12", "01234567-ABCDEF12"},
	}
	for _, tc := range cases {
		if got := shortID(tc.in); got != tc.want {
			t.Errorf("shortID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestChoiceCustomID_StableShape verifies the "c:<short>:<idx>"
// encoding used by every choice button.
func TestChoiceCustomID_StableShape(t *testing.T) {
	state := &choiceState{RequestID: "req-1"}
	got := choiceCustomID(state, 2)
	if got != "c:req-1:2" {
		t.Errorf("choiceCustomID = %q", got)
	}
}

// TestInputCustomID_StableShape verifies the "i:<short>" encoding
// reused as the modal-level CustomID.
func TestInputCustomID_StableShape(t *testing.T) {
	state := &choiceState{RequestID: "req-1"}
	got := inputCustomID(state)
	if got != "i:req-1" {
		t.Errorf("inputCustomID = %q", got)
	}
}

// TestParseChoiceCustomID_ValidAndInvalid verifies the click-time
// decoder accepts well-formed IDs and rejects malformed ones.
func TestParseChoiceCustomID_ValidAndInvalid(t *testing.T) {
	cases := []struct {
		in      string
		wantOK  bool
		wantID  string
		wantIdx int
	}{
		{"c:abc:0", true, "abc", 0},
		{"c:abc:42", true, "abc", 42},
		{"c::0", false, "", 0},
		{"c:abc", false, "", 0},
		{"c:abc:-1", false, "", 0},
		{"i:abc", false, "", 0},
	}
	for _, tc := range cases {
		id, idx, ok := parseChoiceCustomID(tc.in)
		if ok != tc.wantOK {
			t.Errorf("parseChoiceCustomID(%q) ok = %v, want %v", tc.in, ok, tc.wantOK)
			continue
		}
		if ok && (id != tc.wantID || idx != tc.wantIdx) {
			t.Errorf("parseChoiceCustomID(%q) = (%q, %d), want (%q, %d)",
				tc.in, id, idx, tc.wantID, tc.wantIdx)
		}
	}
}

// TestParseInputCustomID_ValidAndInvalid verifies the "i:<short>"
// decoder.
func TestParseInputCustomID_ValidAndInvalid(t *testing.T) {
	cases := []struct {
		in     string
		wantOK bool
		want   string
	}{
		{"i:abc", true, "abc"},
		{"c:abc", false, ""},
		{"i:", false, ""},
		{"i:abc:0", true, "abc:0"}, // SplitN keeps the suffix
	}
	for _, tc := range cases {
		got, ok := parseInputCustomID(tc.in)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("parseInputCustomID(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

// TestCloneChoiceValue_DeepCopy verifies the choice snapshot is
// independent of upstream mutations.
func TestCloneChoiceValue_DeepCopy(t *testing.T) {
	original := &messages.Choice{
		RequestID: "req-1",
		Options:   messages.ChoiceOptionsFromLabels([]string{"yes", "no"}),
		Questions: []messages.ChoiceQuestion{{
			ID: "q1", Options: messages.ChoiceOptionsFromLabels([]string{"a"}),
		}},
	}
	clone := cloneChoiceValue(original)
	original.Options[0].Label = "MUTATED"
	original.Questions[0].Options[0].Label = "MUTATED"
	if clone.Options[0].Label != "yes" {
		t.Errorf("clone.Options[0] = %q, want yes", clone.Options[0].Label)
	}
	if clone.Questions[0].Options[0].Label != "a" {
		t.Errorf("clone.Questions[0].Options[0] = %q, want a", clone.Questions[0].Options[0].Label)
	}
}

// TestRenderChoiceContent_Defaults verifies the title-default
// behaviour (Permission → "Waiting for approval", otherwise
// "Action Needed").
func TestRenderChoiceContent_Defaults(t *testing.T) {
	perm := &choiceState{Choice: &messages.Choice{Kind: messages.ChoiceKindPermission}}
	got := renderChoiceContent(perm)
	if !contains(got, "Waiting for approval") {
		t.Errorf("Permission default = %q", got)
	}
	question := &choiceState{Choice: &messages.Choice{Kind: messages.ChoiceKindQuestion}}
	got = renderChoiceContent(question)
	if !contains(got, "Action Needed") {
		t.Errorf("Question default = %q", got)
	}
}

// contains is a thin wrapper so the test compiles without
// importing strings.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
