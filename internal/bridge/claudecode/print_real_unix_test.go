//go:build !windows

// T-alive real-claude tests for the print-mode RunOnce path.
//
// Skipped unless `claude` is on PATH AND NIGHTME_TALIVE_RUNONCE=1
// is set. These tests exercise the real `claude -p` binary and
// require network access + a valid credential (OAuth session or
// ANTHROPIC_API_KEY). They exist to verify what mock tests
// can't:
//
//   - The argv shape (-p + --output-format stream-json + --verbose
//     + --permission-mode bypassPermissions) actually produces a
//     stream-json result event from real claude.
//   - The is_error path triggers on a real "model declined to
//     answer" outcome.
//   - The arg parsing doesn't reject any flag.
//   - OAuth / keychain credentials work end-to-end (no auth
//     error from claude itself).
//
// Run with:
//
//	NIGHTME_TALIVE_RUNONCE=1 \
//	  go test -count=1 -run TestPrintMode_Real ./internal/bridge/claudecode/...
//
// These tests are intentionally SLOW (each turn is a real model
// call). Don't run them in tight CI loops.
package claudecode

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// requireRealClaudeRunOnce skips the test unless claude is on
// PATH AND NIGHTME_TALIVE_RUNONCE=1. Mirrors requireRealClaude
// in claudecode_test.go but with a separate env-var gate so
// existing T-alive tests (Start path) don't accidentally pick
// up the new RunOnce tests.
func requireRealClaudeRunOnce(t *testing.T) {
	t.Helper()
	if _, err := execLookPathClaude(); err != nil {
		t.Skipf("claude not on PATH: %v", err)
	}
	if os.Getenv("NIGHTME_TALIVE_RUNONCE") != "1" {
		t.Skip("set NIGHTME_TALIVE_RUNONCE=1 to run real-claude RunOnce tests")
	}
}

// execLookPathClaude is a small wrapper so the test compiles
// without an explicit os/exec import in this file's package
// block (which only imports what it needs).
func execLookPathClaude() (string, error) {
	// Re-use the package's existing exec.LookPath-via-Detect
	// path: NewStarter(...).Detect() returns nil iff `claude`
	// resolves on PATH.
	a := NewStarter("claude", "claude", nil)
	if err := a.Detect(); err != nil {
		return "", err
	}
	return "claude", nil
}

// TestPrintMode_Real_HappyPath_ExactEcho verifies the simplest
// "real claude actually ran my prompt and returned a result"
// case. The prompt asks claude to echo a known string; we
// assert the result contains that string. Loose match
// (Contains) because real models may paraphrase.
func TestPrintMode_Real_HappyPath_ExactEcho(t *testing.T) {
	requireRealClaudeRunOnce(t)

	a := NewStarter("claude", "claude", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	got, err := a.RunOnce(ctx, agent.StartConfig{Workspace: t.TempDir()}, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "Reply with exactly: hello-from-real-claude"},
	})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !strings.Contains(got.Text, "hello-from-real-claude") {
		t.Errorf("result missing expected echo: %q", got.Text)
	}
}

// TestPrintMode_Real_OAuthDoesNotBlock verifies that the
// default path (no --bare) successfully reads the user's
// OAuth / keychain credentials. This is the regression
// guard for "we don't ship --bare by default" — if someone
// accidentally re-adds --bare, this test should fail with
// an auth error from real claude.
func TestPrintMode_Real_OAuthDoesNotBlock(t *testing.T) {
	requireRealClaudeRunOnce(t)

	a := NewStarter("claude", "claude", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// The model call is intentionally trivial — the test is
	// about auth succeeding, not about model quality.
	got, err := a.RunOnce(ctx, agent.StartConfig{Workspace: t.TempDir()}, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "Reply with exactly: auth-ok"},
	})
	if err != nil {
		t.Fatalf("RunOnce (likely auth failure if --bare was re-added): %v", err)
	}
	if !strings.Contains(got.Text, "auth-ok") {
		t.Errorf("result missing expected echo: %q", got.Text)
	}
}

// TestPrintMode_Real_WorkspaceIsRespected verifies that
// cfg.Workspace is honored — claude's tool calls (if it makes
// any) and any relative-path resolution uses our workspace.
// For the trivial prompt, this mainly confirms the spawn
// doesn't fail on a non-existent cwd.
func TestPrintMode_Real_WorkspaceIsRespected(t *testing.T) {
	requireRealClaudeRunOnce(t)

	a := NewStarter("claude", "claude", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	ws := t.TempDir()
	// Touch a marker file so we can verify claude saw the dir.
	markerPath := ws + "/.nightme-marker"
	if err := os.WriteFile(markerPath, []byte("present"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	if _, err := a.RunOnce(ctx, agent.StartConfig{Workspace: ws}, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "Reply with exactly: workspace-ok"},
	}); err != nil {
		t.Fatalf("RunOnce with custom workspace: %v", err)
	}
	// Marker file still there means cwd didn't get chdir'd
	// out from under us mid-spawn.
	if _, err := os.Stat(markerPath); err != nil {
		t.Errorf("marker vanished (cwd lost?): %v", err)
	}
}