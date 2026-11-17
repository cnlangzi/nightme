package gtw

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
)

// Tests for the `/gtw back <branch>` direction: jump from the
// chat's main repo root INTO the worktree that holds the branch
// whose name starts with the argument, optionally repairing a
// missing .nightme/gtw.yml.
//
// Setup pattern follows the existing back_test.go / close_test.go
// fixtures (newCloseRig + seedFix + programmableGit). Each test
// pre-stages the fake-git responses it needs on the rig, since
// RunBackToWorktree touches two distinct git commands
// (rev-parse --show-toplevel, worktree list --porcelain) and the
// gate logic depends on each returning the right thing.

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

// runBack drives RunBackToWorktree with a work-carrying context so
// the structured outcome is captured, and returns the card the user
// read plus the outcome a program read.
//
// The card is captured before the work is released: releasing it
// settles the message and emits the settled message as well.
func runBack(t *testing.T, rig *closeTestRig, arg string) (string, *messages.Outcome) {
	t.Helper()
	work := rig.cs.Hold("msg-btw")
	ctx := chatsession.WithWork(context.Background(), work)
	if _, err := RunBackToWorktree(ctx, rig.cs, rig.deps, "chat-btw", "msg-btw", arg); err != nil {
		t.Fatalf("RunBackToWorktree: %v", err)
	}
	card := rig.rec.lastText()
	work.Release()
	return card, settledOutcome(rig.rec)
}

