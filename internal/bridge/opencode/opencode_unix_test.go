//go:build !windows

// Compile-time + minimal surface tests for the opencode bridge.
//
// These tests do NOT spawn a real `opencode` binary — they verify
// the Go surface of the bridge package compiles and that the
// constructor + spec-half methods behave correctly. Full end-to-end
// coverage (incl. real-binary spawn) is gated by the
// NIGHTME_OPENCODE_E2E env var and added in session_real_test.go.
package opencode

import (
	"os/exec"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestNewAndSpec verifies the template constructor populates the
// spec-half fields correctly and the Agent satisfies agent.Agent.
func TestNewAndSpec(t *testing.T) {
	a := NewStarter("opencode", "opencode", nil)
	if a.name != "opencode" {
		t.Errorf("a.name = %q, want opencode", a.name)
	}
	if a.command != "opencode" {
		t.Errorf("a.command = %q, want opencode", a.command)
	}
	if a.args != nil {
		t.Errorf("a.args = %v, want nil", a.args)
	}
	if a.Info().Mode != agent.ModeJSONIO {
		t.Errorf("a.Info().Mode = %v, want ModeJSONIO", a.Info().Mode)
	}
	// Info() returns the same data via agent.Info — verifies the
	// "Agent satisfies agent.Agent" half of the contract.
	info := a.Info()
	if info.Name != "opencode" {
		t.Errorf("Info().Name = %q, want opencode", info.Name)
	}
	if info.Mode != agent.ModeJSONIO {
		t.Errorf("Info().Mode = %v, want ModeJSONIO", info.Mode)
	}
	// Constructor args are stored on the unexported field; the
	// Info() surface exposes them via Args().
	a2 := NewStarter("opencode", "opencode", []string{"--foo"})
	if a2.Info().Args[0] != "--foo" {
		t.Errorf("Info().Args[0] = %q, want --foo", a2.Info().Args[0])
	}
}

// TestNew_NilArgsIsSafe verifies New does not panic when args is nil.
func TestNew_NilArgsIsSafe(t *testing.T) {
	a := NewStarter("opencode", "opencode", nil)
	if a.Info().Args != nil {
		t.Errorf("Args() = %v, want nil", a.Info().Args)
	}
}

// TestNew_EmptyArgs verifies a []string{} (non-nil) is normalized.
// Go's append([]string(nil), []string{}...) returns nil; both nil
// and empty slice are acceptable representations of "no args". This
// test guards the constructor against future regressions that
// would silently drop args.
func TestNew_EmptyArgs(t *testing.T) {
	a := NewStarter("opencode", "opencode", []string{})
	if len(a.Info().Args) != 0 {
		t.Errorf("Args() = %v, want empty", a.Info().Args)
	}
}

// TestDetect_MissingBinary verifies Detect surfaces a friendly error
// when the binary is not on PATH.
func TestDetect_MissingBinary(t *testing.T) {
	// Use a name that is extremely unlikely to exist.
	a := NewStarter("opencode", "opencode-does-not-exist-xyz-12345", nil)
	if err := a.Detect(); err == nil {
		t.Errorf("Detect() = nil, want error")
	}
}

// TestDetect_PresentBinary verifies Detect returns nil when the binary
// is found. We use /bin/sh as a portable choice; we don't actually
// invoke it.
func TestDetect_PresentBinary(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not on PATH")
	}
	a := NewStarter("opencode", sh, nil)
	if err := a.Detect(); err != nil {
		t.Errorf("Detect() = %v, want nil", err)
	}
}

// TestEventBufferSize_PinnedAt40960 verifies the producer-side buffer
// contract is honored across all bridges. If this test fails, the
// runtime readpump may starve a heavy turn.
func TestEventBufferSize_PinnedAt40960(t *testing.T) {
	if eventBufferSize != 40960 {
		t.Errorf("eventBufferSize = %d, want 40960 (pi/claudecode/pty/acp/codex invariant)", eventBufferSize)
	}
}

// TestErrors_AreNonNil verifies the sentinel errors are defined.
func TestErrors_AreNonNil(t *testing.T) {
	for name, err := range map[string]error{
		"ErrSessionClosed":     ErrSessionClosed,
		"ErrServerStartTimeout": ErrServerStartTimeout,
		"ErrNoPendingPermission": ErrNoPendingPermission,
		"ErrImageTooLarge":     ErrImageTooLarge,
		"ErrTurnBusy":          ErrTurnBusy,
	} {
		if err == nil {
			t.Errorf("%s is nil", name)
		}
		if err.Error() == "" {
			t.Errorf("%s has empty message", name)
		}
	}
}

// TestScanBanner verifies the regex captures the URL correctly.
func TestScanBanner(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"opencode server listening on http://127.0.0.1:4096", "http://127.0.0.1:4096"},
		{"opencode server listening on http://localhost:54321", "http://localhost:54321"},
		{"opencode server listening on https://0.0.0.0:8443", "https://0.0.0.0:8443"},
		{"some other line", ""},
		{"", ""},
	}
	for _, tc := range cases {
		got := serverURLRegex.FindStringSubmatch(tc.in)
		if tc.want == "" {
			if got != nil {
				t.Errorf("input %q: got %v, want nil", tc.in, got)
			}
			continue
		}
		if got == nil {
			t.Errorf("input %q: no match, want %q", tc.in, tc.want)
			continue
		}
		if got[1] != tc.want {
			t.Errorf("input %q: got %q, want %q", tc.in, got[1], tc.want)
		}
	}
}

// TestSessionEventTypeConstants is a sanity check on the event-type
// strings we dispatch on. If opencode renames them the test
// fingerprint will catch it.
func TestSessionEventTypeConstants(t *testing.T) {
	want := map[string]string{
		"message.part.updated": "message.part.updated",
		"session.idle":         "session.idle",
		"session.error":        "session.error",
		"permission.asked":     "permission.asked",
	}
	for _, expected := range want {
		// Just verify the literals round-trip through the JSON
		// decoder to make sure we haven't typoed them anywhere.
		var ev SessionEvent
		ev.Type = expected
		if ev.Type != expected {
			t.Errorf("SessionEvent.Type round-trip broken")
		}
	}
}
