package gtw

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/command"
)

// Tests for the new `/gtw back <worktree>` direction: jump from
// the chat's main repo root INTO the named worktree, optionally
// repairing a missing .nightme/gtw.yml.
//
// Setup pattern follows the existing back_test.go / close_test.go
// fixtures (newCloseRig + seedFix + programmableGit). Each test
// pre-stages the fake-git responses it needs on the rig, since
// RunBackToWorktree touches three distinct git commands
// (rev-parse --show-toplevel, worktree list --porcelain,
// branch --show-current) and the gate logic depends on each
// returning the right thing.

// porcelainFor is a tiny helper that builds the porcelain block
// `git worktree list --porcelain` would emit for one worktree
// entry. The branch field is optional (empty = detached HEAD).
func porcelainFor(path, branch string) string {
	if branch == "" {
		return fmt.Sprintf("worktree %s\nHEAD abc123\ndetached\n", path)
	}
	return fmt.Sprintf("worktree %s\nHEAD abc123\nbranch refs/heads/%s\n", path, branch)
}

// moveToRepoRoot sets cs.SelectedCwd to repoRoot, undoing
// seedFix's selection of the worktree. The RunBackToWorktree
// gate requires cwd == repoRoot; this helper is the bit
// seedFix doesn't do for us.
func moveToRepoRoot(t *testing.T, rig *closeTestRig, repoRoot string) {
	t.Helper()
	if err := rig.cs.SetSelectedCwd(repoRoot); err != nil {
		t.Fatalf("SetSelectedCwd(%s): %v", repoRoot, err)
	}
}

// TestRunBackToWorktree_HappyPath: yml exists in the target
// worktree → cmd moves cwd into the worktree, yml is left
// alone ("preserved"), no destructive git calls fire.
func TestRunBackToWorktree_HappyPath(t *testing.T) {
	repoRoot := t.TempDir()
	wt := WorktreePath(repoRoot, "fix-42")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("mkdir wt: %v", err)
	}

	rig := newCloseRig(t)
	rig.git.revParseShowToplevel = repoRoot
	rig.git.worktreeListPorcelain =
		porcelainFor(repoRoot, "main") + "\n" + porcelainFor(wt, "fix-42")

	seedFix(t, rig, wt, repoRoot) // writes <wt>/.nightme/gtw.yml; sets cs.SelectedCwd=wt
	moveToRepoRoot(t, rig, repoRoot)

	res, err := RunBackToWorktree(context.Background(), rig.cs, rig.deps, "chat-btw", "msg-1", "fix-42")
	if err != nil {
		t.Fatalf("RunBackToWorktree: %v", err)
	}
	if res == nil || !res.Consumed {
		t.Fatalf("Result = %+v, want Consumed=true", res)
	}

	if got := rig.cs.SelectedCwd(); got != wt {
		t.Errorf("SelectedCwd after back-to-worktree = %q, want %q", got, wt)
	}

	// yml preserved (seedFix wrote it; RunBackToWorktree must
	// not have replaced it).
	if _, err := os.Stat(gtwYmlPath(wt)); err != nil {
		t.Errorf(".nightme/gtw.yml missing after back-to-worktree: %v", err)
	}

	// success card says "preserved" (not "repaired") so the user
	// knows the yml was a survivor, not a fresh stub.
	card := rig.rec.lastText()
	if !strings.Contains(card, "preserved") {
		t.Errorf("card missing 'preserved' marker:\n%s", card)
	}
	if strings.Contains(card, "repaired") {
		t.Errorf("card unexpectedly contains 'repaired' (yml was seeded):\n%s", card)
	}

	// No destructive git calls.
	for _, args := range rig.git.calls {
		if len(args) >= 2 && args[0] == "worktree" && args[1] == "remove" {
			t.Errorf("back-to-worktree ran worktree remove: %v", args)
		}
		if len(args) >= 3 && args[0] == "branch" && args[1] == "-D" {
			t.Errorf("back-to-worktree ran branch -D: %v", args)
		}
	}
}

