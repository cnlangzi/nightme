package gtw

import (
	"github.com/cnlangzi/nightme/internal/messages"
	"context"
	"sync"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/chatsession"
)



// closeTestRig bundles the dependencies RunClose needs. Lives
// here rather than in a shared test helper because no other test
// file needs this exact setup.
type closeTestRig struct {
	cs        *chatsession.ChatSession
	slot      *memSlot
	deps      HandlerDeps
	sentTexts []string
	git       *programmableGit
	rec       *closeTestRecCh
}

// memSlot is an in-memory ContextSlot for tests. Real production
// uses Manager.states[chatID] via managerContextSlot; tests just
// need Load/Store round-trips.
type memSlot struct{ c Context }

func (m *memSlot) Load() Context    { return m.c }
func (m *memSlot) Store(c Context)  { m.c = c }

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
	worktreeRemoveErr error
	worktreeRemoveStderr string

	// branchDeleteErr is the error (and stderr) returned for
	// any `branch -D` call. nil = success. Defaults to nil —
	// happy-path close tests rely on this.
	branchDeleteErr error
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
	}
	return "", "", nil
}

func newCloseRig(t *testing.T) *closeTestRig {
	t.Helper()

	rig := &closeTestRig{
		slot: &memSlot{},
		git:  &programmableGit{},
	}
	// Use a per-test ChatSession via the existing helper. Each
	// test gets its own chatID so they don't bleed state. The
	// recording channel captures every Send so tests can assert
	// the reply text via cs.Emitter().
	rec := &closeTestRecCh{}
	cs, _ := chatsession.New("chat-close-" + t.Name(), "test-agent")
	cs.WithEmitter(rec)
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
		Git:                    rig.git,
		Now:                    func() time.Time { return time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC) },
		SkipRefreshDefaultBranch: true,
	}
	return rig
}

// closeTestRecCh is a per-test recording channel that captures
// every Send / SendCard / Patch call. Mirrors the pattern in
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
func (r *closeTestRecCh) SendCard(_ context.Context, m messages.OutboundMessage) (string, error) {
	r.Send(context.Background(), m)
	return "rec-card-id", nil
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
	rig.slot.Store(Context{
		Mode: ModeLocal, Issue: -1, Branch: "fix/42-test",
		Worktree: wt, RepoRoot: repoRoot, State: StateFixing,
		UpdatedAt: rig.deps.Now(),
	})
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

	res, err := RunClose(context.Background(), rig.cs, rig.slot, rig.deps, rig.cs.ChatID, "msg-1")
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
	// In-memory context must be cleared.
	if got := rig.slot.Load(); got != (Context{}) {
		t.Errorf("slot.Load() after close = %+v, want zero Context", got)
	}
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

	res, err := RunClose(context.Background(), rig.cs, rig.slot, rig.deps, rig.cs.ChatID, "msg-1")
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
	// In-memory context must be unchanged.
	gotCtx := rig.slot.Load()
	if gotCtx.Branch != "fix/42-test" || gotCtx.Worktree != wt {
		t.Errorf("slot.Load() after dirty close = %+v, want fix/42-test / %q", gotCtx, wt)
	}
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

	res, err := RunClose(context.Background(), rig.cs, rig.slot, rig.deps, rig.cs.ChatID, "msg-1")
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

	res, err := RunClose(context.Background(), rig.cs, rig.slot, rig.deps, rig.cs.ChatID, "msg-1")
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
	if got := rig.slot.Load(); got.Branch != "fix/42-test" {
		t.Errorf("slot cleared despite git failure: %+v", got)
	}
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

	res, err := RunClose(context.Background(), rig.cs, rig.slot, rig.deps, rig.cs.ChatID, "msg-1")
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
	// State must be untouched (still on worktree, slot still
	// holds the original Context).
	if got := rig.cs.SelectedCwd(); got != wt {
		t.Errorf("SelectedCwd after branch-delete fail = %q, want %q (unchanged)", got, wt)
	}
	if got := rig.slot.Load(); got.Branch != "fix/42-test" {
		t.Errorf("slot cleared despite branch-delete failure: %+v", got)
	}
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

	res, err := RunClose(context.Background(), rig.cs, rig.slot, rig.deps, rig.cs.ChatID, "msg-1")
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
	if got := rig.slot.Load(); got != (Context{}) {
		t.Errorf("slot.Load() after close = %+v, want zero Context", got)
	}
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

	res, err := RunClose(context.Background(), rig.cs, rig.slot, rig.deps, rig.cs.ChatID, "msg-1")
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
	if got := rig.slot.Load(); got != (Context{}) {
		t.Errorf("slot.Load() after close = %+v, want zero Context", got)
	}
}

// fakeExitError stands in for exec.ExitError without dragging in
// os/exec. The real one is a struct with embedded ProcessState;
// RunClose's WorktreeError.Unwrap → Err chain is what matters.
type fakeExitError struct {
	code int
	msg  string
}

func (e *fakeExitError) Error() string { return e.msg }

// (rigSetCwd removed; we don't need it — RunClose takes the
// ChatSession's SelectedCwd directly.)