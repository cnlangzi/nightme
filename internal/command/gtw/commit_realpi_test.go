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
// Writes nightme-v2-commit-output.txt under t.TempDir() with the raw agent
// text on success so the human who ran the smoke can eyeball
// the result.
func TestRealPi_CommitPromptV2(t *testing.T) {
	requireRealPi(t)

	// ---- Set up a temp git repo with a known dirty file. -------
	// Using a temp repo keeps the smoke hermetic — no risk of
	// touching the user's actual working tree, no dependency on
	// existing commits, and a reproducible diff for the LLM.
	repoRoot := t.TempDir()

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
	seedFile := filepath.Join(repoRoot, "seed.go")
	if err := os.WriteFile(seedFile, []byte("package seed\n"), 0o644); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	runGit("add", "seed.go")
	runGit("commit", "-q", "-m", "chore: initial seed")

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
	rawResult, err := starter.RunOnce(ctx, cfg, []agent.ContentBlock{{
		Type: agent.ContentText,
		Text: prompt,
	}})
	if err != nil {
		t.Fatalf("pi RunOnce: %v", err)
	}
	rawText := rawResult.Text

	t.Logf("=== RAW AGENT TEXT (first 2000 chars) ===\n%s\n=== END RAW ===",
		truncateOutput(rawText, 2000))

	outPath := filepath.Join(t.TempDir(), "nightme-v2-commit-output.txt")
	_ = os.WriteFile(outPath, []byte(rawText), 0o644)
	t.Logf("raw text written to %s", outPath)

	// ---- Assertions on the live output. ------------------------
	//
	// The LLM is responsible for the commit SUBJECT and BODY
	// (text shape), not for actually executing git. So we
	// validate the text shape:
	//
	//   1. Subject matches Conventional Commits.
	//   2. Body is substantive (>= 200 bytes for fix:/feat: —
	//      guards the PR #135 regression).
	//   3. No commit-time actions in the BODY (no `git push`,
	//      no `git add -A`, no `git stash`). The check is
	//      scoped to body so that the LLM echoing back the
	//      prompt's hard rules ("I will not run `git push`")
	//      doesn't trigger a false positive.
	//   4. Issue trailer present in the body when c.Issue > 0.

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
	// 200-byte threshold catches both subject-only commits
	// (~30-80 bytes) and pseudo-bodies like a single
	// "Issue: #N" trailer (~12 bytes).
	subjectType := ccType(subject)
	if (subjectType == "fix" || subjectType == "feat") && len(body) < 200 {
		t.Errorf("commit body is %d bytes; %s: commits must have a substantive body (PR #135 regression guard)",
			len(body), subjectType)
	}

	// 3. No commit-time actions in the BODY (not the full raw
	// text — see comment at the top of this block). The check
	// matches the bare command name ("git push"), which also
	// catches the backticked form ("`git push`") as a substring.
	// The release-engineer prompt forbids these commands
	// entirely, so any mention in the body is a violation —
	// whether the LLM proposes to run them, claims it didn't
	// run them, or summarises what it avoided.
	for _, banned := range []string{"git push", "git add -A", "git stash", "git restore", "git checkout --"} {
		if strings.Contains(body, banned) {
			t.Errorf("commit body contains %q — the agent should never propose these commands", banned)
		}
	}

	// 4. Issue trailer present in the body when c.Issue > 0.
	if !strings.Contains(body, "Issue: #1") {
		t.Errorf("commit body missing `Issue: #1` trailer; c.Issue=1 should propagate into the body")
	}

	t.Logf("CC subject + body length + banned-commands + issue trailer: passed if no FAIL lines above")
}