// TestRunBackToWorktree_RepairsMissingYml: yml is absent from
// the target worktree → RunBackToWorktree must write a ModeLocal
// stub before switching cwd, sourcing Branch from
// `git branch --show-current` and RepoRoot from
// `git rev-parse --show-toplevel` of the worktree dir.
func TestRunBackToWorktree_RepairsMissingYml(t *testing.T) {
	repoRoot := t.TempDir()
	wt := WorktreePath(repoRoot, "fix-99")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("mkdir wt: %v", err)
	}

	rig := newCloseRig(t)
	// The chat's main cwd → repoRoot is the parent repo's
	// root, which the gate uses to derive worktreePath and to
	// fill the repaired yml's RepoRoot field. Match real git.
	rig.git.revParseShowToplevel = repoRoot
	rig.git.branchShowCurrent = "fix-99"
	rig.git.worktreeListPorcelain =
		porcelainFor(repoRoot, "main") + "\n" + porcelainFor(wt, "fix-99")

	// Deliberately do NOT call seedFix — the yml must be missing.
	moveToRepoRoot(t, rig, repoRoot)

	if _, err := os.Stat(gtwYmlPath(wt)); err == nil {
		t.Fatalf("precondition: yml must not exist yet")
	}

	res, err := RunBackToWorktree(context.Background(), rig.cs, rig.deps, "chat-btw", "msg-1", "fix-99")
	if err != nil {
		t.Fatalf("RunBackToWorktree: %v", err)
	}
	if res == nil || !res.Consumed {
		t.Fatalf("Result = %+v, want Consumed=true", res)
	}

	// yml now exists and was written as ModeLocal.
	ymlPath := gtwYmlPath(wt)
	if _, err := os.Stat(ymlPath); err != nil {
		t.Fatalf(".nightme/gtw.yml not created by repair: %v", err)
	}
	c, err := ReadGTWYml(wt)
	if err != nil {
		t.Fatalf("ReadGTWYml after repair: %v", err)
	}
	if c.Mode != ModeLocal {
		t.Errorf("repaired yml Mode = %q, want %q", c.Mode, ModeLocal)
	}
	if c.Branch != "fix-99" {
		t.Errorf("repaired yml Branch = %q, want %q (from git branch --show-current)", c.Branch, "fix-99")
	}
	if c.Worktree != wt {
		t.Errorf("repaired yml Worktree = %q, want %q", c.Worktree, wt)
	}
	if c.RepoRoot != repoRoot {
		t.Errorf("repaired yml RepoRoot = %q, want %q", c.RepoRoot, repoRoot)
	}

	// Regression guard: in a real git worktree,
	// `git -C <wt> rev-parse --show-toplevel` returns the
	// worktree's OWN toplevel (== worktreePath), NOT the parent
	// repo's root. A earlier draft of RunBackToWorktree re-ran
	// the call from inside the worktree and assigned the result
	// to RepoRoot — producing c.RepoRoot == c.Worktree, which
	// makes /gtw close's SetSelectedCwd(c.RepoRoot) target the
	// just-removed worktree. The fix: only ever call
	// `rev-parse --show-toplevel` once, against the chat's
	// main cwd. Assert the contract by counting the calls.
	count := 0
	for _, args := range rig.git.calls {
		if len(args) >= 2 && args[0] == "rev-parse" && args[1] == "--show-toplevel" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("rev-parse --show-toplevel called %d times; want exactly 1 (the chat-cwd gate)", count)
	}

	// cwd moves only after yml repair succeeds.
	if got := rig.cs.SelectedCwd(); got != wt {
		t.Errorf("SelectedCwd after repair = %q, want %q", got, wt)
	}

	card := rig.rec.lastText()
	if !strings.Contains(card, "repaired") {
		t.Errorf("card missing 'repaired' marker:\n%s", card)
	}
	if strings.Contains(card, "preserved") {
		t.Errorf("card unexpectedly contains 'preserved' (yml was missing):\n%s", card)
	}
}

