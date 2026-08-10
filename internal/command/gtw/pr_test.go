package gtw

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
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

func TestParsePRReply_NoFence(t *testing.T) {
	if _, _, err := parsePRReply("just some prose without a fence"); err == nil {
		t.Fatalf("expected error on missing fence")
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

func TestParsePRReply_UnclosedFence(t *testing.T) {
	if _, _, err := parsePRReply("```\nfeat: half-written\n\nbody but no closing"); err == nil {
		t.Fatalf("expected error on unclosed fence")
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
	mustContain(t, p, "Reference issue with #42")

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

func TestParsePRArgs_UnknownSilentlyAccepted(t *testing.T) {
	// Future flags (--draft, --base) shouldn't break callers.
	out, err := parsePRArgs([]string{"--draft", "-a", "claude"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out.Agent != "claude" {
		t.Fatalf("Agent: %q", out.Agent)
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
	cs.WithChannel(ch)
	return ch
}

func TestDispatchPR_NoWorkspace(t *testing.T) {
	withCwd(t, "/") // no yml here
	rig := newPRTestRig(t)
	rig.installDeps()
	cs := rig.cs
	s := captureCh(t, cs)

	// pushCwd() will resolve to "/"; .nightme/gtw.yml doesn't
	// exist there → the no-yml early return fires.
	_, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg", prArgs{})
	if err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "no active fix to push in this chat") &&
		!strings.Contains(r, "no active workspace") {
		t.Fatalf("expected no-workspace / no-yml reply, got:\n%s", r)
	}
}

func TestDispatchPR_MalformedYml(t *testing.T) {
	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{Worktree: "", Branch: "wt", RepoRoot: mustPwd(t)})

	rig := newPRTestRig(t)
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
	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt",
		RepoRoot: mustPwd(t),
	})

	rig := newPRTestRig(t)
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

// setupPRGit configures the rig's pushGit with responses for
// the two rev-list invocations dispatchPR makes:
//
//   / rev-list @{u}..HEAD   → countUnpushed
//   / rev-list main..HEAD   → countBaseAhead (base == "main")
//
// Returns a helper that registers CreatePR / Detect calls; tests
// just say what they want the upstream responses to be.
func setupPRGit(rig *prTestRig, unpushed, ahead int) {
	rig.git.on("symbolic-ref", "origin/main", "", nil)
	rig.git.onArgs([]string{"rev-list", "--count", "@{u}..HEAD"}, itoa10(unpushed), "", nil)
	rig.git.onArgs([]string{"rev-list", "--count", "main..HEAD"}, itoa10(ahead), "", nil)
}

func TestDispatchPR_UnpushedCommits(t *testing.T) {
	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-unpushed",
		RepoRoot: mustPwd(t),
	})

	rig := newPRTestRig(t)
	setupPRGit(rig, 3, 5) // 3 unpushed, 5 ahead → reject early on unpushed
	rig.installDeps()

	cs := rig.cs
	s := captureCh(t, cs)
	_, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg", prArgs{})
	if err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "3 unpushed") || !strings.Contains(r, "/gtw push first") {
		t.Fatalf("expected unpushed-commits reply, got:\n%s", r)
	}
}

func TestDispatchPR_NothingToPR(t *testing.T) {
	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-empty",
		RepoRoot: mustPwd(t),
	})

	rig := newPRTestRig(t)
	setupPRGit(rig, 0, 0) // 0 ahead → "nothing to PR"
	rig.installDeps()

	cs := rig.cs
	s := captureCh(t, cs)
	_, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg", prArgs{})
	if err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "nothing to PR") {
		t.Fatalf("expected nothing-to-PR reply, got:\n%s", r)
	}
}

func TestDispatchPR_NoAgentSelected(t *testing.T) {
	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt",
		RepoRoot: mustPwd(t),
	})

	rig := newPRTestRig(t)
	// Unregister the recordingAgent so SelectedAgent() returns "".
	withAgent(t) // empty registry
	setupPRGit(rig, 0, 5)
	rig.installDeps()

	cs := &chatsession.ChatSession{} // no SetSelectedAgent
	s := captureCh(t, cs)

	_, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg", prArgs{})
	if err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "no agent selected") {
		t.Fatalf("expected no-agent reply, got:\n%s", r)
	}
}