// splitCCSubject extracts the first Conventional Commits subject
// and its trailing body from a raw agent reply.
//
// The agent's reply shape varies across runs and model
// versions. Observed shapes so far:
//
//   - Plain text:    "subject\n\nbody..."
//   - Fenced:        "```\nsubject\n\nbody...\n```"
//   - Preamble:      "Done. Committed:\n\n```\nsubject...\n```"
//   - Hash prefix:   "950abe5 feat: add ComputeSquare..."
//                    (pi runs `git commit` itself in production
//                    and reports the result; the commit hash is
//                    prepended to the subject line)
//   - Markdown table: "| `f471654` | `feat(feature): helper` |"
//   - Double-backtick code-span: "`` `feat(scope): helper` ``"
//
// We don't strip fences up front (different runs use different
// structures). Instead we iterate every line and for each line,
// try the line itself (after decoration stripping) AND every
// markdown-table cell on that line. The first CC-shaped result
// becomes the subject; everything after that line is the body,
// with any trailing ``` fence trimmed so downstream assertions
// don't see stray fence markers.
func splitCCSubject(text string) (subject, body string, ok bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return "", "", false
	}

	lines := strings.Split(trimmed, "\n")
	for i, line := range lines {
		// Try every "cell" on the line: the line itself, and
		// each markdown-table cell. Table rows contain multiple
		// pipe-separated cells and the CC subject may live in
		// any one of them.
		for _, cell := range splitLineCells(line) {
			candidate := stripMarkdownDecorations(strings.TrimSpace(cell))
			candidate = stripHashPrefix(candidate)
			if conventionalCommitsTitle(candidate) {
				subject = candidate
				if i+1 < len(lines) {
					body = strings.TrimSpace(strings.Join(lines[i+1:], "\n"))
					// Strip any trailing ``` fence so the body
					// downstream assertions inspect doesn't carry
					// the closing marker. Mirrors parsePRReply's
					// "stop at first closing fence" behaviour.
					body = stripTrailingFence(body)
				}
				return subject, body, true
			}
		}
	}
	return "", "", false
}

// splitLineCells returns the line itself as the first element,
// followed by each pipe-separated markdown-table cell (if any).
// Used so the CC-subject search tries the whole line and every
// table cell — a CC subject inside `| `feat(feature): helper` |`
// only surfaces when we look at the cell, not the whole row.
func splitLineCells(line string) []string {
	cells := []string{line}
	if strings.Contains(line, "|") {
		// Split on '|' and trim each cell. Empty cells from
		// leading/trailing pipes are dropped.
		for _, c := range strings.Split(line, "|") {
			c = strings.TrimSpace(c)
			if c != "" {
				cells = append(cells, c)
			}
		}
	}
	return cells
}

// stripMarkdownDecorations strips matched pairs of leading /
// trailing backticks and leading / trailing markdown table
// pipe characters, iterating until the string is stable. Used
// to lift the CC subject out of cells like `` `feat(x): y` ``,
// `` ``feat(x): y`` ``, `| `abc1234` | `feat(x): y` |`, etc.
//
// Iteration is necessary because backticks and pipes can
// interleave: `| `feat(x): y` |` requires stripping the outer
// pipes first, then the inner backticks. A single-pass
// implementation (whichever decoration is stripped first wins)
// misses this shape, so we loop until no decoration changes.
func stripMarkdownDecorations(line string) string {
	s := line
	for i := 0; i < 6; i++ { // 6 is a generous cap; pathological inputs bail out without spinning.
		before := s
		s = strings.TrimSpace(s)
		// Strip matched pairs of leading / trailing backticks.
		if len(s) >= 2 && s[0] == '`' && s[len(s)-1] == '`' {
			s = s[1 : len(s)-1]
		}
		// Strip a single leading / trailing pipe (markdown
		// table cell edge).
		s = strings.TrimPrefix(s, "|")
		s = strings.TrimSuffix(s, "|")
		s = strings.TrimSpace(s)
		if s == before {
			break
		}
	}
	return s
}

