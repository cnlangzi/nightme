package opencode

// Shared skip guard for tests that spawn the REAL `opencode` CLI.
// These tests exercise the actual HTTP server + SSE stream against
// the user's local `opencode` install and are inherently environment-
// dependent (provider config, network, model routing, mcp servers).
// CI (and any machine without `opencode` on PATH) must SKIP them,
// not fail the build.
//
// Mirror of codex's testhelpers_realcodex_test.go. Every test file
// under this package that drives a real `opencode` process MUST call
// requireRealOpencode(t) as its first line.

import (
	"os/exec"
	"testing"
)

// requireRealOpencode skips the calling test when the `opencode`
// binary is not resolvable on PATH. Call this as the first line of
// any test that spawns a real opencode process.
func requireRealOpencode(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skipf("opencode binary not on PATH: %v", err)
	}
}