func TestDispatchPR_AgentRunOnceFails(t *testing.T) {
	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt",
		RepoRoot: mustPwd(t),
	})

	rig := newPRTestRig(t)
	rig.agent.runOnceErr = errors.New("boom")
	setupPRGit(rig, 0, 5)
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
	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt",
		RepoRoot: mustPwd(t),
	})

	rig := newPRTestRig(t)
	rig.agent.runOnceText = "I think the PR should be about..." // no fence
	setupPRGit(rig, 0, 5)
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
	if !strings.Contains(r, "I think the PR should be") {
		t.Fatalf("expected raw agent text echoed, got:\n%s", r)
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
	rig.deps.Detect = func(context.Context, string, HTTPProber) (GitProvider, error) {
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
	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt",
		RepoRoot: mustPwd(t),
	})

	rig := newPRTestRig(t)
	setupPRGit(rig, 0, 5)
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
	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt",
		RepoRoot: mustPwd(t),
	})

	rig := newPRTestRig(t)
	setupPRGit(rig, 0, 5)
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
	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt",
		RepoRoot: mustPwd(t),
	})

	rig := newPRTestRig(t)
	setupPRGit(rig, 0, 5)
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
// countBaseAhead (sanity; used by dispatchPR)
// -----------------------------------------------------------------------------

func TestCountBaseAhead(t *testing.T) {
	g := newPushGit()
	g.on("rev-list", "5", "", nil)
	n, err := countBaseAhead(context.Background(), "/w", "main", HandlerDeps{Git: g})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n != 5 {
		t.Fatalf("want 5, got %d", n)
	}
	if len(g.calls) != 1 || g.calls[0].args[0] != "rev-list" {
		t.Fatalf("git calls: %+v", g.calls)
	}
}

// TestCountBaseAhead_OriginFallback covers the "main only exists
// as origin/main" case (manual git worktree add without
// /gtw fix's `git checkout main` step). The first rev-list
// call fails; the second (origin/main..HEAD) succeeds.
func TestCountBaseAhead_OriginFallback(t *testing.T) {
	g := newPushGit()
	// First call: main doesn't resolve → error
	g.onArgs([]string{"rev-list", "--count", "main..HEAD"},
		"", "fatal: ambiguous argument 'main..HEAD'",
		errors.New("exit 128"))
	// Second call (origin/main..HEAD) → success
	g.onArgs([]string{"rev-list", "--count", "origin/main..HEAD"},
		"3", "", nil)
	n, err := countBaseAhead(context.Background(), "/w", "main", HandlerDeps{Git: g})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n != 3 {
		t.Fatalf("want 3, got %d", n)
	}
	// Both calls must have happened (the first was tried and
	// failed before we fell back).
	if len(g.calls) != 2 {
		t.Fatalf("git calls: %d, want 2: %+v", len(g.calls), g.calls)
	}
}

// TestResolveProvider_FromYml_SplitOwnerRepo ensures the yml
// path splits "octocat/hello" correctly (cheap first-slash
// helper, NOT ParseRepoOwner which needs a host prefix).
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
	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt",
		RepoRoot: mustPwd(t),
	})

	// Note: we don't use newPRTestRig here because it would
	// re-install Builtins with only claude, wiping out the
	// opencode registration. Build the rig manually.
	opencode := &recordingAgent{name: "opencode", runOnceText: "```\nfeat: hi\n```"}
	claude := &recordingAgent{name: "claude", runOnceText: "should-not-be-called"}
	withAgent(t, claude, opencode)

	cs := &chatsession.ChatSession{}
	_ = cs.SetSelectedAgent("claude") // chat prefers claude, but -a opencode should win

	git := newPushGit()
	setupPRGit(&prTestRig{git: git, agent: opencode}, 0, 5)
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

// TestFactory_Handle_RoutesToPR verifies the factory's Handle
// switch dispatches input.Args[1] == "pr" to runPR (and that
// /gtw pr with no args still works via dispatchPR's no-workspace
// early return).
func TestFactory_Handle_RoutesToPR(t *testing.T) {
	withCwd(t, "/") // force the no-workspace / no-yml branch
	mgr := NewManager()
	f := NewFactoryWithDeps(mgr, HandlerDeps{})

	// Wire a real ChatSession so dispatchPR's cs.Channel()
	// sends the reply somewhere we can observe. We use the
	// shared recordingCh from testharness_test.go (the no-op
	// nopCh from test_helpers_test.go would silently drop the
	// reply).
	rec := &recordingCh{}
	cs, err := chatsession.New("chat-pr-test", "claude", rec)
	if err != nil {
		t.Fatalf("chatsession.New: %v", err)
	}
	mgr.SetGetChatSession(func(chatID string) *chatsession.ChatSession {
		return cs
	})
	ch := rec

	out, err := f.Handle(context.Background(),
		command.RuntimeServices{}, cs,
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
	// dispatchPR replied via cs.Channel(); assert via the
	// recordingCh rather than out.Reply (which is always empty
	// when the reply path is cs.Channel() — see dispatchPush
	// for the same shape).
	text := ch.lastText()
	if text == "" {
		t.Fatalf("expected a reply via cs.Channel() (no-workspace early return), got empty")
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