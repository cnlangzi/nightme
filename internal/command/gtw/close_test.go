package gtw

import (
	"context"
	"fmt"
	"github.com/cnlangzi/nightme/internal/messages"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/chatsession"
)

// closeTestRig bundles the dependencies RunClose needs. Lives
// here rather than in a shared test helper because no other test
// file needs this exact setup.
//
// v1.5: the `slot` field is gone. /gtw close is now driven
// entirely by the cwd-scoped yml at <SelectedCwd>/.nightme/gtw.yml;
// seedFix below writes it, and the success-path assertion is
// "git worktree remove ran" (which removes the yml with the
// worktree in production). Failure-path assertions check that
// no destructive git command ran.
type closeTestRig struct {
	cs   *chatsession.ChatSession
	deps HandlerDeps
	git  *programmableGit
	rec  *closeTestRecCh
}

// programmableGit is a fakeGit whose response per (subcommand)
// the test pre-records. Lets us simulate "worktree remove" /
// "branch -D" success / failure and "status" clean / dirty from
// a single fake without scattering switch-cases across files.
//
// Sync-step calls (`symbolic-ref`, `rev-parse`, `checkout`,
// `pull`) return ("", "", nil) by default — they're harmless
// for tests that have SkipRefreshDefaultBranch=true (set by
// newCloseRig) and which therefore never reach the sync path.
// Tests that DO want to exercise sync override the rig's
// SkipRefreshDefaultBranch flag and pre-stage the responses
// they need on this fake.
type programmableGit struct {
	// statusResp is returned for any `status --porcelain` call.
	// Empty string = clean worktree.
	statusResp string

	// worktreeRemoveErr is the error (and stderr) returned for
	// any `worktree remove` call. nil = success.
	worktreeRemoveErr    error
	worktreeRemoveStderr string

	// branchDeleteErr is the error (and stderr) returned for
	// any `branch -D` call. nil = success. Defaults to nil —
	// happy-path close tests rely on this.
	branchDeleteErr    error
	branchDeleteStderr string

	// syncOriginRef is the value returned for any
	// `symbolic-ref --short refs/remotes/origin/HEAD` call
	// (used by DefaultBranch during the sync step). Empty
	// means "fail to discover default branch" — renderSyncReply
	// falls back to raw pull output in that case.
	syncOriginRef string

	// symbolicRefErr is the error returned for any
	// `symbolic-ref` call. nil = success. Tests that want to
	// exercise sync failure (e.g. the "no upstream" path) set
	// this to a non-nil error; the rest of the sync steps then
	// never run because DefaultBranch short-circuits.
	symbolicRefErr error

	// syncPullOut is the value returned for the `pull --rebase`
	// call inside RefreshDefaultBranch. Empty means success
	// with no captured stdout (which still falls through to the
	// "renderSyncReply can't parse, return raw" path). Tests
	// that want a specific sync card pre-set this to something
	// like "Already up to date." or "Updating abc..def\n...".
	syncPullOut string

	// remoteURLResp is the value returned for any
	// `remote get-url origin` call (used by the new
	// removeWIPLabel step to detect the provider / parse
	// owner/repo). Empty means "no origin remote" — the label
	// helper surfaces this as a ❌ reply and aborts the close.
	// Tests that wire a fake provider for label cleanup
	// pre-set this to e.g. "https://github.com/cnlangzi/nightme.git".
	remoteURLResp string

	// calls records every (args) the fake saw, for assertions
	// about which commands RunClose issued.
	calls [][]string
}

func (p *programmableGit) Run(_ context.Context, dir string, args ...string) (string, string, error) {
	p.calls = append(p.calls, append([]string(nil), args...))

	switch {
	case len(args) >= 1 && args[0] == "status":
		return p.statusResp, "", nil
	case len(args) >= 2 && args[0] == "worktree" && args[1] == "remove":
		return "", p.worktreeRemoveStderr, p.worktreeRemoveErr
	case len(args) >= 2 && args[0] == "branch" && args[1] == "-D":
		return "", p.branchDeleteStderr, p.branchDeleteErr
	case len(args) >= 1 && args[0] == "symbolic-ref":
		// DefaultBranch's call. Return configured ref +
		// a trailing newline (matches real git); or the
		// configured error if the test wants sync to fail.
		if p.symbolicRefErr != nil {
			return "", "", p.symbolicRefErr
		}
		return p.syncOriginRef + "\n", "", nil
	case len(args) >= 1 && args[0] == "pull":
		return p.syncPullOut, "", nil
	case len(args) >= 3 && args[0] == "remote" && args[1] == "get-url":
		return p.remoteURLResp, "", nil
	}
	return "", "", nil
}

func newCloseRig(t *testing.T) *closeTestRig {
	t.Helper()

	rig := &closeTestRig{
		git: &programmableGit{},
	}
	// Use a per-test ChatSession via the existing helper. Each
	// test gets its own chatID so they don't bleed state. The
	// recording channel captures every Send so tests can assert
	// the reply text via cs.Emitter().
	rec := &closeTestRecCh{}
	cs, _ := chatsession.New("chat-close-"+t.Name(), "test-agent")
	cs.WithEmitter(rec).WithAgentSessionPool(chatsession.NewAgentSessionPool())
	_ = cs.SetSelectedCwd("/tmp/start") // neutral starting cwd; tests overwrite
	rig.cs = cs
	rig.rec = rec
	// Inject a shim HandlerDeps whose Send funnels into the same
	// recorder so the legacy assertion style keeps working while
	// the production path is cs.Emitter().Send.
	//
	// SkipRefreshDefaultBranch=true is the default test seam for
	// /gtw close — the basic happy / sad-path tests don't want
	// the sync side-trip and don't pre-stage fake responses for
	// symbolic-ref / rev-parse / pull. Tests that DO exercise
	// sync (TestRunClose_Sync* cases) flip this flag and
	// configure the fake accordingly.
	rig.deps = HandlerDeps{
		Git:                      rig.git,
		Now:                      func() time.Time { return time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC) },
		SkipRefreshDefaultBranch: true,
	}
	return rig
}

// closeTestRecCh is a per-test recording channel that captures
// every Send call. Mirrors the pattern in
// close_integration_test.go but lives here because the unit
// tests want a no-network rig.
type closeTestRecCh struct {
	mu    sync.Mutex
	sends []messages.OutboundMessage
}

