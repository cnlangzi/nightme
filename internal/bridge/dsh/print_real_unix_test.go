// print_real_unix_test.go — end-to-end smoke tests against the
// real `dsh` binary on PATH. Gated by NIGHTME_REAL_DSH=1 so CI
// (which doesn't install dsh) skips cleanly.
//
//go:build unix

package dsh

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// requireDSH skips the test if `dsh` is not on PATH. Mirrors the
// pattern used by codex / pi real-CLI e2e tests.
func requireDSH(t *testing.T) {
	t.Helper()
	if os.Getenv("NIGHTME_REAL_DSH") == "" {
		t.Skip("NIGHTME_REAL_DSH not set; skipping real dsh e2e")
	}
	if _, err := exec.LookPath("dsh"); err != nil {
		t.Skipf("dsh not in PATH: %v", err)
	}
}

// TestE2E_PongSmoke is the canonical "does it work" test: spawn
// dsh headless with a deterministic prompt and assert the exact
// answer. PONG is the canary string — single-word, no
// trailing-newline ambiguity, easy to assert.
func TestE2E_PongSmoke(t *testing.T) {
	requireDSH(t)

	s := NewStarter("dsh")
	result, err := s.RunOnce(context.Background(), agent.StartConfig{
		Workspace: t.TempDir(),
	}, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "Reply with the single word PONG and nothing else."},
	})
	if err != nil {
		t.Fatalf("RunOnce(PONG) error: %v", err)
	}
	if result.Text != "PONG" {
		t.Errorf("RunOnce(PONG).Text = %q, want %q", result.Text, "PONG")
	}
	if result.Subtype != "completed" {
		t.Errorf("RunOnce(PONG).Subtype = %q, want %q", result.Subtype, "completed")
	}
	if result.DurationMs <= 0 {
		t.Errorf("RunOnce(PONG).DurationMs = %d, want > 0", result.DurationMs)
	}
	if result.DurationMs > 60_000 {
		t.Errorf("RunOnce(PONG).DurationMs = %d, want < 60s", result.DurationMs)
	}
	// dsh headless doesn't expose model / sessionId / usage —
	// these must stay zero-valued so callers don't read stale
	// fields from other bridges.
	if result.Model != "" {
		t.Errorf("RunOnce(PONG).Model = %q, want empty", result.Model)
	}
	if result.SessionID != "" {
		t.Errorf("RunOnce(PONG).SessionID = %q, want empty", result.SessionID)
	}
	if result.Usage != nil {
		t.Errorf("RunOnce(PONG).Usage = %+v, want nil", result.Usage)
	}
}

// TestE2E_MultiTurnIndependentCalls verifies two RunOnce calls
// don't share state (each is a fresh process). If they did share
// state, a session id would leak between calls; we assert both
// calls succeed and produce independent outputs.
func TestE2E_MultiTurnIndependentCalls(t *testing.T) {
	requireDSH(t)

	s := NewStarter("dsh")
	workspace := t.TempDir()

	for _, prompt := range []string{
		"Reply with the single word ALPHA and nothing else.",
		"Reply with the single word BETA and nothing else.",
	} {
		result, err := s.RunOnce(context.Background(), agent.StartConfig{
			Workspace: workspace,
		}, []agent.ContentBlock{{Type: agent.ContentText, Text: prompt}})
		if err != nil {
			t.Fatalf("RunOnce(%q) error: %v", prompt, err)
		}
		switch prompt {
		case "Reply with the single word ALPHA and nothing else.":
			if result.Text != "ALPHA" {
				t.Errorf("ALPHA turn Text = %q, want %q", result.Text, "ALPHA")
			}
		case "Reply with the single word BETA and nothing else.":
			if result.Text != "BETA" {
				t.Errorf("BETA turn Text = %q, want %q", result.Text, "BETA")
			}
		}
	}
}

