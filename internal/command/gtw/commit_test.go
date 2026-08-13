package gtw

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// -----------------------------------------------------------------------------
// buildAgentPrompt tests (F-56 §3 invariant: prompt is role +
// task + 3 hard rules; no reference to nightme, push, or
// verification). v2 invariants below add the prompt-engineering
// guards (tool floor, split anchors, Do NOT block, gold example).
// -----------------------------------------------------------------------------

func TestBuildAgentPrompt_Remote(t *testing.T) {
	c := Context{
		Worktree: "/w",
		Branch:   "fix-42-foo",
		Issue:    42,
	}
	p := buildAgentPrompt(c)

	mustContain(t, p, "release engineer")
	mustContain(t, p, "Branch: fix-42-foo")
	mustContain(t, p, "Worktree: /w")
	mustContain(t, p, "Issue: #42")
	mustContain(t, p, "Conventional Commits")
	mustContain(t, p, "feat, fix, chore, refactor")
	mustContain(t, p, "Do not push.")
	mustContain(t, p, "never run `git push`")
	mustContain(t, p, "Do not revert, restore, or stash")
	mustContain(t, p, "not `git add -A`")
	// Old prompt's "5-step checklist" + the actual push step
	// (not the "never run git push" warning) should be gone.
	if strings.Contains(p, "Step list") || strings.Contains(p, "Task:") ||
		strings.Contains(p, "git push -u origin") || strings.Contains(p, "Reply with: <commit_hash>") ||
		strings.Contains(p, "Working directory: /w\nBranch:") {
		t.Fatalf("old-style prompt leakage detected:\n%s", p)
	}
}

func TestBuildAgentPrompt_Local(t *testing.T) {
	c := Context{
		Worktree: "/w",
		Branch:   "wt-local",
		Issue:    -1, // ModeLocal
	}
	p := buildAgentPrompt(c)

	mustContain(t, p, "release engineer")
	mustContain(t, p, "Branch: wt-local")
	// The body-rules section legitimately mentions `Issue: #N`
	// as a generic phrase. Check for the more specific "Issue: #N\n"
	// line that only appears when c.Issue > 0.
	if strings.Contains(p, "Issue: #1") || strings.Contains(p, "Issue: #42") {
		t.Fatalf("Local prompt should not contain a specific Issue: #N line:\n%s", p)
	}
	if strings.Contains(p, "Working directory: /w\nBranch:") {
		t.Fatalf("Local prompt should not contain the old 'Working directory / Branch' lines:\n%s", p)
	}
}

// -----------------------------------------------------------------------------
// buildAgentPrompt v2 invariants (mirror the v2 PR prompt
// invariants in pr_test.go). Each test fails loudly if a future
// edit silently drops a guard that prevents one of the observed
// regressions:
//   - subject-only commits for non-trivial work (PR #135's
//     `fix(gtw): restore missing space in the commit-agent prompt`
//     is literal evidence this regression hits production)
//   - split anchors removed → LLM reverts to single-commit default
//   - Do NOT block removed → modal training-data pattern wins
//   - tool floor removed → LLM skips git inspection, writes from
//     file names alone
//   - issue trailer dropped → commits lose their bug-tracker link
// -----------------------------------------------------------------------------

// TestBuildAgentPromptV2_ToolFloorMandatory: the agent MUST run
// git status + git diff + git diff --staged + git log before
// staging anything. Without this, the LLM falls back to guessing
// from file names — same regression the v2 PR prompt fixes.
func TestBuildAgentPromptV2_ToolFloorMandatory(t *testing.T) {
	c := Context{Worktree: "/w", Branch: "feat/x", Issue: 1}
	p := buildAgentPrompt(c)

	mustContain(t, p, "## Before staging — tool floor")
	mustContain(t, p, "You MUST run")
	mustContain(t, p, "`git status`")
	mustContain(t, p, "`git diff` (no args)")
	mustContain(t, p, "`git diff --staged`")
	mustContain(t, p, "`git log --oneline -5`")
	mustContain(t, p, "Do NOT stage files without reading their diff")
}