func (r *closeTestRecCh) Send(_ context.Context, m messages.OutboundMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sends = append(r.sends, m)
	return nil
}
func (r *closeTestRecCh) Patch(_ context.Context, m messages.OutboundMessage) error {
	r.Send(context.Background(), m)
	return nil
}

func (r *closeTestRecCh) lastText() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.sends) == 0 {
		return ""
	}
	return r.sends[len(r.sends)-1].Text
}

// seedFix writes a complete fix snapshot into
// `<wt>/.nightme/gtw.yml` (the way /gtw fix does) and points
// the ChatSession at the worktree, mimicking the post-fix state
// that RunClose expects to find.
func seedFix(t *testing.T, rig *closeTestRig, wt, repoRoot string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(wt, nightmeDirName), 0o755); err != nil {
		t.Fatalf("mkdir .nightme: %v", err)
	}
	if err := WriteGTWYml(wt, Context{
		Mode:     ModeLocal,
		Issue:    -1,
		Branch:   "fix/42-test",
		Worktree: wt,
		RepoRoot: repoRoot,
		State:    StateFixing,
	}, rig.deps.Now); err != nil {
		t.Fatalf("seed WriteGTWYml: %v", err)
	}
	if err := rig.cs.SetSelectedCwd(wt); err != nil {
		t.Fatalf("seed SetSelectedCwd: %v", err)
	}
	// v1.5: no in-memory slot to seed — the yml is the cwd-scoped
	// source of truth, and the chat's SelectedCwd + yml together
	// are the full "active fix" state.
}

// seedFixRemote writes a ModeRemote yml at <wt>/.nightme/gtw.yml
// so the new step 2.5 (nightme/wip label removal) has a real
// issue to act on. Wires the fake provider into rig.deps.Detect
// so resolveProvider's Detect-fallback returns the test's
// recording stub.
//
// Repo / Provider are intentionally left empty in the yml:
// resolveProvider's yml-cached shortcut (pr.go:774) bypasses
// deps.Detect and constructs a real provider via NewProvider —
// impossible to fake in a unit test. Leaving the yml empty
// forces the Detect-fallback path, which honours deps.Detect.
// The shortcut path itself is covered by pr_test.go:2297; what
// these tests assert (RemoveIssueLabel called with the right
// args, gate semantics on failure) is invariant across the two
// routes because both converge on the same provider method call.
//
// Defaults to issue #42 — match the existing fix-side test
// fixtures so the test names stay grep-able.
func seedFixRemote(t *testing.T, rig *closeTestRig, prov *fakeGitProvider, wt, repoRoot string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(wt, nightmeDirName), 0o755); err != nil {
		t.Fatalf("mkdir .nightme: %v", err)
	}
	if err := WriteGTWYml(wt, Context{
		Mode:     ModeRemote,
		Issue:    42,
		Branch:   "fix/42-test",
		Worktree: wt,
		RepoRoot: repoRoot,
		State:    StateFixing,
	}, rig.deps.Now); err != nil {
		t.Fatalf("seed WriteGTWYml: %v", err)
	}
	if err := rig.cs.SetSelectedCwd(wt); err != nil {
		t.Fatalf("seed SetSelectedCwd: %v", err)
	}
	// Wire the fake provider so resolveProvider's Detect
	// fallback returns it. Without this, Detect falls through
	// to the package-level Detect, which probes the live
	// internet (and would fail in tests anyway).
	rig.deps.Detect = fakeDetect(prov)
	// Stage the git fake's `remote get-url origin` response to
	// match the fake provider's host. cnlangzi/nightme on
	// github.com is the canonical test fixture.
	rig.git.remoteURLResp = "https://github.com/cnlangzi/nightme.git"
}

// --- tests ---

// TestRunClose_CleanWorktree_Success is the happy path: yml
// exists, status is clean, git worktree remove succeeds, the
// local branch is deleted, CWD switches back, in-memory Context
// is cleared, and a success card lands in the chat.
//
// Sync is exercised separately by TestRunClose_CleanWorktree_Syncs;
// here SkipRefreshDefaultBranch=true (the rig default) means
// buildSyncReply returns ("", nil) and no second card is sent.
func TestRunClose_CleanWorktree_Success(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	seedFix(t, rig, wt, repoRoot)

	res, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1")
	if err != nil {
		t.Fatalf("RunClose: %v", err)
	}
	if res == nil || !res.Consumed {
		t.Fatalf("Result = %+v, want Consumed=true", res)
	}

	// CWD must be back at repoRoot.
	if got := rig.cs.SelectedCwd(); got != repoRoot {
		t.Errorf("SelectedCwd after close = %q, want %q", got, repoRoot)
	}
	// v1.5: no in-memory slot to clear. The yml goes away with
	// the worktree at step 4 in production; in this test the
	// fake git doesn't actually remove the directory, but the
	// success-card assertions below prove the happy path
	// completed end-to-end.
	// git worktree remove must have been called from inside
	// repoRoot, with the worktree path as argument.
	sawRemove := false
	for _, args := range rig.git.calls {
		if len(args) >= 3 && args[0] == "worktree" && args[1] == "remove" {
			sawRemove = true
			if filepath.Clean(args[2]) != filepath.Clean(wt) {
				t.Errorf("worktree remove target = %q, want %q", args[2], wt)
			}
		}
	}
	if !sawRemove {
		t.Errorf("git worktree remove not called; calls=%v", rig.git.calls)
	}
	// git branch -D must have been called against the branch
	// captured in the yml snapshot. Order vs worktree remove
	// isn't asserted here — only that both fire — because the
	// step ordering is covered by TestRunClose_BranchDeleteFails
	// (which proves worktree must come first).
	sawBranchDel := false
	for _, args := range rig.git.calls {
		if len(args) >= 3 && args[0] == "branch" && args[1] == "-D" {
			sawBranchDel = true
			if args[2] != "fix/42-test" {
				t.Errorf("branch -D target = %q, want %q", args[2], "fix/42-test")
			}
		}
	}
	if !sawBranchDel {
		t.Errorf("git branch -D not called; calls=%v", rig.git.calls)
	}
	// Success reply must mention the branch + worktree + branch
	// deletion. The SkipRefreshDefaultBranch path sends exactly
	// one card (close's own), so lastText() is the full reply.
	got := rig.rec.lastText()
	if !strings.Contains(got, "fix/42-test") {
		t.Errorf("success reply missing branch:\n%s", got)
	}
	if !strings.Contains(got, "worktree") {
		t.Errorf("success reply missing worktree label:\n%s", got)
	}
	if !strings.Contains(got, "deleted") {
		t.Errorf("success reply missing branch-deleted line:\n%s", got)
	}
}

