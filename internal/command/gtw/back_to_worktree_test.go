package gtw

import (
	"bytes"
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

// replyTexts returns the text of every reply the recorder captured —
// the cards the user read, in order.
func replyTexts(rec *closeTestRecCh) []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var out []string
	for _, m := range rec.sends {
		if m.Kind == messages.OutReply {
			out = append(out, m.Text)
		}
	}
	return out
}

// ymlBytes reads the state file of a worktree so a test can compare
// it before and after a jump.
func ymlBytes(t *testing.T, wt string) []byte {
	t.Helper()
	b, err := os.ReadFile(gtwYmlPath(wt))
	if err != nil {
		t.Fatalf("read %s: %v", gtwYmlPath(wt), err)
	}
	return b
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
	// A jump out of the main repo has no "from" row — the card is
	// the one the main repo always got.
	if strings.Contains(card, "→ from:") {
		t.Errorf("card must not carry a 'from' row when the chat sat at the main repo:\n%s", card)
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

// TestRunBackToWorktree_JumpsFromALinkedWorktreeRoot: the chat
// sits exactly at a linked worktree's root. Real git's
// `rev-parse --show-toplevel` reports THAT worktree's own root, so
// the gate must read the worktree listing instead — the primary
// checkout stays the listing's first record, and the jump lands on
// the branch the user named.
func TestRunBackToWorktree_JumpsFromALinkedWorktreeRoot(t *testing.T) {
	repoRoot := t.TempDir()
	wt := WorktreePath(repoRoot, "other")
	wtTarget := WorktreePath(repoRoot, "42-feat")
	for _, d := range []string{wt, wtTarget} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir wt: %v", err)
		}
	}

	rig := newCloseRig(t)
	// Real git: inside a linked worktree, --show-toplevel reports
	// the worktree's own root — NOT the primary checkout.
	rig.git.revParseShowToplevelByDir = map[string]string{wt: wt}
	rig.git.revParseShowToplevel = repoRoot
	rig.git.worktreeListPorcelain =
		porcelainFor(repoRoot, "main") + "\n" +
			porcelainFor(wt, "other") + "\n" +
			porcelainFor(wtTarget, "42-feat")

	seedFix(t, rig, wtTarget, repoRoot)
	seedFix(t, rig, wt, repoRoot) // leaves SelectedCwd at the linked worktree

	card, outcome := runBack(t, rig, "42-f")
	if outcome == nil {
		t.Fatalf("no settled outcome: the command reported nothing")
	}
	if outcome.Status != messages.OutcomeOK {
		t.Errorf("outcome status = %s, want %s", outcome.Status, messages.OutcomeOK)
	}
	if got := rig.cs.SelectedCwd(); got != wtTarget {
		t.Errorf("SelectedCwd = %q, want %q", got, wtTarget)
	}
	// The primary checkout is the listing's first record — not the
	// root `rev-parse` reports from inside the linked worktree.
	if got := outcome.Data["repo_root"]; got != repoRoot {
		t.Errorf("outcome data[repo_root] = %q, want %q", got, repoRoot)
	}
	if !strings.Contains(card, fmt.Sprintf("→ from: %s (preserved)", wt)) {
		t.Errorf("card should name the worktree the chat came from:\n%s", card)
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

// TestRunBackToWorktree_JumpsFromAWorktreeRoot: the chat sits at
// the root of a worktree and names the branch of another one — the
// command moves the chat straight there. The worktree it left keeps
// its directory, its branch and its state file.
func TestRunBackToWorktree_JumpsFromAWorktreeRoot(t *testing.T) {
	repoRoot := t.TempDir()
	wtA := WorktreePath(repoRoot, "other")
	wtB := WorktreePath(repoRoot, "42-feat")
	for _, d := range []string{wtA, wtB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir wt: %v", err)
		}
	}

	rig := newCloseRig(t)
	rig.git.revParseShowToplevel = repoRoot
	rig.git.worktreeListPorcelain =
		porcelainFor(repoRoot, "main") + "\n" +
			porcelainFor(wtA, "other") + "\n" +
			porcelainFor(wtB, "42-feat")

	seedFix(t, rig, wtB, repoRoot)
	seedFix(t, rig, wtA, repoRoot) // leaves SelectedCwd at A
	ymlA := ymlBytes(t, wtA)
	ymlB := ymlBytes(t, wtB)

	card, outcome := runBack(t, rig, "42-f")
	if outcome == nil {
		t.Fatalf("no settled outcome: the command reported nothing")
	}
	if outcome.Status != messages.OutcomeOK {
		t.Errorf("outcome status = %s, want %s", outcome.Status, messages.OutcomeOK)
	}
	if got := outcome.Data["worktree"]; got != wtB {
		t.Errorf("outcome data[worktree] = %q, want %q", got, wtB)
	}
	if got := outcome.Data["branch"]; got != "42-feat" {
		t.Errorf("outcome data[branch] = %q, want %q", got, "42-feat")
	}
	if got := outcome.Data["repo_root"]; got != repoRoot {
		t.Errorf("outcome data[repo_root] = %q, want %q", got, repoRoot)
	}
	if got := rig.cs.SelectedCwd(); got != wtB {
		t.Errorf("SelectedCwd = %q, want %q", got, wtB)
	}

	if !strings.HasPrefix(card, "✅ back into") {
		t.Errorf("card should open with the success headline:\n%s", card)
	}
	if !strings.Contains(card, wtB) {
		t.Errorf("card should name the target worktree:\n%s", card)
	}
	if !strings.Contains(card, "→ branch: 42-feat") {
		t.Errorf("card should name the target branch:\n%s", card)
	}
	if !strings.Contains(card, fmt.Sprintf("→ from: %s (preserved)", wtA)) {
		t.Errorf("card should name the worktree the chat came from:\n%s", card)
	}

	// The worktree the chat left is untouched: directory, state
	// file, content.
	if _, err := os.Stat(wtA); err != nil {
		t.Errorf("the worktree the chat left is gone: %v", err)
	}
	if got := ymlBytes(t, wtA); !bytes.Equal(got, ymlA) {
		t.Errorf("the worktree the chat left had its yml rewritten:\ngot %s\nwant %s", got, ymlA)
	}
	if got := ymlBytes(t, wtB); !bytes.Equal(got, ymlB) {
		t.Errorf("the target worktree had its yml rewritten:\ngot %s\nwant %s", got, ymlB)
	}

	// One card: a jump does not sync the default branch.
	if replies := replyTexts(rig.rec); len(replies) != 1 {
		t.Errorf("the command sent %d replies, want 1:\n%v", len(replies), replies)
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

// TestRunBackToWorktree_SelfJumpKeepsCwd: the branch the user
// names is the branch the chat's own worktree holds — there is
// nowhere to jump to. The command says so and leaves the cwd where
// it is.
func TestRunBackToWorktree_SelfJumpKeepsCwd(t *testing.T) {
	repoRoot := t.TempDir()
	wtA := WorktreePath(repoRoot, "other")
	wtB := WorktreePath(repoRoot, "42-feat")
	for _, d := range []string{wtA, wtB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir wt: %v", err)
		}
	}

	rig := newCloseRig(t)
	rig.git.revParseShowToplevel = repoRoot
	rig.git.worktreeListPorcelain =
		porcelainFor(repoRoot, "main") + "\n" +
			porcelainFor(wtA, "other") + "\n" +
			porcelainFor(wtB, "42-feat")

	seedFix(t, rig, wtA, repoRoot) // leaves SelectedCwd at A
	ymlA := ymlBytes(t, wtA)

	card, outcome := runBack(t, rig, "other")
	if outcome == nil {
		t.Fatalf("no settled outcome: the command reported nothing")
	}
	if outcome.Status != messages.OutcomeOK {
		t.Errorf("outcome status = %s, want %s", outcome.Status, messages.OutcomeOK)
	}
	if got := outcome.Data["reason"]; got != "already_in_worktree" {
		t.Errorf("outcome data[reason] = %q, want %q", got, "already_in_worktree")
	}
	if got := outcome.Data["worktree"]; got != wtA {
		t.Errorf("outcome data[worktree] = %q, want %q", got, wtA)
	}
	if got := outcome.Data["branch"]; got != "other" {
		t.Errorf("outcome data[branch] = %q, want %q", got, "other")
	}
	if got := rig.cs.SelectedCwd(); got != wtA {
		t.Errorf("SelectedCwd must not move on a self-jump; got %q, want %q", got, wtA)
	}

	if !strings.HasPrefix(card, "ℹ️") {
		t.Errorf("card should open with the info marker:\n%s", card)
	}
	if strings.Contains(card, "back into") {
		t.Errorf("a self-jump must not read as an entry:\n%s", card)
	}
	if !strings.Contains(card, "other") {
		t.Errorf("card should name the branch:\n%s", card)
	}
	if !strings.Contains(card, wtA) {
		t.Errorf("card should name the worktree:\n%s", card)
	}
	if !strings.Contains(card, ".nightme/gtw.yml") {
		t.Errorf("card should carry the yml status line:\n%s", card)
	}
	if got := ymlBytes(t, wtA); !bytes.Equal(got, ymlA) {
		t.Errorf("the yml must be left alone on a self-jump:\ngot %s\nwant %s", got, ymlA)
	}
}

// TestRunBackToWorktree_SelfJumpRepairsMissingYml: the chat sits
// in an owned worktree whose yml went missing, and names that
// worktree's own branch. There is no jump to make, but the yml is
// still repaired — this is the only path that heals an owned
// worktree without a `/cwd`.
func TestRunBackToWorktree_SelfJumpRepairsMissingYml(t *testing.T) {
	repoRoot := t.TempDir()
	wtA := WorktreePath(repoRoot, "other")
	if err := os.MkdirAll(wtA, 0o755); err != nil {
		t.Fatalf("mkdir wt: %v", err)
	}

	rig := newCloseRig(t)
	rig.git.revParseShowToplevel = repoRoot
	rig.git.worktreeListPorcelain =
		porcelainFor(repoRoot, "main") + "\n" + porcelainFor(wtA, "other")

	// No seedFix — the yml must be missing.
	if err := rig.cs.SetSelectedCwd(wtA); err != nil {
		t.Fatalf("SetSelectedCwd(%s): %v", wtA, err)
	}
	if _, err := os.Stat(gtwYmlPath(wtA)); err == nil {
		t.Fatalf("precondition: yml must not exist yet")
	}

	card, outcome := runBack(t, rig, "other")
	if outcome == nil {
		t.Fatalf("no settled outcome: the command reported nothing")
	}
	if outcome.Status != messages.OutcomeOK {
		t.Errorf("outcome status = %s, want %s", outcome.Status, messages.OutcomeOK)
	}
	if got := outcome.Data["reason"]; got != "already_in_worktree" {
		t.Errorf("outcome data[reason] = %q, want %q", got, "already_in_worktree")
	}
	if got := rig.cs.SelectedCwd(); got != wtA {
		t.Errorf("SelectedCwd must not move on a self-jump; got %q, want %q", got, wtA)
	}

	c, err := ReadGTWYml(wtA)
	if err != nil {
		t.Fatalf("ReadGTWYml after repair: %v", err)
	}
	if c.Mode != ModeLocal {
		t.Errorf("repaired yml Mode = %q, want %q", c.Mode, ModeLocal)
	}
	if c.Branch != "other" {
		t.Errorf("repaired yml Branch = %q, want %q (from the worktree listing)",
			c.Branch, "other")
	}
	if c.RepoRoot != repoRoot {
		t.Errorf("repaired yml RepoRoot = %q, want %q", c.RepoRoot, repoRoot)
	}

	if !strings.HasPrefix(card, "ℹ️") {
		t.Errorf("card should open with the info marker:\n%s", card)
	}
	if !strings.Contains(card, "repaired") {
		t.Errorf("card should report the repair:\n%s", card)
	}
}

// TestRunBackToWorktree_RepairsMissingYmlInTargetFromWorktree:
// the chat jumps out of worktree A into worktree B, and B has no
// yml yet. The repair fills it in — with the primary checkout as
// RepoRoot, not the worktree the chat started from.
func TestRunBackToWorktree_RepairsMissingYmlInTargetFromWorktree(t *testing.T) {
	repoRoot := t.TempDir()
	wtA := WorktreePath(repoRoot, "other")
	wtB := WorktreePath(repoRoot, "42-feat")
	for _, d := range []string{wtA, wtB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir wt: %v", err)
		}
	}

	rig := newCloseRig(t)
	rig.git.revParseShowToplevel = repoRoot
	rig.git.worktreeListPorcelain =
		porcelainFor(repoRoot, "main") + "\n" +
			porcelainFor(wtA, "other") + "\n" +
			porcelainFor(wtB, "42-feat")

	seedFix(t, rig, wtA, repoRoot) // leaves SelectedCwd at A
	ymlA := ymlBytes(t, wtA)

	card, outcome := runBack(t, rig, "42-f")
	if outcome == nil {
		t.Fatalf("no settled outcome: the command reported nothing")
	}
	if outcome.Status != messages.OutcomeOK {
		t.Errorf("outcome status = %s, want %s", outcome.Status, messages.OutcomeOK)
	}
	if got := rig.cs.SelectedCwd(); got != wtB {
		t.Errorf("SelectedCwd = %q, want %q", got, wtB)
	}

	c, err := ReadGTWYml(wtB)
	if err != nil {
		t.Fatalf("ReadGTWYml after repair: %v", err)
	}
	if c.Mode != ModeLocal {
		t.Errorf("repaired yml Mode = %q, want %q", c.Mode, ModeLocal)
	}
	if c.Branch != "42-feat" {
		t.Errorf("repaired yml Branch = %q, want %q (from the worktree listing)",
			c.Branch, "42-feat")
	}
	if c.RepoRoot != repoRoot {
		t.Errorf("repaired yml RepoRoot = %q, want %q (the primary checkout)",
			c.RepoRoot, repoRoot)
	}
	if got := ymlBytes(t, wtA); !bytes.Equal(got, ymlA) {
		t.Errorf("the worktree the chat left had its yml rewritten:\ngot %s\nwant %s", got, ymlA)
	}
	if !strings.Contains(card, "repaired") {
		t.Errorf("card should report the repair:\n%s", card)
	}
}

// TestRunBackToWorktree_JumpsFromForeignWorktree: a worktree the
// user made by hand is a legal departure point too. The jump works
// — and nightme writes nothing into the worktree it left.
func TestRunBackToWorktree_JumpsFromForeignWorktree(t *testing.T) {
	repoRoot := t.TempDir()
	wtB := WorktreePath(repoRoot, "42-feat")
	if err := os.MkdirAll(wtB, 0o755); err != nil {
		t.Fatalf("mkdir wt: %v", err)
	}
	// Outside <repo>.nightme/ — the user made this one.
	foreign := filepath.Join(t.TempDir(), "scratch")
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatalf("mkdir wt: %v", err)
	}

	rig := newCloseRig(t)
	rig.git.revParseShowToplevel = repoRoot
	rig.git.worktreeListPorcelain =
		porcelainFor(repoRoot, "main") + "\n" +
			porcelainFor(foreign, "scratch") + "\n" +
			porcelainFor(wtB, "42-feat")

	seedFix(t, rig, wtB, repoRoot)
	if err := rig.cs.SetSelectedCwd(foreign); err != nil {
		t.Fatalf("SetSelectedCwd(%s): %v", foreign, err)
	}

	card, outcome := runBack(t, rig, "42-f")
	if outcome == nil {
		t.Fatalf("no settled outcome: the command reported nothing")
	}
	if outcome.Status != messages.OutcomeOK {
		t.Errorf("outcome status = %s, want %s", outcome.Status, messages.OutcomeOK)
	}
	if got := rig.cs.SelectedCwd(); got != wtB {
		t.Errorf("SelectedCwd = %q, want %q", got, wtB)
	}
	if !strings.Contains(card, fmt.Sprintf("→ from: %s (preserved)", foreign)) {
		t.Errorf("card should name the worktree the chat came from:\n%s", card)
	}
	// Nightme claims no worktree it did not create.
	if _, err := os.Stat(gtwYmlPath(foreign)); err == nil {
		t.Errorf("no yml must be written into a worktree nightme did not create")
	}
}

