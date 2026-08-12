//go:build !windows

// Shared skip guard for tests in this package that spawn the REAL
// `pi` CLI.
//
// Counterpart to:
//   - internal/bridge/claudecode/testhelpers_realclaude_test.go
//     (requireRealClaude)
//   - internal/chatsession/testhelpers_realpi_test.go
//     (requireRealPi)
//
// These tests drive an actual pi process and are inherently
// environment-dependent (binary present, model routing, network).
// CI — and any machine without `pi` on PATH — must SKIP them, not
// fail the build.
//
// Every test in this package that spawns a real pi process MUST
// call requireRealPi(t) as its first line rather than duplicating
// the exec.LookPath check inline, so there is one place to update
// if the guard needs to grow.
package gtw

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// requireRealPi skips the calling test when the `pi` binary is
// not resolvable on PATH, when testing.Short() is set (CI / fast
// dev loop), or when NIGHTME_REAL_PI is set to anything other
// than "1"/"true"/"yes"/"on" (default: skipped). Call this as
// the first line of any test that spawns a real pi process.
//
// The pi-E2E tests are inherently environment-dependent: they
// need a working `pi` install AND tolerate the bridge's variable
// startup-event timing. They are also slow (10s+ per test).
// Skip them by default; opt in explicitly:
//
//	go test ./internal/command/gtw -run RealPi -v
//	NIGHTME_REAL_PI=1 go test ./internal/command/gtw -run RealPi -v
//	go test -short ./internal/command/gtw               # always skip
func requireRealPi(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping real-pi test in -short mode (use -run RealPi to opt in)")
	}
	switch strings.ToLower(os.Getenv("NIGHTME_REAL_PI")) {
	case "", "0", "false", "no", "off":
		t.Skip("set NIGHTME_REAL_PI=1 to enable real-pi e2e tests")
	}
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skipf("pi binary not on PATH: %v", err)
	}
}

// ccTypes is the Conventional Commits 1.0.0 type allow-list.
// Single source of truth for splitCCSubject / conventionalCommitsTitle
// in the real-pi smokes (and mirrors the type allow-list baked
// into prTitleRegex in pr.go).
var ccTypes = []string{
	"feat", "fix", "chore", "refactor", "docs", "test",
	"build", "ci", "perf", "style", "revert",
}

// ccType extracts the type token from a Conventional Commits
// subject. Returns "" if the subject is not CC-shaped.
func ccType(subject string) string {
	for _, ty := range ccTypes {
		if strings.HasPrefix(subject, ty+"(") || strings.HasPrefix(subject, ty+":") {
			return ty
		}
	}
	return ""
}

// conventionalCommitsTitle reports whether title starts with a
// Conventional Commits type token followed by "(" or ":".
// Mirrors the type allow-list in prTitleRegex (pr.go).
func conventionalCommitsTitle(title string) bool {
	return ccType(strings.TrimSpace(title)) != ""
}

// truncateOutput returns s unchanged if its byte length is at
// most n, otherwise returns the first n bytes followed by a
// truncation marker. Used by the real-pi smoke tests to keep
// t.Logf of long agent output readable.
func truncateOutput(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n... [truncated]"
}