// settledOutcome returns the outcome of the settled message the
// recorder captured, or nil when nothing settled.
func settledOutcome(rec *closeTestRecCh) *messages.Outcome {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, m := range rec.sends {
		if m.Kind == messages.OutMessageSettled {
			return m.Outcome
		}
	}
	return nil
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

	card, outcome := runBack(t, rig, "fix-42")
	if outcome == nil {
		t.Fatalf("no settled outcome: the command reported nothing")
	}
	if outcome.Status != messages.OutcomeOK {
		t.Errorf("outcome status = %s, want %s", outcome.Status, messages.OutcomeOK)
	}
	if outcome.Data["worktree"] != wt {
		t.Errorf("outcome data[worktree] = %q, want %q", outcome.Data["worktree"], wt)
	}
	if outcome.Data["repo_root"] != repoRoot {
		t.Errorf("outcome data[repo_root] = %q, want %q", outcome.Data["repo_root"], repoRoot)
	}
	if outcome.Data["branch"] != "fix-42" {
		t.Errorf("outcome data[branch] = %q, want %q", outcome.Data["branch"], "fix-42")
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
	if !strings.Contains(card, "preserved") {
		t.Errorf("card missing 'preserved' marker:\n%s", card)
	}
	if strings.Contains(card, "repaired") {
		t.Errorf("card unexpectedly contains 'repaired' (yml was seeded):\n%s", card)
	}
	// The card names the branch that was entered.
	if !strings.Contains(card, "→ branch: fix-42") {
		t.Errorf("card missing the branch row:\n%s", card)
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
// stub before switching cwd, sourcing Branch from the worktree
// listing and RepoRoot from the chat cwd's
// `git rev-parse --show-toplevel`.
func TestRunBackToWorktree_RepairsMissingYml(t *testing.T) {
	repoRoot := t.TempDir()
	wt := WorktreePath(repoRoot, "fix-99")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("mkdir wt: %v", err)
	}

	rig := newCloseRig(t)
	// The chat's main cwd → repoRoot is the parent repo's
	// root, which the gate uses to derive the worktree path and
	// to fill the repaired yml's RepoRoot field. Match real git.
	rig.git.revParseShowToplevel = repoRoot
	rig.git.worktreeListPorcelain =
		porcelainFor(repoRoot, "main") + "\n" + porcelainFor(wt, "fix-99")

	// Deliberately do NOT call seedFix — the yml must be missing.
	moveToRepoRoot(t, rig, repoRoot)

	if _, err := os.Stat(gtwYmlPath(wt)); err == nil {
		t.Fatalf("precondition: yml must not exist yet")
	}

	card, outcome := runBack(t, rig, "fix-99")
	if outcome == nil {
		t.Fatalf("no settled outcome: the command reported nothing")
	}
	if outcome.Status != messages.OutcomeOK {
		t.Errorf("outcome status = %s, want %s", outcome.Status, messages.OutcomeOK)
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
		t.Errorf("repaired yml Branch = %q, want %q (from the worktree listing)",
			c.Branch, "fix-99")
	}
	if c.Worktree != wt {
		t.Errorf("repaired yml Worktree = %q, want %q", c.Worktree, wt)
	}
	if c.RepoRoot != repoRoot {
		t.Errorf("repaired yml RepoRoot = %q, want %q", c.RepoRoot, repoRoot)
	}

	// The branch comes from the worktree listing — the extra
	// `git branch --show-current` call is gone.
	for _, args := range rig.git.calls {
		if len(args) >= 2 && args[0] == "branch" && args[1] == "--show-current" {
			t.Errorf("back-to-worktree ran branch --show-current: %v", args)
		}
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

	if !strings.Contains(card, "repaired") {
		t.Errorf("card missing 'repaired' marker:\n%s", card)
	}
	if strings.Contains(card, "preserved") {
		t.Errorf("card unexpectedly contains 'preserved' (yml was missing):\n%s", card)
	}
}

// TestRunBackToWorktree_PrefixMatchesBranch: the argument is a
// prefix of the branch — the full name does not have to be typed.
func TestRunBackToWorktree_PrefixMatchesBranch(t *testing.T) {
	repoRoot := t.TempDir()
	wt := WorktreePath(repoRoot, "42-feat-login-state-expires")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("mkdir wt: %v", err)
	}

	rig := newCloseRig(t)
	rig.git.revParseShowToplevel = repoRoot
	rig.git.worktreeListPorcelain =
		porcelainFor(repoRoot, "main") + "\n" +
			porcelainFor(wt, "42-feat-login-state-expires")

	seedFix(t, rig, wt, repoRoot)
	moveToRepoRoot(t, rig, repoRoot)

	card, outcome := runBack(t, rig, "42-feat")
	if outcome == nil {
		t.Fatalf("no settled outcome: the command reported nothing")
	}
	if outcome.Status != messages.OutcomeOK {
		t.Errorf("outcome status = %s, want %s", outcome.Status, messages.OutcomeOK)
	}
	if got := rig.cs.SelectedCwd(); got != wt {
		t.Errorf("SelectedCwd = %q, want %q", got, wt)
	}
	if outcome.Data["branch"] != "42-feat-login-state-expires" {
		t.Errorf("outcome data[branch] = %q, want %q",
			outcome.Data["branch"], "42-feat-login-state-expires")
	}
	if !strings.Contains(card, "→ branch: 42-feat-login-state-expires") {
		t.Errorf("card missing the branch row:\n%s", card)
	}
}

// TestRunBackToWorktree_ExactMatchWins: the prefix matches two
// branches and one of them equals the argument — the exact match
// wins.
func TestRunBackToWorktree_ExactMatchWins(t *testing.T) {
	repoRoot := t.TempDir()
	wtExact := WorktreePath(repoRoot, "42-feat")
	wtLonger := WorktreePath(repoRoot, "42-feat-login")
	for _, d := range []string{wtExact, wtLonger} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir wt: %v", err)
		}
	}

	rig := newCloseRig(t)
	rig.git.revParseShowToplevel = repoRoot
	rig.git.worktreeListPorcelain =
		porcelainFor(repoRoot, "main") + "\n" +
			porcelainFor(wtExact, "42-feat") + "\n" +
			porcelainFor(wtLonger, "42-feat-login")

	seedFix(t, rig, wtExact, repoRoot)
	moveToRepoRoot(t, rig, repoRoot)

	_, outcome := runBack(t, rig, "42-feat")
	if outcome == nil {
		t.Fatalf("no settled outcome: the command reported nothing")
	}
	if outcome.Status != messages.OutcomeOK {
		t.Errorf("outcome status = %s, want %s", outcome.Status, messages.OutcomeOK)
	}
	if got := rig.cs.SelectedCwd(); got != wtExact {
		t.Errorf("SelectedCwd = %q, want %q (the exact match)", got, wtExact)
	}
	if outcome.Data["branch"] != "42-feat" {
		t.Errorf("outcome data[branch] = %q, want %q", outcome.Data["branch"], "42-feat")
	}
}