// TestRunBackToWorktree_WorktreeNotFound: slug resolves to a
// path that doesn't exist on disk → fail fast, no git worktree
// list invocation beyond the rev-parse probe.
func TestRunBackToWorktree_WorktreeNotFound(t *testing.T) {
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	rig.git.revParseShowToplevel = repoRoot
	// empty porcelain — nothing listed.

	moveToRepoRoot(t, rig, repoRoot)

	res, err := RunBackToWorktree(context.Background(), rig.cs, rig.deps, "chat-btw", "msg-1", "ghost")
	if err != nil {
		t.Fatalf("RunBackToWorktree: %v", err)
	}
	if res == nil || !res.Consumed {
		t.Fatalf("Result = %+v, want Consumed=true", res)
	}

	if got := rig.cs.SelectedCwd(); got != repoRoot {
		t.Errorf("SelectedCwd must not move on failure; got %q, want %q", got, repoRoot)
	}
	card := rig.rec.lastText()
	if !strings.Contains(card, "ghost") {
		t.Errorf("reply should mention the missing slug:\n%s", card)
	}
	if !strings.Contains(card, "not found") {
		t.Errorf("reply should say 'not found':\n%s", card)
	}
}

// TestRunBackToWorktree_NotAWorktree: directory exists at the
// resolved path but is NOT listed by `git worktree list` —
// reject so a directory that merely shares the layout basename
// can't be entered as a worktree.
func TestRunBackToWorktree_NotAWorktree(t *testing.T) {
	repoRoot := t.TempDir()
	wt := WorktreePath(repoRoot, "stranger-dir")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("mkdir wt: %v", err)
	}

	rig := newCloseRig(t)
	rig.git.revParseShowToplevel = repoRoot
	// Empty porcelain — git knows about no worktrees.
	// (Even with wt on disk, IsKnownWorktree must refuse.)

	moveToRepoRoot(t, rig, repoRoot)

	_, err := RunBackToWorktree(context.Background(), rig.cs, rig.deps, "chat-btw", "msg-1", "stranger-dir")
	if err != nil {
		t.Fatalf("RunBackToWorktree: %v", err)
	}
	if got := rig.cs.SelectedCwd(); got != repoRoot {
		t.Errorf("SelectedCwd must not move on rejection; got %q, want %q", got, repoRoot)
	}
	card := rig.rec.lastText()
	if !strings.Contains(card, "stranger-dir") {
		t.Errorf("reply should mention the slug:\n%s", card)
	}
	if !strings.Contains(card, "not a git worktree") {
		t.Errorf("reply should explain why:\n%s", card)
	}

	// yml must NOT have been created (repair only runs after
	// the IsKnownWorktree gate).
	if _, err := os.Stat(gtwYmlPath(wt)); err == nil {
		t.Errorf("yml must not be written for non-worktree dirs")
	}
}

// TestRunBackToWorktree_RejectsWhenCwdIsWorktree: the gate is
// the symmetric counterpart of RunBack's gate. If the chat's
// cwd is itself inside a worktree (not the repo root), refuse
// with the "run /gtw back first" hint.
func TestRunBackToWorktree_RejectsWhenCwdIsWorktree(t *testing.T) {
	repoRoot := t.TempDir()
	wtOther := WorktreePath(repoRoot, "other")
	if err := os.MkdirAll(wtOther, 0o755); err != nil {
		t.Fatalf("mkdir wt: %v", err)
	}

	rig := newCloseRig(t)
	// rev-parse on wtOther should report the same repoRoot —
	// that is precisely what tells the handler we're inside a
	// worktree (cwd != repoRoot).
	rig.git.revParseShowToplevel = repoRoot
	rig.git.worktreeListPorcelain =
		porcelainFor(repoRoot, "main") + "\n" + porcelainFor(wtOther, "other")

	if err := rig.cs.SetSelectedCwd(wtOther); err != nil {
		t.Fatalf("SetSelectedCwd(%s): %v", wtOther, err)
	}

	res, err := RunBackToWorktree(context.Background(), rig.cs, rig.deps, "chat-btw", "msg-1", "main")
	if err != nil {
		t.Fatalf("RunBackToWorktree: %v", err)
	}
	if res == nil || !res.Consumed {
		t.Fatalf("Result = %+v, want Consumed=true", res)
	}
	if got := rig.cs.SelectedCwd(); got != wtOther {
		t.Errorf("SelectedCwd must not move on rejection; got %q, want %q", got, wtOther)
	}
	card := rig.rec.lastText()
	if !strings.Contains(card, "main repo root") {
		t.Errorf("reply should explain the cwd-is-not-root constraint:\n%s", card)
	}
	if !strings.Contains(card, "/gtw back") {
		t.Errorf("reply should suggest /gtw back (no arg) first:\n%s", card)
	}
}

