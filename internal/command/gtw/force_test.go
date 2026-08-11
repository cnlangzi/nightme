package gtw

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/chatsession"
)



// --- parseFixArgs unit tests ---

func TestParseFixArgs_BareIssueID(t *testing.T) {
	got, err := parseFixArgs([]string{"42"})
	if err != nil {
		t.Fatalf("parseFixArgs: %v", err)
	}
	if got.Mode != ModeRemote {
		t.Errorf("Mode = %q, want remote", got.Mode)
	}
	if got.RawArg != "42" {
		t.Errorf("RawArg = %q, want 42", got.RawArg)
	}
	if got.Force {
		t.Errorf("Force = true, want false")
	}
}

func TestParseFixArgs_NameBranch(t *testing.T) {
	got, err := parseFixArgs([]string{"--name", "feat-x"})
	if err != nil {
		t.Fatalf("parseFixArgs: %v", err)
	}
	if got.Mode != ModeLocal {
		t.Errorf("Mode = %q, want local", got.Mode)
	}
	if got.RawArg != "feat-x" {
		t.Errorf("RawArg = %q, want feat-x", got.RawArg)
	}
	if got.Force {
		t.Errorf("Force = true, want false")
	}
}

func TestParseFixArgs_ForceBeforeID(t *testing.T) {
	got, err := parseFixArgs([]string{"--force", "42"})
	if err != nil {
		t.Fatalf("parseFixArgs: %v", err)
	}
	if got.Mode != ModeRemote {
		t.Errorf("Mode = %q, want remote", got.Mode)
	}
	if got.RawArg != "42" {
		t.Errorf("RawArg = %q, want 42", got.RawArg)
	}
	if !got.Force {
		t.Errorf("Force = false, want true")
	}
}

func TestParseFixArgs_ForceAfterID(t *testing.T) {
	got, err := parseFixArgs([]string{"42", "--force"})
	if err != nil {
		t.Fatalf("parseFixArgs: %v", err)
	}
	if !got.Force {
		t.Errorf("Force = false, want true")
	}
	if got.RawArg != "42" {
		t.Errorf("RawArg = %q, want 42", got.RawArg)
	}
}

func TestParseFixArgs_ForceShortFlag(t *testing.T) {
	got, err := parseFixArgs([]string{"-f", "42"})
	if err != nil {
		t.Fatalf("parseFixArgs: %v", err)
	}
	if !got.Force {
		t.Errorf("Force = false, want true")
	}
}

func TestParseFixArgs_ForceWithName(t *testing.T) {
	got, err := parseFixArgs([]string{"--name", "feat", "-f"})
	if err != nil {
		t.Fatalf("parseFixArgs: %v", err)
	}
	if got.Mode != ModeLocal {
		t.Errorf("Mode = %q, want local", got.Mode)
	}
	if got.RawArg != "feat" {
		t.Errorf("RawArg = %q, want feat", got.RawArg)
	}
	if !got.Force {
		t.Errorf("Force = false, want true")
	}
}

// TestParseFixArgs_ShortFlagN pins down the `-n` shorthand
// for `--name` — both must produce the same (ModeLocal, raw,
// false) shape. Pre-fix the parser only recognised `--name`
// and a typo or partial-refactor could silently regress this.
// Lock the behaviour in test so the short flag stays
// supported.
func TestParseFixArgs_ShortFlagN(t *testing.T) {
	got, err := parseFixArgs([]string{"-n", "feat"})
	if err != nil {
		t.Fatalf("parseFixArgs: %v", err)
	}
	if got.Mode != ModeLocal {
		t.Errorf("Mode = %q, want local", got.Mode)
	}
	if got.RawArg != "feat" {
		t.Errorf("RawArg = %q, want feat", got.RawArg)
	}
	if got.Force {
		t.Errorf("Force = true, want false")
	}
}

