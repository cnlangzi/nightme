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

// --- /gtw close --force ---

// TestParseCloseForce covers the boolean-flag-only parser
// used by the close subcommand. Unlike /gtw fix, /gtw close
// takes no positional arg today, so the parser just
// recognises --force / -f.
func TestParseCloseForce(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want bool
	}{
		{"empty", nil, false},
		{"no flag", []string{"42"}, false},
		{"--force", []string{"--force"}, true},
		{"-f", []string{"-f"}, true},
		{"force mixed", []string{"foo", "--force", "bar"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseCloseForce(c.argv)
			if err != nil {
				t.Fatalf("parseCloseForce: %v", err)
			}
			if got != c.want {
				t.Errorf("force = %v, want %v", got, c.want)
			}
		})
	}
}

// TestRunClose_ForceOverridesDirty is the integration
// equivalent: the worktree has uncommitted changes, the
// default close refuses; --force proceeds and force-removes
// the worktree, with the reply noting that local edits were
// discarded.
//
// Uses a real git repo (initTempRepo) because the unit
// fake's `status` returns empty regardless of dirty state;
// the integration test asserts on real porcelain output.
func TestRunClose_ForceOverridesDirty(t *testing.T) {
	repoRoot := initTempRepo(t)
	wt := filepath.Join(filepath.Dir(repoRoot), filepath.Base(repoRoot)+".wt-dirty")
	mustGit(t, repoRoot, "worktree", "add", "-b", "fix/dirty", wt, "HEAD")

	// Ensure + Commit .gitignore so the worktree is clean
	// BEFORE the sentinel write. Otherwise git status would
	// see the untracked .gitignore and report "dirty" even
	// without our sentinel.
	if err := EnsureGitignore(wt); err != nil {
		t.Fatalf("EnsureGitignore: %v", err)
	}
	if err := CommitGitignoreIfDirty(context.Background(), wt, ExecGitRunner{}); err != nil {
		t.Fatalf("CommitGitignoreIfDirty: %v", err)
	}

	if err := WriteGTWYml(wt, Context{
		Mode: ModeLocal, Issue: -1, Branch: "fix/dirty",
		Worktree: wt, RepoRoot: repoRoot, State: StateFixing,
	}, func() time.Time { return time.Date(2026, 8, 8, 14, 0, 0, 0, time.UTC) }); err != nil {
		t.Fatalf("WriteGTWYml: %v", err)
	}

	rec := &recordingCh{}
	cs, _ := chatsession.New("chat-force-dirty", "test-agent", rec)
	_ = cs.SetActiveCwd(wt)
	slot := &memSlot{Context{
		Mode: ModeLocal, Issue: -1, Branch: "fix/dirty",
		Worktree: wt, RepoRoot: repoRoot, State: StateFixing,
	}}
	deps := HandlerDeps{
		Git: ExecGitRunner{},
		Now: func() time.Time { return time.Date(2026, 8, 8, 14, 0, 0, 0, time.UTC) },
	}

	// Make the worktree dirty.
	if err := os.WriteFile(filepath.Join(wt, "sentinel.txt"), []byte("dirty"), 0o644); err != nil {
		t.Fatalf("write dirty sentinel: %v", err)
	}

	// Without force: refused.
	res, err := RunClose(context.Background(), cs, slot, deps, cs.ChatID, "msg-no", false /* force */)
	if err != nil {
		t.Fatalf("RunClose (no force): %v", err)
	}
	if !res.Consumed {
		t.Errorf("Result.Consumed = false")
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree removed despite no-force: %v", err)
	}
	noForceReply := rec.lastText()
	if !strings.Contains(noForceReply, "uncommitted") {
		t.Errorf("no-force reply missing dirty hint:\n%s", noForceReply)
	}

	// With force: proceeds. yml + slot are still valid from
	// the no-force attempt (no state change on dirty refuse).
	res, err = RunClose(context.Background(), cs, slot, deps, cs.ChatID, "msg-yes", true /* force */)
	if err != nil {
		t.Fatalf("RunClose (force): %v", err)
	}
	if !res.Consumed {
		t.Errorf("Result.Consumed = false")
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("worktree still present after force-close: %v", err)
	}
	forceReply := rec.lastText()
	if !strings.Contains(forceReply, "force-discarded") {
		t.Errorf("force reply missing force-discarded note:\n%s", forceReply)
	}
	if !strings.Contains(forceReply, "✅") {
		t.Errorf("force reply missing success marker:\n%s", forceReply)
	}
}

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
	cs, _ := chatsession.New("chat-force", "test-agent", rec)
	if err := cs.SetActiveCwd(repoRoot); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
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

	// After the fix, ActiveCwd must be the (re-created) worktree.
	if got := cs.ActiveCwd(); !pathsEqual(got, wt) {
		t.Errorf("ActiveCwd = %q, want %q", got, wt)
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
	cs, _ := chatsession.New("chat-noforce", "test-agent", rec)
	_ = cs.SetActiveCwd(repoRoot)

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
