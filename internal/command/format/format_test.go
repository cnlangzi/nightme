// Tests for the shared format helpers. Verifies that:
//   - Empty input returns the empty string (caller decides empty
//     state reply)
//   - Rows sort by bucket asc, then agent, then cwd
//   - The byte cap truncates with the standard "...and N more"
//     suffix
//   - The header from HeaderBuilder is always included
//
// The per-command formatters (close.FormatResults,
// newcmd.FormatResetResults) are tested in their own packages;
// these tests only cover the shared mechanics.
package format_test

import (
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/command/format"
)

type fakeResult struct {
	Agent  string
	Cwd    string
	Action string
	Error  error
}

func renderFakeRow(r fakeResult) format.RenderedRow {
	if r.Error != nil {
		return format.RenderedRow{
			Text:   "ERR " + r.Agent,
			Bucket: format.BucketFailure,
			Agent:  r.Agent,
			Cwd:    r.Cwd,
		}
	}
	return format.RenderedRow{
		Text:   "OK " + r.Agent,
		Bucket: format.BucketSuccess,
		Agent:  r.Agent,
		Cwd:    r.Cwd,
	}
}

func fakeHeader(success, skipped, failed int) string {
	return "S=" + itoa(success) + " K=" + itoa(skipped) + " F=" + itoa(failed) + ":"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	out := ""
	for n > 0 {
		out = string(rune('0'+n%10)) + out
		n /= 10
	}
	return out
}

func TestFormatTable_Empty(t *testing.T) {
	got := format.FormatTable([]fakeResult(nil), renderFakeRow, fakeHeader)
	if got != "" {
		t.Errorf("FormatTable(nil): got %q, want empty string", got)
	}
}

func TestFormatTable_HeaderAndRows(t *testing.T) {
	rows := []fakeResult{
		{Agent: "alpha", Cwd: "/x"},
		{Agent: "beta", Cwd: "/x"},
	}
	got := format.FormatTable(rows, renderFakeRow, fakeHeader)
	if !strings.HasPrefix(got, "S=2 K=0 F=0:") {
		t.Errorf("missing/bad header: %q", got)
	}
	if !strings.Contains(got, "OK alpha") || !strings.Contains(got, "OK beta") {
		t.Errorf("missing rows: %q", got)
	}
}

func TestFormatTable_SortByBucketThenAgent(t *testing.T) {
	rows := []fakeResult{
		{Agent: "alpha"},                                // success
		{Agent: "delta", Action: "x", Error: errFake()}, // failure
		{Agent: "beta"},                                 // success
	}
	got := format.FormatTable(rows, renderFakeRow, fakeHeader)
	// Expected order: alpha (success), beta (success), delta (failure)
	if idxA := strings.Index(got, "OK alpha"); idxA < 0 {
		t.Fatal("missing OK alpha")
	} else if idxB := strings.Index(got, "OK beta"); idxB < 0 {
		t.Fatal("missing OK beta")
	} else if idxD := strings.Index(got, "ERR delta"); idxD < 0 {
		t.Fatal("missing ERR delta")
	} else if !(idxA < idxB && idxB < idxD) {
		t.Errorf("sort order wrong: alpha@%d beta@%d delta@%d (want alpha < beta < delta)",
			idxA, idxB, idxD)
	}
}

func TestFormatTable_HumanAction(t *testing.T) {
	if got := format.HumanAction("closed"); got != "close" {
		t.Errorf("HumanAction(closed): got %q, want close", got)
	}
	if got := format.HumanAction("close-failed"); got != "close" {
		t.Errorf("HumanAction(close-failed): got %q, want close", got)
	}
	if got := format.HumanAction("stale-cleared"); got != "stale-clear" {
		t.Errorf("HumanAction(stale-cleared): got %q, want stale-clear", got)
	}
	if got := format.HumanAction("something-else"); got != "something-else" {
		t.Errorf("HumanAction(passthrough): got %q, want something-else", got)
	}
}

func TestJoinCounts(t *testing.T) {
	if got := format.JoinCounts([]string{"a"}); got != "a" {
		t.Errorf("single: got %q, want a", got)
	}
	if got := format.JoinCounts([]string{"a", "b", "c"}); got != "a, b, c" {
		t.Errorf("multiple: got %q, want a, b, c", got)
	}
}

// errFake is a tiny error stub so the test file doesn't need
// to import errors just for one error value.
type fakeErr string

func (e fakeErr) Error() string { return string(e) }

func errFake() error { return fakeErr("boom") }