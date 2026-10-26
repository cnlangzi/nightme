// PID-identity comparison rules for `nightme kill`.
//
// They live in a build-tag-free file so a single unit-test file can
// pin them on every platform: the unix rules (case-sensitive, with
// the `ps comm` truncation allowance) and the Windows rules (`.exe`
// suffix, case-insensitive) are two flavours of the same question —
// "does this PID still belong to the agent we expect?".

package main

import (
	"strings"

	"github.com/cnlangzi/nightme/internal/pathutil"
)

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
// there, and rejecting that would make `kill` refuse to clean up a
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