// TestBuildAgentPromptV2_SplitAnchorsConcrete: the prompt names
// five concrete split patterns so the LLM has a decision rubric,
// not an adjective. "ONE commit beats a forced split" is the
// counter-anchor that prevents the opposite regression.
func TestBuildAgentPromptV2_SplitAnchorsConcrete(t *testing.T) {
	c := Context{Worktree: "/w", Branch: "feat/x", Issue: 1}
	p := buildAgentPrompt(c)

	mustContain(t, p, "## Splitting into multiple commits")
	mustContain(t, p, "**Code change + its test**")
	mustContain(t, p, "**Implementation + refactor**")
	mustContain(t, p, "**Style/formatting churn mixed with logic change**")
	mustContain(t, p, "**Unrelated chore")
	mustContain(t, p, "ONE commit with a clear subject is better than a forced split")
}

// TestBuildAgentPromptV2_TwoTierBodyRule: the prompt must
// distinguish commit body (short, focused, per-change intent)
// from PR body (long, structured, all four dimensions). Without
// this distinction the LLM either pads commit bodies to PR
// length or strips them to nothing.
func TestBuildAgentPromptV2_TwoTierBodyRule(t *testing.T) {
	c := Context{Worktree: "/w", Branch: "feat/x", Issue: 1}
	p := buildAgentPrompt(c)

	mustContain(t, p, "commit body is NOT a PR body")
	mustContain(t, p, "5-15 lines total")
	mustContain(t, p, "longer than the `git diff`")
	mustContain(t, p, "empty for a `fix:` / `feat:`")
}

// TestBuildAgentPromptV2_TypeByBodyRule: chore: gets an OPTIONAL
// body, fix:/feat: get a REQUIRED body. Without this anchor the
// LLM either pads every chore commit or strips every fix commit.
func TestBuildAgentPromptV2_TypeByBodyRule(t *testing.T) {
	c := Context{Worktree: "/w", Branch: "feat/x", Issue: 1}
	p := buildAgentPrompt(c)

	mustContain(t, p, "`chore:` (typos, dep bumps")
	mustContain(t, p, "OPTIONAL")
	mustContain(t, p, "`fix:` and `feat:` a body is REQUIRED")
}

// TestBuildAgentPromptV2_IssueTrailerEnforced: when c.Issue > 0,
// the prompt must both show `Issue: #N` in the example and emit
// a Do NOT line that punishes dropping it. PR #135's
// `fix(gtw): restore missing space in the commit-agent prompt`
// shows that regression-hits-production; this test guards the
// fix.
//
// The no-issue branch checks for the Do NOT line specifically
// (not just any "Issue: #" substring) because the body-rules
// section legitimately mentions `Issue: #N` as a generic phrase.
func TestBuildAgentPromptV2_IssueTrailerEnforced(t *testing.T) {
	withIssue := buildAgentPrompt(Context{Worktree: "/w", Branch: "x", Issue: 42})
	mustContain(t, withIssue, "Issue: #42")
	mustContain(t, withIssue, "Do NOT skip the `Issue: #42` trailer")

	noIssue := buildAgentPrompt(Context{Worktree: "/w", Branch: "x", Issue: -1})
	if strings.Contains(noIssue, "Do NOT skip the `Issue:") {
		t.Fatalf("no-issue prompt should not have Issue-trailer Do NOT line:\n%s", noIssue)
	}
	// Also verify the gold example omits the Issue trailer when
	// no issue is provided.
	if strings.Contains(noIssue, "Issue: #\n") || strings.Contains(noIssue, "Issue: #1\n") {
		t.Fatalf("no-issue prompt's gold example should not include Issue: #N trailer:\n%s", noIssue)
	}
}

// TestBuildAgentPromptV2_AntiModalPattern: the single most
// important regression guard. The "subject-only commit for
// non-trivial work" line directly attacks the modal training-data
// pattern. Without it, every other improvement is overridden
// within a few commits. If a future edit drops this line, the
// regression observed in PR #135's commit-prompt-fix returns.
func TestBuildAgentPromptV2_AntiModalPattern(t *testing.T) {
	c := Context{Worktree: "/w", Branch: "feat/x", Issue: 1}
	p := buildAgentPrompt(c)

	mustContain(t, p, "## Do NOT")
	mustContain(t, p, "single subject-only commit for non-trivial work")
	mustContain(t, p, "modal training-data pattern")
}