// TestRunClose_DirtyWorktree_Rejected verifies the v1 "no
// --force" rule. Any porcelain output from `git status` makes
// the close fail with a per-file preview and the in-memory
// Context must NOT be touched.
func TestRunClose_DirtyWorktree_Rejected(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	seedFix(t, rig, wt, repoRoot)

	rig.git.statusResp = " M foo.txt\n?? untracked.go\n"

	res, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1")
	if err != nil {
		t.Fatalf("RunClose: %v", err)
	}
	if res == nil || !res.Consumed {
		t.Fatalf("Result = %+v, want Consumed=true", res)
	}

	// CWD must still be the worktree.
	if got := rig.cs.SelectedCwd(); got != wt {
		t.Errorf("SelectedCwd after dirty close = %q, want %q (unchanged)", got, wt)
	}
	// v1.5: SelectedCwd unchanged above is the canonical
	// "state preserved on failure" check; no in-memory slot
	// to verify.
	// git worktree remove must NOT have been called.
	for _, args := range rig.git.calls {
		if len(args) >= 2 && args[0] == "worktree" && args[1] == "remove" {
			t.Errorf("git worktree remove called despite dirty worktree: %v", args)
		}
	}
	// Reply must list the dirty files.
	reply := rig.rec.lastText()
	if !strings.Contains(reply, "foo.txt") {
		t.Errorf("dirty reply missing foo.txt:\n%s", reply)
	}
	if !strings.Contains(reply, "untracked.go") {
		t.Errorf("dirty reply missing untracked.go:\n%s", reply)
	}
	if !strings.Contains(reply, "commit or stash") {
		t.Errorf("dirty reply missing commit hint:\n%s", reply)
	}
}

// TestRunClose_NoYml verifies the "no active fix" path. CWD
// points somewhere with no .nightme/gtw.yml — reply explains
// and no git commands run.
func TestRunClose_NoYml(t *testing.T) {
	wt := t.TempDir()

	rig := newCloseRig(t)
	if err := rig.cs.SetSelectedCwd(wt); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}

	res, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1")
	if err != nil {
		t.Fatalf("RunClose: %v", err)
	}
	if res == nil || !res.Consumed {
		t.Fatalf("Result = %+v, want Consumed=true", res)
	}

	if len(rig.git.calls) != 0 {
		t.Errorf("git called despite missing yml: %v", rig.git.calls)
	}
	reply := rig.rec.lastText()
	if !strings.Contains(reply, "no active fix") {
		t.Errorf("reply = %q, want 'no active fix'", reply)
	}
	if !strings.Contains(reply, "/cwd") {
		t.Errorf("reply missing /cwd hint:\n%s", reply)
	}
}

// TestRunClose_GitRemoveFails verifies a failed `git worktree
// remove` produces an error reply with the stderr tail, and
// state (CWD + slot) is left intact for the user to retry.
func TestRunClose_GitRemoveFails(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	seedFix(t, rig, wt, repoRoot)

	rig.git.worktreeRemoveErr = &fakeExitError{code: 128, msg: "worktree remove failed"}
	rig.git.worktreeRemoveStderr = "fatal: could not remove worktree"

	res, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1")
	if err != nil {
		t.Fatalf("RunClose: %v", err)
	}
	if !res.Consumed {
		t.Fatalf("Result.Consumed = false")
	}

	// State must be untouched so user can retry.
	if got := rig.cs.SelectedCwd(); got != wt {
		t.Errorf("SelectedCwd after git failure = %q, want %q (unchanged)", got, wt)
	}
	// v1.5: SelectedCwd unchanged above is the canonical
	// "state preserved on failure" check; no in-memory slot
	// to verify.
	// Reply must surface the git stderr tail.
	reply := rig.rec.lastText()
	if !strings.Contains(reply, "git worktree remove failed") {
		t.Errorf("reply missing git error:\n%s", reply)
	}
	if !strings.Contains(reply, "git stderr tail") {
		t.Errorf("reply missing stderr-tail label:\n%s", reply)
	}
}

// TestRunClose_BranchDeleteFails verifies the new step-5 branch
// delete: when `git branch -D` errors AFTER the worktree has
// already been removed, RunClose surfaces a manual cleanup hint
// and leaves the chat state (CWD + slot) untouched — matching
// the worktree-remove failure contract. The branch ref lingers
// in the local repo; the user follows the printed cleanup
// command.
//
// This test relies on SkipRefreshDefaultBranch=true (the rig
// default) so the failure is isolated to step 5, not step 9.
func TestRunClose_BranchDeleteFails(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	seedFix(t, rig, wt, repoRoot)

	rig.git.branchDeleteErr = &fakeExitError{code: 1, msg: "branch delete failed"}
	rig.git.branchDeleteStderr = "error: branch 'fix/42-test' not found"

	res, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1")
	if err != nil {
		t.Fatalf("RunClose: %v", err)
	}
	if !res.Consumed {
		t.Fatalf("Result.Consumed = false")
	}

	// Order matters: worktree remove must have run first, then
	// branch -D. Anything after step 5 is forbidden.
	sawRemove, sawBranchDel := false, false
	branchDelIdx := -1
	for i, args := range rig.git.calls {
		if len(args) >= 3 && args[0] == "worktree" && args[1] == "remove" {
			sawRemove = true
		}
		if len(args) >= 3 && args[0] == "branch" && args[1] == "-D" {
			sawBranchDel = true
			branchDelIdx = i
		}
	}
	if !sawRemove {
		t.Errorf("git worktree remove not called; calls=%v", rig.git.calls)
	}
	if !sawBranchDel {
		t.Errorf("git branch -D not called; calls=%v", rig.git.calls)
	}
	if branchDelIdx >= 0 && branchDelIdx < len(rig.git.calls)-1 {
		// If something ran AFTER branch -D, the failure didn't
		// short-circuit as designed.
		t.Errorf("git commands ran after branch -D failure: %v", rig.git.calls[branchDelIdx+1:])
	}
	// State must be untouched (still on worktree).
	if got := rig.cs.SelectedCwd(); got != wt {
		t.Errorf("SelectedCwd after branch-delete fail = %q, want %q (unchanged)", got, wt)
	}
	// v1.5: SelectedCwd unchanged above is the canonical
	// "state preserved on failure" check; no in-memory slot
	// to verify.
	// Reply must surface the manual cleanup hint.
	got := rig.rec.lastText()
	if !strings.Contains(got, "git branch -D") {
		t.Errorf("reply missing branch-delete error label:\n%s", got)
	}
	if !strings.Contains(got, "worktree at") || !strings.Contains(got, "already removed") {
		t.Errorf("reply missing manual cleanup hint:\n%s", got)
	}
	if !strings.Contains(got, "git branch -D fix/42-test") {
		t.Errorf("reply missing concrete cleanup command:\n%s", got)
	}
}