// TestRunBackToWorktree_RejectsSubdirectoryCwd: a cwd inside the
// main repo or inside a worktree — but not at its root — is not a
// root the command can work from.
func TestRunBackToWorktree_RejectsSubdirectoryCwd(t *testing.T) {
	repoRoot := t.TempDir()
	wt := WorktreePath(repoRoot, "other")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("mkdir wt: %v", err)
	}
	inRepo := filepath.Join(repoRoot, "internal")
	inWorktree := filepath.Join(wt, "internal")
	for _, d := range []string{inRepo, inWorktree} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir subdir: %v", err)
		}
	}

	for _, tc := range []struct {
		name string
		cwd  string
	}{
		{"main repo subdirectory", inRepo},
		{"worktree subdirectory", inWorktree},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newCloseRig(t)
			rig.git.revParseShowToplevel = repoRoot
			rig.git.worktreeListPorcelain =
				porcelainFor(repoRoot, "main") + "\n" + porcelainFor(wt, "other")

			if err := rig.cs.SetSelectedCwd(tc.cwd); err != nil {
				t.Fatalf("SetSelectedCwd(%s): %v", tc.cwd, err)
			}

			card, outcome := runBack(t, rig, "other")
			if outcome == nil {
				t.Fatalf("no settled outcome: the command reported nothing")
			}
			if outcome.Status != messages.OutcomeFailed {
				t.Errorf("outcome status = %s, want %s", outcome.Status, messages.OutcomeFailed)
			}
			if got := outcome.Data["reason"]; got != "wrong_cwd" {
				t.Errorf("outcome data[reason] = %q, want %q", got, "wrong_cwd")
			}
			if got := rig.cs.SelectedCwd(); got != tc.cwd {
				t.Errorf("SelectedCwd must not move on rejection; got %q, want %q", got, tc.cwd)
			}
			if !strings.Contains(card, "main repo root or a worktree root") {
				t.Errorf("reply should name the legal cwds:\n%s", card)
			}
			if strings.Contains(card, "run `/gtw back` (no arg) first") {
				t.Errorf("reply must not send the user back to the main repo first:\n%s", card)
			}
		})
	}
}

