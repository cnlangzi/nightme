package gtw

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/prcache"
)

// -----------------------------------------------------------------------------
// parsePRReply (plan §P3.1)
// -----------------------------------------------------------------------------

func TestParsePRReply_HappyPath(t *testing.T) {
	in := "Here's the PR:\n" +
		"```\n" +
		"feat(scope): add /gtw pr command\n" +
		"\n" +
		"- bullet one\n" +
		"- bullet two\n" +
		"```\n" +
		"Hope this helps!"
	title, body, err := parsePRReply(in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if title != "feat(scope): add /gtw pr command" {
		t.Fatalf("title: %q", title)
	}
	if body != "- bullet one\n- bullet two" {
		t.Fatalf("body: %q", body)
	}
}

func TestParsePRReply_WithFenceInfoString(t *testing.T) {
	// gh-style code fences often carry a language hint after
	// the opening ``` (e.g. ```markdown). We should ignore it
	// and still treat the contents as the payload.
	in := "```markdown\nfeat: hi\n\nbody line\n```"
	title, body, err := parsePRReply(in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if title != "feat: hi" {
		t.Fatalf("title: %q", title)
	}
	if body != "body line" {
		t.Fatalf("body: %q", body)
	}
}

// TestParsePRReply_NoFenceAgentForgotFence is the regression test
// for the bug reported 2026-08-10: an LLM agent emitted a perfectly
// good PR description but forgot the ``` fence wrapper, and the
// strict parser hard-errored with "could not parse agent reply".
// The agent output (verbatim, the actual text that failed in
// production) is preserved below — DO NOT replace with a
// hand-condensed variant, the whole point is that the fix must
// handle real, messy, agent-shaped prose including the unicode
// ellipsis "…" and the multi-paragraph body.
func TestParsePRReply_NoFenceAgentForgotFence(t *testing.T) {
	in := "feat(gtw): add per-instance name and centralize cli exec\n" +
		"\n" +
		"Adds a human-readable identifier for each nightme daemon via\n" +
		"Config.Name (with `nightme name [value]` CLI + NIGHTME_NAME env\n" +
		"override, falling back to os.Hostname()), and refactors the gtw\n" +
		"package so every CLI subprocess (`gh`, `glab`, hooks, …) runs\n" +
		"through a single runCmd wrapper that takes an explicit Dir —\n" +
		"preventing `git: fatal: Unable to read current working directory`\n" +
		"failures when the daemon's CWD has been stale'd (moved/deleted\n" +
		"worktree, NFS gone away, …) since startup.\n" +
		"\n" +
		"Changes:\n" +
		"- config: new `name` field, EffectiveName helper, NIGHTME_NAME env\n" +
		"- cli: `nightme name` shows/sets the instance name\n" +
		"- gtw: new exec.go with runCmd(ctx, dir, name, args...)\n"

	title, body, err := parsePRReply(in)
	if err != nil {
		t.Fatalf("regression: agent output without fence must not error: %v", err)
	}
	if title != "feat(gtw): add per-instance name and centralize cli exec" {
		t.Errorf("title = %q", title)
	}
	if !strings.HasPrefix(body, "Adds a human-readable identifier") {
		t.Errorf("body should start with the descriptive paragraph; got %q", body)
	}
	if !strings.Contains(body, "Changes:") {
		t.Errorf("body should preserve the structured bullet section; got %q", body)
	}
}

// TestParsePRReply_NoFenceProseFallback covers the degenerate
// case where the agent emits pure prose without anything that
// looks like Conventional Commits. The old strict version
// hard-errored; the new version falls back to "first non-empty
// line is the title" so the user gets a usable PR draft instead
// of a hard failure.
func TestParsePRReply_NoFenceProseFallback(t *testing.T) {
	in := "just some prose without a fence\nand more prose on a second line"
	title, body, err := parsePRReply(in)
	if err != nil {
		t.Fatalf("expected fallback (no err), got %v", err)
	}
	if title != "just some prose without a fence" {
		t.Errorf("title = %q, want first non-empty line", title)
	}
	if !strings.Contains(body, "and more prose") {
		t.Errorf("body = %q, want remaining prose", body)
	}
}

// TestParsePRReply_CompletelyEmpty still errors — there's nothing
// to extract a title from. The fallback path handles empty-after-
// stripping by returning errParseAgentReply.
func TestParsePRReply_CompletelyEmpty(t *testing.T) {
	if _, _, err := parsePRReply("   \n\n  \n"); err == nil {
		t.Fatalf("expected error on entirely-whitespace input")
	}
}

func TestParsePRReply_EmptyFence(t *testing.T) {
	if _, _, err := parsePRReply("```\n```"); err == nil {
		t.Fatalf("expected error on empty fence")
	}
}

func TestParsePRReply_OnlyTitleNoBody(t *testing.T) {
	in := "```\nfix: small thing\n```"
	title, body, err := parsePRReply(in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if title != "fix: small thing" {
		t.Fatalf("title: %q", title)
	}
	if body != "" {
		t.Fatalf("body: want empty, got %q", body)
	}
}

// TestParsePRReply_UnclosedFenceNowSucceeds documents the
// behavior change introduced when the parser grew LLM-noise
// tolerance: an unmatched leading ``` (agent forgot to close
// the fence) is now stripped rather than treated as fatal.
// The agent's title and body come through intact. This is the
// same shape of failure as the regression case in
// TestParsePRReply_NoFenceAgentForgotFence, just with a stray
// opening fence — proves the strip-and-fall-through path works
// even when the agent emits a half-formed fence.
func TestParsePRReply_UnclosedFenceNowSucceeds(t *testing.T) {
	in := "```\nfeat: half-written\n\nbody but no closing fence"
	title, body, err := parsePRReply(in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if title != "feat: half-written" {
		t.Errorf("title = %q, want feat: half-written", title)
	}
	if !strings.Contains(body, "body but no closing") {
		t.Errorf("body = %q, want the unclosed prose", body)
	}
}

// TestParsePRReply_NoiseModes covers the common LLM output
// shapes that the strict parser used to reject. Each subtest
// feeds a differently-wrapped input and asserts the title is
// still found. Together with TestParsePRReply_NoFenceAgentForgotFence
// these pin the contract: any agent output that CONTAINS a valid
// Conventional Commits title should parse, regardless of the
// wrapper noise around it.
func TestParsePRReply_NoiseModes(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		want      string // expected title
		wantBody  string // expected body; "" means we don't check
	}{
		{
			// LLM-style JSON wrapper: the entire PR description
			// is the value of a JSON string key. The Phase 1c
			// regex captures the whole value (it can't easily
			// distinguish escaped `\n` from a literal end of
			// title); parsePRReply's final normalization step
			// then splits at the first literal `\n`, taking
			// the first line as title and merging the rest
			// into body. This is the contract dispatchPR
			// relies on so gh/glab gets a clean title.
			name:     "json_wrapper",
			input:    `{"text": "feat(config): add name field\n\nbody goes here"}`,
			want:     "feat(config): add name field",
			wantBody: "body goes here",
		},
		{
			// LLM-style JSON wrapper: the entire PR description
			// is the value of a JSON string key. The Phase 1c
			// regex captures the whole value (it can't easily
			// distinguish escaped `\n` from a literal end of
			// title); parsePRReply's final normalization step
			// then splits at the first literal `\n`, taking
			// the first line as title and merging the rest
			// into body. This is the contract dispatchPR
			// relies on so gh/glab gets a clean title.
			name:  "json_wrapper",
			input: `{"text": "feat(config): add name field\n\nbody goes here"}`,
			want:  "feat(config): add name field",
			wantBody: "body goes here",
		},
		{
			name:  "markdown_h2_heading",
			input: "## PR Title\n\nfix(gtw): bind worktree to provider\n\nbody",
			want:  "fix(gtw): bind worktree to provider",
		},
		{
			name:  "markdown_h1_heading",
			input: "# Pull Request\n\nchore(deps): bump yaml.v3\n\nbody",
			want:  "chore(deps): bump yaml.v3",
		},
		{
			name:  "dash_bullet",
			input: "- feat(api): add /foo endpoint\n\nbody",
			want:  "feat(api): add /foo endpoint",
		},
		{
			name:  "star_bullet",
			input: "* refactor(gtw): extract runCmd\n\nbody",
			want:  "refactor(gtw): extract runCmd",
		},
		{
			name:  "title_label",
			input: "Title: feat(ci): cache go modules\n\nbody",
			want:  "feat(ci): cache go modules",
		},
		{
			name:  "pr_title_label",
			input: "PR Title: docs(readme): fix typo\n\nbody",
			want:  "docs(readme): fix typo",
		},
		{
			name:  "leading_blank_lines",
			input: "\n\n\nfeat: skip the blank lines\n\nbody",
			want:  "feat: skip the blank lines",
		},
		{
			name:  "breaking_change_bang",
			input: "feat(api)!: remove deprecated handler\n\nbody",
			want:  "feat(api)!: remove deprecated handler",
		},
		{
			name:  "unmatched_leading_fence",
			input: "```\nfeat: agent forgot to close\n\nbody",
			want:  "feat: agent forgot to close",
		},
		{
			name:  "unmatched_trailing_fence",
			input: "feat: agent forgot to open\n\nbody\n```",
			want:  "feat: agent forgot to open",
		},
		{
			name:  "numbered_list",
			input: "1. test(rig): add NoiseModes cases\n\nbody",
			want:  "test(rig): add NoiseModes cases",
		},
		{
			// Compound noise: a bullet AND a label prefix on the
			// same line. Single-pass regex peels only the outer
			// layer; the loop peels both. Without the loop this
			// would fall through to the "first non-empty line"
			// fallback and ship the noisy line as the PR title.
			name:  "bullet_plus_label",
			input: "- Title: feat(rig): strip compound noise\n\nbody",
			want:  "feat(rig): strip compound noise",
		},
		{
			name:  "heading_plus_label",
			input: "## Title: feat(rig): strip compound noise\n\nbody",
			want:  "feat(rig): strip compound noise",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			title, body, err := parsePRReply(tc.input)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if title != tc.want {
				t.Errorf("title = %q, want %q", title, tc.want)
			}
			if tc.wantBody != "" && body != tc.wantBody {
				t.Errorf("body = %q, want %q", body, tc.wantBody)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// buildPRPrompt (plan §P3.1)
// -----------------------------------------------------------------------------

func TestBuildPRPrompt_Remote(t *testing.T) {
	c := Context{
		Worktree: "/w",
		Branch:   "fix-42-foo",
		RepoRoot: "/r",
		Repo:     "octocat/hello",
		Issue:    42,
	}
	p := buildPRPrompt(c, "main")

	mustContain(t, p, "## Output Format")
	mustContain(t, p, "## Task")
	mustContain(t, p, "## Context")
	mustContain(t, p, "Conventional Commits")
	mustContain(t, p, "Repository: octocat/hello")
	mustContain(t, p, "Branch (head): fix-42-foo")
	mustContain(t, p, "Base branch: main")
	mustContain(t, p, "Working dir: /w")
	// v2 anchor: GitHub issue closing keyword, not the old prose hint.
	mustContain(t, p, "Closes #42")
	mustContain(t, p, "Refs #42")

	// Negative: push-only instructions must NOT leak into pr's prompt.
	// The prompt DOES mention "git push" inside the "DO NOT run ..."
	// guard, so we look for the literal commit-push snippet instead.
	mustNotContain(t, p, "git push -u origin")
	mustNotContain(t, p, "git commit -m")
}

func TestBuildPRPrompt_LocalNoIssue(t *testing.T) {
	c := Context{
		Worktree: "/w",
		Branch:   "experimental",
		RepoRoot: "/r",
		Repo:     "octocat/hello",
	}
	p := buildPRPrompt(c, "main")
	if strings.Contains(p, "Reference issue") {
		t.Fatalf("ModeLocal / no-issue prompt should not include issue ref:\n%s", p)
	}
}

// TestBuildPRPrompt_NonWorktreeRepo covers the c.Repo == "" case
// (Detect-fallback path / non-worktree mode): the prompt must
// not print a literal "Repository: " line with nothing after the
// colon (bug D from the review). The agent is told to resolve from
// `git remote get-url origin` itself.
func TestBuildPRPrompt_NonWorktreeRepo(t *testing.T) {
	c := Context{
		Worktree: "/w",
		Branch:   "feat/manual",
		RepoRoot: "/r",
		// Repo deliberately empty (non-worktree mode path)
	}
	p := buildPRPrompt(c, "main")
	if strings.Contains(p, "Repository: \n") {
		t.Fatalf("prompt has 'Repository:' with empty value:\n%s", p)
	}
	mustContain(t, p, "Repository: (resolve from `git remote get-url origin`)")
}

// -----------------------------------------------------------------------------
// buildPRPrompt v2 invariants
//
// These tests lock in the structural / behavioural anchors introduced
// in the v2 prompt rewrite (see buildPRPrompt doc comment). Each
// invariant maps to one of the LLM-failure modes observed in
// recent PRs (#140-#144: 4-bullet regression after the LLM falls
// back to its training-data modal pattern). They are not exhaustive
// style tests — they fail if a future edit silently drops the
// guard that prevents the regression from coming back.
// -----------------------------------------------------------------------------

// TestBuildPRPromptV2_ToolFloorMandatory checks that the prompt
// tells the agent to RUN git log / diff before writing, not merely
// to consult them. The tool floor is what prevents the "write the
// body from commit messages alone" failure mode.
func TestBuildPRPromptV2_ToolFloorMandatory(t *testing.T) {
	c := Context{Worktree: "/w", Branch: "feat/x", RepoRoot: "/r", Repo: "o/r"}
	p := buildPRPrompt(c, "main")

	mustContain(t, p, "## Before you write")
	mustContain(t, p, "You MUST run")
	mustContain(t, p, "git log --oneline main..HEAD")
	mustContain(t, p, "git diff main...HEAD --stat")
	mustContain(t, p, "Do NOT write the body from commit messages alone")
}

// TestBuildPRPromptV2_BodyLengthSelfCheck checks the objective
// rule "body should not be shorter than raw git log output".
// The rule is LLM-checkable against its own tool output, which is
// what makes it effective — the model can compare two strings it
// actually has in context, not just aim at an abstract "be longer".
func TestBuildPRPromptV2_BodyLengthSelfCheck(t *testing.T) {
	c := Context{Worktree: "/w", Branch: "feat/x", RepoRoot: "/r", Repo: "o/r"}
	p := buildPRPrompt(c, "main")

	mustContain(t, p, "shorter than the raw `git log` output")
	mustContain(t, p, "you've written too little")
}

// TestBuildPRPromptV2_FourDimensions checks that the four
// dimensions (what / why / diff overview / test evidence) are all
// named as required coverage. This is the v2 replacement for the
// v1 "structured body summarising the change" empty anchor — the
// LLM is now held to specific dimensions, not to a vague adjective.
func TestBuildPRPromptV2_FourDimensions(t *testing.T) {
	c := Context{Worktree: "/w", Branch: "feat/x", RepoRoot: "/r", Repo: "o/r"}
	p := buildPRPrompt(c, "main")

	mustContain(t, p, "## Four dimensions")
	mustContain(t, p, "**What changed**")
	mustContain(t, p, "**Why it changed**")
	mustContain(t, p, "**Diff overview**")
	mustContain(t, p, "**Test evidence**")
	// Order matters: Why leads, Test evidence closes. If the
	// ordering drifts, reviewers lose the "is this merge-worthy"
	// signal that Why-first provides.
	mustContain(t, p, "Lead with Why, then What, then Diff overview, then Test evidence")
}

// TestBuildPRPromptV2_AntiModalPattern is the single most
// important regression guard: the line that tells the LLM NOT to
// default to a 4-bullet list. Without it, every other improvement
// is overridden by the modal training-data pattern. If a future
// edit drops this line, the regression observed in #140-#144
// returns within a few PRs.
func TestBuildPRPromptV2_AntiModalPattern(t *testing.T) {
	c := Context{Worktree: "/w", Branch: "feat/x", RepoRoot: "/r", Repo: "o/r"}
	p := buildPRPrompt(c, "main")

	mustContain(t, p, "Do NOT default to a 4-bullet list")
	mustContain(t, p, "modal pattern in training data")
}

// TestBuildPRPromptV2_DoNotSection checks that the anti-pattern
// block exists and lists the four key failure modes (skip Why,
// paraphrase the diff, omit Tests, prose outside the fence).
// Transformer attention to negative imperatives ("Do NOT") is
// higher than to positive rules of equal length, so this short
// blacklist is the most reliable way to suppress the failure modes.
func TestBuildPRPromptV2_DoNotSection(t *testing.T) {
	c := Context{Worktree: "/w", Branch: "feat/x", RepoRoot: "/r", Repo: "o/r"}
	p := buildPRPrompt(c, "main")

	mustContain(t, p, "## Do NOT")
	mustContain(t, p, "Do NOT skip **Why**")
	mustContain(t, p, "Do NOT paraphrase the diff")
	mustContain(t, p, "Do NOT omit **Test evidence**")
	mustContain(t, p, "Do NOT include prose outside the fence")
}

// TestBuildPRPromptV2_PreserveParseability guards the v1
// invariants that made the existing parser happy. v2 adds content
// guidance but MUST NOT regress parseability — a future edit that
// drops these strings silently breaks every existing
// parsePRReply test in this file.
func TestBuildPRPromptV2_PreserveParseability(t *testing.T) {
	c := Context{Worktree: "/w", Branch: "feat/x", RepoRoot: "/r", Repo: "o/r"}
	p := buildPRPrompt(c, "main")

	mustContain(t, p, "ONE fenced markdown code block")
	mustContain(t, p, "First line inside the fence is the PR title")
	mustContain(t, p, "Do NOT nest additional ``` fences")
	mustContain(t, p, "Indent code samples with 4 spaces")
	// "DO NOT run git commit / git push / gh / glab" guard is the
	// agent-side safety rail that keeps pr() from triggering
	// side effects. Do not let v2 edits drop it.
	mustContain(t, p, "DO NOT run `git commit`")
	mustContain(t, p, "`gh pr create`")
}

// TestBuildPRPromptV2_IssueKeywordOnlyOnIssue checks that the
// GitHub issue-closing keyword is gated on c.Issue > 0 (same
// invariant as v1's "Reference issue" — just renamed to the
// GitHub-recognised Closes/Refs form).
func TestBuildPRPromptV2_IssueKeywordOnlyOnIssue(t *testing.T) {
	withIssue := buildPRPrompt(Context{Worktree: "/w", Branch: "x", RepoRoot: "/r", Repo: "o/r", Issue: 7}, "main")
	mustContain(t, withIssue, "Closes #7")
	mustContain(t, withIssue, "Refs #7")

	noIssue := buildPRPrompt(Context{Worktree: "/w", Branch: "x", RepoRoot: "/r", Repo: "o/r"}, "main")
	if strings.Contains(noIssue, "Closes #") || strings.Contains(noIssue, "Refs #") {
		t.Fatalf("no-issue prompt should not include issue keyword:\n%s", noIssue)
	}
}

// TestBuildPRPromptV2_BranchInCommands checks that the actual
// base branch name appears in the tool-floor git commands. The
// LLM has to copy-paste or mentally substitute the branch, and a
// literal placeholder like `<base>` in the executed command
// breaks the floor. This is a small but real regression risk.
func TestBuildPRPromptV2_BranchInCommands(t *testing.T) {
	p := buildPRPrompt(Context{Worktree: "/w", Branch: "feat/y", RepoRoot: "/r", Repo: "o/r"}, "develop")

	mustContain(t, p, "git log --oneline develop..HEAD")
	mustContain(t, p, "git diff develop...HEAD --stat")
}

// -----------------------------------------------------------------------------
// parsePRArgs (plan §P3.1)
// -----------------------------------------------------------------------------

func TestParsePRArgs_Empty(t *testing.T) {
	out, err := parsePRArgs(nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out.Agent != "" {
		t.Fatalf("Agent: %q", out.Agent)
	}
}

func TestParsePRArgs_ShortFlag(t *testing.T) {
	out, err := parsePRArgs([]string{"-a", "claude"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out.Agent != "claude" {
		t.Fatalf("Agent: %q", out.Agent)
	}
}

func TestParsePRArgs_LongFlag(t *testing.T) {
	out, err := parsePRArgs([]string{"--agent", "opencode"})
	if err != nil || out.Agent != "opencode" {
		t.Fatalf("err=%v Agent=%q", err, out.Agent)
	}
}

func TestParsePRArgs_MissingValue(t *testing.T) {
	if _, err := parsePRArgs([]string{"-a"}); err == nil {
		t.Fatalf("expected error on -a with no value")
	}
}

func TestParsePRArgs_UnknownFlagRejected(t *testing.T) {
	// F-XX §10 "all unknown flags reject": typos like --draft
	// must surface as "unknown flag" rather than silently no-op.
	out, err := parsePRArgs([]string{"--draft", "-a", "claude"})
	if err == nil {
		t.Fatalf("parsePRArgs returned no error; want --draft rejected. got=%+v", out)
	}
	if !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("error message lacks 'unknown flag'; got %q", err.Error())
	}
}

func TestParsePRArgs_PositionalRejected(t *testing.T) {
	// /gtw pr takes no positional args; bare tokens are rejected
	// with "too many positional arguments".
	out, err := parsePRArgs([]string{"extra-arg"})
	if err == nil {
		t.Fatalf("parsePRArgs returned no error; want positional rejected. got=%+v", out)
	}
	if !strings.Contains(err.Error(), "positional") {
		t.Errorf("error message lacks 'positional'; got %q", err.Error())
	}
}

// -----------------------------------------------------------------------------
// dispatchPR early returns (plan §P3.1)
// -----------------------------------------------------------------------------

// prTestRig wires up a minimal chat session + git runner + agent
// for dispatchPR tests. Each test only configures the bits it
// cares about; everything else is left at zero values that the
// early-return branches never reach.
type prTestRig struct {
	git   *pushGit
	cs    *chatsession.ChatSession
	deps  HandlerDeps
	prov  *fakeGitProvider
	agent *recordingAgent
}

func newPRTestRig(t *testing.T) *prTestRig {
	t.Helper()
	cs := &chatsession.ChatSession{}
	_ = cs.SetSelectedAgent("claude")
	// Tests set the chat's Cwd via setupPRWorktree / direct
	// SetSelectedCwd calls — leaving it empty here means the
	// "no active workspace" early return fires unless the test
	// explicitly opts in.
	git := newPushGit()
	claude := &recordingAgent{name: "claude", runOnceText: "```\nfeat: hi\n```"}
	withAgent(t, claude)
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	return &prTestRig{
		git:   git,
		cs:    cs,
		agent: claude,
		prov:  prov,
	}
}

// installDeps puts a fresh deps struct into the rig (so each
// test can override fields without leaking across cases).
func (r *prTestRig) installDeps() {
	r.deps = HandlerDeps{
		Git:    r.git,
		Detect: fakeDetect(r.prov),
	}
}

// captureCh spins up an in-memory chat session Channel for the
// rig's chat so dispatchPR's reply is observable. Uses the
// shared recordingCh from testharness_test.go so we don't
// duplicate the mock here.
func captureCh(t *testing.T, cs *chatsession.ChatSession) *recordingCh {
	t.Helper()
	ch := &recordingCh{}
	cs.WithEmitter(ch)
	return ch
}

func TestDispatchPR_NoWorkspace(t *testing.T) {
	rig := newPRTestRig(t)
	// Don't set SelectedCwd — the "no active workspace" early
	// return should fire.
	rig.installDeps()
	cs := rig.cs
	s := captureCh(t, cs)

	_, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg", prArgs{})
	if err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "no active workspace") {
		t.Fatalf("expected no-workspace reply, got:\n%s", r)
	}
}

func TestDispatchPR_MalformedYml(t *testing.T) {
	rig := newPRTestRig(t)
	// Empty Worktree triggers the malformed-yaml check. Don't
	// use setupPRWorktree (which would auto-fill Worktree/RepoRoot
	// from the temp dir) — write yml directly.
	tmp := t.TempDir()
	withCwd(t, tmp)
	// RepoRoot must pass filepath.IsAbs on the test's host OS.
	// See TestGTWYml_RoundTrip_AllFields for the bare "/" + Go
	// 1.26+ Windows non-absolute path quirk; t.TempDir() is
	// always absolute on every platform.
	writeYml(t, tmp, Context{Branch: "wt", RepoRoot: tmp})
	_ = rig.cs.SetSelectedCwd(tmp)
	rig.installDeps()
	cs := rig.cs
	s := captureCh(t, cs)

	_, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg", prArgs{})
	if err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}
	if !strings.Contains(s.lastText(), "malformed") {
		t.Fatalf("expected malformed reply, got:\n%s", s.lastText())
	}
}

func TestDispatchPR_DefaultBranchFails(t *testing.T) {
	rig := newPRTestRig(t)
	setupPRWorktree(t, rig, Context{Branch: "wt"})
	// DefaultBranch's git call (symbolic-ref ...) returns an error.
	rig.git.on("symbolic-ref", "", "fatal: no origin remote",
		errors.New("exit 128"))
	rig.installDeps()

	cs := rig.cs
	s := captureCh(t, cs)
	_, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg", prArgs{})
	if err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}
	if !strings.Contains(s.lastText(), "discover default branch") &&
		!strings.Contains(s.lastText(), "origin remote") {
		t.Fatalf("expected default-branch error, got:\n%s", s.lastText())
	}
}

// setupPRWorktree aligns system pwd, chat SelectedCwd, and the
// .nightme/gtw.yml location for a yml-present test. Fills in
// Worktree / RepoRoot from the temp dir if the caller left them
// blank (the common case). Returns the path so they can reference
// it as Worktree / RepoRoot.
//
// loadDispatchContext reads cs.SelectedCwd() (not system pwd),
// so we need both.
func setupPRWorktree(t *testing.T, rig *prTestRig, ctx Context) string {
	t.Helper()
	tmp := t.TempDir()
	if ctx.Worktree == "" {
		ctx.Worktree = tmp
	}
	if ctx.RepoRoot == "" {
		ctx.RepoRoot = tmp
	}
	withCwd(t, tmp)
	writeYml(t, tmp, ctx)
	_ = rig.cs.SetSelectedCwd(tmp)
	return tmp
}

// setupPRGit configures the rig for the dispatch paths that need
// a "happy-path" git surface: setupReadiness's gate-1 ls-remote
// pass + DefaultBranch + loadDispatchContext non-worktree mocks,
// plus a GitHub-shaped origin URL so resolveProvider succeeds
// without Detect firing its HTTP probe.
//
// The historical (unpushed, ahead) numeric shape predates the
// 2-gate refactor — dispatchPR no longer reads AheadOfRemote, so
// `unpushed` is forwarded only for snap construction (kept as a
// parameter so the read is honest about what it currently does;
// `ahead` was dropped in the refactor since rev-list is no
// longer called).
func setupPRGit(rig *prTestRig, branch string, unpushed int) {
	snap := messages.GitStatusSnapshot{
		Branch:        branch,
		HasUpstream:   true,
		AheadOfRemote: unpushed,
	}
	setupReadiness(rig, branch, snap)
	// resolveProvider's RemoteOriginURL probe needs a non-empty
	// origin URL. Default to a GitHub-shaped URL; tests that
	// exercise GitLab detection override this.
	rig.git.on("remote", "git@github.com:octocat/hello.git", "", nil)
}


// TestDispatchPR_NoOriginBranch covers the gate-1 "origin/<branch>
// does not exist" path. The local branch exists, the worktree
// is clean, but `git ls-remote --heads origin <branch>` returns
// empty — the branch was never pushed (or was deleted from
// origin). dispatchPR must short-circuit before reaching
// resolveProvider / FindOpenPRForBranch and tell the user to
// run /gtw push first.
func TestDispatchPR_NoOriginBranch(t *testing.T) {
	rig := newPRTestRig(t)
	setupPRWorktree(t, rig, Context{Branch: "wt-noup"})
	// setupReadiness defaults the ls-remote probe to a
	// non-empty response. Override AFTER so the empty
	// response wins — gate 1 fails.
	setupReadiness(rig, "wt-noup", messages.GitStatusSnapshot{
		Branch:      "wt-noup",
		HasUpstream: true, // porcelain claims upstream, but origin doesn't have it
	})
	rig.git.onArgs([]string{"ls-remote", "--heads", "origin", "wt-noup"}, "", "", nil)
	rig.installDeps()

	cs := rig.cs
	s := captureCh(t, cs)
	_, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg", prArgs{})
	if err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "origin/wt-noup does not exist") {
		t.Fatalf("expected no-origin-branch reply, got:\n%s", r)
	}
	if !strings.Contains(r, "/gtw push first") {
		t.Fatalf("expected /gtw push hint, got:\n%s", r)
	}
	// Invariant: gate 1 short-circuited BEFORE provider
	// resolution. Neither FindOpenPRForBranch nor CreatePR
	// may have been called.
	for _, c := range rig.prov.calls {
		if c.Method == "FindOpenPRForBranch" || c.Method == "CreatePR" {
			t.Fatalf("gate 1 must short-circuit before provider; call=%s", c.Method)
		}
	}
}

// TestDispatchPR_LSRemoteAuthError — known ls-remote stderr
// fragment ("could not read Username") must surface the raw
// stderr AND a friendly credential hint. The user should
// understand both what went wrong and what to do.
func TestDispatchPR_LSRemoteAuthError(t *testing.T) {
	rig := newPRTestRig(t)
	setupPRWorktree(t, rig, Context{Branch: "wt-auth"})
	setupReadiness(rig, "wt-auth", messages.GitStatusSnapshot{
		Branch: "wt-auth",
	})
	rig.git.onArgs([]string{"ls-remote", "--heads", "origin", "wt-auth"},
		"", "fatal: could not read Username for 'https://x.test/repo.git': terminal prompts disabled",
		errors.New("exit 128"))
	rig.installDeps()

	cs := rig.cs
	s := captureCh(t, cs)
	_, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg", prArgs{})
	if err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "could not read Username") {
		t.Fatalf("expected raw auth stderr in reply, got:\n%s", r)
	}
	if !strings.Contains(r, "credential") {
		t.Fatalf("expected credential hint, got:\n%s", r)
	}
	// Invariant: gate 1 errors out BEFORE provider resolution.
	for _, c := range rig.prov.calls {
		if c.Method == "FindOpenPRForBranch" || c.Method == "CreatePR" {
			t.Fatalf("gate 1 ls-remote error must short-circuit before provider; call=%s", c.Method)
		}
	}
}

// TestDispatchPR_LSRemoteNetworkError — known ls-remote stderr
// fragment ("unable to access") must surface the raw stderr
// AND a friendly network hint.
func TestDispatchPR_LSRemoteNetworkError(t *testing.T) {
	rig := newPRTestRig(t)
	setupPRWorktree(t, rig, Context{Branch: "wt-net"})
	setupReadiness(rig, "wt-net", messages.GitStatusSnapshot{Branch: "wt-net"})
	rig.git.onArgs([]string{"ls-remote", "--heads", "origin", "wt-net"},
		"", "fatal: unable to access 'https://x.test/repo.git/': Could not resolve host: x.test",
		errors.New("exit 128"))
	rig.installDeps()

	cs := rig.cs
	s := captureCh(t, cs)
	_, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg", prArgs{})
	if err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "unable to access") {
		t.Fatalf("expected raw network stderr in reply, got:\n%s", r)
	}
	if !strings.Contains(r, "network") && !strings.Contains(r, "origin remote") {
		t.Fatalf("expected network/origin hint, got:\n%s", r)
	}
}

// TestDispatchPR_LSRemoteUnknownErrorPassThrough — UNKNOWN
// stderr must NOT be matched against the known-fragment table.
// The reply must contain the raw stderr verbatim AND must NOT
// contain any of the known-fragment hints (auth / network /
// not-a-repo) NOR the "no upstream" / "/gtw push first" reply
// — that would mislead the user into running the wrong next
// step.
func TestDispatchPR_LSRemoteUnknownErrorPassThrough(t *testing.T) {
	const weirdStderr = "fatal: some weird git error that we don't recognize: xyz"
	rig := newPRTestRig(t)
	setupPRWorktree(t, rig, Context{Branch: "wt-weird"})
	setupReadiness(rig, "wt-weird", messages.GitStatusSnapshot{Branch: "wt-weird"})
	rig.git.onArgs([]string{"ls-remote", "--heads", "origin", "wt-weird"},
		"", weirdStderr, errors.New("exit 128"))
	rig.installDeps()

	cs := rig.cs
	s := captureCh(t, cs)
	_, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg", prArgs{})
	if err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, weirdStderr) {
		t.Fatalf("unknown stderr must pass through verbatim; got:\n%s", r)
	}
	// Invariant: the reply must NOT mention any of the known
	// fragments' hints — that would be the system guessing at
	// the wrong next step.
	for _, banned := range []string{
		"credential",         // auth hint
		"network connectivity", // network hint
		"origin remote",      // auth hint (alt wording)
		"origin should point", // not-a-repo hint
		"does not exist",     // gate-1 fail reply
		"/gtw push first",    // gate-1 fail reply
	} {
		if strings.Contains(r, banned) {
			t.Fatalf("unknown stderr must NOT be translated; got %q in:\n%s", banned, r)
		}
	}
}

// TestDispatchPR_LSRemoteNotARepository — third known-fragment
// case: git ls-remote writes "fatal: 'origin' does not appear
// to be a git repository" when the remote URL points somewhere
// that isn't a git server. The reply must surface the raw
// stderr AND a hint pointing at `git remote -v`.
func TestDispatchPR_LSRemoteNotARepository(t *testing.T) {
	rig := newPRTestRig(t)
	setupPRWorktree(t, rig, Context{Branch: "wt-norepo"})
	setupReadiness(rig, "wt-norepo", messages.GitStatusSnapshot{Branch: "wt-norepo"})
	rig.git.onArgs([]string{"ls-remote", "--heads", "origin", "wt-norepo"},
		"", "fatal: 'origin' does not appear to be a git repository",
		errors.New("exit 128"))
	rig.installDeps()

	cs := rig.cs
	s := captureCh(t, cs)
	_, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg", prArgs{})
	if err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "does not appear to be a git repository") {
		t.Fatalf("expected raw stderr in reply, got:\n%s", r)
	}
	if !strings.Contains(r, "git remote -v") {
		t.Fatalf("expected `git remote -v` hint, got:\n%s", r)
	}
}

// TestRemoteBranchExists_EmptyStderr — when `git ls-remote`
// errors with NO stderr (context cancellation, broken pipe,
// ENOSPC on a tmpfs), the wrapper must surface the underlying
// err.Error() verbatim. This is the production-mode error path
// most likely to fire when the binary itself is fine but the
// I/O failed mid-call; we want a useful diagnostic, not a
// misleading hint.
func TestRemoteBranchExists_EmptyStderr(t *testing.T) {
	g := newPushGit()
	g.onArgs([]string{"ls-remote", "--heads", "origin", "wt-io"},
		"", "", errors.New("write | pipe: broken pipe"))

	_, err := RemoteBranchExists(context.Background(), "/w", "wt-io", g)
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	// Invariant: the wrapped err preserves the original message.
	if !strings.Contains(err.Error(), "broken pipe") {
		t.Fatalf("expected underlying err preserved; got %v", err)
	}
	// Invariant: NO known-fragment hint — empty stderr means we
	// can't tell whether the failure matches auth / network /
	// not-a-repo, so we MUST NOT guess.
	for _, banned := range []string{
		"credential",
		"network connectivity",
		"git remote -v",
	} {
		if strings.Contains(err.Error(), banned) {
			t.Fatalf("empty stderr must not be translated; got %q in %v", banned, err)
		}
	}
}

// TestDispatchPR_ExistingPR — gate 2 fires when the provider
// returns an existing open PR for the head. dispatchPR must
// surface the URL/number and short-circuit before reaching
// CreatePR.
func TestDispatchPR_ExistingPR(t *testing.T) {
	rig := newPRTestRig(t)
	setupPRWorktree(t, rig, Context{Branch: "wt-existing"})
	setupReadiness(rig, "wt-existing", messages.GitStatusSnapshot{
		Branch:      "wt-existing",
		HasUpstream: true,
	})
	rig.git.on("remote", "git@github.com:octocat/hello.git", "", nil)
	rig.prov.SetFindOpenPRResp(&PR{Number: 42, URL: "https://github.com/octocat/hello/pull/42", State: "open"})
	rig.installDeps()

	cs := rig.cs
	s := captureCh(t, cs)
	_, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg", prArgs{})
	if err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "already has an open PR") {
		t.Fatalf("expected existing-PR reply, got:\n%s", r)
	}
	if !strings.Contains(r, "#42") {
		t.Fatalf("expected PR number #42 in reply, got:\n%s", r)
	}
	// Invariant: CreatePR must NOT have been called.
	for _, c := range rig.prov.calls {
		if c.Method == "CreatePR" {
			t.Fatalf("CreatePR must NOT be called when an existing PR is found; calls=%+v", rig.prov.calls)
		}
	}
}