// TestParseFixArgs_ShortFlagNWithForce covers the
// combination `/gtw fix -n <branch> --force`. The order
// matters: the parser must accept `-n` in any position and
// `--force` in any position, including both at once.
func TestParseFixArgs_ShortFlagNWithForce(t *testing.T) {
	cases := [][]string{
		{"-n", "feat", "--force"},
		{"-n", "feat", "-f"},
		{"--force", "-n", "feat"},
		{"-f", "-n", "feat"},
	}
	for _, argv := range cases {
		t.Run(strings.Join(argv, "_"), func(t *testing.T) {
			got, err := parseFixArgs(argv)
			if err != nil {
				t.Fatalf("parseFixArgs: %v", err)
			}
			if got.Mode != ModeLocal {
				t.Errorf("Mode = %q, want local", got.Mode)
			}
			if got.RawArg != "feat" {
				t.Errorf("RawArg = %q, want feat", got.RawArg)
			}
			if !got.Force {
				t.Errorf("Force = false, want true")
			}
		})
	}
}

// TestParseFixArgs_ShortFlagNMergedWithArg verifies that the
// older "concatenated" form `-nfeat` is NOT supported — only
// space-separated `-n feat` works. We're a slash command, not
// a POSIX getopt; users get one form. Documenting the
// rejection explicitly so a future refactor doesn't quietly
// start accepting the merged form (which would also need a
// decision on `-f<value>` semantics for force-with-value).
func TestParseFixArgs_ShortFlagNMergedWithArg(t *testing.T) {
	got, err := parseFixArgs([]string{"-nfeat"})
	if err != nil {
		t.Fatalf("parseFixArgs: %v", err)
	}
	// The literal token "-nfeat" is treated as a bare
	// argument → ModeRemote (issue id). Users who want
	// `-n` MUST space-separate: `/gtw fix -n feat`.
	if got.Mode != ModeRemote {
		t.Errorf("Mode = %q, want remote (merged -nfeat treated as bare)", got.Mode)
	}
	if got.RawArg != "-nfeat" {
		t.Errorf("RawArg = %q, want -nfeat", got.RawArg)
	}
}

func TestParseFixArgs_MissingArg(t *testing.T) {
	_, err := parseFixArgs([]string{"--force"})
	if err == nil {
		t.Fatal("parseFixArgs: want error for --force with no issue id, got nil")
	}
}

// TestParseCloseForce + TestRunClose_ForceOverridesDirty were
// removed when --force was dropped from /gtw close (close is
// intentionally all-or-nothing; the user must commit / stash /
// discard before re-running). The remaining force_test.go cases
// cover /gtw fix --force.
// dropped from /gtw close (close is intentionally all-or-nothing;
// the user must commit / stash / discard before re-running).
// The remaining force_test.go cases cover /gtw fix --force.

// --- forceCleanWorktreePath unit tests ---

func TestForceCleanWorktreePath_NoopWhenMissing(t *testing.T) {
	dir := t.TempDir()
	wt := filepath.Join(dir, "nonexistent")
	if err := forceCleanWorktreePath(context.Background(), dir, wt, ExecGitRunner{}); err != nil {
		t.Fatalf("forceCleanWorktreePath: %v", err)
	}
}

// --- ID-mode integration tests ---