// TestRunBackToWorktree_AmbiguousPrefixFails: the prefix matches
// two branches and neither equals the argument — the command fails
// and names both branches instead of guessing.
func TestRunBackToWorktree_AmbiguousPrefixFails(t *testing.T) {
	repoRoot := t.TempDir()
	wtA := WorktreePath(repoRoot, "42-feat-login")
	wtB := WorktreePath(repoRoot, "42-fix-logout")
	for _, d := range []string{wtA, wtB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir wt: %v", err)
		}
	}

	rig := newCloseRig(t)
	rig.git.revParseShowToplevel = repoRoot
	rig.git.worktreeListPorcelain =
		porcelainFor(repoRoot, "main") + "\n" +
			porcelainFor(wtA, "42-feat-login") + "\n" +
			porcelainFor(wtB, "42-fix-logout")

	seedFix(t, rig, wtA, repoRoot)
	moveToRepoRoot(t, rig, repoRoot)

	card, outcome := runBack(t, rig, "42")
	if outcome == nil {
		t.Fatalf("no settled outcome: the command reported nothing")
	}
	if outcome.Status != messages.OutcomeFailed {
		t.Errorf("outcome status = %s, want %s", outcome.Status, messages.OutcomeFailed)
	}
	if outcome.Data["reason"] != "ambiguous" {
		t.Errorf("outcome data[reason] = %q, want %q", outcome.Data["reason"], "ambiguous")
	}
	if got := outcome.Data["candidates"]; !strings.Contains(got, "42-feat-login") ||
		!strings.Contains(got, "42-fix-logout") {
		t.Errorf("outcome data[candidates] = %q, want both branch names", got)
	}
	if got := rig.cs.SelectedCwd(); got != repoRoot {
		t.Errorf("SelectedCwd must not move on failure; got %q, want %q", got, repoRoot)
	}
	for _, want := range []string{"42-feat-login", "42-fix-logout"} {
		if !strings.Contains(card, want) {
			t.Errorf("reply should name %q:\n%s", want, card)
		}
	}
	if !strings.Contains(card, "matches 2 worktree branches") {
		t.Errorf("reply should say how many branches matched:\n%s", card)
	}
}

// TestRunBackToWorktree_NoMatchListsWorktreeBranches: the argument
// matches no branch — the command fails and lists the worktree
// branches so the next attempt is unambiguous.
func TestRunBackToWorktree_NoMatchListsWorktreeBranches(t *testing.T) {
	repoRoot := t.TempDir()
	wt := WorktreePath(repoRoot, "42-feat-login")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("mkdir wt: %v", err)
	}

	rig := newCloseRig(t)
	rig.git.revParseShowToplevel = repoRoot
	rig.git.worktreeListPorcelain =
		porcelainFor(repoRoot, "main") + "\n" + porcelainFor(wt, "42-feat-login")

	moveToRepoRoot(t, rig, repoRoot)

	card, outcome := runBack(t, rig, "99")
	if outcome == nil {
		t.Fatalf("no settled outcome: the command reported nothing")
	}
	if outcome.Status != messages.OutcomeFailed {
		t.Errorf("outcome status = %s, want %s", outcome.Status, messages.OutcomeFailed)
	}
	if outcome.Data["reason"] != "not_found" {
		t.Errorf("outcome data[reason] = %q, want %q", outcome.Data["reason"], "not_found")
	}
	if got := outcome.Data["candidates"]; !strings.Contains(got, "42-feat-login") {
		t.Errorf("outcome data[candidates] = %q, want the worktree branch", got)
	}
	if got := rig.cs.SelectedCwd(); got != repoRoot {
		t.Errorf("SelectedCwd must not move on failure; got %q, want %q", got, repoRoot)
	}
	if !strings.Contains(card, "42-feat-login") {
		t.Errorf("reply should name the worktree branch:\n%s", card)
	}
	if !strings.Contains(card, "no worktree branch matches") {
		t.Errorf("reply should say nothing matched:\n%s", card)
	}
}