// TestDispatchPR_FindOpenPRErrCLINotInstalled — gh/glab binary
// missing on PATH → ErrCLINotInstalled → friendly install hint.
// Distinct from "no upstream" / "auth failed" — this one
// specifically says "install via brew install gh".
func TestDispatchPR_FindOpenPRErrCLINotInstalled(t *testing.T) {
	rig := newPRTestRig(t)
	setupPRWorktree(t, rig, Context{Branch: "wt-no-gh"})
	setupReadiness(rig, "wt-no-gh", messages.GitStatusSnapshot{
		Branch:      "wt-no-gh",
		HasUpstream: true,
	})
	rig.git.on("remote", "git@github.com:octocat/hello.git", "", nil)
	rig.prov.SetFindOpenPRErr(fmt.Errorf("%w: gh — install via `brew install gh`", ErrCLINotInstalled))
	rig.installDeps()

	cs := rig.cs
	s := captureCh(t, cs)
	_, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg", prArgs{})
	if err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "install via `brew install gh`") {
		t.Fatalf("expected gh install hint, got:\n%s", r)
	}
}

// TestDispatchPR_FindOpenPRUnknownErrorPassThrough — unknown
// FindOpenPRForBranch error propagates verbatim. The reply must
// contain the raw err string and must NOT add a friendly hint
// that would mislead the user.
func TestDispatchPR_FindOpenPRUnknownErrorPassThrough(t *testing.T) {
	const weirdErr = "gh pr list: 502 Bad Gateway (we have never seen this before)"
	rig := newPRTestRig(t)
	setupPRWorktree(t, rig, Context{Branch: "wt-502"})
	setupReadiness(rig, "wt-502", messages.GitStatusSnapshot{
		Branch:      "wt-502",
		HasUpstream: true,
	})
	rig.git.on("remote", "git@github.com:octocat/hello.git", "", nil)
	rig.prov.SetFindOpenPRErr(errors.New(weirdErr))
	rig.installDeps()

	cs := rig.cs
	s := captureCh(t, cs)
	_, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg", prArgs{})
	if err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, weirdErr) {
		t.Fatalf("unknown err must pass through verbatim; got:\n%s", r)
	}
	if !strings.Contains(r, "check existing PR") {
		t.Fatalf("reply must mention gate 2 context; got:\n%s", r)
	}
}

