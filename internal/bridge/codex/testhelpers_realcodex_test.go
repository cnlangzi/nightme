package codex

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
	"os"
	"os/exec"
	"strings"
	"testing"
)

// requireRealCodex skips the calling test when the `codex` binary
// is not resolvable on PATH, when testing.Short() is set (CI / fast
// dev loop), or when NIGHTME_REAL_CODEX is set to anything other than
// "1"/"true"/"yes"/"on" (default: skipped). Call this as the first
// line of any test that spawns a real codex process.
//
// Real-codex tests are inherently environment-dependent (API key,
// network, proxy, model routing, sandbox). They are also slow
// (10s+ per test). Skip them by default; opt in explicitly:
//
//	go test ./internal/bridge/codex -run RealCodex -v
//	NIGHTME_REAL_CODEX=1 go test ./internal/bridge/codex -run RealCodex -v
//	go test -short ./internal/bridge/codex             # always skip
func requireRealCodex(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping real-codex test in -short mode (use -run RealCodex to opt in)")
	}
	switch strings.ToLower(os.Getenv("NIGHTME_REAL_CODEX")) {
	case "", "0", "false", "no", "off":
		t.Skip("set NIGHTME_REAL_CODEX=1 to enable real-codex e2e tests")
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skipf("codex binary not on PATH: %v", err)
	}
}
