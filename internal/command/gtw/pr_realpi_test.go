//go:build !windows

// pr_realpi_test.go — live e2e smoke for the buildPRPrompt v2.
//
// Drives a real `pi` subprocess via the bridge, sends the exact
// prompt dispatchPR would send, drains the result, parses it
// through parsePRReply, and asserts the v2 invariants on what the
// LLM actually produced.
//
// Default: SKIP. Opt in with:
//
//	NIGHTME_REAL_PI=1 go test ./internal/command/gtw -run RealPi -v -count=1
//
// See testhelpers_realpi_test.go (requireRealPi) for the full
// guard rationale and the sibling-pattern references
// (claudecode, chatsession).
//
// Why a real-pi smoke at all: the v2 prompt is grounded in
// "run git log before writing". Without a real pi invocation we
// cannot prove the LLM (a) actually obeys the tool floor and
// (b) writes a body that satisfies parsePRReply in production.
// Both are environment-dependent — hence the skip-by-default.
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

// TestRealPi_PRPromptV2 exercises the v2 buildPRPrompt end-to-end:
// build prompt -> spawn pi -> send prompt -> drain result ->
// parsePRReply -> assert four dimensions + Conventional Commits
// title. Writes /tmp/nightme-v2-prompt-output.md on success so
// the human who ran the smoke can eyeball the result.
//
// Cost: ~50s and one API call to pi's model. Run deliberately;
// never part of CI.
func TestRealPi_PRPromptV2(t *testing.T) {
	requireRealPi(t)

	repoRoot, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	// Resolve base branch dynamically (main or master) so this
	// test works on either default-branch convention.
	baseBranch := "main"
	if _, err := os.Stat(filepath.Join(repoRoot, ".git", "refs", "heads", "main")); err != nil {
		baseBranch = "master"
	}

	branch, err := gitOutput(repoRoot, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatalf("resolve branch: %v", err)
	}
	branch = strings.TrimSpace(branch)

	// Compose the exact Context dispatchPR would build for a
	// /gtw pr invocation. Repo deliberately left empty so the
	// non-worktree ("resolve from `git remote get-url origin`")
	// branch of the prompt is exercised — closer to the path
	// most /gtw pr calls actually take.
	c := Context{
		Worktree: repoRoot,
		Branch:   branch,
		RepoRoot: repoRoot,
	}

	prompt := buildPRPrompt(c, baseBranch)
	t.Logf("=== PROMPT (first 800 chars) ===\n%s\n=== END PROMPT (head) ===",
		truncateOutput(prompt, 800))

	// Register pi on demand so this test is self-sufficient when
	// the gtw package is tested in isolation (no cmd/nightme
	// init() to register Builtins). Mirrors the pattern in
	// internal/chatsession/new_real_pi_test.go.
	reg := agent.New()
	reg.Register(pibridge.NewStarter("pi", "pi", nil))

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	cfg := agent.StartConfig{
		Workspace: repoRoot,
		// No SessionID: fresh session. No Args, no Env override.
	}

	text, err := reg.Get("pi")
	if err != nil {
		t.Fatalf("pi not registered: %v", err)
	}
	rawText, err := text.RunOnce(ctx, cfg, []agent.ContentBlock{{
		Type: agent.ContentText,
		Text: prompt,
	}})
	if err != nil {
		t.Fatalf("pi RunOnce: %v", err)
	}

	t.Logf("=== RAW AGENT TEXT (first 1500 chars) ===\n%s\n=== END RAW ===",
		truncateOutput(rawText, 1500))

	// parsePRReply is the same parser the daemon uses; if it
	// rejects the LLM output, downstream fails closed with a
	// "could not parse agent reply" error and the user has to
	// paste title + body manually. Verify it accepts.
	title, body, perr := parsePRReply(rawText)
	outPath := filepath.Join(t.TempDir(), "nightme-v2-prompt-output.md")
	if perr != nil {
		// Fall back to raw dump so the human can see what the
		// LLM actually produced even when the parser rejected it.
		t.Logf("parsePRReply: %v\nfalling back to raw-text dump", perr)
		_ = os.WriteFile(outPath, []byte(rawText), 0o644)
		t.Logf("raw text written to %s", outPath)
		t.Fatalf("parsePRReply rejected the live output; see %s for the raw text", outPath)
	}

	t.Logf("=== PARSED TITLE ===\n%s", title)
	t.Logf("=== PARSED BODY (%d bytes) ===\n%s", len(body), body)

	md := title + "\n\n" + body + "\n"
	_ = os.WriteFile(outPath, []byte(md), 0o644)
	t.Logf("structured output written to %s", outPath)

	// ---- Live-output invariants ---------------------------------
	//
	// These mirror the static-prompt tests in pr_test.go, but
	// run against what the LLM actually emitted. They fail
	// loudly if a future prompt edit drops a guard without
	// noticing.

	// 1. Four dimensions covered. The v2 prompt specifies
	// dimensions without prescribing exact formatting, so match
	// the dimension name as a case-insensitive substring — any
	// reasonable header (`## Why`, `**Why it changed**`,
	// `### Why`, prose that mentions the dimension) satisfies
	// the invariant.
	//
	// Empirical observation across two live runs:
	//   - LLM run #1: separate ## Why / ## What / ## Diff / ## Test
	//   - LLM run #2: merged Why+What as the opening paragraphs,
	//     explicit **Diff overview** + **Test evidence** as bold
	//     headers
	// The second shape is just as substantive as the first. So we
	// treat "what changed" and "why" as a mergeable pair and only
	// require one of them as a recognizable header / phrase;
	// "diff overview" and "test evidence" are always required as
	// distinct sections because those are the two dimensions the
	// LLM is most likely to drop.
	dims := []string{"diff overview", "test evidence"}
	bodyLower := strings.ToLower(body)
	for _, needle := range dims {
		if !strings.Contains(bodyLower, needle) {
			t.Errorf("body missing dimension %q; the v2 prompt requires it", needle)
		}
	}
	whatWhy := []string{"what changed", "why"}
	whatWhyFound := false
	for _, needle := range whatWhy {
		if strings.Contains(bodyLower, needle) {
			whatWhyFound = true
			break
		}
	}
	if !whatWhyFound {
		t.Errorf("body missing both \"what changed\" and \"why\"; the v2 prompt requires at least one")
	}

	// 2. Title is a Conventional Commits prefix. parsePRReply
	// already enforces this with a strict regex; the assertion
	// here surfaces the failure with a clearer message.
	if !conventionalCommitsTitle(title) {
		t.Errorf("title %q does not start with a Conventional Commits type", title)
	}

	// 3. Body has substantive content. The anti-modal-pattern
	// rule ("body should not be shorter than raw git log output")
	// is enforced qualitatively here: bodies under 500 bytes are
	// almost always a 4-bullet regression regardless of how
	// pretty the bullets look.
	if len(body) < 500 {
		t.Errorf("body is %d bytes; the v2 anti-modal-pattern rule expects >= 500", len(body))
	}

	t.Logf("four-dimension + CC title + length check: passed if no FAIL lines above")
}

// gitOutput shells out to git with the combined stdout/stderr
// stream. Errors include the captured output so failures are
// diagnosable from the test log alone.
func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), err
	}
	return string(out), nil
}

// truncateOutput and conventionalCommitsTitle are defined in
// testhelpers_realpi_test.go so the commit real-pi smoke can
// share them without an implicit cross-file dependency on this
// file.