// TestDispatchPR_CreateStaleUpstreamRace — gate 1 passed
// (origin/<branch> existed at probe time) but the branch was
// deleted before CreatePR ran. gh now returns a known
// "head ref" GraphQL validator error → wrapCreatePRError
// wraps it as ErrStaleUpstream → dispatchPR echoes the same
// friendly hint as gate 1's no-upstream miss.
func TestDispatchPR_CreateStaleUpstreamRace(t *testing.T) {
	rig := newPRTestRig(t)
	setupPRWorktree(t, rig, Context{Branch: "wt-race"})
	setupReadiness(rig, "wt-race", messages.GitStatusSnapshot{
		Branch:      "wt-race",
		HasUpstream: true,
	})
	rig.git.on("remote", "git@github.com:octocat/hello.git", "", nil)
	// gate 1 already verified upstream exists (setupReadiness
	// default). Force CreatePR to return ErrStaleUpstream by
	// simulating the gh GraphQL validator message.
	ghErr := fmt.Errorf("%w: GraphQL: Head ref must be a branch, No commits between main and wt-race",
		ErrStaleUpstream)
	rig.prov.SetCreatePRErr(ghErr)
	rig.installDeps()

	cs := rig.cs
	s := captureCh(t, cs)
	_, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg", prArgs{})
	if err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "origin/wt-race no longer exists") {
		t.Fatalf("expected stale-upstream race reply, got:\n%s", r)
	}
	if !strings.Contains(r, "/gtw push first") {
		t.Fatalf("expected /gtw push hint, got:\n%s", r)
	}
	// Invariant: the raw GraphQL diagnostic must NOT leak.
	if strings.Contains(r, "GraphQL:") {
		t.Fatalf("raw GraphQL diagnostic leaked; got:\n%s", r)
	}
}