// TestRunClose_CleanWorktree_Syncs verifies the new step-9
// sync: when SkipRefreshDefaultBranch is FALSE, RunClose issues
// a `git pull --rebase` against repoRoot and emits a SECOND
// card after the close card. The fake git here returns a
// representative "Already up to date." stdout for `pull`, which
// is the simplest sync-success path through renderSyncReply.
//
// This test exists because the other close tests deliberately
// set SkipRefreshDefaultBranch=true and therefore never reach
// step 9; the "happy path with sync" combination needs an
// explicit home.
func TestRunClose_CleanWorktree_Syncs(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	seedFix(t, rig, wt, repoRoot)
	// Opt INTO sync — the rig default is to skip it.
	rig.deps.SkipRefreshDefaultBranch = false
	// Stage sync responses: an "Already up to date." stdout is
	// the simplest path through renderSyncReply — branch name
	// comes from symbolic-ref, then the "Already" branch in
	// renderSyncReply short-circuits before touching `log`.
	rig.git.syncOriginRef = "origin/main"
	rig.git.syncPullOut = "Already up to date.\n"

	res, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1")
	if err != nil {
		t.Fatalf("RunClose: %v", err)
	}
	if !res.Consumed {
		t.Fatalf("Result.Consumed = false")
	}

	// Exactly two cards must have been sent: close's own + sync's.
	if got := len(rig.rec.sends); got != 2 {
		t.Fatalf("cards sent = %d, want 2 (close + sync); sends=%v", got, rig.rec.sends)
	}
	closeCard := rig.rec.sends[0].Text
	syncCard := rig.rec.sends[1].Text
	if !strings.Contains(closeCard, "closed") {
		t.Errorf("first card missing 'closed':\n%s", closeCard)
	}
	if !strings.Contains(closeCard, "deleted") {
		t.Errorf("first card missing 'deleted':\n%s", closeCard)
	}
	if !strings.Contains(syncCard, "already up to date") {
		t.Errorf("second card missing sync success line:\n%s", syncCard)
	}
	// State cleanup still applied (close ran to completion).
	if got := rig.cs.SelectedCwd(); got != repoRoot {
		t.Errorf("SelectedCwd after close = %q, want %q", got, repoRoot)
	}
	// v1.5: no in-memory slot to verify.
}

// TestRunClose_SyncFails verifies that when step 9 (sync) errors,
// close's local-cleanup success card has already been sent AND a
// SECOND error card surfaces the sync failure with a ❌ prefix.
// CWD + slot are cleared (close ran to completion); the user just
// sees two cards explaining the partial result.
//
// We force sync failure by making the programmableGit return an
// error for the very first sync call (`symbolic-ref` from
// DefaultBranch). This is the simplest way to simulate "sync
// can't refresh main" without pre-staging a dozen fake git
// responses.
func TestRunClose_SyncFails(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	seedFix(t, rig, wt, repoRoot)
	rig.deps.SkipRefreshDefaultBranch = false

	// Make the very first sync call (`symbolic-ref`) fail —
	// DefaultBranch then errors, buildSyncReply returns the
	// error, RunClose emits the sync-failure card.
	rig.git.symbolicRefErr = &fakeExitError{code: 128, msg: "no upstream"}

	res, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1")
	if err != nil {
		t.Fatalf("RunClose: %v", err)
	}
	if !res.Consumed {
		t.Fatalf("Result.Consumed = false")
	}

	// Two cards: close success + sync error.
	if got := len(rig.rec.sends); got != 2 {
		t.Fatalf("cards sent = %d, want 2 (close + sync error); sends=%v", got, rig.rec.sends)
	}
	closeCard := rig.rec.sends[0].Text
	syncCard := rig.rec.sends[1].Text
	if !strings.Contains(closeCard, "closed") {
		t.Errorf("first card missing close headline:\n%s", closeCard)
	}
	if !strings.Contains(syncCard, "❌") {
		t.Errorf("second card missing ❌ prefix:\n%s", syncCard)
	}
	if !strings.Contains(syncCard, "sync failed") {
		t.Errorf("second card missing sync-failure label:\n%s", syncCard)
	}
	if !strings.Contains(syncCard, "origin") && !strings.Contains(syncCard, "upstream") && !strings.Contains(syncCard, "remote") {
		t.Errorf("second card missing sync-failure guidance:\n%s", syncCard)
	}
	// State still cleaned up — close's local steps ran to completion.
	if got := rig.cs.SelectedCwd(); got != repoRoot {
		t.Errorf("SelectedCwd after close = %q, want %q", got, repoRoot)
	}
	// v1.5: no in-memory slot to verify.
}

// fakeExitError stands in for exec.ExitError without dragging in
// os/exec. The real one is a struct with embedded ProcessState;
// RunClose's WorktreeError.Unwrap → Err chain is what matters.
type fakeExitError struct {
	code int
	msg  string
}

func (e *fakeExitError) Error() string { return e.msg }

// --- regression tests for the step 0.5 / step 5.5 fix ---
//
// Two bug classes drove this fix:
//   1. /gtw close in a chat whose SelectedCwd had been torn down
//      by another chat (or an external `git worktree remove`)
//      reported the misleading "❌ no active fix to close in this
//      chat" line. The step 0.5 safety net now distinguishes
//      "directory missing" from "yml missing" and emits a
//      tailored warning.
//   2. Every /gtw close left AgentSession subprocesses that
//      were spawned into the now-deleted worktree running as
//      orphans. Long-lived daemons accumulated these until
//      they dragged the host down. The fix closes+drops them
//      on every /gtw close (both step 0.5 and step 5.5 happy
//      path).
//
// Helper-level coverage of EvictAgentSessionsInCwd
// (with a live bridge handle, asserting as.Close() is invoked)
// lives in internal/chatsession/test_close_drop_test.go. The
// tests here exercise RunClose's logic flow only; the seeded
// AgentSessions use StatusExited so the helper takes the "stale
// entry, no live process to signal" branch — still cleans the
// pool, but doesn't increment the user-facing "closed N
// orphaned agent(s)" line. That count is exercised by the
// chatsession-package test.