// TestFixRemote_ForceRemovesLeftoverWorktree is the canonical
// --force happy path: a stale worktree occupies the target
// path with a DIFFERENT branch attached (e.g. the user did
// /gtw fix 99 last week, the yml is long gone, but the
// directory + branch-ref linger). Without --force,
// PreflightWorktreeCreate refuses on path-occupied. With
// --force, we force-remove the leftover and create a fresh
// worktree for the new branch.
func TestFixRemote_ForceRemovesLeftoverWorktree(t *testing.T) {
	repoRoot := initTempRepo(t)
	mustGit(t, repoRoot, "remote", "add", "origin",
		"https://github.com/cnlangzi/nightme.git")
	mustGit(t, repoRoot, "symbolic-ref", "refs/remotes/origin/HEAD",
		"refs/remotes/origin/main")

	// The fix flow derives a branch from the issue title;
	// for issue "Title" that's branch="title", path=
	// <repo>.nightme/title (WorktreePath uses LabelPrefix
	// ".nightme" — see gtw/slug.go). Pre-seed a worktree at
	// THAT exact path with a DIFFERENT branch (e.g. "oldbranch")
	// so the path is occupied but the new branch is unattached.
	wt := WorktreePath(repoRoot, "title")
	mustGit(t, repoRoot, "worktree", "add", "-b", "oldbranch", wt, "HEAD")

	// Sanity: preflight (without force) must reject because
	// the path is occupied.
	if err := PreflightWorktreeCreate(context.Background(), repoRoot, "title", wt, ExecGitRunner{}); err == nil {
		t.Fatal("PreflightWorktreeCreate should reject occupied path")
	}

	rec := &recordingCh{}
	cs, _ := chatsession.New("chat-force", "test-agent")
	cs.WithEmitter(rec)
	if err := cs.SetSelectedCwd(repoRoot); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}

	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	prov.SetIssue(42, &Issue{ID: 42, Title: "Title", State: "open"})

	deps := HandlerDeps{
		Git: ExecGitRunner{},
		Now: func() time.Time { return time.Date(2026, 8, 8, 14, 0, 0, 0, time.UTC) },
		Detect:                  fakeDetect(prov),
		SkipRefreshDefaultBranch: true,
	}

	res, err := RunFix(
		context.Background(),
		ModeRemote,
		cs,
		&memSlot{},
		newMemDrafts(),
		deps,
		cs.ChatID,
		"msg",
		[]string{"42"},
		true, /* force */
	)
	if err != nil {
		t.Fatalf("RunFix: %v", err)
	}
	if !res.Consumed {
		t.Errorf("Result.Consumed = false")
	}

	// After the fix, SelectedCwd must be the (re-created) worktree.
	if got := cs.SelectedCwd(); !pathsEqual(got, wt) {
		t.Errorf("SelectedCwd = %q, want %q", got, wt)
	}
	// yml must be re-written under the same path.
	if _, err := os.Stat(filepath.Join(wt, ".nightme", "gtw.yml")); err != nil {
		t.Errorf("yml not at fresh worktree: %v", err)
	}
	// The leftover "oldbranch" must be gone (force-removed).
	wtList, _ := mustGitOut(t, repoRoot, "worktree", "list", "--porcelain")
	if strings.Contains(wtList, "oldbranch") {
		t.Errorf("leftover oldbranch still present in worktree list:\n%s", wtList)
	}
	// Success reply mentions the new fix.
	last := rec.lastText()
	if !strings.Contains(last, "Fix #42") {
		t.Errorf("reply missing Fix #42:\n%s", last)
	}
}

// TestFixRemote_WithoutForceStillRejectsOccupied is the
// negative case — without --force, the same pre-occupied
// setup must be rejected by the preflight path-occupied check.
func TestFixRemote_WithoutForceStillRejectsOccupied(t *testing.T) {
	repoRoot := initTempRepo(t)
	mustGit(t, repoRoot, "remote", "add", "origin",
		"https://github.com/cnlangzi/nightme.git")
	mustGit(t, repoRoot, "symbolic-ref", "refs/remotes/origin/HEAD",
		"refs/remotes/origin/main")

	wt := WorktreePath(repoRoot, "title")
	mustGit(t, repoRoot, "worktree", "add", "-b", "oldbranch", wt, "HEAD")

	rec := &recordingCh{}
	cs, _ := chatsession.New("chat-noforce", "test-agent")
	cs.WithEmitter(rec)
	_ = cs.SetSelectedCwd(repoRoot)

	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	prov.SetIssue(42, &Issue{ID: 42, Title: "Title", State: "open"})
	deps := HandlerDeps{
		Git: ExecGitRunner{},
		Now: func() time.Time { return time.Date(2026, 8, 8, 14, 0, 0, 0, time.UTC) },
		Detect:                  fakeDetect(prov),
		SkipRefreshDefaultBranch: true,
	}

	res, err := RunFix(
		context.Background(), ModeRemote, cs,
		&memSlot{}, newMemDrafts(), deps,
		cs.ChatID, "msg", []string{"42"},
		false, /* force — disabled */
	)
	if err != nil {
		t.Fatalf("RunFix: %v", err)
	}
	if !res.Consumed {
		t.Errorf("Result.Consumed = false")
	}
	last := rec.lastText()
	// Without --force, preflight rejects on path-occupied.
	if !strings.Contains(last, "already exists on filesystem") {
		t.Errorf("reply should mention path-occupied:\n%s", last)
	}
}