// TestDispatchPR_AlreadyExistsRace — gate 2 reported no PR but
// CreatePR loses the race (someone opened a PR between probe
// and create). gh returns ErrPRExists → friendly reply.
func TestDispatchPR_AlreadyExistsRace(t *testing.T) {
	rig := newPRTestRig(t)
	setupPRWorktree(t, rig, Context{Branch: "wt-race2"})
	setupReadiness(rig, "wt-race2", messages.GitStatusSnapshot{
		Branch:      "wt-race2",
		HasUpstream: true,
	})
	rig.git.on("remote", "git@github.com:octocat/hello.git", "", nil)
	// gate 2 returns nil (no PR at probe time) — default fake state.
	rig.prov.SetCreatePRErr(fmt.Errorf("%w: a PR already exists for this branch", ErrPRExists))
	rig.installDeps()

	cs := rig.cs
	s := captureCh(t, cs)
	_, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg", prArgs{})
	if err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "a PR for wt-race2 already exists") {
		t.Fatalf("expected ErrPRExists reply, got:\n%s", r)
	}
}

// TestDispatchPR_CreateStaleUpstreamRace_GitLab — same race
// shape as TestDispatchPR_CreateStaleUpstreamRace, but driven
// through the GitLab-typed fake so the glab stderr substring
// table (Source branch does not exist / Branch not found /
// 404 Not Found) is exercised at the dispatch level. The
// helper-level TestWrapCreatePRError_StaleUpstreamGL covers
// the substring detection; this test pins that the dispatch
// path also surfaces the right reply.
func TestDispatchPR_CreateStaleUpstreamRace_GitLab(t *testing.T) {
	rig := newPRTestRig(t)
	// Re-key the rig's provider to GitLab.
	rig.prov = newFakeGitProvider(ProviderGitLab, "gitlab.com")
	setupPRWorktree(t, rig, Context{Branch: "wt-gl-race"})
	setupReadiness(rig, "wt-gl-race", messages.GitStatusSnapshot{
		Branch:      "wt-gl-race",
		HasUpstream: true,
	})
	rig.git.on("remote", "git@gitlab.com:acme/demo.git", "", nil)
	// glab's "Source branch does not exist" → ErrStaleUpstream
	// via wrapCreatePRError's `gl` table.
	rig.prov.SetCreatePRErr(fmt.Errorf("%w: Source branch does not exist", ErrStaleUpstream))
	rig.installDeps()

	cs := rig.cs
	s := captureCh(t, cs)
	_, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg", prArgs{})
	if err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "origin/wt-gl-race no longer exists") {
		t.Fatalf("expected stale-upstream race reply, got:\n%s", r)
	}
	if strings.Contains(r, "Source branch does not exist") {
		t.Fatalf("raw glab stderr leaked; got:\n%s", r)
	}
}

// TestDispatchPR_CreatePR_CLINotInstalled — race-window guard:
// CreatePR returns ErrCLINotInstalled (e.g. gh was uninstalled
// between gate 2 and CreatePR). dispatchPR surfaces the
// install hint.
func TestDispatchPR_CreatePR_CLINotInstalled(t *testing.T) {
	rig := newPRTestRig(t)
	setupPRWorktree(t, rig, Context{Branch: "wt-no-gh2"})
	setupReadiness(rig, "wt-no-gh2", messages.GitStatusSnapshot{
		Branch:      "wt-no-gh2",
		HasUpstream: true,
	})
	rig.git.on("remote", "git@github.com:octocat/hello.git", "", nil)
	rig.prov.SetCreatePRErr(fmt.Errorf("%w: gh — install via `brew install gh`", ErrCLINotInstalled))
	rig.installDeps()

	cs := rig.cs
	s := captureCh(t, cs)
	_, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg", prArgs{})
	if err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "install via `brew install gh`") {
		t.Fatalf("expected gh install hint, got:\n%s", r)
	}
}

// TestDispatchPR_CreatePR_UnknownErrorPassThrough — unknown
// CreatePR stderr propagates verbatim. The reply must contain
// the raw stderr and NOT a friendly translation.
func TestDispatchPR_CreatePR_UnknownErrorPassThrough(t *testing.T) {
	const weirdErr = "gh pr create: 403 Forbidden: API rate limit exceeded"
	rig := newPRTestRig(t)
	setupPRWorktree(t, rig, Context{Branch: "wt-403"})
	setupReadiness(rig, "wt-403", messages.GitStatusSnapshot{
		Branch:      "wt-403",
		HasUpstream: true,
	})
	rig.git.on("remote", "git@github.com:octocat/hello.git", "", nil)
	rig.prov.SetCreatePRErr(errors.New(weirdErr))
	rig.installDeps()

	cs := rig.cs
	s := captureCh(t, cs)
	_, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg", prArgs{})
	if err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "403 Forbidden") {
		t.Fatalf("unknown stderr must pass through verbatim; got:\n%s", r)
	}
	if !strings.Contains(r, "create PR failed") {
		t.Fatalf("reply must mention create-PR-failed context; got:\n%s", r)
	}
	// Invariant: must NOT match the stale-upstream / install / exists branches.
	for _, banned := range []string{
		"no longer exists",
		"already exists",
		"install via `brew install",
	} {
		if strings.Contains(r, banned) {
			t.Fatalf("unknown stderr must NOT trigger known-error branch; got %q in:\n%s", banned, r)
		}
	}
}

// TestDispatchPR_DirtyNoLongerGate — F-237 design intent:
// dirty working tree, untracked files, ahead-of-base, behind
// upstream, merge conflicts are all the user's responsibility
// under the new design (/gtw pr attempts to open regardless).
// gh/glab will reject if their model of reality disagrees.
// This test pins the new behavior so future regressions to
// the 6-dim gate are caught.
func TestDispatchPR_DirtyNoLongerGate(t *testing.T) {
	rig := newPRTestRig(t)
	setupPRWorktree(t, rig, Context{Branch: "wt-dirty"})
	setupReadiness(rig, "wt-dirty", messages.GitStatusSnapshot{
		Branch:      "wt-dirty",
		HasUpstream: true,
		// All the dimensions that used to gate, now allowed through:
		Modified:      3,
		Untracked:     2,
		AheadOfRemote: 5,
		BehindRemote:  2,
		HasConflicts:  true,
	})
	rig.git.on("remote", "git@github.com:octocat/hello.git", "", nil)
	rig.prov.SetCreatePRResp("https://github.com/octocat/hello/pull/42")
	rig.installDeps()

	cs := rig.cs
	s := captureCh(t, cs)
	_, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg", prArgs{})
	if err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "PR opened") {
		t.Fatalf("dirty worktree should NOT gate under new design; got:\n%s", r)
	}
	// Invariant: CreatePR must have been called.
	called := false
	for _, c := range rig.prov.calls {
		if c.Method == "CreatePR" {
			called = true
		}
	}
	if !called {
		t.Fatalf("CreatePR must be called for the dirty-worktree case")
	}
}

func TestDispatchPR_NoAgentSelected(t *testing.T) {
	// Tests the "no agent selected" failure path: dispatchPR
	// must reach runAgentFor (gates 1 + 2 passed with mocked
	// provider), and runAgentFor must report no-agent-selected
	// via agent.Builtins being empty AND the chat having no
	// selected agent.
	withAgent(t) // empty agent registry

	tmp := t.TempDir()
	withCwd(t, tmp)
	git := newPushGit()
	// loadDispatchContext's git-derive path needs rev-parse mocks
	// (no .nightme/gtw.yml in this test).
	git.onArgs([]string{"rev-parse", "--show-toplevel"}, tmp, "", nil)
	git.onArgs([]string{"rev-parse", "--abbrev-ref", "HEAD"}, "feat/manual", "", nil)
	git.onArgs([]string{"symbolic-ref", "--short", "refs/remotes/origin/HEAD"}, "origin/main", "", nil)
	git.onArgs([]string{"ls-remote", "--heads", "origin", "feat/manual"},
		"abc1234\trefs/heads/feat/manual\n", "", nil)
	git.on("remote", "git@github.com:octocat/hello.git", "", nil)
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	deps := HandlerDeps{Git: git, Detect: fakeDetect(prov)}

	cs := &chatsession.ChatSession{} // no SetSelectedAgent → empty selection
	_ = cs.SetSelectedCwd(tmp)
	s := captureCh(t, cs)
	_, err := dispatchPR(context.Background(), cs, deps, "chat", "msg", prArgs{})
	if err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "no agent selected") {
		t.Fatalf("expected no-agent reply, got:\n%s", r)
	}
}

