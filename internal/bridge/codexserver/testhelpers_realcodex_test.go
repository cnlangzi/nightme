package codexserver

// Shared skip guard for tests that spawn the REAL `codex` CLI's
// app-server protocol. These tests exercise actual codex behaviour
// (handshake, thread/start, turn lifecycle, JSON-RPC envelopes)
// against the user's local `codex` install and are inherently
// environment-dependent (API key, network, proxy, model routing,
// sandbox — see docs/bridge/codex.md §Testing). CI (and any machine
// without `codex` on PATH) must SKIP them, not fail the build.
//
// Every test file under this package that drives a real `codex`
// process MUST call requireRealCodex(t) as its first line.

import (
	"os/exec"
	"testing"
)

// requireRealCodex skips the calling test when the `codex` binary
// is not resolvable on PATH. Call this as the first line of any
// test that spawns a real codex process.
func requireRealCodex(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skipf("codex binary not on PATH: %v", err)
	}
}