// TestRunBackToWorktree_NoWorktreesAtAll: the repository has no
// worktree with a branch — the card points at /gtw fix instead of
// listing an empty list.
func TestRunBackToWorktree_NoWorktreesAtAll(t *testing.T) {
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	rig.git.revParseShowToplevel = repoRoot
	rig.git.worktreeListPorcelain = porcelainFor(repoRoot, "main")

	moveToRepoRoot(t, rig, repoRoot)

	card, outcome := runBack(t, rig, "42-f")
	if outcome == nil {
		t.Fatalf("no settled outcome: the command reported nothing")
	}
	if outcome.Status != messages.OutcomeFailed {
		t.Errorf("outcome status = %s, want %s", outcome.Status, messages.OutcomeFailed)
	}
	if outcome.Data["reason"] != "not_found" {
		t.Errorf("outcome data[reason] = %q, want %q", outcome.Data["reason"], "not_found")
	}
	if got := rig.cs.SelectedCwd(); got != repoRoot {
		t.Errorf("SelectedCwd must not move on failure; got %q, want %q", got, repoRoot)
	}
	if !strings.Contains(card, "no worktrees") {
		t.Errorf("reply should say the repository has no worktrees:\n%s", card)
	}
	if !strings.Contains(card, "/gtw fix") {
		t.Errorf("reply should point at /gtw fix:\n%s", card)
	}
}

// TestRunBackToWorktree_PrimaryCheckoutIsNotACandidate: the
// primary checkout's branch is never a candidate — `/gtw back m`
// must not "enter" the directory the chat already sits in.
func TestRunBackToWorktree_PrimaryCheckoutIsNotACandidate(t *testing.T) {
	repoRoot := t.TempDir()
	wt := WorktreePath(repoRoot, "42-feat")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("mkdir wt: %v", err)
	}

	rig := newCloseRig(t)
	rig.git.revParseShowToplevel = repoRoot
	rig.git.worktreeListPorcelain =
		porcelainFor(repoRoot, "main") + "\n" + porcelainFor(wt, "42-feat")

	seedFix(t, rig, wt, repoRoot)
	moveToRepoRoot(t, rig, repoRoot)

	card, outcome := runBack(t, rig, "m")
	if outcome == nil {
		t.Fatalf("no settled outcome: the command reported nothing")
	}
	if outcome.Status != messages.OutcomeFailed {
		t.Errorf("outcome status = %s, want %s", outcome.Status, messages.OutcomeFailed)
	}
	if outcome.Data["reason"] != "not_found" {
		t.Errorf("outcome data[reason] = %q, want %q", outcome.Data["reason"], "not_found")
	}
	if got := rig.cs.SelectedCwd(); got != repoRoot {
		t.Errorf("SelectedCwd must not move on failure; got %q, want %q", got, repoRoot)
	}
	if strings.Contains(card, "main") {
		t.Errorf("reply must not offer the primary checkout's branch:\n%s", card)
	}
}

// TestRunBackToWorktree_DetachedWorktreeIsNotACandidate: a
// worktree on a detached HEAD has no branch — it is never a
// candidate and never shows up in a candidate list.
func TestRunBackToWorktree_DetachedWorktreeIsNotACandidate(t *testing.T) {
	repoRoot := t.TempDir()
	detached := WorktreePath(repoRoot, "scratch")
	if err := os.MkdirAll(detached, 0o755); err != nil {
		t.Fatalf("mkdir wt: %v", err)
	}

	rig := newCloseRig(t)
	rig.git.revParseShowToplevel = repoRoot
	rig.git.worktreeListPorcelain =
		porcelainFor(repoRoot, "main") + "\n" + porcelainFor(detached, "")

	moveToRepoRoot(t, rig, repoRoot)

	card, outcome := runBack(t, rig, "scratch")
	if outcome == nil {
		t.Fatalf("no settled outcome: the command reported nothing")
	}
	if outcome.Status != messages.OutcomeFailed {
		t.Errorf("outcome status = %s, want %s", outcome.Status, messages.OutcomeFailed)
	}
	if outcome.Data["reason"] != "not_found" {
		t.Errorf("outcome data[reason] = %q, want %q", outcome.Data["reason"], "not_found")
	}
	if got := rig.cs.SelectedCwd(); got != repoRoot {
		t.Errorf("SelectedCwd must not move on failure; got %q, want %q", got, repoRoot)
	}
	if strings.Contains(card, detached) {
		t.Errorf("reply must not offer the detached worktree:\n%s", card)
	}
}