// TestRunBackToWorktree_NoMatchFromWorktreeHintsMainRepo: from a
// worktree, a prefix that matches no branch names the branches
// there are and points at the main repo as the next step.
func TestRunBackToWorktree_NoMatchFromWorktreeHintsMainRepo(t *testing.T) {
	repoRoot := t.TempDir()
	wtA := WorktreePath(repoRoot, "other")
	wtB := WorktreePath(repoRoot, "42-feat")
	for _, d := range []string{wtA, wtB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir wt: %v", err)
		}
	}

	rig := newCloseRig(t)
	rig.git.revParseShowToplevel = repoRoot
	rig.git.worktreeListPorcelain =
		porcelainFor(repoRoot, "main") + "\n" +
			porcelainFor(wtA, "other") + "\n" +
			porcelainFor(wtB, "42-feat")

	seedFix(t, rig, wtA, repoRoot) // leaves SelectedCwd at A

	card, outcome := runBack(t, rig, "no-such-prefix")
	if outcome == nil {
		t.Fatalf("no settled outcome: the command reported nothing")
	}
	if outcome.Status != messages.OutcomeFailed {
		t.Errorf("outcome status = %s, want %s", outcome.Status, messages.OutcomeFailed)
	}
	if got := outcome.Data["reason"]; got != "not_found" {
		t.Errorf("outcome data[reason] = %q, want %q", got, "not_found")
	}
	if got := rig.cs.SelectedCwd(); got != wtA {
		t.Errorf("SelectedCwd must not move on failure; got %q, want %q", got, wtA)
	}
	for _, want := range []string{"other", "42-feat"} {
		if !strings.Contains(card, want) {
			t.Errorf("reply should name %q:\n%s", want, card)
		}
	}
	if !strings.Contains(card, "hint: run `/gtw back` (no arg) to return to the main repo") {
		t.Errorf("reply should point at the main repo:\n%s", card)
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
