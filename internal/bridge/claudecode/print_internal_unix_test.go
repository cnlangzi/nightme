// print_internal_test.go — unit tests for the audit-field
// helper used by the claudecode print-mode is_error path.
//
// Mirrors pi's print_internal_test.go so the cross-bridge
// error format stays symmetric and grep-friendly across both
// bridges.

//go:build !windows

package claudecode

import (
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestAppendAuditFields_EmptyResult pins the no-data fast path.
func TestAppendAuditFields_EmptyResult(t *testing.T) {
	got := appendAuditFields(agent.RunResult{})
	if got != "" {
		t.Errorf("empty RunResult: got %q, want \"\"", got)
	}
}

// TestAppendAuditFields_OnlySessionID pins the session-only
// branch. claudecode captures session_id from system/init and
// surfaces it on every Result, so a usage-free result with a
// session_id produces a single [session_id=X] token.
func TestAppendAuditFields_OnlySessionID(t *testing.T) {
	got := appendAuditFields(agent.RunResult{SessionID: "sess-abc"})
	want := " [session_id=sess-abc]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if strings.Contains(got, "usage") {
		t.Errorf("got unexpected usage: %q", got)
	}
}

// TestAppendAuditFields_OnlyUsage pins the no-session usage
// branch — covers the (rare) case where the result event
// arrived without a preceding system/init.
func TestAppendAuditFields_OnlyUsage(t *testing.T) {
	got := appendAuditFields(agent.RunResult{
		Usage: &agent.UsageInfo{InputTokens: 1234, OutputTokens: 56, CacheReadInputTokens: 128},
	})
	want := " [usage in=1234 out=56 cache_read=128]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if strings.Contains(got, "session_id") {
		t.Errorf("got unexpected session_id: %q", got)
	}
}

// TestAppendAuditFields_SessionAndUsage pins that both tokens
// emit, in session-id → usage order, so the format is grep-
// stable across runs and bridges.
func TestAppendAuditFields_SessionAndUsage(t *testing.T) {
	got := appendAuditFields(agent.RunResult{
		SessionID: "sess-123",
		Usage:     &agent.UsageInfo{InputTokens: 1, OutputTokens: 2, CacheReadInputTokens: 3},
	})
	want := " [session_id=sess-123] [usage in=1 out=2 cache_read=3]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestAppendAuditFields_NilUsageDoesNotPanic pins the
// nil-safety contract: nil Usage means no usage token (rather
// than "[usage in=0 out=0 cache_read=0]"), so a usage-less
// run doesn't surface spurious zeros in operator grep output.
func TestAppendAuditFields_NilUsageDoesNotPanic(t *testing.T) {
	got := appendAuditFields(agent.RunResult{SessionID: "sess-x", Usage: nil})
	if !strings.Contains(got, "session_id=sess-x") {
		t.Errorf("session_id dropped when usage is nil: %q", got)
	}
	if strings.Contains(got, "usage") {
		t.Errorf("nil usage should not emit usage token: %q", got)
	}
}