// seedAgentSession attaches a minimal AgentSession to the rig's
// ChatSession pool, pinned to `cwd`, marked as StatusExited.
// StatusExited is intentional: the gtw-package close_test rig
// has no live bridge handle to give to `agent.NewAgent` (the
// driver type is unexported). StatusExited trips the
// not-alive branch in EvictAgentSessionsInCwd —
// DropAgentSession still fires, but no goroutine for as.Close()
// is spawned, so the "closed N orphaned agent(s)" count stays
// at 0. Real-bridge, count-asserting tests live in
// internal/chatsession/test_close_drop_test.go.
func seedAgentSession(t *testing.T, rig *closeTestRig, agentName, cwd string) {
	t.Helper()
	as := chatsession.NewAgentSession("as-"+agentName+"-test", rig.cs.ChatID, agentName, cwd, nil)
	as.SetStatusForTest(chatsession.StatusExited)
	rig.cs.AttachAgentSessionForTest(as)
}

// TestRunClose_DanglingSelectedCwd exercises the safety net for
// SelectedCwd pointing at a path that no longer exists (case
// (a)/(b) in step 0.5). The yml+dir are gone (we RemoveAll'd wt
// before invoking), one AgentSession pinned to wt sits in the
// pool; RunClose must short-circuit before git worktree remove,
// emit the unified "directory is unreachable" reply, clear
// SelectedCwd, and tidy up the pool.
//
// v1.5: the slotMatched branch (which used to distinguish
// "this chat's fix worktree was torn down" from "unrelated
// dangling dir") was removed along with the in-memory slot.
// The unified message covers both root causes via its
// parenthetical. See TestRunClose_TransientStatErrorPreservesState
// for the matching stat-stub coverage.
func TestRunClose_DanglingSelectedCwd(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	seedFix(t, rig, wt, repoRoot)
	seedAgentSession(t, rig, "test-agent", wt)

	// Simulate "another chat's /gtw close tore down our worktree"
	// (or an external `rm -rf`) by removing the entire worktree
	// directory. The yml goes with it.
	if err := os.RemoveAll(wt); err != nil {
		t.Fatalf("RemoveAll wt: %v", err)
	}

	res, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1")
	if err != nil {
		t.Fatalf("RunClose: %v", err)
	}
	if res == nil || !res.Consumed {
		t.Fatalf("Result = %+v, want Consumed=true", res)
	}
	if got := rig.cs.SelectedCwd(); got != "" {
		t.Errorf("SelectedCwd after dangling close = %q, want \"\"", got)
	}
	// v1.5: no in-memory slot to verify.
	// No git calls — the safety net must short-circuit before
	// step 4 (WorktreeRemove).
	for _, args := range rig.git.calls {
		if len(args) >= 2 && args[0] == "worktree" && args[1] == "remove" {
			t.Errorf("git worktree remove called on dangling close: %v", args)
		}
	}
	// Pool must be empty — the seeded StatusExited AS still got
	// dropped by EvictAgentSessionsInCwd's stale-entry
	// branch.
	if n := len(rig.cs.Pool()); n != 0 {
		t.Errorf("pool after dangling close = %d entries, want 0", n)
	}
	reply := rig.rec.lastText()
	// The seeded AS was StatusExited (no live bridge), but
	// it was still pinned to the now-gone worktree — the
	// pool entry was still removed by the safety net, so
	// the "closed N orphaned agent(s)" line MUST appear
	// with N=1.
	if !strings.Contains(reply, "dropped 1 orphaned agent session(s)") {
		t.Errorf("reply missing 'dropped 1 orphaned agent session(s)' line:\n%s", reply)
	}
	if !strings.Contains(reply, "directory is unreachable") {
		t.Errorf("reply missing 'directory is unreachable' prefix:\n%s", reply)
	}
	if !strings.Contains(reply, "another /gtw close") {
		t.Errorf("reply missing root-cause line:\n%s", reply)
	}
	if !strings.Contains(reply, "/cwd") {
		t.Errorf("reply missing /cwd hint:\n%s", reply)
	}
}

// TestRunClose_EmptySelectedCwd defends the cmd.go:364-389
// dispatcher (which doesn't preflight empty SelectedCwd before
// invoking RunClose). Without the explicit empty-cwd branch in
// step 0.5, ReadGTWYml("") would resolve to "./.nightme/gtw.yml"
// — whatever the daemon's CWD happens to be — and emit a
// misleading "no active fix" reply.
func TestRunClose_EmptySelectedCwd(t *testing.T) {
	rig := newCloseRig(t)
	// newCloseRig seeds SelectedCwd="/tmp/start"; reset via the
	// new public API to exercise the empty-cwd branch.
	rig.cs.ClearSelectedCwd()

	res, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1")
	if err != nil {
		t.Fatalf("RunClose: %v", err)
	}
	if res == nil || !res.Consumed {
		t.Fatalf("Result = %+v, want Consumed=true", res)
	}
	if len(rig.git.calls) != 0 {
		t.Errorf("git called despite empty SelectedCwd: %v", rig.git.calls)
	}
	reply := rig.rec.lastText()
	if !strings.Contains(reply, "No active workspace") {
		t.Errorf("reply = %q, want 'No active workspace'", reply)
	}
	if !strings.Contains(reply, "/cwd") {
		t.Errorf("reply missing /cwd hint:\n%s", reply)
	}
}

// TestRunClose_Success_NoAgents_NoExtraLine is a
// regression-coverage extension of TestRunClose_CleanWorktree_Success:
// the Step 5.5 AS cleanup must NOT add the "→ agents: N closed"
// line to the success card when no AS was seeded. The existing
// happy path is gated on zero ASes; this test pins that.
func TestRunClose_Success_NoAgents_NoExtraLine(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	seedFix(t, rig, wt, repoRoot)
	// Intentionally NO seedAgentSession call — pool stays empty.

	// The success path needs git worktree remove + branch -D to
	// succeed. programmableGit returns ("", "", nil) by default
	// for unrecognised calls, so just running RunClose is enough.
	// No preconditions on git calls; programmableGit returns
	// success by default. RunClose is expected to issue
	// worktree remove + branch -D; the happy-path assertions
	// below verify both effects indirectly via CWD + slot
	// state.

	res, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1")
	if err != nil {
		t.Fatalf("RunClose: %v", err)
	}
	if res == nil || !res.Consumed {
		t.Fatalf("Result = %+v, want Consumed=true", res)
	}
	reply := rig.rec.lastText()
	if strings.Contains(reply, "agents:") {
		t.Errorf("reply incorrectly mentions agents when pool was empty:\n%s", reply)
	}
	if !strings.Contains(reply, "fix/42-test") {
		t.Errorf("reply missing branch label:\n%s", reply)
	}
}

