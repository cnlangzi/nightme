// Package procsweep — PID termination and PID-ownership
// verification for agent child processes.
//
// Two callers share these primitives: `nightme kill` (an operator
// sweep over the live rows of agent_sessions.json) and the runtime's
// startup sweep (the orphan cleanup a crashed daemon leaves behind).
// They must apply identical PID rules — a divergence would let the
// runtime kill a process `nightme kill` would have refused to touch.
//
// The package owns two concerns:
//
//   - Termination. Kill stops a PID with the SIGTERM → grace →
//     SIGKILL policy, so an agent CLI gets the chance to flush its
//     own session state before it dies. KillGroup does the same for
//     the process group the CLI leads, which is the unit that also
//     covers the tool subprocesses it spawned.
//   - Attribution. VerifyOwner answers "does this PID still run the
//     binary we expect?", so a PID the OS recycled onto an unrelated
//     process is never signalled.
//
// Both are deliberately fail-open where they cannot get a definite
// answer: an unreadable binary name must not disable the cleanup
// the caller exists to perform. Only a positive mismatch blocks the
// termination.
package procsweep

import (
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/pathutil"
)

const (
	// Grace is how long a child gets to exit after SIGTERM before
	// it is SIGKILLed.
	//
	// Two seconds matches the per-bridge grace window the runtime
	// uses in its own Close() paths, so a manual `nightme kill` is
	// no harsher than a normal shutdown.
	Grace = 2 * time.Second

	// PollInterval is how often the liveness is re-checked while
	// waiting out Grace. 50ms mirrors the daemon lifecycle polling
	// in cmd/nightme/daemon_lifecycle.go.
	PollInterval = 50 * time.Millisecond
)

// Owner is the outcome of a PID-ownership check.
//
// The three fields separate two questions a caller must not
// conflate: "may I signal this PID?" (Match) and "do I know whose
// process group this is?" (Verified).
type Owner struct {
	// Binary is the executable the PID was found to run. Empty when
	// the check could not get a definite answer.
	Binary string

	// Match is false when the PID runs a DIFFERENT binary than the
	// caller expected. A mismatch must never be signalled.
	Match bool

	// Verified is true when the PID was positively identified as the
	// expected binary. It is false when the check failed open —
	// there was nothing to compare against, or the binary could not
	// be read.
	Verified bool
}

// commTruncLen is the length at which Linux truncates a process's
// `comm`: the kernel stores it in TASK_COMM_LEN (16) bytes including
// the NUL, so `ps -o comm=` yields at most 15 characters. macOS does
// not truncate — it reports the full executable path (verified on
// darwin 24: `ps -p <pid> -o comm=` →
// "/Users/.../nightme/bin/nightme").
const commTruncLen = 15

// sameCommand compares a `ps comm` value with a configured agent
// command.
//
// Both sides may be a bare name ("claude") or an absolute path
// (/opt/homebrew/bin/claude), so the comparison is on the base name.
// A Linux-truncated comm is accepted as a match when it is a prefix
// of the expected name at exactly the truncation length: an agent
// configured as "my-long-agent-runner" shows up as "my-long-agent-r"
// there, and rejecting that would make the sweep refuse to clean up a
// perfectly valid session.
//
// This only ever loosens the check, which is the correct direction:
// verification exists to catch a PID that was recycled by something
// DIFFERENT, and a 15-character prefix match is strong evidence it
// was not.
func sameCommand(actual, want string) bool {
	a, w := pathutil.Base(actual), pathutil.Base(want)
	if a == w {
		return true
	}
	return len(a) == commTruncLen && len(w) > commTruncLen && strings.HasPrefix(w, a)
}

// sameCommandFold is the Windows flavour of sameCommand: Windows
// executable names carry an .exe suffix the configured command
// usually lacks, and the filesystem is case-insensitive. Both
// adjustments only loosen the check, which is the correct
// direction — verification exists to catch a PID recycled by
// something DIFFERENT.
func sameCommandFold(actual, want string) bool {
	a := stripExeSuffix(pathutil.Base(actual))
	w := stripExeSuffix(pathutil.Base(want))
	return strings.EqualFold(a, w)
}

// stripExeSuffix removes a trailing ".exe" (any case) from s.
//
// A name that IS the suffix (".exe") is left alone — there is no
// stem to keep.
func stripExeSuffix(s string) string {
	if len(s) > len(".exe") && strings.EqualFold(s[len(s)-len(".exe"):], ".exe") {
		return s[:len(s)-len(".exe")]
	}
	return s
}

// commandExt returns the lower-cased extension of a configured agent
// command ("cmd", "ps1", "js", "exe", …), or "" when it has none.
func commandExt(want string) string {
	base := strings.ToLower(pathutil.Base(want))
	if i := strings.LastIndex(base, "."); i > 0 {
		return base[i+1:]
	}
	return ""
}