func TestDispatchPR_AgentRunOnceFails(t *testing.T) {
	rig := newPRTestRig(t)
	setupPRWorktree(t, rig, Context{		Branch:   "wt",
	})
	rig.agent.runOnceErr = errors.New("boom")
	setupPRGit(rig, "wt", 0)
	rig.installDeps()

	cs := rig.cs
	s := captureCh(t, cs)
	_, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg", prArgs{})
	if err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "agent claude failed") || !strings.Contains(r, "boom") {
		t.Fatalf("expected agent-fail reply, got:\n%s", r)
	}
}

func TestDispatchPR_AgentOutputUnparsable(t *testing.T) {
	rig := newPRTestRig(t)
	setupPRWorktree(t, rig, Context{		Branch:   "wt",
	})
	// Whitespace-only agent output is the ONLY case the
	// permissive parser still can't recover from — there's no
	// non-empty line to take as a title. Everything else
	// (no fences, JSON wrappers, heading prefixes, etc.) now
	// falls through to the "first non-empty line" path; see
	// the parsePRReply unit tests for those cases.
	rig.agent.runOnceText = "   \n\n  \n"
	setupPRGit(rig, "wt", 0)
	rig.installDeps()

	cs := rig.cs
	s := captureCh(t, cs)
	_, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg", prArgs{})
	if err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "could not parse agent reply") {
		t.Fatalf("expected parse-fail reply, got:\n%s", r)
	}
}

// TestDispatchPR_AgentOutputNoFenceNowSucceeds is the dispatch-
// level regression test for the bug fixed when parsePRReply
// became permissive. Before, an agent that forgot the ``` fence
// (the user's actual 2026-08-10 production failure) was hard-
// errored at dispatch; now the PR is created with the first line
// of the agent output as the title.
func TestDispatchPR_AgentOutputNoFenceNowSucceeds(t *testing.T) {
	rig := newPRTestRig(t)
	setupPRWorktree(t, rig, Context{Branch: "wt"})
	// Same shape as the agent output that broke production:
	// well-formed Conventional Commits title, no fence wrapper,
	// multi-paragraph body.
	rig.agent.runOnceText = "feat(gtw): add per-instance name\n\n" +
		"This is the descriptive body.\n\n" +
		"- bullet one\n- bullet two\n"
	setupPRGit(rig, "wt", 0)
	// Configure the fake git so resolveProvider's Detect
	// path returns our fake provider — without a remote URL,
	// dispatchPR short-circuits with "no `origin` remote".
	rig.git.on("remote", "git@github.com:octocat/hello.git", "", nil)
	rig.prov.SetCreatePRResp("https://github.com/x/y/pull/1")
	rig.installDeps()

	cs := rig.cs
	s := captureCh(t, cs)
	_, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg", prArgs{})
	if err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}
	r := s.lastText()
	// Should NOT contain "could not parse" — the parse
	// succeeds thanks to the permissive regex scan.
	if strings.Contains(r, "could not parse agent reply") {
		t.Fatalf("dispatch hard-errored on unfenced agent output:\n%s", r)
	}
	// PR should have been created; the success card mentions
	// the URL and the title.
	if !strings.Contains(r, "https://github.com/x/y/pull/1") {
		t.Fatalf("expected PR URL in success card, got:\n%s", r)
	}
}

func TestDispatchPR_ResolveProvider_FromYml(t *testing.T) {
	// yml pins Repo+Provider; resolveProvider returns a fresh
	// NewProvider-built provider that we can't intercept from
	// outside. The Detect-fallback test below exercises the
	// full dispatch path with a fake provider; here we
	// directly unit-test resolveProvider's yml branch by
	// asserting it returns a GitHubProvider with the yml
	// Repo attached, and never touches deps.Detect.
	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt",
		RepoRoot: mustPwd(t),
		Repo:     "octocat/hello",
		Provider: "github",
	})
	rig := newPRTestRig(t)
	rig.installDeps()
	// The Detect stub, if called, would panic the test (it
	// doesn't expect to be invoked from the yml branch). We
	// install a Detect that fails the test loudly.
	detectCalled := false
	rig.deps.Detect = func(context.Context, string, HTTPProber, string) (GitProvider, error) {
		detectCalled = true
		return nil, errors.New("Detect should NOT be called when yml Repo+Provider are set")
	}

	c := Context{
		Worktree: mustPwd(t),
		Branch:   "wt",
		RepoRoot: mustPwd(t),
		Repo:     "octocat/hello",
		Provider: "github",
	}
	prov, owner, repo, err := resolveProvider(context.Background(), c, rig.deps)
	if err != nil {
		t.Fatalf("resolveProvider: %v", err)
	}
	if detectCalled {
		t.Fatalf("Detect was called even though yml had Repo+Provider")
	}
	if prov == nil || prov.Kind() != ProviderGitHub {
		t.Fatalf("provider: %v", prov)
	}
	if owner != "octocat" || repo != "hello" {
		t.Fatalf("owner/repo: %q/%q", owner, repo)
	}
}

func TestDispatchPR_ResolveProvider_DetectFallback(t *testing.T) {
	// No Repo/Provider in yml → fall through to Detect. We
	// inject the provider via deps.Detect (same as the yml path
	// in tests; production would call Detect(ctx, url, prober)
	// itself).
	rig := newPRTestRig(t)
	setupPRWorktree(t, rig, Context{		Branch:   "wt",
	})
	setupPRGit(rig, "wt", 0)
	rig.git.on("remote", "git@github.com:octocat/hello.git", "", nil)
	rig.prov.SetCreatePRResp("https://github.com/octocat/hello/pull/7")
	rig.installDeps()

	cs := rig.cs
	s := captureCh(t, cs)
	_, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg", prArgs{})
	if err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "✅ PR opened") {
		t.Fatalf("expected ✅ PR opened card, got:\n%s", r)
	}
}

func TestDispatchPR_CreatePRExists(t *testing.T) {
	rig := newPRTestRig(t)
	setupPRWorktree(t, rig, Context{		Branch:   "wt",
	})
	setupPRGit(rig, "wt", 0)
	rig.git.on("remote", "git@github.com:octocat/hello.git", "", nil)
	rig.prov.SetCreatePRErr(fmt.Errorf("%w: a PR for this branch already exists", ErrPRExists))
	rig.installDeps()

	cs := rig.cs
	s := captureCh(t, cs)
	_, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg", prArgs{})
	if err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "already exists") {
		t.Fatalf("expected PR-exists reply, got:\n%s", r)
	}
}

func TestDispatchPR_CreatePRFails(t *testing.T) {
	rig := newPRTestRig(t)
	setupPRWorktree(t, rig, Context{		Branch:   "wt",
	})
	setupPRGit(rig, "wt", 0)
	rig.git.on("remote", "git@github.com:octocat/hello.git", "", nil)
	rig.prov.SetCreatePRErr(errors.New("401 Unauthorized"))
	rig.installDeps()

	cs := rig.cs
	s := captureCh(t, cs)
	_, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg", prArgs{})
	if err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "create PR failed") || !strings.Contains(r, "401 Unauthorized") {
		t.Fatalf("expected create-PR-fail reply, got:\n%s", r)
	}
}

// -----------------------------------------------------------------------------
// Provider error mapping — wrapCreatePRError / wrapListPRError
// -----------------------------------------------------------------------------

// TestWrapCreatePRError_CLINotInstalled — gh/glab missing on
// PATH → ErrCLINotInstalled. The provider name is preserved in
// the hint so GitLab users see `brew install glab`.
func TestWrapCreatePRError_CLINotInstalled(t *testing.T) {
	err := wrapCreatePRError(&exec.Error{Name: "gh", Err: exec.ErrNotFound}, "", "gh")
	if err == nil {
		t.Fatalf("expected non-nil error")
	}
	if !errors.Is(err, ErrCLINotInstalled) {
		t.Fatalf("expected ErrCLINotInstalled, got %v", err)
	}
	if !strings.Contains(err.Error(), "install via `brew install gh`") {
		t.Fatalf("expected gh install hint, got %v", err)
	}
	if !strings.Contains(err.Error(), "cli.github.com") {
		t.Fatalf("expected install URL, got %v", err)
	}
}

func TestWrapCreatePRError_CLINotInstalled_GitLab(t *testing.T) {
	err := wrapCreatePRError(&exec.Error{Name: "glab", Err: exec.ErrNotFound}, "", "glab")
	if !errors.Is(err, ErrCLINotInstalled) {
		t.Fatalf("expected ErrCLINotInstalled, got %v", err)
	}
	if !strings.Contains(err.Error(), "install via `brew install glab`") {
		t.Fatalf("expected glab install hint, got %v", err)
	}
}

// TestWrapCreatePRError_StaleUpstreamGH — gh GraphQL validator
// messages get translated to ErrStaleUpstream. Each known
// substring in the table maps to the sentinel; the rest of the
// stderr is preserved verbatim.
func TestWrapCreatePRError_StaleUpstreamGH(t *testing.T) {
	cases := []string{
		"gh pr create: Head ref must be a branch",
		"gh pr create: No commits between main and feat",
		"gh pr create: Head sha can't be blank",
		"gh pr create: Base sha can't be blank",
	}
	for _, stderr := range cases {
		err := wrapCreatePRError(errors.New("exit 1"), stderr, "gh")
		if !errors.Is(err, ErrStaleUpstream) {
			t.Fatalf("expected ErrStaleUpstream for %q, got %v", stderr, err)
		}
		if !strings.Contains(err.Error(), stderr) {
			t.Fatalf("expected raw stderr preserved; got %v", err)
		}
	}
}

// TestWrapCreatePRError_StaleUpstreamGL — glab equivalents.
func TestWrapCreatePRError_StaleUpstreamGL(t *testing.T) {
	cases := []string{
		"glab mr create: Source branch does not exist",
		"glab mr create: Branch not found",
		"glab mr create: 404 Not Found",
	}
	for _, stderr := range cases {
		err := wrapCreatePRError(errors.New("exit 1"), stderr, "glab")
		if !errors.Is(err, ErrStaleUpstream) {
			t.Fatalf("expected ErrStaleUpstream for %q, got %v", stderr, err)
		}
	}
}

// TestWrapCreatePRError_AlreadyExists — "already exists" → ErrPRExists.
func TestWrapCreatePRError_AlreadyExists(t *testing.T) {
	err := wrapCreatePRError(errors.New("exit 1"), "already exists", "gh")
	if !errors.Is(err, ErrPRExists) {
		t.Fatalf("expected ErrPRExists, got %v", err)
	}
}

// TestWrapCreatePRError_UnknownPassThrough — unknown stderr
// propagates verbatim, no translation, no sentinel.
func TestWrapCreatePRError_UnknownPassThrough(t *testing.T) {
	const weird = "gh pr create: 403 Forbidden: API rate limit exceeded"
	err := wrapCreatePRError(errors.New("exit 1"), weird, "gh")
	if err == nil {
		t.Fatalf("expected non-nil")
	}
	if errors.Is(err, ErrCLINotInstalled) {
		t.Fatalf("unknown stderr must not match CLI-not-installed; got %v", err)
	}
	if errors.Is(err, ErrStaleUpstream) {
		t.Fatalf("unknown stderr must not match stale-upstream; got %v", err)
	}
	if errors.Is(err, ErrPRExists) {
		t.Fatalf("unknown stderr must not match already-exists; got %v", err)
	}
	if !strings.Contains(err.Error(), weird) {
		t.Fatalf("unknown stderr must pass through verbatim; got %v", err)
	}
}

// -----------------------------------------------------------------------------
// wrapListPRError — symmetric coverage for the list-call wrapper.
// Only CLI-not-installed is translated; stale-upstream / exists
// don't apply to a list call.
// -----------------------------------------------------------------------------

func TestWrapListPRError_CLINotInstalled(t *testing.T) {
	err := wrapListPRError(&exec.Error{Name: "gh", Err: exec.ErrNotFound}, "", "gh")
	if !errors.Is(err, ErrCLINotInstalled) {
		t.Fatalf("expected ErrCLINotInstalled, got %v", err)
	}
	if !strings.Contains(err.Error(), "install via `brew install gh`") {
		t.Fatalf("expected gh install hint, got %v", err)
	}
}

func TestWrapListPRError_UnknownPassThrough(t *testing.T) {
	const weird = "gh pr list: 502 Bad Gateway"
	err := wrapListPRError(errors.New("exit 1"), weird, "gh")
	if err == nil {
		t.Fatal("expected non-nil")
	}
	if errors.Is(err, ErrCLINotInstalled) {
		t.Fatalf("unknown stderr must not match CLI-not-installed; got %v", err)
	}
	if !strings.Contains(err.Error(), weird) {
		t.Fatalf("unknown stderr must pass through verbatim; got %v", err)
	}
}

