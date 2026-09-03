package gtw

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
)

// --- tests ---

// TestRunBack_HappyPath: yml exists, cwd moves to repoRoot,
// NO destructive git calls fire (no worktree remove, no branch
// -D), and the success card lands with the "preserved"
// vocabulary that signals back is non-destructive.
//
// The sync card is exercised separately by
// TestRunBack_RunsSync; here SkipRefreshDefaultBranch=true
// (the rig default) means buildSyncReply returns ("", nil) and
// only the back card is sent.
func TestRunBack_HappyPath(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	seedFix(t, rig, wt, repoRoot)

	res, err := RunBack(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1")
	if err != nil {
		t.Fatalf("RunBack: %v", err)
	}
	if res == nil || !res.Consumed {
		t.Fatalf("Result = %+v, want Consumed=true", res)
	}

	// CWD must move back to repoRoot.
	if got := rig.cs.SelectedCwd(); got != repoRoot {
		t.Errorf("SelectedCwd after back = %q, want %q", got, repoRoot)
	}

	// git worktree remove MUST NOT have been called — the
	// whole non-destructive premise.
	for _, args := range rig.git.calls {
		if len(args) >= 2 && args[0] == "worktree" && args[1] == "remove" {
			t.Errorf("back invoked 'git worktree remove' (non-destructive): %v", args)
		}
	}
	// git branch -D MUST NOT have been called either.
	for _, args := range rig.git.calls {
		if len(args) >= 3 && args[0] == "branch" && args[1] == "-D" {
			t.Errorf("back invoked 'git branch -D' (non-destructive): %v", args)
		}
	}

	// The yml MUST still exist on disk — back never removes it.
	if _, statErr := readGTWYmlOrSkip(t, wt); statErr != nil {
		t.Errorf(".nightme/gtw.yml gone after back: %v", statErr)
	}

	// Success card must use the "preserved" vocabulary so the
	// user can tell back apart from close at a glance.
	got := rig.rec.lastText()
	if !strings.Contains(got, "preserved") {
		t.Errorf("back reply missing 'preserved' wording:\n%s", got)
	}
	if !strings.Contains(got, repoRoot) {
		t.Errorf("back reply missing repoRoot %q:\n%s", repoRoot, got)
	}
	// And must NOT contain close's destructive vocabulary —
	// seeing "deleted" or "(removed)" in a back card would
	// mean we accidentally shipped close's body.
	if strings.Contains(got, "deleted") || strings.Contains(got, "(removed)") {
		t.Errorf("back reply uses close-style destructive wording:\n%s", got)
	}
}

// TestRunBack_DirtyWorktree_StillSucceeds is the key behavioral
// difference from /gtw close: a dirty worktree (porcelain-visible
// changes) does NOT block /gtw back. The whole point of back is
// "step out without losing your uncommitted work" — refusing on
// dirty would defeat that.
//
// (Compare TestRunClose_DirtyWorktree_Rejected, which checks the
// same rig.git.statusResp against RunClose and asserts refusal.)
func TestRunBack_DirtyWorktree_StillSucceeds(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	seedFix(t, rig, wt, repoRoot)

	rig.git.statusResp = " M foo.txt\n?? untracked.go\n"

	res, err := RunBack(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1")
	if err != nil {
		t.Fatalf("RunBack: %v", err)
	}
	if res == nil || !res.Consumed {
		t.Fatalf("Result = %+v, want Consumed=true", res)
	}
	if got := rig.cs.SelectedCwd(); got != repoRoot {
		t.Errorf("SelectedCwd after back = %q, want %q", got, repoRoot)
	}
	// The reply must be the success card, not a dirty-refusal
	// error. close's dirty-refusal contains "git status" — we
	// assert back's reply does NOT contain it.
	got := rig.rec.lastText()
	if strings.Contains(got, "git status") {
		t.Errorf("back reply looks like dirty-refusal:\n%s", got)
	}
	if !strings.Contains(got, "preserved") {
		t.Errorf("back reply missing 'preserved' wording:\n%s", got)
	}
}

// TestRunBack_NoYml: missing .nightme/gtw.yml → "no active fix"
// refusal. Mirrors TestRunClose_NoYml; the underlying yml gate
// is the same. Back adds the word "back out of" to the hint so
// the user knows which command produced the refusal (the same
// error string would otherwise look identical to close's, which
// is confusing when a /gtw close and /gtw back race in the
// same chat).
func TestRunBack_NoYml(t *testing.T) {
	wt := t.TempDir()

	rig := newCloseRig(t)
	if err := rig.cs.SetSelectedCwd(wt); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}

	res, err := RunBack(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1")
	if err != nil {
		t.Fatalf("RunBack: %v", err)
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

// TestRunBack_PreservesInMemoryContext: RunBack must NOT call
// slot.Store(Context{}) the way RunClose does. The slot is the
// reaction-routing key for the active fix; clearing it would
// silently break reactions if the user /cwd's back into the
// worktree (the yml is still there, but the in-memory
// hot-path cache would be empty until the next read).
func TestRunBack_PreservesInMemoryContext(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	seedFix(t, rig, wt, repoRoot)

	// Snapshot the pre-back slot state.
	preSlot := rig.slot.Load()
	if preSlot.State == "" {
		t.Fatalf("seedFix failed to populate slot")
	}

	if _, err := RunBack(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1"); err != nil {
		t.Fatalf("RunBack: %v", err)
	}

	postSlot := rig.slot.Load()
	if postSlot != preSlot {
		t.Errorf("slot mutated by back:\n  pre=%+v\n  post=%+v", preSlot, postSlot)
	}
}

// TestRunBack_RunsSync verifies the second card (the sync
// summary from buildSyncReply) lands AFTER the back-success
// card. SkipRefreshDefaultBranch is flipped off here so the
// sync step actually executes; symbolic-ref is pre-staged to
// "main" + a "Already up to date." pull so the sync card has
// predictable text.
func TestRunBack_RunsSync(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	seedFix(t, rig, wt, repoRoot)
	rig.deps.SkipRefreshDefaultBranch = false
	rig.git.syncOriginRef = "origin/main"
	rig.git.syncPullOut = "Already up to date.\n"

	if _, err := RunBack(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1"); err != nil {
		t.Fatalf("RunBack: %v", err)
	}

	// Two cards sent: back-success, then sync.
	rig.rec.mu.Lock()
	defer rig.rec.mu.Unlock()
	if got := len(rig.rec.sends); got < 2 {
		t.Fatalf("sends = %d, want >= 2 (back + sync)", got)
	}
	// First card = back success.
	first := rig.rec.sends[0].Text
	if !strings.Contains(first, "preserved") {
		t.Errorf("first card missing back-success wording:\n%s", first)
	}
	// Last card = sync summary (Already up to date. → renderSyncReply
	// prints the "✅ main @ <sha>" header + "Already up to date." body).
	last := rig.rec.sends[len(rig.rec.sends)-1].Text
	if !strings.Contains(last, "main") {
		t.Errorf("last card missing sync 'main' line:\n%s", last)
	}
}

// TestRunBack_SyncFails: a sync error surfaces a ❌ card AFTER
// the back-success card. The back card must still stand (the
// cwd swap genuinely succeeded) — close has the same contract;
// see close.go step 10's "Single terminal step" comment.
func TestRunBack_SyncFails(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	seedFix(t, rig, wt, repoRoot)
	rig.deps.SkipRefreshDefaultBranch = false
	// Force symbolic-ref to fail so DefaultBranch returns an
	// error and RefreshDefaultBranch surfaces it as the sync
	// error. Stderr/stdout both empty (matches real git's
	// "no upstream configured" shape closely enough).
	rig.git.symbolicRefErr = errors.New("fatal: no upstream configured")

	if _, err := RunBack(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1"); err != nil {
		t.Fatalf("RunBack: %v", err)
	}

	// Cwd still moved (back succeeded before sync ran).
	if got := rig.cs.SelectedCwd(); got != repoRoot {
		t.Errorf("SelectedCwd after back = %q, want %q", got, repoRoot)
	}
	// Slot still populated (back succeeded).
	if rig.slot.Load().State == "" {
		t.Errorf("slot cleared despite back success")
	}

	rig.rec.mu.Lock()
	defer rig.rec.mu.Unlock()
	if got := len(rig.rec.sends); got < 2 {
		t.Fatalf("sends = %d, want >= 2 (back + sync-error)", got)
	}
	last := rig.rec.sends[len(rig.rec.sends)-1].Text
	if !strings.Contains(last, "❌") || !strings.Contains(last, "sync failed") {
		t.Errorf("last card missing '❌ ... sync failed':\n%s", last)
	}
}

// TestFactory_Handle_Back covers the Handle-level dispatch:
// /gtw back must reach runBack (which short-circuits with the
// "no active fix" reply when there's no yml, so we don't need
// a fully-seeded fix to assert the dispatch landed). Mirrors
// TestFactory_Handle_Fix_NoArgs in shape.
//
// F-51 argv convention: commander prefixes Args with the
// command name, so production callers see ["gtw", "back"].
// Args[2:] is the user-supplied tail; for /gtw back this must
// be empty (runBack's ParseCmdArgs gate rejects a non-empty
// tail).
func TestFactory_Handle_Back_NoYml(t *testing.T) {
	// RunBack writes its reply through cs.Emitter().Send, not via
	// SlashOutput.Reply (RunBack itself returns *Result{Consumed:
	// true}, with the user-visible text going through the
	// per-chat channel). Wire up a recording Emitter so the test
	// can assert what actually got sent.
	rec := &closeTestRecCh{}
	cs := &chatsession.ChatSession{}
	cs.WithEmitter(rec)
	f := NewFactory(NewManager())
	got, err := f.Handle(context.Background(), command.RuntimeServices{},
		nil, cs,
		command.SlashInput{Text: "/gtw back", Args: []string{"gtw", "back"}})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !got.Consumed {
		t.Errorf("expected Consumed, got %+v", got)
	}
	// runBack short-circuits on the missing yml with "no active fix".
	text := rec.lastText()
	if !strings.Contains(text, "no active fix") {
		t.Errorf("emitted text = %q, want 'no active fix'", text)
	}
	if !strings.Contains(text, "back out of") {
		t.Errorf("emitted text = %q, want back-variant 'back out of' wording", text)
	}
}

// TestFactory_Handle_Back_RejectsExtraArgs covers the issue
// #291 contract: /gtw back takes no positional args, so a
// non-empty tail must surface an error rather than silently
// running the no-arg flow.
func TestFactory_Handle_Back_RejectsExtraArgs(t *testing.T) {
	cs := &chatsession.ChatSession{}
	f := NewFactory(NewManager())
	got, _ := f.Handle(context.Background(), command.RuntimeServices{},
		nil, cs,
		command.SlashInput{Text: "/gtw back --force",
			Args: []string{"gtw", "back", "--force"}})
	if !got.Consumed {
		t.Errorf("expected Consumed, got %+v", got)
	}
	if !strings.Contains(got.Reply, "❌") {
		t.Errorf("expected ❌ error for /gtw back --force, got %q", got.Reply)
	}
}

// --- helpers ---

// readGTWYmlOrSkip is a thin wrapper that reads the yml from a
// worktree path and returns stat-style errors so the happy-path
// test can assert "yml still on disk after back". It t.Skip()
// if the wrapper can't be reached — fail-loud so a future
// refactor that moves ReadGTWYml doesn't silently make the
// assertion pass for the wrong reason.
func readGTWYmlOrSkip(t *testing.T, wt string) (struct{}, error) {
	t.Helper()
	c, err := ReadGTWYml(wt)
	if err != nil {
		return struct{}{}, err
	}
	if c.Worktree == "" || c.RepoRoot == "" {
		return struct{}{}, fmt.Errorf("yml malformed: Worktree=%q RepoRoot=%q",
			c.Worktree, c.RepoRoot)
	}
	return struct{}{}, nil
}

// silence unused-import warnings if a future refactor removes
// every reference below.
var _ = filepath.Clean
var _ = command.SlashInput{}
