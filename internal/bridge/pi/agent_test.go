
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
// zero-value SessionID does NOT inject the resume flag (so
// daemons with no persisted entry spawn fresh sessions cleanly).
func TestBuildArgs_OmitsSessionIDWhenResumeIDEmpty(t *testing.T) {
	got := buildArgs(nil, agent.StartConfig{})
	for i, a := range got {
		if a == "--session-id" {
			t.Errorf("argv at [%d] = %q, want no --session-id when SessionID is empty: %v", i, a, got)
		}
	}
}

// TestBuildArgs_AppendsSessionIDWhenResumeIDSet verifies P1: the
// resume contract surfaces Pi's native `--session-id <id>` CLI
// flag at the tail of the argv. The flag value is forwarded
// verbatim — Pi's own validator decides whether the id is legal.
func TestBuildArgs_AppendsSessionIDWhenResumeIDSet(t *testing.T) {
	got := buildArgs(nil, agent.StartConfig{SessionID: "sess-abc-123"})
	want := []string{"--mode", "rpc", "--session-id", "sess-abc-123"}
	if !equalSlice(got, want) {
		t.Errorf("buildArgs with SessionID = %v, want %v", got, want)
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
			SessionID: "sess-xyz",
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

// ----- filterSessionFlags (Finding 3 — config Args collision) -----

// TestFilterSessionFlags_ResumeIDWins_StripsValueArg verifies that
// when the runtime-persisted SessionID is set, a user-supplied
// --session-id <value> pair in cfg.Args is stripped —
// runtime-persisted identity wins so Pi never sees conflicting
// session-selection flags.
func TestFilterSessionFlags_ResumeIDWins_StripsValueArg(t *testing.T) {
	got := filterSessionFlags(
		[]string{"--session-id", "user-supplied-id", "--model", "anthropic/claude"},
		"runtime-resume-id",
		nil,
	)
	want := []string{"--model", "anthropic/claude"}
	if !equalSlice(got, want) {
		t.Errorf("filterSessionFlags = %v, want %v", got, want)
	}
}

// TestFilterSessionFlags_ResumeIDWins_StripsSessionPath covers
// the equal-looking --session <path|id> flag. Same treatment as
// --session-id when SessionID is set.
func TestFilterSessionFlags_ResumeIDWins_StripsSessionPath(t *testing.T) {
	got := filterSessionFlags(
		[]string{"--session", "/tmp/old.jsonl", "--verbose"},
		"resume-x",
		nil,
	)
	want := []string{"--verbose"}
	if !equalSlice(got, want) {
		t.Errorf("filterSessionFlags = %v, want %v", got, want)
	}
}

// TestFilterSessionFlags_ResumeIDWins_StripsNoSession covers
// --no-session (boolean, no value arg). Stripped without
// consuming the next element.
func TestFilterSessionFlags_ResumeIDWins_StripsNoSession(t *testing.T) {
	got := filterSessionFlags(
		[]string{"--no-session", "--model", "x"},
		"resume-y",
		nil,
	)
	want := []string{"--model", "x"}
	if !equalSlice(got, want) {
		t.Errorf("filterSessionFlags = %v, want %v", got, want)
	}
}

// TestFilterSessionFlags_NoResume_PassesThrough is the legitimate
// "spawn fresh with explicit id" path: user supplied
// --session-id <their-id> themselves with no runtime SessionID.
// Pass through unchanged so the flag actually reaches Pi.
func TestFilterSessionFlags_NoResume_PassesThrough(t *testing.T) {
	in := []string{"--session-id", "user-fresh", "--model", "y"}
	got := filterSessionFlags(in, "", nil)
	if !equalSlice(got, in) {
		t.Errorf("filterSessionFlags = %v, want %v (passthrough)", got, in)
	}
}

// TestBuildArgs_ResumeIDStripsConflictingArg is the end-to-end
// argv-shape guarantee: when cfg.SessionID is set, the final argv
// contains --session-id <resume-id> exactly once (not duplicated
// with a user --session-id).
func TestBuildArgs_ResumeIDStripsConflictingArg(t *testing.T) {
	got := buildArgs(
		nil,
		agent.StartConfig{
			Args:     []string{"--session-id", "user-id", "--model", "x"},
			SessionID: "resume-id",
		},
	)
	want := []string{"--mode", "rpc", "--model", "x", "--session-id", "resume-id"}
	if !equalSlice(got, want) {
		t.Errorf("buildArgs = %v, want %v", got, want)
	}
	// No duplicate --session-id.
	count := 0
	for _, a := range got {
		if a == "--session-id" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("argv has %d --session-id occurrences, want 1", count)
	}
}

// TestBuildArgs_NoResume_PassesSessionIDThrough verifies the
// "spawn fresh with explicit id" path: cfg.SessionID empty +
// cfg.Args={"--session-id", <id>} should produce argv with
// exactly that --session-id (no runtime override).
func TestBuildArgs_NoResume_PassesSessionIDThrough(t *testing.T) {
	got := buildArgs(
		nil,
		agent.StartConfig{
			Args:     []string{"--session-id", "user-fresh"},
			SessionID: "",
		},
	)
	want := []string{"--mode", "rpc", "--session-id", "user-fresh"}
	if !equalSlice(got, want) {
		t.Errorf("buildArgs = %v, want %v", got, want)
	}
}