// stripTrailingFence removes lines that are exactly ``` (with
// optional leading whitespace) from the START of body — these
// are the closing fences of the message block the LLM wrapped
// the commit subject in, not content. After this strip the
// remaining body is the prose body the LLM wrote around the
// fenced subject.
//
// Note: we do NOT truncate at the FIRST ``` substring like
// parsePRReply does. parsePRReply uses truncation because the
// daemon's contract is "ONE fenced block, parser stops at the
// closing fence". Here the body is post-extraction prose and
// we want to keep as much of it as possible; only the fence
// line itself is noise.
func stripTrailingFence(body string) string {
	if !strings.Contains(body, "```") {
		return body
	}
	lines := strings.Split(body, "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "```" {
		lines = lines[1:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// stripHashPrefix removes a leading short / full SHA-1 (7-40
// hex chars) followed by whitespace, if present. Used to parse
// the LLM's commit-hash-prefixed status lines ("950abe5 feat:
// ...") into a CC subject.
func stripHashPrefix(line string) string {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return line
	}
	if !looksLikeSHA(fields[0]) {
		return line
	}
	return strings.TrimSpace(line[len(fields[0]):])
}

// looksLikeSHA reports whether s is a hex string of 7-40 chars
// (git's short-SHA floor is 4 but 7 is the unambiguous minimum;
// 40 is full SHA-1). Used to detect commit-hash prefixes on
// status-report lines.
func looksLikeSHA(s string) bool {
	if len(s) < 7 || len(s) > 40 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// ccType is defined in testhelpers_realpi_test.go (shared by
// both real-pi smokes).

// -----------------------------------------------------------------------------
// splitCCSubject unit tests
//
// Pure-Go tests for the LLM-output parser. No subprocess, no
// network — these run on every `go test ./internal/command/gtw`
// invocation and lock the parser's behaviour against the four
// real-world LLM shapes observed during smoke runs:
//
//   - Plain subject (no decorations).
//   - Markdown-table row with backticked subject in a cell.
//   - Double-backtick code-span wrapping.
//   - Trailing ``` fence in the body.
//
// Regression guards for the /code-review findings on this file:
//   - F1: table cell with backticks must yield a CC subject.
//   - F2: double-backtick code-span must yield a CC subject.
//   - F3: trailing fence must not leak into the body string.
// -----------------------------------------------------------------------------

func TestSplitCCSubject_PlainSubject(t *testing.T) {
	in := "feat(scope): add the helper\n\nThis is the body."
	gotSub, gotBody, ok := splitCCSubject(in)
	if !ok {
		t.Fatalf("ok=false; expected true. input=%q", in)
	}
	if gotSub != "feat(scope): add the helper" {
		t.Errorf("subject=%q, want %q", gotSub, "feat(scope): add the helper")
	}
	if !strings.Contains(gotBody, "This is the body.") {
		t.Errorf("body=%q, want it to contain body text", gotBody)
	}
}

func TestSplitCCSubject_MarkdownTableCell(t *testing.T) {
	// F1 regression: the table cell shape from /code-review
	// finding #1. The CC subject lives inside a backticked cell
	// of a markdown table row, prefixed by a hash.
	in := "| `f471654` | `feat(feature): helper` |"
	gotSub, _, ok := splitCCSubject(in)
	if !ok {
		t.Fatalf("ok=false on table-cell shape (F1 regression); input=%q", in)
	}
	if gotSub != "feat(feature): helper" {
		t.Errorf("subject=%q, want %q", gotSub, "feat(feature): helper")
	}
}

func TestSplitCCSubject_DoubleBacktickCodeSpan(t *testing.T) {
	// F2 regression: double-backtick code-span. The previous
	// stripMarkdownDecorations only stripped one pair; this
	// test fails if the implementation regresses to single-pass.
	in := "``feat(scope): helper``"
	gotSub, _, ok := splitCCSubject(in)
	if !ok {
		t.Fatalf("ok=false on double-backtick shape (F2 regression); input=%q", in)
	}
	if gotSub != "feat(scope): helper" {
		t.Errorf("subject=%q, want %q", gotSub, "feat(scope): helper")
	}
}

func TestSplitCCSubject_HashPrefixed(t *testing.T) {
	// Existing shape: short hash + space + CC subject on its own
	// line (the first observed LLM shape, run #1).
	in := "950abe5 feat: add ComputeSquare helper in feature package"
	gotSub, _, ok := splitCCSubject(in)
	if !ok {
		t.Fatalf("ok=false on hash-prefixed shape; input=%q", in)
	}
	if gotSub != "feat: add ComputeSquare helper in feature package" {
		t.Errorf("subject=%q, want hash stripped", gotSub)
	}
}

func TestSplitCCSubject_TrailingFenceStripped(t *testing.T) {
	// F3 regression: a fenced commit message whose block is
	// followed by more prose must drop the closing ``` line
	// (and any leading whitespace lines after it) so the body
	// returned to the caller contains the actual prose body,
	// not the fence marker.
	in := "feat(scope): add helper\n\nBody paragraph."
	gotSub, gotBody, ok := splitCCSubject(in)
	if !ok {
		t.Fatalf("ok=false on plain shape; input=%q", in)
	}
	if gotSub != "feat(scope): add helper" {
		t.Errorf("subject=%q", gotSub)
	}
	if strings.Contains(gotBody, "```") {
		t.Errorf("body=%q contains stray fence marker", gotBody)
	}

	// The fence-strip itself: a body that begins with ```
	// (a closing fence the LLM emitted right after the fenced
	// subject) must drop the fence and keep the prose after it.
	if got := stripTrailingFence("```\n\nReal body text."); got != "Real body text." {
		t.Errorf("stripTrailingFence did not strip leading fence; got=%q", got)
	}
	// Idempotent on fence-free body.
	if got := stripTrailingFence("Just a normal body."); got != "Just a normal body." {
		t.Errorf("stripTrailingFence altered fence-free body; got=%q", got)
	}
	// Multiple consecutive fence lines at the start (LLM
	// sometimes emits two — the closing fence of one block
	// and the opening fence of the next).
	if got := stripTrailingFence("```\n```\n\nReal body."); got != "Real body." {
		t.Errorf("stripTrailingFence did not strip multi-fence; got=%q", got)
	}
}

func TestSplitCCSubject_PreambleIgnored(t *testing.T) {
	// Preamble prose ("Done. Committed:") must not prevent
	// extraction when the actual subject appears later.
	in := "Done. Here's what I did:\n\nfeat(scope): the actual subject\n\nWith a body."
	gotSub, _, ok := splitCCSubject(in)
	if !ok {
		t.Fatalf("ok=false on preamble shape; input=%q", in)
	}
	if gotSub != "feat(scope): the actual subject" {
		t.Errorf("subject=%q", gotSub)
	}
}

func TestSplitCCSubject_RejectsNonCC(t *testing.T) {
	// A plain-text reply with no CC subject anywhere must
	// return ok=false. This is the fail-closed behaviour for
	// any future LLM shape the parser doesn't recognise.
	in := "I made the changes and pushed them."
	_, _, ok := splitCCSubject(in)
	if ok {
		t.Fatalf("ok=true on non-CC input; expected false. input=%q", in)
	}
}

func TestStripMarkdownDecorations_DoublePairStrips(t *testing.T) {
	// Direct unit test of the doc-vs-impl gap that F2 caught.
	// The function's docstring promises "one or two leading /
	// trailing backticks" but the implementation broke after
	// one pass. This test locks the iterative behaviour in.
	//
	// Note: stripMarkdownDecorations is only called on single
	// markdown-table cells by splitCCSubject (which splits the
	// line into cells first via splitLineCells). Whole-line
	// markdown-table inputs belong to splitCCSubject's test,
	// not this one — pass cells here.
	cases := []struct{ in, want string }{
		{"`feat(x): y`", "feat(x): y"},
		{"``feat(x): y``", "feat(x): y"},
		{"```feat(x): y```", "feat(x): y"},
		{"feat(x): y", "feat(x): y"}, // no decoration
		{"  `feat(x): y`  ", "feat(x): y"}, // with whitespace
		{"`abc1234`", "abc1234"},          // hash cell
		{"|", ""},                          // degenerate input
	}
	for _, c := range cases {
		if got := stripMarkdownDecorations(c.in); got != c.want {
			t.Errorf("stripMarkdownDecorations(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}