// TestRunClose_Success_ClosesOrphanedAgents covers step 5.5
// (the happy-path AS cleanup). Seeded ASes are StatusExited so
// the gtw-package rig avoids needing a live bridge — the
// close-and-drop path runs (pool entry gets dropped), but the
// "closed N orphaned agent(s)" count stays 0 (StatusExited is
// not "alive" per the helper). The pool emptiness after the
// success path is the testable assertion for THIS rig; the
// count line is covered by the chatsession-package test.
func TestRunClose_Success_ClosesOrphanedAgents(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	seedFix(t, rig, wt, repoRoot)
	seedAgentSession(t, rig, "test-agent", wt)
	seedAgentSession(t, rig, "other-agent", wt)

	if n := len(rig.cs.Pool()); n != 2 {
		t.Fatalf("setup: pool = %d entries, want 2", n)
	}

	// No preconditions on git calls; programmableGit returns
	// success by default. RunClose is expected to issue
	// worktree remove + branch -D; the happy-path assertions
	// below verify both effects indirectly via CWD + slot
	// state.

	res, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1")
	if err != nil {
		t.Fatalf("RunClose: %v", err)
	}
	if res == nil || !res.Consumed {
		t.Fatalf("Result = %+v, want Consumed=true", res)
	}
	if n := len(rig.cs.Pool()); n != 0 {
		t.Errorf("pool after success close = %d entries, want 0", n)
	}
	if got := rig.cs.SelectedCwd(); got != repoRoot {
		t.Errorf("SelectedCwd after success close = %q, want %q", got, repoRoot)
	}
	// v1.5: no in-memory slot to verify.
	reply := rig.rec.lastText()
	// Both seeded ASes are StatusExited but are still pinned
	// to the worktree about to be removed — the safety net
	// removes their pool entries, so the count must reflect
	// them. The "closed N orphaned agent(s)" line appears with
	// N=2.
	if !strings.Contains(reply, "agents: 2 dropped") {
		t.Errorf("reply missing 'agents: 2 dropped' line:\n%s", reply)
	}
}

// TestRunClose_Success_ResetsRepoRootAgentContext exercises
// step 10: an AgentSession pinned to c.RepoRoot (the new cwd
// after step 6) survives step 2.5's worktree-pinned cleanup
// and must be reset by /new. The "marked fresh" path applies
// (StatusExited → no live conversation to reset, but stale
// SessionID cleared per F-43 §5.4).
//
// Asserts:
//   - The repoRoot AS stays in the pool but its SessionID is
//     cleared (a fresh cold-start would otherwise resume the
//     dead conversation).
//   - Two cards are sent (close + /new; sync is skipped via
//     SkipRefreshDefaultBranch=true). The /new card names the
//     agent and uses the "marked fresh" vocabulary.
//   - The worktree-pinned AS is still dropped by step 2.5 —
//     proves the /new step does NOT accidentally re-evict it.
func TestRunClose_Success_ResetsRepoRootAgentContext(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	seedFix(t, rig, wt, repoRoot)
	// Two ASes at the worktree (must be dropped by step 2.5).
	seedAgentSession(t, rig, "wt-agent-a", wt)
	seedAgentSession(t, rig, "wt-agent-b", wt)
	// One AS at the new cwd (must survive, then be reset by step 10).
	seedAgentSession(t, rig, "repo-agent", repoRoot)

	// Capture the repoRoot AS reference and pre-populate its
	// SessionID so we can assert step 10 cleared it.
	var repoAS *chatsession.AgentSession
	for _, as := range rig.cs.Pool() {
		if as.Cwd == repoRoot {
			repoAS = as
			break
		}
	}
	if repoAS == nil {
		t.Fatalf("setup: no AS pinned to repoRoot")
	}
	repoAS.SetSessionID("stale-resume-id-must-be-cleared")
	if got := repoAS.SessionID(); got != "stale-resume-id-must-be-cleared" {
		t.Fatalf("setup: SessionID = %q, want pre-populated", got)
	}

	res, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1")
	if err != nil {
		t.Fatalf("RunClose: %v", err)
	}
	if res == nil || !res.Consumed {
		t.Fatalf("Result = %+v, want Consumed=true", res)
	}

	// repoRoot AS SessionID must be cleared (step 10). It may remain
	// warm in asPool without being remounted into cs.pool.
	if got := repoAS.SessionID(); got != "" {
		t.Errorf("repoRoot AS SessionID after close = %q, want empty (step 10 must clear)", got)
	}
	if n := len(rig.cs.AgentSessionsForCwd(repoRoot)); n != 1 {
		t.Errorf("asPool/repoRoot after close = %d entries, want 1 (the repoRoot survivor)", n)
	}
	// Worktree-pinned ASes must be gone from asPool.
	if n := len(rig.cs.AgentSessionsForCwd(wt)); n != 0 {
		t.Errorf("worktree asPool after close = %d, want 0", n)
	}

	// Card count: SkipRefreshDefaultBranch=true means no sync
	// card. Two cards expected — close + /new.
	rig.rec.mu.Lock()
	defer rig.rec.mu.Unlock()
	if got := len(rig.rec.sends); got != 2 {
		// Dump texts on mismatch so the failure message
		// tells us which cards fired.
		texts := make([]string, len(rig.rec.sends))
		for i, m := range rig.rec.sends {
			texts[i] = m.Text
		}
		t.Fatalf("cards sent = %d, want 2 (close + /new):\n%s",
			got, strings.Join(texts, "\n---\n"))
	}

	// First card: close's own. Second card: /new result.
	closeCard := rig.rec.sends[0].Text
	newCard := rig.rec.sends[1].Text
	if !strings.Contains(closeCard, "fix/42-test") {
		t.Errorf("first card missing branch label:\n%s", closeCard)
	}
	if !strings.Contains(newCard, "repo-agent") {
		t.Errorf("/new card missing repo-agent name:\n%s", newCard)
	}
	// FormatResetResults uses "marked fresh" wording for
	// StatusExited survivors — proves the right branch ran.
	if !strings.Contains(newCard, "marked fresh") {
		t.Errorf("/new card missing 'marked fresh' wording:\n%s", newCard)
	}
}

