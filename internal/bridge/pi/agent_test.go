// Tests for the spawn argv builder. Mirrors
// internal/bridge/claudecode/claudecode_test.go in spirit: pure
// argv shaping, no child process. The same builder is used in
// production by Agent.Start so the contract here is the same one
// the runtime exercises.

package pi

import (
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestBuildArgs_Defaults verifies the baseline argv the runtime
// receives when no extra args / resume id are configured.
func TestBuildArgs_Defaults(t *testing.T) {
	got := buildArgs(nil, agent.StartConfig{})
	want := []string{"--mode", "rpc"}
	if !equalSlice(got, want) {
		t.Errorf("buildArgs(nil, {}) = %v, want %v", got, want)
	}
}

// TestBuildArgs_PassesExtraArgsThroughInOrder verifies the
// ordering invariant: DefaultArgs → extraArgs → cfg.Args → optional
// resume flag. Anything user-supplied before the resume flag
// remains grep-visible — same convention as the claudecode bridge.
func TestBuildArgs_PassesExtraArgsThroughInOrder(t *testing.T) {
	got := buildArgs(
		[]string{"--no-builtin-tools"},
		agent.StartConfig{Args: []string{"--model", "anthropic/claude-sonnet"}},
	)
	want := []string{
		"--mode", "rpc",
		"--no-builtin-tools",
		"--model", "anthropic/claude-sonnet",
	}
	if !equalSlice(got, want) {
		t.Errorf("buildArgs = %v, want %v", got, want)
	}
}

// TestBuildArgs_OmitsSessionIDWhenResumeIDEmpty verifies that a
// zero-value ResumeID does NOT inject the resume flag (so
// daemons with no persisted entry spawn fresh sessions cleanly).
func TestBuildArgs_OmitsSessionIDWhenResumeIDEmpty(t *testing.T) {
	got := buildArgs(nil, agent.StartConfig{})
	for i, a := range got {
		if a == "--session-id" {
			t.Errorf("argv at [%d] = %q, want no --session-id when ResumeID is empty: %v", i, a, got)
		}
	}
}

// TestBuildArgs_AppendsSessionIDWhenResumeIDSet verifies P1: the
// resume contract surfaces Pi's native `--session-id <id>` CLI
// flag at the tail of the argv. The flag value is forwarded
// verbatim — Pi's own validator decides whether the id is legal.
func TestBuildArgs_AppendsSessionIDWhenResumeIDSet(t *testing.T) {
	got := buildArgs(nil, agent.StartConfig{ResumeID: "sess-abc-123"})
	want := []string{"--mode", "rpc", "--session-id", "sess-abc-123"}
	if !equalSlice(got, want) {
		t.Errorf("buildArgs with ResumeID = %v, want %v", got, want)
	}
}

// TestBuildArgs_ResumeFlagGoesLastAfterUserArgs verifies the
// ordering rationale: the resume flag is appended AFTER cfg.Args
// so a user-supplied --model / --provider override remains
// visible to a human `ps`-grepping the process tree.
func TestBuildArgs_ResumeFlagGoesLastAfterUserArgs(t *testing.T) {
	got := buildArgs(
		nil,
		agent.StartConfig{
			Args:     []string{"--model", "google/gemini"},
			ResumeID: "sess-xyz",
		},
	)
	want := []string{
		"--mode", "rpc",
		"--model", "google/gemini",
		"--session-id", "sess-xyz",
	}
	if !equalSlice(got, want) {
		t.Errorf("buildArgs = %v, want %v", got, want)
	}
	// Sanity: --session-id is the SECOND-TO-LAST element (last is
	// the id itself); nothing sits after the id.
	if got[len(got)-2] != "--session-id" {
		t.Errorf("argv[-2] = %q, want --session-id", got[len(got)-2])
	}
	if got[len(got)-1] != "sess-xyz" {
		t.Errorf("argv[-1] = %q, want sess-xyz", got[len(got)-1])
	}
}

func equalSlice(a, b []string) bool {
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
