// Shared skip guard for tests that spawn the REAL `pi` CLI.
//
// Counterpart to internal/bridge/claudecode/testhelpers_realclaude_test.go
// (`requireRealClaude`). These tests drive an actual pi process and
// are inherently environment-dependent (binary present, model
// routing, network). CI — and any machine without `pi` on PATH —
// must SKIP them, not fail the build.
//
// Every test in this package that spawns a real pi process MUST
// call requireRealPi(t) as its first line rather than duplicating
// the exec.LookPath check inline, so there is one place to update
// if the guard needs to grow (e.g. an API-key check).
package chatsession

import (
	"os/exec"
	"testing"
)

// requireRealPi skips the calling test when the `pi` binary is not
// resolvable on PATH. Call this as the first line of any test that
// spawns a real pi process.
func requireRealPi(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skipf("pi binary not on PATH: %v", err)
	}
}