// (rigSetCwd removed; we don't need it — RunClose takes the
// ChatSession's SelectedCwd directly.)

// withStatStub swaps the package-level statPath for the duration
// of the test and restores it via t.Cleanup. Used by transient
// stat-error tests that need a non-IsNotExist failure (EACCES,
// EIO, etc.) without chmod'ing the worktree directory —
// chmod 000 is silently bypassed for root on Linux and the test
// would race with later reads anyway.
func withStatStub(t *testing.T, fn func(string) (os.FileInfo, error)) {
	t.Helper()
	orig := statPath
	statPath = fn
	t.Cleanup(func() { statPath = orig })
}

// TestRunClose_TransientStatError_PreservesState covers the
// non-IsNotExist branch of RunClose's stat-failure handling
// (close.go step 0.5): when the worktree is reachable but stat
// fails for a transient reason (EACCES / EIO / ESTALE / ENOTDIR
// / ENOTCONN), the chat must surface the error as an IM reply
// WITHOUT clearing slot, selectedCwd, or the AgentSession pool.
// The user can retry once the path is reachable again.
//
// Companion to TestRunClose_DanglingSelectedCwd_FixActive
// (IsNotExist branch with slot) and
// TestRunClose_DanglingSelectedCwd_SlotEmpty (IsNotExist branch
// without slot). Locks in the wording of the transient-reply
// and the state-preservation contract; a future "simplification"
// pass that conflates the two branches would regress this.
func TestRunClose_TransientStatError_PreservesState(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()
	rig := newCloseRig(t)
	seedFix(t, rig, wt, repoRoot)
	seedAgentSession(t, rig, "test-agent", wt)

	// Snapshot pre-state so we can prove nothing was cleared.
	selectedCwdBefore := rig.cs.SelectedCwd()
	poolLenBefore := len(rig.cs.Pool())
	gitCallsBefore := len(rig.git.calls)

	// Force a non-IsNotExist stat error (EACCES-like). The
	// worktree directory still exists on disk — stat is what
	// fails. (os.ErrPermission is the EACCES-equivalent sentinel;
	// os.IsNotExist(os.ErrPermission) is false.)
	withStatStub(t, func(path string) (os.FileInfo, error) {
		return nil, os.ErrPermission
	})

	res, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1")
	if err != nil {
		t.Fatalf("RunClose: %v", err)
	}
	if res == nil || !res.Consumed {
		t.Fatalf("Result = %+v, want Consumed=true", res)
	}

	// State preserved: selectedCwd, AS pool, no git calls.
	// (v1.5: no in-memory slot to verify.)
	if got := rig.cs.SelectedCwd(); got != selectedCwdBefore {
		t.Errorf("SelectedCwd = %q after transient stat error, want %q (preserved)", got, selectedCwdBefore)
	}
	if n := len(rig.cs.Pool()); n != poolLenBefore {
		t.Errorf("pool after transient stat error = %d entries, want %d (preserved)", n, poolLenBefore)
	}
	// No git calls at all — the safety net short-circuits before
	// step 4 (WorktreeRemove), and step 2.5 (close pinned ASes)
	// did not run either (stat failure path is above both).
	if len(rig.git.calls) != gitCallsBefore {
		t.Errorf("git runner got %d new calls on transient stat error, want 0",
			len(rig.git.calls)-gitCallsBefore)
	}

	// Reply text: the new transient-reply wording.
	reply := rig.rec.lastText()
	for _, want := range []string{
		"cannot reach workspace",
		"(stat:",
		"transient",
		"retry /gtw close",
		"agent sessions are left intact",
	} {
		if !strings.Contains(reply, want) {
			t.Errorf("reply missing %q:\n%s", want, reply)
		}
	}
	// The reply must NOT contain the dangling-clear wording from
	// the IsNotExist branch — that branch's whole job is to clear
	// state, which we explicitly do NOT want here.
	for _, forbidden := range []string{
		"cleared the in-flight fix",
		"cleared the dangling cwd",
		"worktree directory is unreachable",
		"directory is unreachable",
		"orphaned agent", // the IsNotExist branch counts dropped ASes
	} {
		if strings.Contains(reply, forbidden) {
			t.Errorf("reply unexpectedly contains %q (transient path should not say this):\n%s", forbidden, reply)
		}
	}
}

// --- step 2.5: nightme/wip label removal (ModeRemote) --------

// TestRunClose_ModeRemote_RemovesWIPLabel verifies the happy
// path of the new label-cleanup gate: ModeRemote yml, fake
// provider returns nil from RemoveIssueLabel, RunClose should
//
//   (a) call provider.RemoveIssueLabel with the right
//       (owner, repo, issueID, label) — i.e. nightme/wip on
//       cnlangzi/nightme#42,
//   (b) THEN proceed to the local cleanup (worktree remove,
//       branch -D) — i.e. the gate succeeded and execution
//       continued, and
//   (c) surface the label removal on the success card so the
//       user sees both halves of the close.
func TestRunClose_ModeRemote_RemovesWIPLabel(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	seedFixRemote(t, rig, prov, wt, repoRoot)

	res, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1")
	if err != nil {
		t.Fatalf("RunClose: %v", err)
	}
	if res == nil || !res.Consumed {
		t.Fatalf("Result = %+v, want Consumed=true", res)
	}

	// (a) the helper must have called RemoveIssueLabel exactly
	// once, with the right args. CallsByMethod filters by method
	// name so unrelated future calls don't pollute the slice.
	rmCalls := prov.CallsByMethod("RemoveIssueLabel")
	if len(rmCalls) != 1 {
		t.Fatalf("RemoveIssueLabel calls = %d, want 1; all calls=%v", len(rmCalls), prov.Calls())
	}
	got := rmCalls[0]
	if got.Owner != "cnlangzi" || got.Repo != "nightme" {
		t.Errorf("RemoveIssueLabel owner/repo = %s/%s, want cnlangzi/nightme", got.Owner, got.Repo)
	}
	if got.ID != 42 {
		t.Errorf("RemoveIssueLabel ID = %d, want 42", got.ID)
	}
	if got.Label != LabelWIP {
		t.Errorf("RemoveIssueLabel label = %q, want %q", got.Label, LabelWIP)
	}

	// (b) local cleanup must have happened AFTER label removal.
	// Order matters: RemoveIssueLabel is the gate. We assert it
	// came before worktree remove + branch -D by index in
	// (prov.calls ∪ git.calls) — but since those slices are on
	// different goroutines (mock provider vs fake git), the
	// only robust ordering check is "both local-cleanup
	// commands fired AND the provider call fired". The unit
	// test for ordering under failure (next test) covers the
	// "local cleanup did NOT fire on label failure" half.
	sawRemove, sawBranchDel := false, false
	for _, args := range rig.git.calls {
		if len(args) >= 3 && args[0] == "worktree" && args[1] == "remove" {
			sawRemove = true
		}
		if len(args) >= 3 && args[0] == "branch" && args[1] == "-D" {
			sawBranchDel = true
		}
	}
	if !sawRemove {
		t.Errorf("git worktree remove not called after label success; calls=%v", rig.git.calls)
	}
	if !sawBranchDel {
		t.Errorf("git branch -D not called after label success; calls=%v", rig.git.calls)
	}

	// (c) success card must surface the label removal so the
	// user sees the platform-side half of the close. We assert
	// on the full text — "nightme/wip removed" is the literal
	// phrasing of the new card line — and on owner/repo/issue
	// appearing in canonical form.
	gotText := rig.rec.lastText()
	if !strings.Contains(gotText, "nightme/wip removed") {
		t.Errorf("success card missing 'nightme/wip removed':\n%s", gotText)
	}
	if !strings.Contains(gotText, "cnlangzi/nightme#42") {
		t.Errorf("success card missing canonical issue ref:\n%s", gotText)
	}
}

