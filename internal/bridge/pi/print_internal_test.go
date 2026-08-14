// print_internal_test.go — unit tests for the audit-field
// helper used by the pi print-mode failure paths.
//
// Mirrors claudecode's analogous inline formatting (which is
// inlined in print.go's parsePrintStream; the helper lives
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
// session_id field is omitted when whenSessionID=false (a
// caller that hasn't captured one, or a bridge that doesn't
// surface it) and included when whenSessionID=true. pi
// print-mode passes true here as of F-PI-PRINT-002 — peekPrintMeta
// pulls the session id off the wire's {"type":"session","id":..}
// frame and parsePrintMode threads it into the audit suffix.
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

// TestPeekPrintMeta pins the wire-frame extraction helper that
// feeds the F-PI-PRINT-002 AgentBar fix. Cases:
//
//   - session event with id → SessionID set, Model untouched.
//   - message_start assistant with model → Model set, SessionID
//     untouched.
//   - message_update assistant with model → Model set (real pi
//     also surfaces model on the update delta; first-non-empty
//     still wins).
//   - user-role message_* → no Model set (only assistant counts).
//   - missing id / missing model → no-op (zero values stay).
//   - malformed JSON → silent no-op (translator.translate
//     surfaces JSON errors via the wrapped "pi: translate:" path
//     so peekPrintMeta does not double-report).
//   - pre-populated result.SessionID / result.Model → not
//     overwritten (first-non-empty-wins invariant).
func TestPeekPrintMeta(t *testing.T) {
	type tc struct {
		name string
		line string
		// pre-populated fields on a fresh RunResult before
		// the peek runs; "" means unset.
		preSID   string
		preModel string
		wantSID  string
		wantMod  string
	}
	cases := []tc{
		{
			name:    "session_with_id",
			line:    `{"type":"session","id":"sess-abc","version":3}`,
			wantSID: "sess-abc",
		},
		{
			name: "session_no_id_noop",
			line: `{"type":"session","version":3}`,
		},
		{
			name:    "message_start_assistant",
			line:    `{"type":"message_start","message":{"role":"assistant","model":"sonnet-5","content":[]}}`,
			wantMod: "sonnet-5",
		},
		{
			name:    "message_update_assistant",
			line:    `{"type":"message_update","message":{"role":"assistant","model":"haiku-4"}}`,
			wantMod: "haiku-4",
		},
		{
			name:    "message_end_assistant",
			line:    `{"type":"message_end","message":{"role":"assistant","model":"opus-4"}}`,
			wantMod: "opus-4",
		},
		{
			name: "user_message_no_model",
			line: `{"type":"message_start","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`,
		},
		{
			name: "unknown_event_type",
			line: `{"type":"agent_start"}`,
		},
		{
			name: "malformed_json_silent",
			line: `{"type":`,
		},
		{
			name:    "first_non_empty_wins_session",
			line:    `{"type":"session","id":"sess-second"}`,
			preSID:  "sess-first",
			wantSID: "sess-first",
		},
		{
			name:     "first_non_empty_wins_model",
			line:     `{"type":"message_start","message":{"role":"assistant","model":"second-model"}}`,
			preModel: "first-model",
			wantMod:  "first-model",
		},
		{
			name:    "session_id_picked_up_when_pre_empty",
			line:    `{"type":"session","id":"sess-recovered"}`,
			wantSID: "sess-recovered",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := agent.RunResult{SessionID: c.preSID, Model: c.preModel}
			peekPrintMeta([]byte(c.line), &res)
			if res.SessionID != c.wantSID {
				t.Errorf("SessionID = %q, want %q", res.SessionID, c.wantSID)
			}
			if res.Model != c.wantMod {
				t.Errorf("Model = %q, want %q", res.Model, c.wantMod)
			}
		})
	}
}
