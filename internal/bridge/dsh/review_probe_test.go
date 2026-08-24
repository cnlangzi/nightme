//go:build unix

package dsh

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestReviewProbe_RealDSH_WithNonEmptyDiff runs a real-dsh /review
// against a workspace with actual diff content (so the model has
// something concrete to review), and dumps everything that flows
// through the sink + the merged RunResult. Used to verify the agent
// actually receives the simplify prompt (vs being confused by dsh
// startup context like the browser-skill auto-load).
//
// Gated by NIGHTME_REAL_DSH=1 + dsh on PATH.
func TestReviewProbe_RealDSH_WithNonEmptyDiff(t *testing.T) {
	if os.Getenv("NIGHTME_REAL_DSH") == "" {
		t.Skip("NIGHTME_REAL_DSH not set; skipping real dsh e2e")
	}
	if _, err := exec.LookPath("dsh"); err != nil {
		t.Skipf("dsh not on PATH: %v", err)
	}

	// Build a workspace with a real diff (vs the empty --allow-empty
	// commit the existing e2e uses).
	workspace, err := os.MkdirTemp("", "dsh-review-probe-*")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workspace) })

	if err := initGitRepoWithDiff(workspace); err != nil {
		t.Skipf("git not available: %v", err)
	}

	s := NewStarter("dsh")
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	sink := func(ev agent.AgentEvent) {
		switch ev.Kind {
		case agent.EventAgentText:
			t.Logf("[SINK text] (%d chars) %s", len(ev.Text), truncate(ev.Text, 200))
		case agent.EventAgentToolStart:
			t.Logf("[SINK tool-start] name=%s", ev.ToolStart.Name)
		case agent.EventAgentToolEnd:
			t.Logf("[SINK tool-end] name=%s", ev.ToolEnd.Name)
		case agent.EventAgentReady:
			t.Logf("[SINK ready] session=%s model=%s", ev.SessionID, ev.Model)
		case agent.EventAgentResult:
			t.Logf("[SINK result] subtype=%s text-len=%d", ev.Result.Subtype, len(ev.Result.Text))
		case agent.EventAgentDone:
			t.Logf("[SINK done] reason=%s", ev.Done.Reason)
		case agent.EventAgentError:
			t.Logf("[SINK error] %v", ev.Err)
		}
	}

	res, err := s.Review(ctx, agent.StartConfig{Workspace: workspace}, agent.WithEventSink(sink))
	if err != nil {
		t.Fatalf("Review failed: %v", err)
	}
	t.Logf("=== RunResult: sessionId=%s model=%s durationMs=%d ===", res.SessionID, res.Model, res.DurationMs)
	t.Logf("=== TEXT (first 1500 chars) ===\n%s", truncate(res.Text, 1500))

	hasSummary := strings.Contains(res.Text, "## Summary")
	hasFindings := strings.Contains(res.Text, "## Findings")
	if !hasSummary || !hasFindings {
		t.Logf("WARN: merged review text lacks Summary/Findings (hasSummary=%v hasFindings=%v)", hasSummary, hasFindings)
	}
}

// initGitRepoWithDiff creates a git repo with one empty commit and
// a follow-up commit that adds a real file — so /review has actual
// diff content to look at.
func initGitRepoWithDiff(dir string) error {
	cmds := [][]string{
		{"git", "init", "-q"},
		{"git", "config", "user.email", "probe@nightme.local"},
		{"git", "config", "user.name", "nightme-probe"},
		{"git", "checkout", "-q", "-b", "main"},
		{"git", "commit", "--allow-empty", "-q", "-m", "init"},
	}
	for _, args := range cmds {
		c := exec.Command(args[0], args[1:]...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			return fmt.Errorf("%v: %s", args, out)
		}
	}
	// Add a real Go file with some content + an intentional bug.
	const sample = `package probe

import "fmt"

// Add takes two ints and returns the sum. Intentional bug for /review:
// swallows the error and never returns it; uses fmt.Println in a hot
// loop instead of buffered output; uses string concatenation instead
// of strconv.Itoa.
func Add(a, b int) string {
	for i := 0; i < 100; i++ {
		fmt.Println("loop iter", i)
	}
	return fmt.Sprintf("%d", a+b)
}

var _ = Add
`
	if err := os.WriteFile(dir+"/probe.go", []byte(sample), 0o644); err != nil {
		return err
	}
	c := exec.Command("git", "add", "-A")
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("git add: %s", out)
	}
	c = exec.Command("git", "commit", "-q", "-m", "add probe.go")
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("git commit: %s", out)
	}
	return nil
}