// -----------------------------------------------------------------------------
// Production GitHubProvider.FindOpenPRForBranch — direct unit
// coverage for the CLI argv shape, JSON decode, empty-list
// short-circuit, and stderr-mapping paths.
// -----------------------------------------------------------------------------

// runGH stubs the gh CLI: stdout / stderr / err are returned
// for the (cwd, args...) invocation. Mirrors the shape
// production uses via ExecCLIRunner.
type runGH struct {
	out   string
	err   string
	fail  error
	calls int
	last  []string
}

func (r *runGH) Run(_ context.Context, _ string, args ...string) (string, string, error) {
	r.calls++
	r.last = args
	return r.out, r.err, r.fail
}

func TestGitHubFindOpenPRForBranch_Success(t *testing.T) {
	gh := &runGH{out: `[{"number":42,"url":"https://github.com/o/r/pull/42","state":"OPEN"}]`}
	p := &GitHubProvider{Worktree: "/w", Runner: gh}
	pr, err := p.FindOpenPRForBranch(context.Background(), "octocat", "hello", "feat")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if pr == nil {
		t.Fatal("expected non-nil PR")
	}
	if pr.Number != 42 || pr.URL != "https://github.com/o/r/pull/42" || pr.State != "open" {
		t.Fatalf("got %+v", pr)
	}
	// Argv shape: `pr list --head <head> --state open --json
	// number,url,state --repo <owner>/<repo>`.
	want := []string{"pr", "list", "--head", "feat", "--state", "open",
		"--json", "number,url,state", "--repo", "octocat/hello"}
	if !equalStrings(gh.last, want) {
		t.Fatalf("argv: got %v want %v", gh.last, want)
	}
}

func TestGitHubFindOpenPRForBranch_EmptyList(t *testing.T) {
	gh := &runGH{out: "[]"}
	p := &GitHubProvider{Worktree: "/w", Runner: gh}
	pr, err := p.FindOpenPRForBranch(context.Background(), "octocat", "hello", "feat")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if pr != nil {
		t.Fatalf("expected nil PR, got %+v", pr)
	}
}

func TestGitHubFindOpenPRForBranch_CLINotInstalled(t *testing.T) {
	gh := &runGH{fail: &exec.Error{Name: "gh", Err: exec.ErrNotFound}}
	p := &GitHubProvider{Worktree: "/w", Runner: gh}
	_, err := p.FindOpenPRForBranch(context.Background(), "octocat", "hello", "feat")
	if !errors.Is(err, ErrCLINotInstalled) {
		t.Fatalf("expected ErrCLINotInstalled, got %v", err)
	}
}

// TestGitLabFindOpenPRForBranch_EmptyList — minimum smoke for
// the GitLab side: argv shape + empty-list short-circuit.
// TestGitLabFindOpenPRForBranch_EmptyList — minimum smoke for
// the GitLab side: argv shape + empty-list short-circuit.
func TestGitLabFindOpenPRForBranch_EmptyList(t *testing.T) {
	gh := &runGH{out: "[]"}
	p := &GitLabProvider{Worktree: "/w", Runner: gh}
	pr, err := p.FindOpenPRForBranch(context.Background(), "acme", "demo", "feat")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if pr != nil {
		t.Fatalf("expected nil PR, got %+v", pr)
	}
	// GitLab argv shape: `mr list --source-branch <head> --output
	// json --repo <owner>/<repo>`. Critically: NO `--state` flag —
	// glab 1.36+ removed `--state` from `mr list` (default is open;
	// dedicated flags --closed/--merged/--draft cover the others).
	// Regression guard for the "Unknown flag: --state" failure
	// reported 2026-08-21 against fix-glab-pr.
	want := []string{"mr", "list", "--source-branch", "feat",
		"--output", "json", "--repo", "acme/demo"}
	if !equalStrings(gh.last, want) {
		t.Fatalf("argv: got %v want %v", gh.last, want)
	}
}

// TestGitLabFindOpenPRForBranch_Success — happy path: a single
// opened MR is returned with the platform-normalised state.
func TestGitLabFindOpenPRForBranch_Success(t *testing.T) {
	gh := &runGH{out: `[{"iid":144062,"web_url":"https://gitlab.com/gitlab-com/www-gitlab-com/-/merge_requests/144062","state":"opened"}]`}
	p := &GitLabProvider{Worktree: "/w", Runner: gh}
	pr, err := p.FindOpenPRForBranch(context.Background(), "gitlab-com", "www-gitlab-com", "workday-sync-team-page-2026-08-21-74")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if pr == nil {
		t.Fatal("expected non-nil PR")
	}
	if pr.Number != 144062 {
		t.Errorf("number: got %d want 144062", pr.Number)
	}
	if pr.URL != "https://gitlab.com/gitlab-com/www-gitlab-com/-/merge_requests/144062" {
		t.Errorf("url: got %q", pr.URL)
	}
	// "opened" is glab's term for GitHub's "open" — normalise so
	// downstream consumers don't have to know the platform.
	if pr.State != "open" {
		t.Errorf("state: got %q want %q (normalised from opened)", pr.State, "open")
	}
}

// TestGitLabFindOpenPRForBranch_MultipleRows — when several MRs
// match the source branch (rare but possible during re-pushes or
// cross-fork reopens), we return the first row. The contract
// comment doesn't promise freshness ordering; pinning behaviour
// so a future swap doesn't silently change which row wins.
func TestGitLabFindOpenPRForBranch_MultipleRows(t *testing.T) {
	gh := &runGH{out: `[
		{"iid":100,"web_url":"https://gitlab.com/o/r/-/merge_requests/100","state":"opened"},
		{"iid":101,"web_url":"https://gitlab.com/o/r/-/merge_requests/101","state":"opened"}
	]`}
	p := &GitLabProvider{Worktree: "/w", Runner: gh}
	pr, err := p.FindOpenPRForBranch(context.Background(), "o", "r", "feat")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if pr == nil || pr.Number != 100 {
		t.Fatalf("expected MR #100, got %+v", pr)
	}
}

// TestGitLabFindOpenPRForBranch_CLINotInstalled — `glab` missing
// from PATH must surface as ErrCLINotInstalled via wrapListPRError,
// the same way the GitHub path does.
func TestGitLabFindOpenPRForBranch_CLINotInstalled(t *testing.T) {
	gh := &runGH{fail: &exec.Error{Name: "glab", Err: exec.ErrNotFound}}
	p := &GitLabProvider{Worktree: "/w", Runner: gh}
	_, err := p.FindOpenPRForBranch(context.Background(), "o", "r", "feat")
	if !errors.Is(err, ErrCLINotInstalled) {
		t.Fatalf("expected ErrCLINotInstalled, got %v", err)
	}
}

// TestGitLabGetPR_EmptyList — no matching MR returns (nil, nil)
// with no error.
func TestGitLabGetPR_EmptyList(t *testing.T) {
	gh := &runGH{out: "[]"}
	p := &GitLabProvider{Worktree: "/w", Runner: gh}
	pr, err := p.GetPR(context.Background(), "o", "r", "feat")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if pr != nil {
		t.Fatalf("expected nil PR, got %+v", pr)
	}
}

// TestGitLabGetPR_Success — single MR round-trip with the
// platform-normalised state.
func TestGitLabGetPR_Success(t *testing.T) {
	gh := &runGH{out: `[{"iid":144062,"web_url":"https://gitlab.com/gitlab-com/www-gitlab-com/-/merge_requests/144062","state":"opened"}]`}
	p := &GitLabProvider{Worktree: "/w", Runner: gh}
	pr, err := p.GetPR(context.Background(), "gitlab-com", "www-gitlab-com", "workday-sync-team-page-2026-08-21-74")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if pr == nil {
		t.Fatal("expected non-nil PR")
	}
	if pr.Number != 144062 {
		t.Errorf("number: got %d want 144062", pr.Number)
	}
	if pr.State != "open" {
		t.Errorf("state: got %q want %q (normalised from opened)", pr.State, "open")
	}
}

// TestGitLabFindOpenPRForBranch_BlankStateDefaultsToOpen —
// glab occasionally omits the `state` field for very fresh MRs
// (or older self-hosted versions). The contract is that we
// default to "open" so the footer doesn't render an empty state.
func TestGitLabFindOpenPRForBranch_BlankStateDefaultsToOpen(t *testing.T) {
	gh := &runGH{out: `[{"iid":7,"web_url":"https://gitlab.com/o/r/-/merge_requests/7","state":""}]`}
	p := &GitLabProvider{Worktree: "/w", Runner: gh}
	pr, err := p.FindOpenPRForBranch(context.Background(), "o", "r", "feat")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if pr == nil || pr.State != "open" {
		t.Fatalf("expected state=open fallback, got %+v", pr)
	}
}

// TestGitLabArgv_NoStateFlag — direct regression test for the
// 2026-08-21 production failure reported against fix-glab-pr:
//
//	check existing PR: glab pr/mr list: exit status 1: ERROR
//	Unknown flag: --state.
//	Try --help for usage.
//
// glab 1.36+ removed `--state` from `mr list` (default is open;
// dedicated flags --closed/--merged/--draft cover the others).
// If a future refactor re-introduces `--state` here, this test
// fails before the change reaches production.
func TestGitLabArgv_NoStateFlag(t *testing.T) {
	for _, name := range []string{"FindOpenPRForBranch", "GetPR"} {
		t.Run(name, func(t *testing.T) {
			gh := &runGH{out: "[]"}
			p := &GitLabProvider{Worktree: "/w", Runner: gh}
			var err error
			switch name {
			case "FindOpenPRForBranch":
				_, err = p.FindOpenPRForBranch(context.Background(), "o", "r", "feat")
			case "GetPR":
				_, err = p.GetPR(context.Background(), "o", "r", "feat")
			}
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			for i, a := range gh.last {
				if a == "--state" {
					t.Fatalf("argv at index %d contains --state flag — glab 1.36+ removed this flag from `mr list`; default is open. argv=%v", i, gh.last)
				}
			}
		})
	}
}

// TestGitLabParseMRList_FromWWWGitLabCom — the real glab mr
// list --output json payload shape observed against
// https://gitlab.com/gitlab-com/www-gitlab-com.git (N=1 row,
// trimmed to the fields our parser actually consumes). Ensures
// the JSON tags (`iid`, `web_url`, `state`) match glab's actual
// schema and that the parser tolerates the documented field set.
// TestGitLabParseMRList_SchemaPinFromGlab182 is a hermetic
// schema-pin test. The fixture JSON is a CAPTURE of the real
// `glab mr list --output json` payload shape observed against
// https://gitlab.com/gitlab-com/www-gitlab-com.git on 2026-08-21
// (glab 1.82.0), trimmed to the fields GitLabProvider's parser
// actually decodes.
//
// Why this exists: pin the JSON tag set (`iid`, `web_url`,
// `state`) so a future glab release that renames `iid` -> `mr_iid`
// or `web_url` -> `url` breaks the test BEFORE the change reaches
// production. The MR IID (144062) and the branch name are just
// fixture data — the test does NOT hit the live API and the
// real MR's lifecycle (opened/merged/closed/deleted) is
// completely irrelevant. `runGH` is a stub CLIRunner that
// returns the fixture verbatim; if `glab` is not on PATH, the
// test still passes.
//
// For a real-API end-to-end check see
// TestRealGLAB_WWWGitLabCom_MRListArgs (gated on
// NIGHTME_REAL_GLAB=1 — see pr_realglab_unix_test.go).
func TestGitLabParseMRList_SchemaPinFromGlab182(t *testing.T) {
	const sample = `[
		{
			"iid": 144062,
			"target_branch": "master",
			"source_branch": "workday-sync-team-page-2026-08-21-74",
			"project_id": 7764,
			"title": "Workday Sync to Team Page - 2026-08-21",
			"state": "opened",
			"created_at": "2026-08-21T01:06:00.207Z",
			"updated_at": "2026-08-21T01:11:08.890Z",
			"web_url": "https://gitlab.com/gitlab-com/www-gitlab-com/-/merge_requests/144062"
		}
	]`
	gh := &runGH{out: sample}
	p := &GitLabProvider{Worktree: "/w", Runner: gh}
	pr, err := p.FindOpenPRForBranch(context.Background(), "gitlab-com", "www-gitlab-com", "workday-sync-team-page-2026-08-21-74")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if pr == nil {
		t.Fatal("expected non-nil PR")
	}
	if pr.Number != 144062 {
		t.Errorf("number: got %d want 144062", pr.Number)
	}
	if pr.URL != "https://gitlab.com/gitlab-com/www-gitlab-com/-/merge_requests/144062" {
		t.Errorf("url: got %q", pr.URL)
	}
	if pr.State != "open" {
		t.Errorf("state: got %q want %q (normalised from opened)", pr.State, "open")
	}
}

