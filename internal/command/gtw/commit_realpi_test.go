//go:build !windows

// commit_realpi_test.go — live e2e smoke for the buildAgentPrompt v2.
//
// Mirrors pr_realpi_test.go: spawns a real `pi` subprocess via the
// bridge, sends the exact prompt dispatchCommit would send, drains
// the result, and asserts the v2 invariants on what the LLM actually
// produced.
//
// Default: SKIP. Opt in with:
//
//	NIGHTME_REAL_PI=1 go test ./internal/command/gtw -run RealPi -v -count=1
//
// The smoke uses a temp git repo as the workspace so the test does
// not touch the user's real working tree. A single dirty file is
// pre-seeded to give the LLM concrete changes to inspect (this is
// the "you MUST read the diff before staging" tool floor).
//
// Cost: ~50s and one API call to pi's model. Run deliberately;
// never part of CI.
package gtw

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	pibridge "github.com/cnlangzi/nightme/internal/bridge/pi"
)

// TestRealPi_CommitPromptV2 exercises the v2 buildAgentPrompt
// end-to-end. Creates a temp git repo with one dirty file, sends
// the v2 prompt to pi, captures the agent's text, and asserts:
//
//   1. The output starts with a Conventional Commits subject
//      (parsePRReply-style regex check).
//   2. The body is substantive — non-trivial commits must NOT be
//      subject-only (PR #135 regression guard).
//   3. The body does not contain commit-time actions (push /
//      add -A / stash) — the agent only commits.
//
// Writes /tmp/nightme-v2-commit-output.txt with the raw agent
// text on success so the human who ran the smoke can eyeball
// the result.
func TestRealPi_CommitPromptV2(t *testing.T) {
	requireRealPi(t)

	// ---- Set up a temp git repo with a known dirty file. -------
	// Using a temp repo keeps the smoke hermetic — no risk of
	// touching the user's actual working tree, no dependency on
	// existing commits, and a reproducible diff for the LLM.
	repoRoot := t.TempDir()

	run := func(args ...string) {
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

	run("init", "-q", "-b", "main")
	run("config", "user.email", "smoke@test")
	run("config", "user.name", "smoke")

	// Seed an initial commit so HEAD exists. Without this, the
	// very first commit the LLM tries to make has no parent.
	seedFile := filepath.Join(repoRoot, "seed.go")
	if err := os.WriteFile(seedFile, []byte("package seed\n"), 0o644); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	run("add", "seed.go")
	run("commit", "-q", "-m", "chore: initial seed")

	// Create the dirty file the LLM will commit. Content is
	// realistic enough that the LLM's tool floor has a real
	// diff to inspect.
	dirtyFile := filepath.Join(repoRoot, "feature.go")
	dirtyContent := strings.Join([]string{
		"package feature",
		"",
		"// ComputeSquare returns n * n. Used by the printer.",
		"func ComputeSquare(n int) int {",
		"\treturn n * n",
		"}",
		"",
	}, "\n")
	if err := os.WriteFile(dirtyFile, []byte(dirtyContent), 0o644); err != nil {
		t.Fatalf("dirty write: %v", err)
	}

	// ---- Build the v2 prompt and invoke pi. -------------------
	c := Context{
		Worktree: repoRoot,
		Branch:   "main",
		Issue:    1, // exercises the Issue trailer guard
	}
	prompt := buildAgentPrompt(c)
	t.Logf("=== PROMPT (first 1000 chars) ===\n%s\n=== END PROMPT (head) ===",
		truncateOutput(prompt, 1000))

	// Register pi on demand so the test is self-sufficient when
	// the gtw package is tested in isolation (no cmd/nightme
	// init()).
	reg := agent.New()
	reg.Register(pibridge.NewStarter("pi", "pi", nil))

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	cfg := agent.StartConfig{
		Workspace: repoRoot,
	}

	starter, err := reg.Get("pi")
	if err != nil {
		t.Fatalf("pi not registered: %v", err)
	}
	rawText, err := starter.RunOnce(ctx, cfg, []agent.ContentBlock{{
		Type: agent.ContentText,
		Text: prompt,
	}})
	if err != nil {
		t.Fatalf("pi RunOnce: %v", err)
	}

	t.Logf("=== RAW AGENT TEXT (first 2000 chars) ===\n%s\n=== END RAW ===",
		truncateOutput(rawText, 2000))

	_ = os.WriteFile("/tmp/nightme-v2-commit-output.txt", []byte(rawText), 0o644)
	t.Logf("raw text written to /tmp/nightme-v2-commit-output.txt")

	// ---- Assertions on the live output. ------------------------
	//
	// The LLM is responsible for the commit SUBJECT and BODY
	// (text shape), not for actually executing git. So we
	// validate the text shape:
	//
	//   1. Subject matches Conventional Commits.
	//   2. Body is substantive (>= 100 bytes for a non-trivial
	//      change — guards the PR #135 regression).
	//   3. No commit-time actions leaked into the text
	//      (no `git push`, no `git add -A`, no `git stash`).
	//   4. Issue trailer is present when c.Issue > 0.

	subject, body, ok := splitCCSubject(rawText)
	if !ok {
		t.Fatalf("could not extract Conventional Commits subject from agent output:\n%s", rawText)
	}
	t.Logf("=== SUBJECT ===\n%s", subject)
	if body != "" {
		t.Logf("=== BODY (%d bytes) ===\n%s", len(body), body)
	} else {
		t.Logf("=== BODY === (empty)")
	}

	// 1. Subject must be a valid CC type+scope+subject.
	if !conventionalCommitsTitle(subject) {
		t.Errorf("subject %q does not start with a Conventional Commits type", subject)
	}

	// 2. Body substantive check (PR #135 regression guard).
	// A non-trivial `fix:` or `feat:` must have a body. The
	// threshold is generous on purpose — even a 100-byte body
	// is enough to escape the "subject-only" modal pattern.
	subjectType := ccType(subject)
	if (subjectType == "fix" || subjectType == "feat") && len(body) < 100 {
		t.Errorf("commit body is %d bytes; %s: commits must have a substantive body (PR #135 regression guard)",
			len(body), subjectType)
	}

	// 3. No commit-time actions in the LLM's text.
	for _, banned := range []string{"`git push`", "`git add -A`", "`git stash`", "`git restore`", "`git checkout --"} {
		if strings.Contains(rawText, banned) {
			t.Errorf("agent output contains %q — the agent should never propose these commands", banned)
		}
	}

	// 4. Issue trailer present when c.Issue > 0.
	if !strings.Contains(rawText, "Issue: #1") {
		t.Errorf("agent output missing `Issue: #1` trailer; c.Issue=1 should propagate into the body")
	}

	t.Logf("CC subject + body length + banned-commands + issue trailer: passed if no FAIL lines above")
}

// splitCCSubject extracts the first Conventional Commits subject
// and its trailing body from a raw agent reply. The agent's reply
// shape is "subject\n\nbody..." for a multi-line commit, or just
// "subject\n" for a subject-only commit. Returns ok=false if the
// first non-blank line does not match a CC type.
func splitCCSubject(text string) (subject, body string, ok bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return "", "", false
	}
	lines := strings.SplitN(trimmed, "\n", 2)
	first := strings.TrimSpace(lines[0])
	if !conventionalCommitsTitle(first) {
		return "", "", false
	}
	subject = first
	if len(lines) == 2 {
		body = strings.TrimLeft(lines[1], "\n")
		body = strings.TrimRight(body, "\n")
	}
	return subject, body, true
}

// ccType extracts the type token from a Conventional Commits
// subject. Returns "" if the subject is not CC-shaped.
func ccType(subject string) string {
	for _, ty := range []string{"feat", "fix", "chore", "refactor", "docs", "test", "build", "ci", "perf", "style", "revert"} {
		if strings.HasPrefix(subject, ty+"(") || strings.HasPrefix(subject, ty+":") {
			return ty
		}
	}
	return ""
}