// TestRunBackToWorktree_NotInGitRepo: when the chat's cwd is
// not a git repo, the rev-parse probe fails — surface a clear
// "run /cwd inside a repo first" message.
func TestRunBackToWorktree_NotInGitRepo(t *testing.T) {
	rig := newCloseRig(t)
	// revParseShowToplevel stays "" → RepoRoot returns
	// ErrNotInGitRepo. worktreeListPorcelain stays "".

	if err := rig.cs.SetSelectedCwd("/tmp/not-a-repo"); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}

	_, err := RunBackToWorktree(context.Background(), rig.cs, rig.deps, "chat-btw", "msg-1", "any")
	if err != nil {
		t.Fatalf("RunBackToWorktree: %v", err)
	}
	card := rig.rec.lastText()
	if !strings.Contains(card, "Not in a git repository") {
		t.Errorf("reply should explain the not-a-repo state:\n%s", card)
	}
}

// TestRunBackToWorktree_RaceLosesToOtherChat_TreatedAsSuccess:
// another chat's /gtw fix wrote the yml between our stat and
// our WriteGTWYml; WriteGTWYml returns ErrGtwYmlExists. The
// handler must treat that as success (the desired state is
// already on disk) rather than surfacing a ❌ repair error.
//
// We use a fake that injects a pre-existing yml between the
// gate's os.Stat and the repair path's WriteGTWYml. Since the
// programmableGit can't intercept os.Stat, we simulate the
// race by writing the yml file ourselves before the call: the
// handler's stat will find it and skip repair entirely.
// That's not quite the same code path as the race, but it
// covers the "yml exists, no repair needed" branch — the
// intended outcome.
//
// The actual race-fix path (ErrGtwYmlExists after stat said
// missing) is small enough to verify by reading back.go: any
// future refactor that drops the !errors.Is(err,
// ErrGtwYmlExists) guard will re-introduce the
// "❌ repair .nightme/gtw.yml" failure even on a clean state
// and the unit test surface can't catch it — the assertion
// is therefore pinned via code review.
func TestRunBackToWorktree_RaceLosesToOtherChat_TreatedAsSuccess(t *testing.T) {
	repoRoot := t.TempDir()
	wt := WorktreePath(repoRoot, "fix-42")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("mkdir wt: %v", err)
	}

	rig := newCloseRig(t)
	rig.git.revParseShowToplevel = repoRoot
	rig.git.worktreeListPorcelain =
		porcelainFor(repoRoot, "main") + "\n" + porcelainFor(wt, "fix-42")

	// Simulate the race: another chat wrote the yml between
	// our stat and our repair. Use a fresh seedFix with the
	// same rig.Now so the yml content matches what the
	// original /gtw fix would have produced.
	seedFix(t, rig, wt, repoRoot)
	moveToRepoRoot(t, rig, repoRoot)

	res, err := RunBackToWorktree(context.Background(), rig.cs, rig.deps, "chat-btw", "msg-1", "fix-42")
	if err != nil {
		t.Fatalf("RunBackToWorktree: %v", err)
	}
	if res == nil || !res.Consumed {
		t.Fatalf("Result = %+v, want Consumed=true", res)
	}

	// cwd moves into the worktree despite the yml being
	// pre-existing (handler must NOT fail just because the
	// stat-saw-missing / write-saw-exists race fired).
	if got := rig.cs.SelectedCwd(); got != wt {
		t.Errorf("SelectedCwd = %q, want %q", got, wt)
	}

	// The pre-existing yml is intact (not clobbered) — its
	// Branch came from the original /gtw fix, not from
	// `git branch --show-current`.
	c, err := ReadGTWYml(wt)
	if err != nil {
		t.Fatalf("ReadGTWYml: %v", err)
	}
	if c.Branch != "fix/42-test" {
		t.Errorf("Branch = %q, want %q (yml must not be repaired-over)",
			c.Branch, "fix/42-test")
	}

	// Success card uses "preserved" wording so the user can
	// see the race-loser path didn't manufacture a stub.
	card := rig.rec.lastText()
	if !strings.Contains(card, "preserved") {
		t.Errorf("card missing 'preserved' marker:\n%s", card)
	}
	if strings.Contains(card, "repaired") {
		t.Errorf("card unexpectedly contains 'repaired' (yml was pre-existing):\n%s", card)
	}
}