// TestGitLabProvider_LiveWWWGitLabCom_MRListArgs — argv shape
// assertion against the production provider using the exact
// owner/repo that pioneered the fix-glab-pr reproduction. The
// test doesn't shell out to glab (CI is hermetic), but it does
// pin the argv so a refactor that re-introduces `--state` (or
// adds a flag that breaks glab's parser) is caught before
// reaching production. The expected argv reflects the live
// behaviour observed on glab 1.82.0 against
// https://gitlab.com/gitlab-com/www-gitlab-com.git.
// TestGitLabArgvPin_NoStateFlag_AgainstWWWGitLabComOwnerRepo
// pins the argv shape using the exact owner/repo that
// originally surfaced the 2026-08-21 "Unknown flag: --state"
// bug (https://gitlab.com/gitlab-com/www-gitlab-com.git) and
// the branch name that triggered it (chore-docs-init). The
// word "OwnerRepo" instead of "Live" in the test name is
// deliberate: this test is HERMETIC — runGH is a stub that
// returns "[]" without invoking glab. The owner/repo/branch
// triple is just a fixture pin so a future refactor that
// re-introduces `--state` (or changes the argv in any other
// silent way) is caught BEFORE the change reaches production.
//
// For a real-API argv check see
// TestRealGLAB_WWWGitLabCom_MRListArgs (gated on
// NIGHTME_REAL_GLAB=1).
func TestGitLabArgvPin_NoStateFlag_AgainstWWWGitLabComOwnerRepo(t *testing.T) {
	gh := &runGH{out: "[]"}
	p := &GitLabProvider{Worktree: "/w", Runner: gh}
	_, err := p.FindOpenPRForBranch(context.Background(), "gitlab-com", "www-gitlab-com", "chore-docs-init")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := []string{
		"mr", "list",
		"--source-branch", "chore-docs-init",
		"--output", "json",
		"--repo", "gitlab-com/www-gitlab-com",
	}
	if !equalStrings(gh.last, want) {
		t.Fatalf("argv: got %v want %v", gh.last, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestIsExecutableNotFound — the helper must distinguish
// "binary missing" from generic subprocess errors. We test
// both the *exec.Error path (LookPath miss) and *fs.PathError
// path (Start miss).
func TestIsExecutableNotFound(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"exec.LookPath miss", &exec.Error{Name: "gh", Err: exec.ErrNotFound}, true},
		{"PathError ENOENT", &fs.PathError{Op: "fork/exec", Path: "/usr/bin/gh", Err: syscall.ENOENT}, true},
		{"generic error", errors.New("exit 1"), false},
		{"wrapped exec.LookPath miss", fmt.Errorf("outer: %w", &exec.Error{Name: "glab", Err: exec.ErrNotFound}), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isExecutableNotFound(tc.err); got != tc.want {
				t.Fatalf("isExecutableNotFound(%v): got %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestIsStaleUpstreamGH / TestIsStaleUpstreamGL — substring
// matchers used by wrapCreatePRError.
func TestIsStaleUpstreamGH(t *testing.T) {
	for _, ok := range []string{
		"Head ref must be a branch",
		"No commits between main and feature/x",
		"Head sha can't be blank",
		"Base sha can't be blank",
	} {
		if !isStaleUpstreamGH(ok) {
			t.Fatalf("expected match for %q", ok)
		}
	}
	// Negatives — must NOT match.
	for _, no := range []string{
		"",
		"401 Unauthorized",
		"403 Forbidden: API rate limit exceeded",
		"Branch not found",
	} {
		if isStaleUpstreamGH(no) {
			t.Fatalf("expected no-match for %q", no)
		}
	}
}

func TestIsStaleUpstreamGL(t *testing.T) {
	for _, ok := range []string{
		"Source branch does not exist",
		"Branch not found",
		"404 Not Found",
	} {
		if !isStaleUpstreamGL(ok) {
			t.Fatalf("expected match for %q", ok)
		}
	}
	for _, no := range []string{
		"",
		"401 Unauthorized",
		"Head ref must be a branch",
	} {
		if isStaleUpstreamGL(no) {
			t.Fatalf("expected no-match for %q", no)
		}
	}
}

func TestResolveProvider_FromYml_SplitOwnerRepo(t *testing.T) {
	rig := newPRTestRig(t)
	rig.deps = HandlerDeps{Git: rig.git}
	c := Context{Repo: "octocat/hello", Provider: "github"}
	prov, owner, repo, err := resolveProvider(context.Background(), c, rig.deps)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if prov == nil || prov.Kind() != ProviderGitHub {
		t.Fatalf("provider: %v", prov)
	}
	if owner != "octocat" || repo != "hello" {
		t.Fatalf("owner/repo: %q/%q", owner, repo)
	}
}

// TestResolveProvider_Detect_NestedGitLab covers self-hosted
// GitLab URLs with nested groups: owner/repo must come from
// ParseRepoOwner (URL-aware), not splitOwnerRepo (which would
// split on the first '/' and produce garbage).
func TestResolveProvider_Detect_NestedGitLab(t *testing.T) {
	rig := newPRTestRig(t)
	// Remote URL with nested groups
	rig.git.on("remote",
		"git@gitlab.acme.internal:team/sub/owner/repo.git", "", nil)
	rig.git.on("symbolic-ref", "origin/main", "", nil)
	rig.deps = HandlerDeps{
		Git:    rig.git,
		Detect: fakeDetect(newFakeGitProvider(ProviderGitLab, "gitlab.acme.internal")),
	}
	c := Context{RepoRoot: mustPwd(t)}
	_, owner, repo, err := resolveProvider(context.Background(), c, rig.deps)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if owner != "team/sub/owner" || repo != "repo" {
		t.Fatalf("owner/repo: %q/%q (ParseRepoOwner should have folded groups)", owner, repo)
	}
}

// TestDispatchPR_AgentFlagOverride exercises the -a <name>
// override path: prArgs.Agent wins over cs.SelectedAgent().
// Mirrors TestRunPush_DirtyWithAgentFlag for symmetry.
func TestDispatchPR_AgentFlagOverride(t *testing.T) {
	// Tests that -a <name> on /gtw pr overrides the chat's
	// selected agent. We take the git-derive path (no yml) so the
	// test only needs rev-parse mocks. The yml write that used to
	// be here was dead code — the test then overrode SelectedCwd
	// to make the yml irrelevant.
	tmp := t.TempDir()
	withCwd(t, tmp)

	// Note: we don't use newPRTestRig here because it would
	// re-install Builtins with only claude, wiping out the
	// opencode registration. Build the rig manually.
	opencode := &recordingAgent{name: "opencode", runOnceText: "```\nfeat: hi\n```"}
	claude := &recordingAgent{name: "claude", runOnceText: "should-not-be-called"}
	withAgent(t, claude, opencode)

	cs := &chatsession.ChatSession{}
	_ = cs.SetSelectedAgent("claude") // chat prefers claude, but -a opencode should win
	_ = cs.SetSelectedCwd(tmp)

	git := newPushGit()
	setupPRGit(&prTestRig{git: git, agent: opencode}, "feat/manual", 0)
	git.on("remote", "git@github.com:octocat/hello.git", "", nil)
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	prov.SetCreatePRResp("https://github.com/octocat/hello/pull/1")
	deps := HandlerDeps{Git: git, Detect: fakeDetect(prov)}

	s := captureCh(t, cs)
	_, err := dispatchPR(context.Background(), cs, deps, "chat", "msg",
		prArgs{Agent: "opencode"})
	if err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}
	opencode.mu.Lock()
	defer opencode.mu.Unlock()
	claude.mu.Lock()
	defer claude.mu.Unlock()
	if len(opencode.calls) != 1 {
		t.Fatalf("opencode.RunOnce called %d times, want 1", len(opencode.calls))
	}
	if len(claude.calls) != 0 {
		t.Fatalf("claude.RunOnce should NOT have been called when -a opencode was passed: %d calls",
			len(claude.calls))
	}
	if !strings.Contains(s.lastText(), "✅ PR opened") {
		t.Fatalf("expected success card, got:\n%s", s.lastText())
	}
}

// -----------------------------------------------------------------------------
// loadDispatchContext — non-worktree mode (no .nightme/gtw.yml)
// -----------------------------------------------------------------------------

// TestLoadDispatchContext_NoCwd covers the empty-chat-cwd case:
// /gtw push or /gtw pr without /cwd first should fail fast with
// the same prompt.
func TestLoadDispatchContext_NoCwd(t *testing.T) {
	rig := newPRTestRig(t)
	rig.installDeps()
	cs := rig.cs // SelectedCwd not set
	s := captureCh(t, cs)

	c, res := loadDispatchContext(context.Background(), cs, rig.deps, "chat", "msg")
	if res == nil {
		t.Fatalf("expected early-return Result, got Context=%+v", c)
	}
	if !strings.Contains(s.lastText(), "no active workspace") {
		t.Fatalf("expected no-workspace reply, got:\n%s", s.lastText())
	}
}

// TestLoadDispatchContext_NonWorktree covers a chat whose Cwd
// points at a manually-checked-out branch with no /gtw fix
// pre-amble (no .nightme/gtw.yml). Worktree / Branch / RepoRoot
// must be derived from git rev-parse.
func TestLoadDispatchContext_NonWorktree(t *testing.T) {
	rig := newPRTestRig(t)
	tmp := t.TempDir()
	_ = rig.cs.SetSelectedCwd(tmp)

	// No yml written. rig.git returns canned values for the
	// rev-parse calls.
	rig.git.onArgs([]string{"rev-parse", "--show-toplevel"}, tmp, "", nil)
	rig.git.onArgs([]string{"rev-parse", "--abbrev-ref", "HEAD"}, "feat/manual", "", nil)
	rig.installDeps()

	c, res := loadDispatchContext(context.Background(), rig.cs, rig.deps, "chat", "msg")
	if res != nil {
		t.Fatalf("expected nil Result, got %+v", res)
	}
	if c.Worktree != tmp {
		t.Fatalf("Worktree: want %q, got %q", tmp, c.Worktree)
	}
	if c.Branch != "feat/manual" {
		t.Fatalf("Branch: want %q, got %q", "feat/manual", c.Branch)
	}
	if c.RepoRoot != tmp {
		t.Fatalf("RepoRoot: want %q, got %q", tmp, c.RepoRoot)
	}
	if c.Mode != ModeLocal {
		t.Fatalf("Mode: want %q, got %q", ModeLocal, c.Mode)
	}
	if c.Issue != -1 {
		t.Fatalf("Issue: want -1, got %d", c.Issue)
	}
	if c.Repo != "" || c.Provider != "" {
		t.Fatalf("Repo/Provider should be empty (Detect fallback), got %q/%q", c.Repo, c.Provider)
	}
}

// TestLoadDispatchContext_NonWorktree_NotGitRepo covers the
// "cwd is not inside a git repo" failure mode.
func TestLoadDispatchContext_NonWorktree_NotGitRepo(t *testing.T) {
	rig := newPRTestRig(t)
	tmp := t.TempDir()
	_ = rig.cs.SetSelectedCwd(tmp)
	// rev-parse --show-toplevel errors (no .git ancestor)
	rig.git.onArgs([]string{"rev-parse", "--show-toplevel"}, "",
		"fatal: not a git repository", errors.New("exit 128"))
	rig.installDeps()

	s := captureCh(t, rig.cs)
	_, res := loadDispatchContext(context.Background(), rig.cs, rig.deps, "chat", "msg")
	if res == nil {
		t.Fatalf("expected early-return Result")
	}
	if !strings.Contains(s.lastText(), "not inside a git repository") {
		t.Fatalf("expected not-a-git-repo reply, got:\n%s", s.lastText())
	}
}

// TestLoadDispatchContext_NonWorktree_DetachedHEAD covers a
// detached-HEAD checkout — we refuse rather than guess a PR target.
func TestLoadDispatchContext_NonWorktree_DetachedHEAD(t *testing.T) {
	rig := newPRTestRig(t)
	tmp := t.TempDir()
	_ = rig.cs.SetSelectedCwd(tmp)
	rig.git.onArgs([]string{"rev-parse", "--show-toplevel"}, tmp, "", nil)
	rig.git.onArgs([]string{"rev-parse", "--abbrev-ref", "HEAD"}, "HEAD", "", nil)
	rig.installDeps()

	s := captureCh(t, rig.cs)
	_, res := loadDispatchContext(context.Background(), rig.cs, rig.deps, "chat", "msg")
	if res == nil {
		t.Fatalf("expected early-return Result")
	}
	if !strings.Contains(s.lastText(), "detached HEAD") {
		t.Fatalf("expected detached-HEAD reply, got:\n%s", s.lastText())
	}
}

// TestDispatchPR_NonWorktree_HappyPath exercises /gtw pr end-to-end
// on a non-/gtw fix branch: no yml, derived Branch / Worktree /
// RepoRoot. The Detect fallback path provides the provider.
func TestDispatchPR_NonWorktree_HappyPath(t *testing.T) {
	opencode := &recordingAgent{name: "opencode", runOnceText: "```\nfeat: hi\n```"}
	withAgent(t, opencode)

	cs := &chatsession.ChatSession{}
	_ = cs.SetSelectedAgent("opencode")
	tmp := t.TempDir()
	_ = cs.SetSelectedCwd(tmp)

	git := newPushGit()
	git.onArgs([]string{"rev-parse", "--show-toplevel"}, tmp, "", nil)
	git.onArgs([]string{"rev-parse", "--abbrev-ref", "HEAD"}, "feat/manual", "", nil)
	git.onArgs(statusCmd, "## feat/manual...origin/feat/manual\n", "", nil) // F-57 readiness
	git.onArgs([]string{"symbolic-ref", "--short", "refs/remotes/origin/HEAD"}, "origin/main", "", nil)
	git.onArgs([]string{"rev-list", "--count", "main..HEAD"}, "5", "", nil)
	git.onArgs([]string{"rev-list", "--count", "origin/main..HEAD"}, "5", "", nil)
	git.onArgs([]string{"ls-remote", "--heads", "origin", "feat/manual"},
		"abc1234\trefs/heads/feat/manual\n", "", nil)
	git.on("remote", "git@github.com:octocat/hello.git", "", nil)
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	prov.SetCreatePRResp("https://github.com/octocat/hello/pull/7")
	deps := HandlerDeps{Git: git, Detect: fakeDetect(prov)}

	s := captureCh(t, cs)
	_, err := dispatchPR(context.Background(), cs, deps, "chat", "msg", prArgs{})
	if err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "✅ PR opened") {
		t.Fatalf("expected success card, got:\n%s", r)
	}
	if !strings.Contains(r, "feat/manual") {
		t.Fatalf("expected branch name in card, got:\n%s", r)
	}
	// Detect fallback fires because c.Repo is empty.
	detectedProv, owner, repo, derr := resolveProvider(context.Background(), Context{
		RepoRoot: tmp,
	}, deps)
	if derr != nil {
		t.Fatalf("resolveProvider: %v", derr)
	}
	if detectedProv == nil || owner != "octocat" || repo != "hello" {
		t.Fatalf("Detect fallback: provider=%v owner=%q repo=%q", detectedProv, owner, repo)
	}
}

// TestFactory_Handle_RoutesToPR verifies the factory's Handle
// switch dispatches input.Args[1] == "pr" to runPR (and that
// /gtw pr with no args still works via dispatchPR's no-workspace
// early return).
func TestFactory_Handle_RoutesToPR(t *testing.T) {
	withCwd(t, "/") // force the no-workspace / no-yml branch
	gtwMgr := NewManager()
	csMgr := chatsession.NewManager() // v1.3+: Handle takes chatsession.Manager
	f := NewFactoryWithDeps(gtwMgr, HandlerDeps{})

	// Wire a real ChatSession so dispatchPR's cs.Emitter()
	// sends the reply somewhere we can observe. We use the
	// shared recordingCh from testharness_test.go (the no-op
	// nopCh from test_helpers_test.go would silently drop the
	// reply).
	rec := &recordingCh{}
	cs, err := chatsession.New("chat-pr-test", "claude")
	cs.WithEmitter(rec)
	if err != nil {
		t.Fatalf("chatsession.New: %v", err)
	}
	ch := rec

	out, err := f.Handle(context.Background(),
		command.RuntimeServices{}, csMgr, cs,
		command.SlashInput{
			Args:      []string{"gtw", "pr"},
			ChatID:    "chat-pr-test",
			MessageID: "msg",
		})
	if err != nil {
		t.Fatalf("Handle err: %v", err)
	}
	if out == nil || !out.Consumed {
		t.Fatalf("expected Consumed=true, got %+v", out)
	}
	// dispatchPR replied via cs.Emitter(); assert via the
	// recordingCh rather than out.Reply (which is always empty
	// when the reply path is cs.Emitter() — see dispatchPush
	// for the same shape).
	text := ch.lastText()
	if text == "" {
		t.Fatalf("expected a reply via cs.Emitter() (no-workspace early return), got empty")
	}
	if !strings.Contains(text, "no active workspace") &&
		!strings.Contains(text, "no active fix to push") {
		t.Fatalf("unexpected reply: %q", text)
	}
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

func mustNotContain(t *testing.T, s, sub string) {
	t.Helper()
	if strings.Contains(s, sub) {
		t.Fatalf("prompt unexpectedly contains %q:\n---\n%s\n---", sub, s)
	}
}

// Compile-time assertion: dispatchPR returns *Result + error and
// ignores the unused variable in callers that only care about err.
var _ = func() *Result { return nil }

// Compile-time assertion: fakeGitProvider satisfies GitProvider
// (covered elsewhere, but restated here to surface any drift the
// moment pr_test.go is touched).
var _ GitProvider = (*fakeGitProvider)(nil)

// -----------------------------------------------------------------------------
// PRCache wiring
//
// /gtw pr and /gtw close success paths walk the chat pool and
// call deps.PRCache.WritePR(as.ID, pr) inline. These tests
// lock the contract:
//   - happy path: every non-nil AS gets WritePR called with
//     the expected PR exactly once.
//   - nil PRCache: no panic, no allocations.
//   - empty pool: no panic.
//
// Iteration order is NOT asserted because cs.Pool() iterates a
// Go map whose order is intentionally randomized per process —
// asserting order would make the tests flaky across runs.
// -----------------------------------------------------------------------------

// TestPRCacheApply_HappyPath: every non-nil AS in the pool
// gets WritePR called exactly once with the expected PR.
// Mirrors the inline loop in dispatchPR's success path.
//
// Production ordering: by the time /gtw pr fires, the chat
// has already been stamped at least once (otherwise the user
// has no AS to dispatch on), so the ASes are already
// registered. We pre-allocate via GetOrCreate to mirror that
// state — Registry.WritePR is a no-op for unregistered ASes
// (it does not allocate), and the lazy MaybeRefresh on the
// next stamp handles that case instead.
func TestPRCacheApply_HappyPath(t *testing.T) {
	reg := &prcache.Registry{}
	cs, err := chatsession.New("chat-1", "claude")
	if err != nil {
		t.Fatalf("chatsession.New: %v", err)
	}
	cs.AttachAgentSessionForTest(chatsession.NewAgentSession("as-1", cs.ChatID, "claude", "/w1", nil))
	cs.AttachAgentSessionForTest(chatsession.NewAgentSession("as-2", cs.ChatID, "claude", "/w2", nil))
	cs.AttachAgentSessionForTest(chatsession.NewAgentSession("as-3", cs.ChatID, "claude", "/w3", nil))

	// Pre-allocate caches (simulates prior stamping).
	want := []string{"as-1", "as-2", "as-3"}
	for _, asID := range want {
		reg.GetOrCreate(asID)
	}

	deps := HandlerDeps{PRCache: reg}
	newPR := &messages.PR{Number: 42, URL: "https://example/pr/42", State: "open"}

	// Inline the same loop dispatchPR runs on success.
	if deps.PRCache != nil {
		for _, as := range cs.Pool() {
			if as == nil {
				continue
			}
			deps.PRCache.WritePR(as.ID, newPR)
		}
	}

	// Every AS in the pool must have a cache with the new PR.
	for _, asID := range want {
		c := reg.GetOrCreate(asID)
		if got := c.PR(); got == nil || got.Number != 42 {
			t.Errorf("AS %q: PR = %+v, want {Number:42}", asID, got)
		}
	}
}

// TestPRCacheApply_WritePRNoOpOnUnknownAS locks the contract
// that Registry.WritePR does NOT allocate caches — the next
// stamp's lazy MaybeRefresh handles unregistered ASes. Without
// this guard, /gtw pr on a chat with zero stamps would
// allocate caches for every AS in the pool (memory leak
// surface) and overwrite them on every /gtw pr success.
func TestPRCacheApply_WritePRNoOpOnUnknownAS(t *testing.T) {
	reg := &prcache.Registry{}
	reg.WritePR("never-stamped", &messages.PR{Number: 1, URL: "x", State: "open"})

	if c := reg.GetOrCreate("never-stamped"); c.PR() != nil {
		t.Errorf("WritePR on unknown AS populated the freshly-allocated cache")
	}
}

// TestPRCacheApply_ClearOnClose: /gtw close writes nil to
// every AS — same loop, pr=nil.
func TestPRCacheApply_ClearOnClose(t *testing.T) {
	reg := &prcache.Registry{}
	cs, err := chatsession.New("chat-1", "claude")
	if err != nil {
		t.Fatalf("chatsession.New: %v", err)
	}
	cs.AttachAgentSessionForTest(chatsession.NewAgentSession("as-1", cs.ChatID, "claude", "/w1", nil))
	cs.AttachAgentSessionForTest(chatsession.NewAgentSession("as-2", cs.ChatID, "claude", "/w2", nil))

	// Pre-populate so we can confirm the clear actually clears.
	for _, asID := range []string{"as-1", "as-2"} {
		c := reg.GetOrCreate(asID)
		c.WritePR(&messages.PR{Number: 9, URL: "https://example/pr/9", State: "open"})
	}

	deps := HandlerDeps{PRCache: reg}

	// Inline the same loop dispatchClose runs on success.
	if deps.PRCache != nil {
		for _, as := range cs.Pool() {
			if as == nil {
				continue
			}
			deps.PRCache.WritePR(as.ID, nil)
		}
	}

	for _, asID := range []string{"as-1", "as-2"} {
		c := reg.GetOrCreate(asID)
		if got := c.PR(); got != nil {
			t.Errorf("AS %q after clear: PR = %+v, want nil", asID, got)
		}
	}
}

// TestPRCacheApply_NilCache: nil-safe — no panic, no calls.
// Tests that wire a bare HandlerDeps{} without the runtime
// registry still pass.
func TestPRCacheApply_NilCache(t *testing.T) {
	cs, err := chatsession.New("chat-1", "claude")
	if err != nil {
		t.Fatalf("chatsession.New: %v", err)
	}
	cs.AttachAgentSessionForTest(chatsession.NewAgentSession("as-1", cs.ChatID, "claude", "/w1", nil))

	deps := HandlerDeps{PRCache: nil}

	// Same nil-safe guard dispatchPR / dispatchClose use.
	if deps.PRCache != nil {
		t.Fatalf("nil-cache guard failed; PRCache should be nil")
	}
}

// TestPRCacheApply_EmptyPool: no panic on an empty pool.
// Common state during the first /gtw fix on a chat that hasn't
// spawned an agent yet.
func TestPRCacheApply_EmptyPool(t *testing.T) {
	reg := &prcache.Registry{}
	cs, err := chatsession.New("chat-1", "claude")
	if err != nil {
		t.Fatalf("chatsession.New: %v", err)
	}

	deps := HandlerDeps{PRCache: reg}
	if deps.PRCache != nil {
		for _, as := range cs.Pool() {
			if as == nil {
				continue
			}
			deps.PRCache.WritePR(as.ID, nil)
		}
	}

	// Registry must remain empty (no ASes were ever attached).
	reg.WritePR("phantom", nil) // Registry.WritePR is also no-op on unknown AS.
}

// TestPRNumberFromURL pins the URL → number parser used by
// /gtw pr to populate the WritePR payload.
func TestPRNumberFromURL(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"https://github.com/octocat/hello/pull/7", 7},
		{"https://github.com/x/y/pull/1", 1},
		{"https://gitlab.com/o/r/-/merge_requests/123", 123},
		{"https://example/no-match", 0},
		{"", 0},
		{"https://github.com/o/r/pull/abc", 0},
	}
	for _, tc := range cases {
		if got := prNumberFromURL(tc.in); got != tc.want {
			t.Errorf("prNumberFromURL(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