// TestE2E_TempWorkspaceUsed confirms that the workspace argument
// is honored (cmd.Dir was set correctly). We pick t.TempDir() as
// workspace and rely on it existing — if cmd.Dir is wrong, the
// spawn would either fail (path doesn't exist) or run in the wrong
// cwd. A successful run from a fresh TempDir proves the path was
// accepted.
func TestE2E_TempWorkspaceUsed(t *testing.T) {
	requireDSH(t)

	ws := t.TempDir()
	s := NewStarter("dsh")
	start := time.Now()
	_, err := s.RunOnce(context.Background(), agent.StartConfig{
		Workspace: ws,
	}, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "Reply with the single word YES and nothing else."},
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("RunOnce(YES) error: %v", err)
	}
	if elapsed > 60*time.Second {
		t.Errorf("RunOnce took %v, want < 60s (TempDir should be a fast workspace)", elapsed)
	}
}

// TestE2E_DSHPermissionEnvPropagates is the real env-injection
// regression test. Unlike the previous soft check (which only
// verified exit 0 + text non-empty and would pass whether or not
// the env var was injected), this version replaces the dsh
// binary with a mock shell script that echoes $DSH_PERMISSION_MODE
// to stdout, then asserts the echoed value is the contract value
// ("danger-full-access"). This catches any future regression
// where the bridge stops injecting the documented env var.
//
// Note: this test does NOT need NIGHTME_REAL_DSH — it intentionally
// bypasses the real dsh binary because we want to verify the
// bridge's env-injection contract independent of dsh's behavior.
func TestE2E_DSHPermissionEnvPropagates(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "mock-dsh.sh")
	script := "#!/bin/sh\n" +
		"echo \"$DSH_PERMISSION_MODE\"\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock script: %v", err)
	}

	// Point the Starter at the mock script instead of `dsh`. This
	// isolates the test: it asserts ONLY that the bridge injects
	// DSH_PERMISSION_MODE=danger-full-access into the child's env,
	// independent of dsh's plugin behavior.
	s := &Starter{
		name:    "dsh-mock",
		command: scriptPath,
		args:    []string{"--profile", "headless"},
	}

	result, err := s.RunOnce(context.Background(), agent.StartConfig{
		Workspace: tmpDir,
	}, []agent.ContentBlock{{Type: agent.ContentText, Text: "ignored"}})
	if err != nil {
		t.Fatalf("RunOnce(mock) error: %v", err)
	}
	if result.Subtype != "completed" {
		t.Errorf("Subtype = %q, want %q", result.Subtype, "completed")
	}
	if result.Text != "danger-full-access" {
		t.Errorf("Text = %q, want %q (DSH_PERMISSION_MODE not injected by bridge)",
			result.Text, "danger-full-access")
	}
}

// TestE2E_ArgsFromStarter verifies that the argv surfaced via
// Info().Args is the same argv that runPrintMode actually
// executes. The bridge reads s.args to compose the spawn argv
// (print.go); if a future change breaks this link, this test
// fails.
//
// Concretely: NewStarter populates args=["--profile","headless"].
// We point Starter.command at a mock script that echoes all
// positional argv, then assert the first two args are exactly
// "--profile" and "headless" — and "--" + the prompt follow.
func TestE2E_ArgsFromStarter(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "mock-dsh.sh")
	// $@ expands to all positional args (one per line, in shell's
	// default IFS handling). We strip the trailing empty line
	// produced by sh's trailing newline.
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do echo \"$a\"; done\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock script: %v", err)
	}

	s := &Starter{
		name:    "dsh-mock",
		command: scriptPath,
		args:    []string{"--profile", "headless"},
	}
	info := s.Info()
	if got, want := info.Args, []string{"--profile", "headless"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("Info.Args = %v, want %v (Starter.args drift)", got, want)
	}

	result, err := s.RunOnce(context.Background(), agent.StartConfig{
		Workspace: tmpDir,
	}, []agent.ContentBlock{{Type: agent.ContentText, Text: "MYPROMPT"}})
	if err != nil {
		t.Fatalf("RunOnce(mock) error: %v", err)
	}
	want := "--profile\nheadless\n--\nMYPROMPT"
	if result.Text != want {
		t.Errorf("spawn argv = %q, want %q\n(Starter.args not propagated to runPrintMode)",
			result.Text, want)
	}
}
