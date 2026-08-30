// Package copilot — Starter tests.
//
// Unit-level tests for the Starter. Real-binary e2e lives in
// separate test files (build-tagged if needed).
//
// Mirrors cursor/starter_test.go: same defensive-copy /
// detect-missing-binary / Info-correctness checks, with the
// argv-ordering test rewritten for copilot's flag shape
// (`--acp --stdio` are flat flags, not a subcommand — so
// order doesn't matter, but `--allow-all-tools` MUST be
// before any prompt).
package copilot

import (
	"os/exec"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestStarter_Info verifies the Starter's Info() exposes the
// correct ModeACP + the canonical DefaultACPArgs (full-access
// parent flags then --acp --stdio).
func TestStarter_Info(t *testing.T) {
	s := NewStarter("copilot", "copilot", DefaultACPArgs)
	info := s.Info()
	if info.Name != "copilot" {
		t.Errorf("Info().Name = %q, want copilot", info.Name)
	}
	if info.Mode != agent.ModeACP {
		t.Errorf("Info().Mode = %v, want agent.ModeACP", info.Mode)
	}
	if info.Command != "copilot" {
		t.Errorf("Info().Command = %q, want copilot", info.Command)
	}
	if !equalStrings(info.Args, DefaultACPArgs) {
		t.Errorf("Info().Args = %v, want %v", info.Args, DefaultACPArgs)
	}
}

// TestDefaultACPArgs_AllowAllBeforeACP locks the canonical
// argv shape Copilot expects: `--allow-all-tools` MUST be
// before `--acp --stdio`. While Copilot's `--acp --stdio` are
// flat flags (not a subcommand, unlike cursor's `acp`), the
// `--allow-all-tools` parent-level flag still has to be
// present for the IM session to act without per-tool approval
// prompts. Verified against `copilot --help` on v0.0.361 +
// docs.github.com/en/copilot/concepts/agents/copilot-cli/
// autopilot for current versions.
func TestDefaultACPArgs_AllowAllBeforeACP(t *testing.T) {
	if len(DefaultACPArgs) < 3 {
		t.Fatalf("DefaultACPArgs too short: %v", DefaultACPArgs)
	}
	// `--acp` and `--stdio` are the trailing two flags.
	last2 := DefaultACPArgs[len(DefaultACPArgs)-2:]
	if last2[0] != "--acp" || last2[1] != "--stdio" {
		t.Errorf("DefaultACPArgs trailing = %v, want [--acp --stdio]", last2)
	}
	// `--allow-all-tools` is the leading flag (FullAccessArgs).
	if DefaultACPArgs[0] != "--allow-all-tools" {
		t.Errorf("DefaultACPArgs[0] = %q, want --allow-all-tools (FullAccessArgs first)", DefaultACPArgs[0])
	}
	want := []string{"--allow-all-tools", "--acp", "--stdio"}
	if !equalStrings(DefaultACPArgs, want) {
		t.Errorf("DefaultACPArgs = %v, want %v", DefaultACPArgs, want)
	}
	if !equalStrings(FullAccessArgs, want[:1]) {
		t.Errorf("FullAccessArgs = %v, want %v", FullAccessArgs, want[:1])
	}
}

// TestPrintModeArgs_PrependsFullAccess verifies print-mode
// uses the same parent flag, still before `-p`. `-s` /
// `--silent` is appended after `-p` to suppress the post-answer
// stats decoration ("Changes / AI Credits / Tokens / Resume")
// so stdout is just the agent's final text (verified on
// copilot 1.0.81 — without `-s` the captured stdout mixes
// the answer with metadata).
func TestPrintModeArgs_PrependsFullAccess(t *testing.T) {
	got := printModeArgs("hello", nil)
	want := []string{"--allow-all-tools", "-p", "hello", "-s"}
	if !equalStrings(got, want) {
		t.Errorf("printModeArgs = %v, want %v", got, want)
	}
	extra := printModeArgs("hello", []string{"--model", "auto"})
	if extra[len(extra)-2] != "--model" || extra[len(extra)-1] != "auto" {
		t.Errorf("printModeArgs extra not appended: %v", extra)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestStarter_Info_DefensiveCopy verifies mutating the caller's
// args slice after NewStarter does not affect the Starter
// (defensive copy invariant shared with all bridges).
func TestStarter_Info_DefensiveCopy(t *testing.T) {
	args := []string{"--acp", "--stdio"}
	s := NewStarter("copilot", "copilot", args)
	args[0] = "MUTATED"
	info := s.Info()
	if info.Args[0] != "--acp" {
		t.Errorf("Info().Args[0] = %q, want --acp (defensive copy failed)", info.Args[0])
	}
}

// TestStarter_Detect_BinaryNotOnPath verifies Detect returns
// an error for a non-existent binary.
func TestStarter_Detect_BinaryNotOnPath(t *testing.T) {
	s := NewStarter("copilot", "copilot-does-not-exist-xyz-12345", nil)
	if err := s.Detect(); err == nil {
		t.Errorf("Detect() error = nil, want non-nil for missing binary")
	}
}

// TestStarter_Detect_PassForRealBinary is skipped unless the
// real `copilot` binary is on PATH. Allows manual verification
// that Detect works against a real Copilot CLI install (the
// official installer drops `copilot` on PATH on every
// platform — npm/npm-wrapper / brew / winget).
//
// Note: ACP support requires Copilot CLI >= 1.0.x — preview
// builds (e.g. 0.0.361) ship the `copilot` binary but reject
// `--acp` with "unknown option". The runtime Start path will
// surface that as a spawn error; this test only verifies
// Detect (binary resolves on PATH), which is satisfied by any
// version.
func TestStarter_Detect_PassForRealBinary(t *testing.T) {
	if _, err := exec.LookPath("copilot"); err != nil {
		t.Skipf("copilot binary not on PATH: %v", err)
	}
	s := NewStarter("copilot", "copilot", nil)
	if err := s.Detect(); err != nil {
		t.Errorf("Detect() error = %v, want nil", err)
	}
}