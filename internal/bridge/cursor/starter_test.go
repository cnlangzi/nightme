// Package cursor — Starter tests.
//
// Unit-level tests for the Starter. Real-binary e2e lives in
// separate test files (build-tagged if needed).
package cursor

import (
	"os/exec"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestStarter_Info verifies the Starter's Info() exposes the
// correct ModeACP + the canonical "acp" arg.
func TestStarter_Info(t *testing.T) {
	s := NewStarter("cursor", "agent", []string{"acp"})
	info := s.Info()
	if info.Name != "cursor" {
		t.Errorf("Info().Name = %q, want cursor", info.Name)
	}
	if info.Mode != agent.ModeACP {
		t.Errorf("Info().Mode = %v, want agent.ModeACP", info.Mode)
	}
	if info.Command != "agent" {
		t.Errorf("Info().Command = %q, want agent", info.Command)
	}
	if len(info.Args) != 1 || info.Args[0] != "acp" {
		t.Errorf("Info().Args = %v, want [acp]", info.Args)
	}
}

// TestStarter_Info_DefensiveCopy verifies mutating the caller's
// args slice after NewStarter does not affect the Starter
// (defensive copy invariant shared with all bridges).
func TestStarter_Info_DefensiveCopy(t *testing.T) {
	args := []string{"acp"}
	s := NewStarter("cursor", "agent", args)
	args[0] = "MUTATED"
	info := s.Info()
	if info.Args[0] != "acp" {
		t.Errorf("Info().Args[0] = %q, want acp (defensive copy failed)", info.Args[0])
	}
}

// TestStarter_Detect_BinaryNotOnPath verifies Detect returns
// an error for a non-existent binary.
func TestStarter_Detect_BinaryNotOnPath(t *testing.T) {
	s := NewStarter("cursor", "cursor-does-not-exist-xyz-12345", nil)
	if err := s.Detect(); err == nil {
		t.Errorf("Detect() error = nil, want non-nil for missing binary")
	}
}

// TestStarter_Detect_PassForRealBinary is skipped unless the
// real `agent` binary is on PATH. Allows manual verification
// that Detect works against a real Cursor CLI install.
func TestStarter_Detect_PassForRealBinary(t *testing.T) {
	if _, err := exec.LookPath("agent"); err != nil {
		t.Skipf("agent binary not on PATH: %v", err)
	}
	s := NewStarter("cursor", "agent", nil)
	if err := s.Detect(); err != nil {
		t.Errorf("Detect() error = %v, want nil", err)
	}
}