// TestBuildAgentPromptV2_GoldExample: a real-looking example
// anchors the LLM's depth target. The example is based on
// PR #139's first commit (the gold-standard series). If the
// example is dropped, the LLM has no depth signal beyond the
// "5-15 lines total" rule and drifts back to short bodies.
func TestBuildAgentPromptV2_GoldExample(t *testing.T) {
	c := Context{Worktree: "/w", Branch: "feat/x", Issue: 139}
	p := buildAgentPrompt(c)

	mustContain(t, p, "## Example — the depth target")
	mustContain(t, p, "fix(agent): SignalProcessGroup helper")
	mustContain(t, p, "Issue: #139")
	// Body excerpt — proves the example is substantive, not a
	// 2-line stub that would re-train the LLM to short bodies.
	mustContain(t, p, "kill(-pid, SIGINT)")
	mustContain(t, p, "ESRCH")
}

// TestBuildAgentPromptV2_PreserveHardRules: the 3 rules from
// F-56 §3 (don't push, don't revert/restore/stash, no `git
// add -A`) work as-is. Do not let v2 edits drop or reword them.
// If they need to change, that's a separate F-XX decision.
func TestBuildAgentPromptV2_PreserveHardRules(t *testing.T) {
	c := Context{Worktree: "/w", Branch: "feat/x", Issue: 1}
	p := buildAgentPrompt(c)

	mustContain(t, p, "## Hard rules (non-negotiable)")
	mustContain(t, p, "Do not push.")
	mustContain(t, p, "never run `git push`")
	mustContain(t, p, "Do not revert, restore, or stash")
	mustContain(t, p, "not `git add -A`")
}

func mustContain(t *testing.T, s, sub string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Fatalf("prompt missing %q\n---\n%s\n---", sub, s)
	}
}

// -----------------------------------------------------------------------------
// parseCommitArgs tests (mirror parsePushArgs shape).
// -----------------------------------------------------------------------------