// TestRunClose_ModeRemote_LabelRemovalFails_AbortsBeforeLocalCleanup
// verifies the gate semantics: when RemoveIssueLabel returns an
// error, RunClose must NOT proceed to worktree remove or branch
// -D, must NOT switch CWD, and must surface the platform error
// in the reply. This is the core invariant of "label cleanup
// first; local cleanup only on success" — without this test the
// gate could regress to "label cleanup happens somewhere in the
// flow, but failures are ignored".
func TestRunClose_ModeRemote_LabelRemovalFails_AbortsBeforeLocalCleanup(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	// Configure the fake to return an error from
	// RemoveIssueLabel — simulates auth expired / network down
	// / repo moved. The error message is intentionally
	// distinctive so we can grep for it in the reply.
	prov.SetRemoveIssueLabelErr(fmt.Errorf("403 Forbidden: token expired"))
	seedFixRemote(t, rig, prov, wt, repoRoot)

	res, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1")
	if err != nil {
		t.Fatalf("RunClose: %v", err)
	}
	if res == nil || !res.Consumed {
		t.Fatalf("Result = %+v, want Consumed=true", res)
	}

	// RemoveIssueLabel was called (the helper tried); we don't
	// need to assert args here since the previous test covers
	// that, but we do verify it WAS attempted and that the
	// attempt's error caused the gate to trigger.
	if calls := prov.CallsByMethod("RemoveIssueLabel"); len(calls) != 1 {
		t.Fatalf("RemoveIssueLabel calls = %d, want 1 (gate must try)", len(calls))
	}

	// THE CORE INVARIANT: no local cleanup must have happened.
	for _, args := range rig.git.calls {
		if len(args) >= 2 && args[0] == "worktree" && args[1] == "remove" {
			t.Errorf("git worktree remove called despite label-removal failure: %v", args)
		}
		if len(args) >= 3 && args[0] == "branch" && args[1] == "-D" {
			t.Errorf("git branch -D called despite label-removal failure: %v", args)
		}
	}

	// CWD unchanged: user is still pointed at the worktree so
	// they can retry /gtw close after fixing auth / network.
	if got := rig.cs.SelectedCwd(); got != wt {
		t.Errorf("SelectedCwd after label-removal failure = %q, want %q (unchanged)", got, wt)
	}

	// Reply must surface the underlying error so the user can
	// act on it (re-auth, retry, etc.) and must NOT claim the
	// close succeeded.
	gotReply := rig.rec.lastText()
	if !strings.Contains(gotReply, "403 Forbidden") {
		t.Errorf("reply missing underlying platform error:\n%s", gotReply)
	}
	if !strings.Contains(gotReply, "nightme/wip") {
		t.Errorf("reply missing label name:\n%s", gotReply)
	}
	if !strings.Contains(gotReply, "left intact") {
		t.Errorf("reply must explain state is left intact for retry:\n%s", gotReply)
	}
	if strings.Contains(gotReply, "✅ closed") {
		t.Errorf("reply must not claim close succeeded on label failure:\n%s", gotReply)
	}
}

// TestRunClose_ModeLocal_SkipsLabelCleanup verifies the ModeLocal
// branch: c.Issue == -1, no LabelWIP was ever added, so the
// helper must short-circuit before touching git remote or the
// provider. This protects the existing ModeLocal happy path
// (TestRunClose_CleanWorktree_Success) from regressing into
// requiring a configured origin remote / provider — local
// worktrees don't have a remote issue.
//
// We use a "detect panic" check: deps.Detect is set to a
// function that panics if called. If the helper tries to call
// detect, the test fails loudly. If the helper short-circuits
// (correct behaviour), detect is never called.
func TestRunClose_ModeLocal_SkipsLabelCleanup(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	seedFix(t, rig, wt, repoRoot)
	// Defensive: prove the helper does NOT touch git remote or
	// the provider in ModeLocal. If anything in the label path
	// fires, this trips before the normal close path runs.
	rig.deps.Detect = func(context.Context, string, HTTPProber, string) (GitProvider, error) {
		t.Fatalf("deps.Detect called in ModeLocal — removeWIPLabel should have short-circuited")
		return nil, nil
	}

	res, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1")
	if err != nil {
		t.Fatalf("RunClose: %v", err)
	}
	if res == nil || !res.Consumed {
		t.Fatalf("Result = %+v, want Consumed=true", res)
	}

	// Local cleanup must still run normally — the gate is
	// label-only, so a skipped label means the rest of the
	// flow is unchanged.
	if got := rig.cs.SelectedCwd(); got != repoRoot {
		t.Errorf("SelectedCwd after close = %q, want %q", got, repoRoot)
	}

	// Success card must NOT mention label removal — there was
	// no label, and the helper returned (false, "", ""), so
	// the card stays on its existing 4-line shape.
	gotText := rig.rec.lastText()
	if strings.Contains(gotText, "nightme/wip removed") {
		t.Errorf("ModeLocal success card must not mention label removal:\n%s", gotText)
	}
}