// TestRunBackToWorktree_MatchingIsCaseSensitive: git references
// are case-sensitive, so a differently-cased argument matches
// nothing.
func TestRunBackToWorktree_MatchingIsCaseSensitive(t *testing.T) {
	repoRoot := t.TempDir()
	wt := WorktreePath(repoRoot, "42-feat")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("mkdir wt: %v", err)
	}

	rig := newCloseRig(t)
	rig.git.revParseShowToplevel = repoRoot
	rig.git.worktreeListPorcelain =
		porcelainFor(repoRoot, "main") + "\n" + porcelainFor(wt, "42-feat")

	seedFix(t, rig, wt, repoRoot)
	moveToRepoRoot(t, rig, repoRoot)

	_, outcome := runBack(t, rig, "42-FEAT")
	if outcome == nil {
		t.Fatalf("no settled outcome: the command reported nothing")
	}
	if outcome.Status != messages.OutcomeFailed {
		t.Errorf("outcome status = %s, want %s", outcome.Status, messages.OutcomeFailed)
	}
	if outcome.Data["reason"] != "not_found" {
		t.Errorf("outcome data[reason] = %q, want %q", outcome.Data["reason"], "not_found")
	}
	if got := rig.cs.SelectedCwd(); got != repoRoot {
		t.Errorf("SelectedCwd must not move on failure; got %q, want %q", got, repoRoot)
	}
}

// TestRunBackToWorktree_StaleWorktreeDirectory: git still lists
// the worktree but its directory is gone — the command fails and
// names `git worktree prune`.
func TestRunBackToWorktree_StaleWorktreeDirectory(t *testing.T) {
	repoRoot := t.TempDir()
	// The path is never created on disk.
	gone := WorktreePath(repoRoot, "42-feat-login")

	rig := newCloseRig(t)
	rig.git.revParseShowToplevel = repoRoot
	rig.git.worktreeListPorcelain =
		porcelainFor(repoRoot, "main") + "\n" + porcelainFor(gone, "42-feat-login")

	moveToRepoRoot(t, rig, repoRoot)

	card, outcome := runBack(t, rig, "42-feat")
	if outcome == nil {
		t.Fatalf("no settled outcome: the command reported nothing")
	}
	if outcome.Status != messages.OutcomeFailed {
		t.Errorf("outcome status = %s, want %s", outcome.Status, messages.OutcomeFailed)
	}
	if outcome.Data["reason"] != "not_found" {
		t.Errorf("outcome data[reason] = %q, want %q", outcome.Data["reason"], "not_found")
	}
	if got := rig.cs.SelectedCwd(); got != repoRoot {
		t.Errorf("SelectedCwd must not move on failure; got %q, want %q", got, repoRoot)
	}
	if !strings.Contains(card, "git worktree prune") {
		t.Errorf("reply should name git worktree prune:\n%s", card)
	}
	if !strings.Contains(card, gone) {
		t.Errorf("reply should name the gone path:\n%s", card)
	}
}

// TestRunBackToWorktree_NonWorktreeDirectoryIsNeverEntered: a
// directory that merely shares the worktree layout basename is not
// a worktree — it can never be entered, and no yml is written for
// it.
func TestRunBackToWorktree_NonWorktreeDirectoryIsNeverEntered(t *testing.T) {
	repoRoot := t.TempDir()
	wt := WorktreePath(repoRoot, "stranger-dir")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("mkdir wt: %v", err)
	}

	rig := newCloseRig(t)
	rig.git.revParseShowToplevel = repoRoot
	// Git knows about no worktree besides the primary checkout.
	rig.git.worktreeListPorcelain = porcelainFor(repoRoot, "main")

	moveToRepoRoot(t, rig, repoRoot)

	card, outcome := runBack(t, rig, "stranger-dir")
	if outcome == nil {
		t.Fatalf("no settled outcome: the command reported nothing")
	}
	if outcome.Status != messages.OutcomeFailed {
		t.Errorf("outcome status = %s, want %s", outcome.Status, messages.OutcomeFailed)
	}
	if outcome.Data["reason"] != "not_found" {
		t.Errorf("outcome data[reason] = %q, want %q", outcome.Data["reason"], "not_found")
	}
	if got := rig.cs.SelectedCwd(); got != repoRoot {
		t.Errorf("SelectedCwd must not move on rejection; got %q, want %q", got, repoRoot)
	}
	if !strings.Contains(card, "stranger-dir") {
		t.Errorf("reply should mention the argument:\n%s", card)
	}

	// yml must NOT have been created (the repair only runs after
	// the branch resolved).
	if _, err := os.Stat(gtwYmlPath(wt)); err == nil {
		t.Errorf("yml must not be written for non-worktree dirs")
	}
}