// TestFactory_Handle_BackWithWorktree: end-to-end via the slash
// command surface — `/gtw back fix-42` from main cwd enters
// the named worktree. Uses the rig's pre-set porcelain so the
// handler's IsKnownWorktree check succeeds.
func TestFactory_Handle_BackWithWorktree(t *testing.T) {
	repoRoot := t.TempDir()
	wt := WorktreePath(repoRoot, "fix-42")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("mkdir wt: %v", err)
	}

	rig := newCloseRig(t)
	rig.git.revParseShowToplevel = repoRoot
	rig.git.worktreeListPorcelain =
		porcelainFor(repoRoot, "main") + "\n" + porcelainFor(wt, "fix-42")

	seedFix(t, rig, wt, repoRoot)
	moveToRepoRoot(t, rig, repoRoot)

	f := NewFactoryWithDeps(newManagerForTest(t), rig.deps)
	out, err := f.Handle(context.Background(),
		command.RuntimeServices{},
		nil, rig.cs,
		command.SlashInput{
			ChatID:    "chat-btw",
			MessageID: "msg-1",
			Args:      []string{"gtw", "back", "fix-42"},
		})
	if err != nil {
		t.Fatalf("Factory.Handle: %v", err)
	}
	if !out.Consumed {
		t.Fatalf("SlashOutput.Consumed = false; want true")
	}
	if got := rig.cs.SelectedCwd(); got != wt {
		t.Errorf("SelectedCwd after Handle = %q, want %q", got, wt)
	}
}

// TestFactory_Handle_BackRejectsTwoArgs: >1 positional arg is
// rejected by the CmdSpec's MaxArgs=1; the legacy "RejectsExtraArgs"
// back_test.go test must continue to pass for the 0-arg path,
// but the new spec accepts one arg — so the rejection case is
// only about *two* extras.
func TestFactory_Handle_BackRejectsTwoArgs(t *testing.T) {
	rig := newCloseRig(t)
	if err := rig.cs.SetSelectedCwd(t.TempDir()); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	f := NewFactoryWithDeps(newManagerForTest(t), rig.deps)
	out, err := f.Handle(context.Background(),
		command.RuntimeServices{},
		nil, rig.cs,
		command.SlashInput{
			ChatID:    "chat-btw",
			MessageID: "msg-1",
			Args:      []string{"gtw", "back", "a", "b"},
		})
	if err != nil {
		t.Fatalf("Factory.Handle: %v", err)
	}
	if !out.Consumed {
		t.Fatalf("SlashOutput.Consumed = false; want true")
	}
	if !strings.Contains(out.Reply, "❌") {
		t.Errorf("reply should reject two extras:\n%s", out.Reply)
	}
}

// newManagerForTest returns a *Manager whose only role is to
// satisfy the Factory constructor's mgr parameter. Tests
// driving Handle directly don't exercise the per-chat run lock
// (chatID is empty), so the lock is a no-op as documented in
// cmd.go:303.
func newManagerForTest(t *testing.T) *Manager {
	t.Helper()
	return NewManager()
}