func TestParseCommitArgs_ShortFlag(t *testing.T) {
	got, err := parseCommitArgs([]string{"-a", "claude"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.Agent != "claude" {
		t.Fatalf("Agent = %q, want claude", got.Agent)
	}
}

func TestParseCommitArgs_LongFlag(t *testing.T) {
	got, err := parseCommitArgs([]string{"--agent", "opencode"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.Agent != "opencode" {
		t.Fatalf("Agent = %q, want opencode", got.Agent)
	}
}

func TestParseCommitArgs_Empty(t *testing.T) {
	got, err := parseCommitArgs(nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.Agent != "" {
		t.Fatalf("Agent = %q, want empty", got.Agent)
	}
}

func TestParseCommitArgs_MissingValue(t *testing.T) {
	_, err := parseCommitArgs([]string{"-a"})
	if err == nil {
		t.Fatalf("missing value should error")
	}
}

func TestParseCommitArgs_Multiple(t *testing.T) {
	got, err := parseCommitArgs([]string{"-a", "opencode", "-a", "claude"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.Agent != "claude" {
		t.Fatalf("Agent = %q, want claude (last one wins)", got.Agent)
	}
}

// -----------------------------------------------------------------------------
// dispatchCommit tests (F-56 Branch 2 ownership, F-57 readiness
// gate, F-XX commit/push split).
//
// /gtw commit now owns the agent path entirely: collects
// readiness → refuses conflicts / detached HEAD / no-op on clean
// → headBefore → runAgentToCommit → verifyAgentCommitted →
// re-snapshot (catch agent-introduced conflicts) → commit card.
// -----------------------------------------------------------------------------

// TestRunCommit_NoWorkToDo — clean tree → no work to do, no
// agent call, exit with "ℹ️ nothing to commit".
func TestRunCommit_NoWorkToDo(t *testing.T) {
	git := newPushGit()
	git.onArgs(statusCmd, "## wt-clean...origin/wt-clean\n", "", nil)

	withAgent(t)
	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-clean",
		RepoRoot: mustPwd(t),
	})

	cs := newPushChatSession(t)
	s := captureCh(t, cs)
	_, err := dispatchCommit(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", commitArgs{}, "")
	if err != nil {
		t.Fatalf("dispatchCommit: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "ℹ️ nothing to commit") {
		t.Fatalf("expected no-work-to-do reply, got:\n%s", r)
	}
	for _, c := range git.calls {
		if c.args[0] == "push" {
			t.Fatalf("commit must not call push: %v", c.args)
		}
	}
}

// TestRunCommit_DetachedHead — Branch is "" → refuse.
func TestRunCommit_DetachedHead(t *testing.T) {
	git := newPushGit()
	git.onArgs(statusCmd, "## HEAD (no branch)\n", "", nil)

	withAgent(t)
	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-detached",
		RepoRoot: mustPwd(t),
	})

	cs := newPushChatSession(t)
	s := captureCh(t, cs)
	_, err := dispatchCommit(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", commitArgs{}, "")
	if err != nil {
		t.Fatalf("dispatchCommit: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "detached HEAD") {
		t.Fatalf("expected detached-HEAD refusal, got:\n%s", r)
	}
}

// TestRunCommit_ConflictState — HasConflicts →
// PushBlockReason → exit before agent.
func TestRunCommit_ConflictState(t *testing.T) {
	git := newPushGit()
	git.onArgs(statusCmd,
		"## wt-conflict...origin/wt-conflict\nUU conflict.go\n", "", nil)

	withAgent(t, &recordingAgent{name: "claude"})
	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-conflict",
		RepoRoot: mustPwd(t),
	})
	cs := newPushChatSession(t)
	_ = cs.SetSelectedAgent("claude")

	_, err := dispatchCommit(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", commitArgs{}, "")
	if err != nil {
		t.Fatalf("dispatchCommit: %v", err)
	}
	for _, c := range git.calls {
		if len(c.args) > 0 && c.args[0] == "push" {
			t.Fatalf("conflict must not call push: %v", c.args)
		}
	}
}

// TestRunCommit_HappyPath — F-XX happy path:
//   - entry: dirty porcelain (1 uncommitted) → agent runs
//   - verify: status clean, branch still on c.Branch, HEAD advanced
//   - re-snapshot: HasConflicts false → render commit card
func TestRunCommit_HappyPath(t *testing.T) {
	git := newPushGit()
	// Status sequence: entry dirty → re-snapshot clean.
	git.onArgsSeq(statusCmd,
		pushGitResp{"## wt-dirty...origin/wt-dirty\n M foo.go\n", "", nil},
		pushGitResp{"## wt-dirty...origin/wt-dirty\n", "", nil},
	)
	// rev-parse HEAD sequence: entry headBefore → verify headAfter.
	git.onArgsSeq([]string{"rev-parse", "HEAD"},
		pushGitResp{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n", "", nil},
		pushGitResp{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n", "", nil},
	)
	// verifyAgentCommitted's listUncommittedFiles via `status --porcelain`.
	git.onArgs([]string{"status", "--porcelain"}, "", "", nil)
	// Branch check: still on wt-dirty.
	git.onArgs([]string{"rev-parse", "--abbrev-ref", "HEAD"},
		"wt-dirty\n", "", nil)
	// commit card log query: headBefore..HEAD.
	git.on("log", "bbbbbbbb feat: agent did the thing\n", "", nil)

	pi := &recordingAgent{
		name:        "pi",
		runOnceText: "", // intentionally empty — agent prose is ignored
	}
	withAgent(t, pi)

	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-dirty",
		RepoRoot: mustPwd(t),
		Issue:    7,
	})
	cs := newPushChatSession(t)
	_ = cs.SetSelectedAgent("pi")
	s := captureCh(t, cs)

	_, err := dispatchCommit(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", commitArgs{}, "")
	if err != nil {
		t.Fatalf("dispatchCommit: %v", err)
	}

	pi.mu.Lock()
	piCalls := len(pi.calls)
	pi.mu.Unlock()
	if piCalls != 1 {
		t.Fatalf("agent.RunOnce called %d times, want 1", piCalls)
	}

	// /gtw commit must NOT call git push under any circumstance.
	for _, c := range git.calls {
		if c.args[0] == "push" {
			t.Fatalf("commit must not call push: %v", c.args)
		}
	}

	r := s.lastText()
	if !strings.Contains(r, "🤖 pi committed 1 change(s) on wt-dirty") {
		t.Errorf("expected commit card header, got:\n%s", r)
	}
	if !strings.Contains(r, "feat: agent did the thing") {
		t.Errorf("commit oneline from git log missing, got:\n%s", r)
	}
	if !strings.Contains(r, "> wt-dirty") {
		t.Errorf("> branch line missing, got:\n%s", r)
	}
	if strings.Contains(r, "pushed") {
		t.Errorf("commit card should not contain 'pushed':\n%s", r)
	}
}

// TestRunCommit_DirtyWithAgentFlag — `-a opencode` overrides
// the chat's selected agent.
func TestRunCommit_DirtyWithAgentFlag(t *testing.T) {
	git := newPushGit()
	// Status sequence: entry dirty → re-snapshot clean.
	git.onArgsSeq(statusCmd,
		pushGitResp{"## wt-flag...origin/wt-flag\n M foo.go\n", "", nil},
		pushGitResp{"## wt-flag...origin/wt-flag\n", "", nil},
	)
	// rev-parse HEAD sequence: entry headBefore → verify headAfter
	// (different SHAs so the HEAD-advance check passes).
	git.onArgsSeq([]string{"rev-parse", "HEAD"},
		pushGitResp{"2222222222222222222222222222222222222222\n", "", nil},
		pushGitResp{"3333333333333333333333333333333333333333\n", "", nil},
	)
	// verify (all checks pass).
	git.onArgs([]string{"status", "--porcelain"}, "", "", nil)
	git.onArgs([]string{"rev-parse", "--abbrev-ref", "HEAD"},
		"wt-flag\n", "", nil)
	git.on("log", "3333 feat\n", "", nil)

	opencode := &recordingAgent{
		name:        "opencode",
		runOnceText: "committed",
	}
	claude := &recordingAgent{
		name:        "claude",
		runOnceText: "should-not-be-called",
	}
	withAgent(t, claude, opencode)

	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-flag",
		RepoRoot: mustPwd(t),
	})
	cs := newPushChatSession(t)
	_ = cs.SetSelectedAgent("claude") // chat default = claude

	_, err := dispatchCommit(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", commitArgs{Agent: "opencode"}, "")
	if err != nil {
		t.Fatalf("dispatchCommit: %v", err)
	}

	if len(claude.calls) != 0 {
		t.Fatalf("claude should NOT be called when -a opencode; got %d", len(claude.calls))
	}
	if len(opencode.calls) != 1 {
		t.Fatalf("opencode should be called exactly once; got %d", len(opencode.calls))
	}
}

// TestRunCommit_DirtyAgentLeavesFiles — verifyAgentCommitted's
// worktree-clean check surfaces the file list and refuses commit.
func TestRunCommit_DirtyAgentLeavesFiles(t *testing.T) {
	git := newPushGit()
	git.onArgs(statusCmd,
		"## wt-dirty-leftover...origin/wt-dirty-leftover\n?? new-secret.env\n", "", nil)
	git.onArgs([]string{"rev-parse", "HEAD"},
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef\n", "", nil)
	// verifyAgentCommitted's listUncommittedFiles: agent left
	// the file uncommitted.
	git.onArgs([]string{"status", "--porcelain"},
		"?? new-secret.env\n", "", nil)
	// Branch check (precedes worktree-clean check, in our impl).
	git.on("rev-parse", "wt-dirty-leftover\n", "", nil)

	claude := &recordingAgent{
		name:        "claude",
		runOnceText: "I committed the safe files. The .env is for the user.",
	}
	withAgent(t, claude)

	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-dirty-leftover",
		RepoRoot: mustPwd(t),
		Issue:    7,
	})

	cs := newPushChatSession(t)
	_ = cs.SetSelectedAgent("claude")
	s := captureCh(t, cs)
	_, err := dispatchCommit(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", commitArgs{}, "")
	if err != nil {
		t.Fatalf("dispatchCommit: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "still uncommitted") {
		t.Fatalf("expected diagnostic to mention uncommitted files, got:\n%s", r)
	}
	if !strings.Contains(r, "new-secret.env") {
		t.Fatalf("expected diagnostic to name the uncommitted file, got:\n%s", r)
	}
}

// TestRunCommit_DirtyAgentClaimsDoneButNoCommit — HEAD-advance
// regression: agent returns "Done." without committing. The
// verify's HEAD-advance check catches it.
func TestRunCommit_DirtyAgentClaimsDoneButNoCommit(t *testing.T) {
	git := newPushGit()
	git.onArgs(statusCmd,
		"## wt-noop...origin/wt-noop\n M foo.go\n", "", nil)
	git.onArgs([]string{"rev-parse", "HEAD"},
		"cafebabecafebabecafebabecafebabecafebabe\n", "", nil)
	// verifyAgentCommitted's worktree-clean check passes
	// (post-agent file list is empty — agent stash/restore'd).
	git.onArgs([]string{"status", "--porcelain"},
		"", "", nil)
	// Branch check passes (still on wt-noop).
	git.on("rev-parse", "wt-noop\n", "", nil)

	claude := &recordingAgent{
		name:        "claude",
		runOnceText: "Done.",
	}
	withAgent(t, claude)

	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-noop",
		RepoRoot: mustPwd(t),
	})

	cs := newPushChatSession(t)
	_ = cs.SetSelectedAgent("claude")
	s := captureCh(t, cs)
	_, err := dispatchCommit(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", commitArgs{}, "")
	if err != nil {
		t.Fatalf("dispatchCommit: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "no new commit was created") {
		t.Fatalf("expected HEAD-advance diagnostic, got:\n%s", r)
	}
	if !strings.Contains(r, "cafebabe") {
		t.Fatalf("expected diagnostic to name the unchanged SHA, got:\n%s", r)
	}
}

// TestRunCommit_DirtyAgentSwitchesBranch — agent `git
// checkout`'d to a side branch and committed there. The
// branch-mismatch check refuses.
func TestRunCommit_DirtyAgentSwitchesBranch(t *testing.T) {
	git := newPushGit()
	git.onArgs(statusCmd,
		"## wt-cbranch...origin/wt-cbranch\n M foo.go\n", "", nil)
	git.onArgs([]string{"rev-parse", "HEAD"},
		"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee\n", "", nil)
	// verify: branch check fails first.
	git.on("rev-parse", "wt-side\n", "", nil)

	claude := &recordingAgent{
		name:        "claude",
		runOnceText: "I made a side branch for this work.",
	}
	withAgent(t, claude)

	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-cbranch",
		RepoRoot: mustPwd(t),
	})

	cs := newPushChatSession(t)
	_ = cs.SetSelectedAgent("claude")
	s := captureCh(t, cs)
	_, err := dispatchCommit(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", commitArgs{}, "")
	if err != nil {
		t.Fatalf("dispatchCommit: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "wt-side") || !strings.Contains(r, "wt-cbranch") {
		t.Fatalf("expected branch-mismatch diagnostic naming both branches, got:\n%s", r)
	}
}

// TestRunCommit_AgentIntroducedConflicts — verify passes
// (clean from listUncommittedFiles' perspective), but the
// re-snapshot's PushBlockReason catches the agent-introduced
// conflict and refuses.
func TestRunCommit_AgentIntroducedConflicts(t *testing.T) {
	git := newPushGit()
	// Status sequence: entry dirty → re-snapshot has conflict.
	git.onArgsSeq(statusCmd,
		pushGitResp{"## wt-bad-agent...origin/wt-bad-agent\n M foo.go\n", "", nil},
		pushGitResp{"## wt-bad-agent...origin/wt-bad-agent\nUU conflict.go\n", "", nil},
	)
	// rev-parse HEAD sequence: entry headBefore → verify headAfter.
	git.onArgsSeq([]string{"rev-parse", "HEAD"},
		pushGitResp{"6666666666666666666666666666666666666666\n", "", nil},
		pushGitResp{"7777777777777777777777777777777777777777\n", "", nil},
	)
	// verify's listUncommittedFiles returns empty porcelain —
	// UU lines look malformed (no path after first 4 chars).
	git.onArgs([]string{"status", "--porcelain"}, "", "", nil)
	git.onArgs([]string{"rev-parse", "--abbrev-ref", "HEAD"},
		"wt-bad-agent\n", "", nil)

	claude := &recordingAgent{name: "claude", runOnceText: "I introduced a conflict."}
	withAgent(t, claude)

	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-bad-agent",
		RepoRoot: mustPwd(t),
	})
	cs := newPushChatSession(t)
	_ = cs.SetSelectedAgent("claude")

	s := captureCh(t, cs)
	_, err := dispatchCommit(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", commitArgs{}, "")
	if err != nil {
		t.Fatalf("dispatchCommit: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "unmerged paths") {
		t.Fatalf("expected conflict reply after agent, got:\n%s", r)
	}
	for _, c := range git.calls {
		if c.args[0] == "push" {
			t.Fatalf("commit must not call push: %v", c.args)
		}
	}
}

// TestRunCommit_NoAgentSelected — no chat SelectedAgent, no
// yml agent, no `-a` flag → agent name is "" → refuse.
func TestRunCommit_NoAgentSelected(t *testing.T) {
	git := newPushGit()
	git.onArgs(statusCmd,
		"## wt-noagent...origin/wt-noagent\n M foo.go\n", "", nil)
	git.onArgs([]string{"rev-parse", "HEAD"},
		"9999999999999999999999999999999999999999\n", "", nil)

	withAgent(t) // no agents registered
	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-noagent",
		RepoRoot: mustPwd(t),
	})

	cs := newPushChatSession(t)
	// Don't set SelectedAgent.
	s := captureCh(t, cs)

	_, err := dispatchCommit(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", commitArgs{}, "")
	if err != nil {
		t.Fatalf("dispatchCommit: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "no agent selected") {
		t.Fatalf("expected no-agent reply, got:\n%s", r)
	}
	// No RunOnce call possible — registry was empty.
	if len(git.calls) > 0 {
		t.Logf("git calls observed (verify is OK, but verify must be skipped): %v", git.calls)
	}
}

// TestRunCommit_UnknownAgent — `-a nope` but registry has no
// `nope` → "unknown agent" reply.
func TestRunCommit_UnknownAgent(t *testing.T) {
	git := newPushGit()
	git.onArgs(statusCmd,
		"## wt-unknown...origin/wt-unknown\n M foo.go\n", "", nil)
	git.onArgs([]string{"rev-parse", "HEAD"},
		"8888888888888888888888888888888888888888\n", "", nil)

	withAgent(t) // empty registry
	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-unknown",
		RepoRoot: mustPwd(t),
	})
	cs := newPushChatSession(t)
	_ = cs.SetSelectedAgent("claude")

	_, err := dispatchCommit(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", commitArgs{Agent: "nope"}, "")
	if err != nil {
		t.Fatalf("dispatchCommit: %v", err)
	}
}

// TestRunCommit_AgentBinaryMissing — agent.Detect fails →
// refuse and never call RunOnce.
func TestRunCommit_AgentBinaryMissing(t *testing.T) {
	git := newPushGit()
	git.onArgs(statusCmd,
		"## wt-missing...origin/wt-missing\n M foo.go\n", "", nil)
	git.onArgs([]string{"rev-parse", "HEAD"},
		"7777777777777777777777777777777777777777\n", "", nil)

	claude := &recordingAgent{
		name:        "claude",
		detectErr:   errors.New("claude: command not found"),
		runOnceText: "should-not-be-called",
	}
	withAgent(t, claude)

	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-missing",
		RepoRoot: mustPwd(t),
	})
	cs := newPushChatSession(t)
	_ = cs.SetSelectedAgent("claude")

	_, err := dispatchCommit(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", commitArgs{}, "")
	if err != nil {
		t.Fatalf("dispatchCommit: %v", err)
	}
	if len(claude.calls) != 0 {
		t.Fatalf("Detect failed → RunOnce must NOT be called")
	}
}

// TestRunCommit_AgentRunOnceError — agent.RunOnce returns
// error → refuse with diagnostic.
func TestRunCommit_AgentRunOnceError(t *testing.T) {
	git := newPushGit()
	git.onArgs(statusCmd,
		"## wt-error...origin/wt-error\n M foo.go\n", "", nil)
	git.onArgs([]string{"rev-parse", "HEAD"},
		"3333333333333333333333333333333333333333\n", "", nil)

	claude := &recordingAgent{
		name:       "claude",
		runOnceErr: errors.New("agent crashed"),
	}
	withAgent(t, claude)

	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-agenterr",
		RepoRoot: mustPwd(t),
	})
	cs := newPushChatSession(t)
	_ = cs.SetSelectedAgent("claude")

	_, err := dispatchCommit(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", commitArgs{}, "")
	if err != nil {
		t.Fatalf("dispatchCommit: %v", err)
	}
	if len(claude.calls) != 1 {
		t.Fatalf("expected 1 RunOnce call (which returned error)")
	}
}
