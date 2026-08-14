// print_internal_test.go — unit tests for the audit-field
// helper used by the pi print-mode failure paths.
//
// Mirrors claudecode's analogous inline formatting (which is
// inlined in print.go's streamPrintEvents; the helper lives
// here because pi uses an auditFields-style suffix rather
// than interleaving the bits into a strings.Builder call).

//go:build !windows

package pi

import (
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestAppendAuditFields_EmptyResult pins the no-data fast path:
// when result has nothing to report the helper returns "" so
// the error message stays clean (no spurious "[]" suffix).
func TestAppendAuditFields_EmptyResult(t *testing.T) {
	got := appendAuditFields(agent.RunResult{}, false)
	if got != "" {
		t.Errorf("empty RunResult: got %q, want \"\"", got)
	}
}

// TestAppendAuditFields_OnlySubtype pins that a non-empty
// Subtype produces a single [subtype=X] token, no usage
// noise, no session_id (because whenSessionID=false is the
// default for pi print-mode).
func TestAppendAuditFields_OnlySubtype(t *testing.T) {
	got := appendAuditFields(agent.RunResult{Subtype: "stop"}, false)
	want := " [subtype=stop]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if strings.Contains(got, "session_id") {
		t.Errorf("got unexpected session_id: %q", got)
	}
	if strings.Contains(got, "usage") {
		t.Errorf("got unexpected usage: %q", got)
	}
}

// TestAppendAuditFields_OnlyUsage pins the no-subtype usage
// branch: a non-nil Usage produces [usage in=N out=N cache_read=N]
// with no other tokens.
func TestAppendAuditFields_OnlyUsage(t *testing.T) {
	got := appendAuditFields(agent.RunResult{
		Usage: &agent.UsageInfo{InputTokens: 1234, OutputTokens: 56, CacheReadInputTokens: 128},
	}, false)
	want := " [usage in=1234 out=56 cache_read=128]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestAppendAuditFields_SubtypeAndUsage pins that both
// tokens emit, in the order subtype → usage (matches the
// appender code and gives grep-friendly fixed format).
func TestAppendAuditFields_SubtypeAndUsage(t *testing.T) {
	got := appendAuditFields(agent.RunResult{
		Subtype: "error_max_turns",
		Usage:   &agent.UsageInfo{InputTokens: 1, OutputTokens: 2, CacheReadInputTokens: 3},
	}, false)
	want := " [subtype=error_max_turns] [usage in=1 out=2 cache_read=3]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestAppendAuditFields_SessionIDGated pins that the
// session_id field is omitted when whenSessionID=false (the
// default for pi print-mode which doesn't surface SessionID)
// and included when whenSessionID=true (a future bridge that
// does capture it, or for parity with claudecode).
func TestAppendAuditFields_SessionIDGated(t *testing.T) {
	res := agent.RunResult{SessionID: "sess-123", Subtype: "stop"}

	if got := appendAuditFields(res, false); strings.Contains(got, "session_id") {
		t.Errorf("whenSessionID=false should omit session_id; got %q", got)
	}
	wantWith := " [session_id=sess-123] [subtype=stop]"
	if got := appendAuditFields(res, true); got != wantWith {
		t.Errorf("whenSessionID=true: got %q, want %q", got, wantWith)
	}
}

// TestAppendAuditFields_NilUsageDoesNotPanic pins the
// nil-safety contract: a non-nil but empty Usage struct is
// still emitted (the "[usage in=0 out=0 cache_read=0]" token
// is unambiguous on the no-data case, and lets operators grep
// for "usage" regardless of whether the run consumed tokens).
func TestAppendAuditFields_NilUsageDoesNotPanic(t *testing.T) {
	got := appendAuditFields(agent.RunResult{Subtype: "stop", Usage: nil}, false)
	if !strings.Contains(got, "subtype=stop") {
		t.Errorf("subtype dropped when usage is nil: %q", got)
	}
	if strings.Contains(got, "usage") {
		t.Errorf("nil usage should not emit usage token: %q", got)
	}
}