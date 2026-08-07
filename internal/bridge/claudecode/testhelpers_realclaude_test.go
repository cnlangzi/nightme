package claudecode

// Shared skip guard for tests that spawn the REAL `claude` CLI
// (not the Python mock in testdata/claude_mock.py). These tests
// exercise actual --resume / stream-json / MCP behavior against
// the user's local `claude` install and are inherently
// environment-dependent (API key, network, proxy, model routing —
// see docs/bridge/claude.md §Testing). CI (and any machine without
// `claude` on PATH) must SKIP them, not fail the build.
//
// Every test file under this package that drives a real `claude`
// process MUST call requireRealClaude(t) as its first line, rather
// than duplicating the `exec.LookPath` check inline — one place to
// update if the guard ever needs to grow (e.g. an ANTHROPIC_API_KEY
// check).

import (
	"os/exec"
	"testing"
)

// requireRealClaude skips the calling test when the `claude` binary
// is not resolvable on PATH. Call this as the first line of any
// test that spawns a real claude process.
func requireRealClaude(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skipf("claude binary not on PATH: %v", err)
	}
}
