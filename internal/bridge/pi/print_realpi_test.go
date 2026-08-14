//go:build !windows

// Real-binary smoke for the print-mode RunOnce path.
//
// This is the regression guard for F-PI-PRINT-001 (2026-08-13):
// the production /gtw commit flow that previously failed in
// 2-5 seconds because the RPC-mode bridge lost events should
// now succeed end-to-end via print-mode (--mode json -p).
//
// The test mirrors TestRealPi_CommitPromptV2's setup (temp
// git repo + dirty file + real /gtw commit prompt) but
// drives it through the print-mode path: starter.RunOnce →
// runPrintMode → pi --mode json -p. A pass demonstrates that
// the print-mode spawn survives the conditions that broke
// RPC-mode in production.
//
// Default: SKIP. Opt in with:
//
//	NIGHTME_REAL_PI=1 go test ./internal/bridge/pi -run PrintMode -v -count=1
package pi

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// requireRealPi gates the smoke on NIGHTME_REAL_PI=1 so the
// default `go test ./...` stays fast on dev machines. Without
// pi installed or the env var unset, the test skips cleanly.
func requireRealPrintMode(t *testing.T) {
	t.Helper()
	if os.Getenv("NIGHTME_REAL_PI") != "1" {
		t.Skip("set NIGHTME_REAL_PI=1 to run real-pi smoke (skipped by default)")
	}
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skipf("pi binary not on PATH: %v", err)
	}
}

func TestPrintMode_RealPi_CommitPrompt(t *testing.T) {
	requireRealPrintMode(t)
	bin, _ := exec.LookPath("pi")
	t.Logf("using pi binary at %s", bin)

	repoRoot := t.TempDir()

	// Set up a temp git repo with a dirty file so the LLM has
	// real changes to inspect. Same shape as the
	// TestRealPi_CommitPromptV2 setup.
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoRoot
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=smoke",
			"GIT_AUTHOR_EMAIL=smoke@test",
			"GIT_COMMITTER_NAME=smoke",
			"GIT_COMMITTER_EMAIL=smoke@test",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	runGit("init", "-q", "-b", "main")
	runGit("config", "user.email", "smoke@test")
	runGit("config", "user.name", "smoke")
	// Seed an initial commit so HEAD exists. Without this, the
	// very first commit the LLM tries to make has no parent.
	if err := os.WriteFile(filepath.Join(repoRoot, "seed.go"),
		[]byte("package seed\n"), 0o644); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	runGit("add", "seed.go")
	runGit("commit", "-qm", "init")

	// Make the workspace dirty so the LLM has something to
	// commit. One modified file mirrors the cwd test scenario.
	if err := os.WriteFile(filepath.Join(repoRoot, "feature.go"),
		[]byte("package feature\n\nfunc ComputeSquare(n int) int { return n * n }\n"),
		0o644); err != nil {
		t.Fatalf("feature write: %v", err)
	}

	// Generate the same prompt that dispatchCommit sends.
	// Kept inline (rather than imported from internal/command/gtw)
	// to keep the smoke test hermetic — if buildAgentPrompt changes,
	// this test updates too, surfacing the change as a test diff.
	prompt := buildPrintModePrompt(repoRoot)

	a := NewStarter("pi", bin, nil)
	if err := a.Detect(); err != nil {
		t.Fatalf("Detect: %v (binary at %q)", err, bin)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	t.Logf("=== RunOnce (print mode) start ===")
	startedAt := time.Now()
	text, err := a.RunOnce(ctx, agent.StartConfig{Workspace: repoRoot}, []agent.ContentBlock{
		{Type: agent.ContentText, Text: prompt},
	})
	elapsed := time.Since(startedAt)
	t.Logf("=== RunOnce returned in %s ===", elapsed)
	if err != nil {
		t.Fatalf("RunOnce (print mode): %v", err)
	}
	if text.Text == "" {
		t.Fatal("RunOnce returned empty text")
	}
	t.Logf("Agent text (first 400 chars): %s", truncateForDisplay(text.Text, 400))

	// Sanity: the prompt instructed the agent not to push and
	// not to add -A. We can't easily assert those didn't happen
	// without parsing the LLM's prose, but we CAN assert the
	// working tree is clean iff the agent actually committed.
	// If it didn't commit, the test catches that here.
	statusCmd := exec.Command("git", "status", "--porcelain")
	statusCmd.Dir = repoRoot
	statusOut, statusErr := statusCmd.CombinedOutput()
	if statusErr != nil {
		t.Logf("git status failed (informational): %v\n%s", statusErr, statusOut)
	} else if len(statusOut) > 0 {
		t.Errorf("expected clean working tree after commit, got:\n%s", statusOut)
	}
}

// buildPrintModePrompt mirrors buildAgentPrompt from
// internal/command/gtw/commit.go, but only the parts the LLM
// actually reads in print-mode tests. Kept inline to keep the
// smoke test hermetic against gtw refactors — if the dispatch
// prompt changes shape, this test follows by copy-paste.
func buildPrintModePrompt(worktree string) string {
	return `You are a release engineer. Your job: turn uncommitted work into one or more well-formed local commits. Push and PR creation are handled by separate steps; you ONLY commit.

Branch: main
Worktree: ` + worktree + `

## Before staging — tool floor
You MUST run and read the output of these commands BEFORE staging anything:
- ` + "`git status`" + ` — see what's dirty.
- ` + "`git diff`" + ` (no args) — the unstaged changes you're about to stage.

## Hard rules (non-negotiable)
- Do not push. Push is the user's decision, not yours; never run ` + "`git push`" + `.
- Do not revert, restore, or stash the user's work.
- ` + "`git add <specific files>`" + `, not ` + "`git add -A`" + `.
`
}

func truncateForDisplay(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