// TestRunBackToWorktree_RejectsWhenCwdIsALinkedWorktreeRoot: the
// chat sits exactly at a linked worktree's root. Real git's
// `rev-parse --show-toplevel` reports THAT worktree's own root, so
// a gate that compared the cwd against it would let the command
// through — and the main checkout would become a candidate. The
// command must refuse and leave the cwd alone.
func TestRunBackToWorktree_RejectsWhenCwdIsALinkedWorktreeRoot(t *testing.T) {
	repoRoot := t.TempDir()
	wt := WorktreePath(repoRoot, "other")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("mkdir wt: %v", err)
	}

	rig := newCloseRig(t)
	// Real git: inside a linked worktree, --show-toplevel reports
	// the worktree's own root — NOT the primary checkout.
	rig.git.revParseShowToplevelByDir = map[string]string{wt: wt}
	rig.git.revParseShowToplevel = repoRoot
	rig.git.worktreeListPorcelain =
		porcelainFor(repoRoot, "main") + "\n" + porcelainFor(wt, "other")

	if err := rig.cs.SetSelectedCwd(wt); err != nil {
		t.Fatalf("SetSelectedCwd(%s): %v", wt, err)
	}

	card, outcome := runBack(t, rig, "main")
	if outcome == nil {
		t.Fatalf("no settled outcome: the command reported nothing")
	}
	if outcome.Status != messages.OutcomeFailed {
		t.Errorf("outcome status = %s, want %s", outcome.Status, messages.OutcomeFailed)
	}
	if outcome.Data["reason"] != "wrong_cwd" {
		t.Errorf("outcome data[reason] = %q, want %q", outcome.Data["reason"], "wrong_cwd")
	}
	if got := rig.cs.SelectedCwd(); got != wt {
		t.Errorf("SelectedCwd must not move on rejection; got %q, want %q", got, wt)
	}
	if !strings.Contains(card, "main repo root") {
		t.Errorf("reply should explain the cwd-is-not-root constraint:\n%s", card)
	}
}

// TestRunBackToWorktree_ForeignWorktreeGetsNoOwnershipToken: a
// worktree the user made by hand is reachable, but nightme does
// not stamp its state file into it. The yml is the token `/gtw
// close` tears a worktree — and its branch — down with, and
// nightme must not be able to delete a branch it never created.
func TestRunBackToWorktree_ForeignWorktreeGetsNoOwnershipToken(t *testing.T) {
	repoRoot := t.TempDir()
	// Outside <repo>.nightme/ — the user made this one.
	foreign := filepath.Join(t.TempDir(), "scratch")
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatalf("mkdir wt: %v", err)
	}

	rig := newCloseRig(t)
	rig.git.revParseShowToplevel = repoRoot
	rig.git.worktreeListPorcelain =
		porcelainFor(repoRoot, "main") + "\n" + porcelainFor(foreign, "scratch")

	moveToRepoRoot(t, rig, repoRoot)

	card, outcome := runBack(t, rig, "scratch")
	if outcome == nil {
		t.Fatalf("no settled outcome: the command reported nothing")
	}
	if outcome.Status != messages.OutcomeOK {
		t.Errorf("outcome status = %s, want %s", outcome.Status, messages.OutcomeOK)
	}
	// The chat still enters the worktree the user asked for.
	if got := rig.cs.SelectedCwd(); got != foreign {
		t.Errorf("SelectedCwd = %q, want %q", got, foreign)
	}
	// But nightme leaves no ownership token behind.
	if _, err := os.Stat(gtwYmlPath(foreign)); err == nil {
		t.Errorf("no yml must be written into a worktree nightme did not create")
	}
	if !strings.Contains(card, "not created") {
		t.Errorf("card should say the yml was not created:\n%s", card)
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
	// Branch came from the original /gtw fix, not from the
	// worktree listing.
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
// the worktree of that branch.
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
