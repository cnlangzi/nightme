//go:build !windows

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
	"os"
	"os/exec"
	"strings"
	"testing"
)

// requireRealClaude skips the calling test when the `claude` binary
// is not resolvable on PATH, when testing.Short() is set (CI / fast
// dev loop), or when NIGHTME_REAL_CLAUDE is set to anything other
// than "1"/"true"/"yes"/"on" (default: skipped). Call this as the
// first line of any test that spawns a real claude process.
//
// Real-claude tests are inherently environment-dependent (API key,
// network, proxy, model routing). They are also slow (10s+ per
// test). Skip them by default; opt in explicitly:
//
//	go test ./internal/bridge/claudecode -run RealClaude -v
//	NIGHTME_REAL_CLAUDE=1 go test ./internal/bridge/claudecode -run RealClaude -v
//	go test -short ./internal/bridge/claudecode           # always skip
func requireRealClaude(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping real-claude test in -short mode (use -run RealClaude to opt in)")
	}
	switch strings.ToLower(os.Getenv("NIGHTME_REAL_CLAUDE")) {
	case "", "0", "false", "no", "off":
		t.Skip("set NIGHTME_REAL_CLAUDE=1 to enable real-claude e2e tests")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skipf("claude binary not on PATH: %v", err)
	}
}
