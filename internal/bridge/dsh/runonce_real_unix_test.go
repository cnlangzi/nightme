//go:build unix

package dsh

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestE2E_RunOnce_RealDSH verifies that Starter.RunOnce works
// end-to-end against a real `dsh --profile web` daemon (no mocks).
//
// Gated by NIGHTME_REAL_DSH=1 + a real dsh binary on PATH. Skipped
// otherwise so unit-test CI passes.
//
// Exercises the full RunOnce path:
//   - Starter.Start → host.EnsureSharedHost (lazy spawn) → handshake →
//     session.create → EventAgentReady
//   - drainForRunResult → SendBlocks → session.prompt →
//     EventAgentResult (or Done) → RunResult
//   - defer a.Close() → workspace.archiveSession
func TestE2E_RunOnce_RealDSH(t *testing.T) {
	if os.Getenv("NIGHTME_REAL_DSH") == "" {
		t.Skip("NIGHTME_REAL_DSH not set; skipping real dsh e2e")
	}
	if _, err := exec.LookPath("dsh"); err != nil {
		t.Skipf("dsh not on PATH: %v", err)
	}

	// Verify dsh is on a port we can talk to. host.EnsureSharedHost
	// will spawn it if it's not running.
	workspace, err := os.MkdirTemp("", "dsh-runonce-e2e-*")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workspace) })

	s := NewStarter("dsh")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	start := time.Now()
	res, err := s.RunOnce(ctx, agent.StartConfig{Workspace: workspace},
		[]agent.ContentBlock{{
			Type: agent.ContentText,
			Text: "Reply with exactly: PONG. Do not add any other text.",
		}})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("RunOnce failed after %s: %v", elapsed, err)
	}

	if res.SessionID == "" {
		t.Errorf("RunResult.SessionID empty; expected a fresh sessionId")
	}
	if !strings.HasPrefix(res.SessionID, "session-") {
		t.Errorf("RunResult.SessionID = %q; want dsh-format session-<uuid>", res.SessionID)
	}
	if res.Text == "" {
		t.Errorf("RunResult.Text empty; expected a non-empty reply from dsh")
	}
	// Note: we don't assert on DurationMs or exact text content —
	// dsh web's session.result wire format doesn't include a duration
	// field on the result event itself, and the agent's actual reply
	// depends on the model (a permission grant for "Reply with
	// exactly PONG" is plausible — the agent might request
	// permission rather than reply). What we care about is that the
	// pipeline delivers a non-empty reply with the right metadata.
	if res.Model == "" {
		t.Errorf("RunResult.Model empty; want the model's id from session.models")
	}

	t.Logf("E2E success: sessionId=%s model=%s elapsed=%s textLen=%d text=%q",
		res.SessionID, res.Model, elapsed, len(res.Text), truncate(res.Text, 120))
}

// TestE2E_Review_RealDSH verifies that Starter.Review (which
// delegates to RunOnce with agent.builtinPrompt()) also works
// against a real dsh web.
func TestE2E_Review_RealDSH(t *testing.T) {
	if os.Getenv("NIGHTME_REAL_DSH") == "" {
		t.Skip("NIGHTME_REAL_DSH not set; skipping real dsh e2e")
	}
	if _, err := exec.LookPath("dsh"); err != nil {
		t.Skipf("dsh not on PATH: %v", err)
	}

	workspace, err := os.MkdirTemp("", "dsh-review-e2e-*")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workspace) })

	// Need a git repo in the workspace for the review prompt to
	// have something to diff against.
	if err := initGitRepo(workspace); err != nil {
		t.Skipf("git not available: %v", err)
	}

	s := NewStarter("dsh")
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	start := time.Now()
	res, err := s.Review(ctx, agent.StartConfig{Workspace: workspace})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Review failed after %s: %v", elapsed, err)
	}
	if res.SessionID == "" {
		t.Errorf("RunResult.SessionID empty")
	}
	// Review text should contain at least the summary or findings
	// sections per agent.builtinPrompt()'s contract. If the
	// model didn't follow the format exactly, log a warning but
	// still consider the test successful — what matters is the
	// pipeline delivered a non-empty review.
	if !strings.Contains(res.Text, "Summary") && !strings.Contains(res.Text, "Findings") {
		t.Logf("Review text doesn't have Summary/Findings sections; raw output (first 500 chars): %s",
			truncate(res.Text, 500))
	}

	t.Logf("Review E2E success: sessionId=%s elapsed=%s textLen=%d",
		res.SessionID, elapsed, len(res.Text))
}

// initGitRepo initialises a minimal git repo in dir with one commit,
// so `git diff <default-branch>...HEAD` (the review prompt's primary
// input) has at least one commit to look at.
func initGitRepo(dir string) error {
	cmds := [][]string{
		{"git", "init", "-q"},
		{"git", "config", "user.email", "e2e@nightme.local"},
		{"git", "config", "user.name", "nightme-e2e"},
		{"git", "checkout", "-q", "-b", "main"},
	}
	for _, args := range cmds {
		c := exec.Command(args[0], args[1:]...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			return errWrapf("%v: %s", args, out)
		}
	}
	// Empty initial commit so the branch exists.
	c := exec.Command("git", "commit", "--allow-empty", "-q", "-m", "init")
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		return errWrapf("git commit: %s", out)
	}
	return nil
}

func errWrapf(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(truncated)"
}
// TestE2E_RunOnce_Sink_RealDSH verifies that WithEventSink delivers
// intermediate AgentEvents (Ready / Text / Result / Done) during a
// real dsh web RunOnce. Confirms the end-to-end pipeline:
// bridge → drain → sink callback runs synchronously per event.
func TestE2E_RunOnce_Sink_RealDSH(t *testing.T) {
	if os.Getenv("NIGHTME_REAL_DSH") == "" {
		t.Skip("NIGHTME_REAL_DSH not set; skipping real dsh e2e")
	}
	if _, err := exec.LookPath("dsh"); err != nil {
		t.Skipf("dsh not on PATH: %v", err)
	}

	workspace, err := os.MkdirTemp("", "dsh-runonce-sink-e2e-*")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workspace) })

	s := NewStarter("dsh")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var (
		mu       sync.Mutex
		events   []agent.AgentEvent
		seenReady bool
		seenRes  bool
	)
	sink := func(ev agent.AgentEvent) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
		if ev.Kind == agent.EventAgentReady {
			seenReady = true
		}
		if ev.Kind == agent.EventAgentResult {
			seenRes = true
		}
	}

	res, err := s.RunOnce(ctx, agent.StartConfig{Workspace: workspace},
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "Reply with exactly: PONG"}},
		agent.WithEventSink(sink),
	)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.SessionID == "" {
		t.Fatal("RunResult.SessionID empty")
	}

	mu.Lock()
	defer mu.Unlock()
	// EventAgentDone is intentionally NOT observed — drain returns
	// as soon as EventAgentResult fires (Result is the terminal for
	// our purposes; Done is emitted by driver.Close on a closed
	// events chan, after our drain has already exited).
	if !seenReady {
		t.Errorf("sink never saw EventAgentReady (events=%d, kinds=%v)",
			len(events), kinds(events))
	}
	if !seenRes {
		t.Errorf("sink never saw EventAgentResult (events=%d, kinds=%v)",
			len(events), kinds(events))
	}
	t.Logf("E2E sink: sessionId=%s events=%d kinds=%v",
		res.SessionID, len(events), kinds(events))
}

func kinds(evs []agent.AgentEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Kind.String()
	}
	return out
}
