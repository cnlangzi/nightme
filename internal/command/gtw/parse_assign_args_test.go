package gtw

// Parser tests for `/gtw assign <issue-id> [--remove|-r]`.
//
// The command names exactly one issue and takes one boolean flag.
// The shared lexer (command.ParseCmdArgs) owns the arity and the
// unknown-flag rejection; parseIssueID owns the id's shape.

import (
	"strings"
	"testing"
)

// assertAssignParseErr fails the test unless err is non-nil, its
// message contains substr, and it carries the spec's Usage tail —
// every parse error tells the user what the command accepts.
func assertAssignParseErr(t *testing.T, err error, substr string) string {
	t.Helper()
	if err == nil {
		t.Fatalf("expected parse error containing %q, got nil", substr)
	}
	msg := err.Error()
	if !strings.Contains(msg, substr) {
		t.Errorf("error %q lacks %q", msg, substr)
	}
	if !strings.Contains(msg, assignCmdSpec.Usage) {
		t.Errorf("error %q does not carry the spec Usage tail", msg)
	}
	return msg
}

// TestParseAssignArgs_IssueID pins the accepted id shapes: digits
// only, an optional leading `#`, and never 0.
func TestParseAssignArgs_IssueID(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want int
	}{
		{"plain id", []string{"42"}, 42},
		{"hash id", []string{"#42"}, 42},
		{"single digit", []string{"7"}, 7},
		{"flag after the id", []string{"42", "--remove"}, 42},
		{"flag before the id", []string{"--remove", "42"}, 42},
		{"hash id with the flag", []string{"#42", "-r"}, 42},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseAssignArgs(tc.in)
			if err != nil {
				t.Fatalf("parseAssignArgs(%v) error: %v", tc.in, err)
			}
			if got.IssueID != tc.want {
				t.Errorf("IssueID = %d, want %d", got.IssueID, tc.want)
			}
		})
	}
}

// TestParseAssignArgs_RemoveFlag pins that `--remove` and `-r` are
// the same boolean, and that the flag defaults to off.
func TestParseAssignArgs_RemoveFlag(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want bool
	}{
		{"no flag", []string{"42"}, false},
		{"long form", []string{"42", "--remove"}, true},
		{"short form", []string{"42", "-r"}, true},
		{"both spellings", []string{"42", "-r", "--remove"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseAssignArgs(tc.in)
			if err != nil {
				t.Fatalf("parseAssignArgs(%v) error: %v", tc.in, err)
			}
			if got.Remove != tc.want {
				t.Errorf("Remove = %v, want %v", got.Remove, tc.want)
			}
		})
	}
}

// TestParseAssignArgs_BadIssueID pins the rejected id shapes: zero,
// non-digits, and an empty id.
func TestParseAssignArgs_BadIssueID(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"zero", []string{"0"}, "cannot be 0"},
		{"letters", []string{"abc"}, "digits only"},
		{"mixed", []string{"4a2"}, "digits only"},
		{"negative", []string{"-1"}, "unknown flag"},
		{"blank", []string{" "}, "empty issue id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseAssignArgs(tc.in)
			if err == nil {
				t.Fatalf("parseAssignArgs(%v) = %+v, want an error", tc.in, got)
			}
			assertAssignParseErr(t, err, tc.want)
		})
	}
}

// TestParseAssignArgs_MissingIssueID pins that the command needs
// exactly one issue id.
func TestParseAssignArgs_MissingIssueID(t *testing.T) {
	cases := [][]string{
		nil,
		{},
		{"--remove"},
		{"-r"},
	}
	for _, in := range cases {
		t.Run(strings.Join(in, " "), func(t *testing.T) {
			got, err := parseAssignArgs(in)
			if err == nil {
				t.Fatalf("parseAssignArgs(%v) = %+v, want an error", in, got)
			}
			assertAssignParseErr(t, err, "needs 1 argument")
		})
	}
}

// TestParseAssignArgs_ExtraPositionalsRejected pins that a second
// issue id is a hard error, not a silently dropped token.
func TestParseAssignArgs_ExtraPositionalsRejected(t *testing.T) {
	cases := [][]string{
		{"1", "2"},
		{"42", "43", "--remove"},
	}
	for _, in := range cases {
		t.Run(strings.Join(in, " "), func(t *testing.T) {
			got, err := parseAssignArgs(in)
			if err == nil {
				t.Fatalf("parseAssignArgs(%v) = %+v, want an error", in, got)
			}
			assertAssignParseErr(t, err, "too many arguments")
		})
	}
}

// TestParseAssignArgs_UnknownFlagRejected pins the CLI-consistent
// behaviour: a flag the command does not declare is rejected, never
// silently ignored.
func TestParseAssignArgs_UnknownFlagRejected(t *testing.T) {
	cases := [][]string{
		{"42", "--dry-run"},
		{"--dry-run", "42"},
		{"42", "-x"},
		{"42", "--reset"},
	}
	for _, in := range cases {
		t.Run(strings.Join(in, " "), func(t *testing.T) {
			got, err := parseAssignArgs(in)
			if err == nil {
				t.Fatalf("parseAssignArgs(%v) = %+v, want an error", in, got)
			}
			assertAssignParseErr(t, err, "unknown flag")
		})
	}
}
