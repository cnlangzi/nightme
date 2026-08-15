package gtw

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestPreflightWorktreeCreate_PathExists verifies the path-
// occupied failure: target worktree path is already on the
// filesystem AND the branch is NOT attached there (e.g. an
// old failed attempt left a stale directory).
func TestPreflightWorktreeCreate_PathExists(t *testing.T) {
	tmp := t.TempDir()
	tgt := filepath.Join(tmp, "blocked")
	if err := os.MkdirAll(tgt, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}

	// Use a branch name that the fakeGit reports as not present
	// (worktree list returns empty → branch not attached
	// anywhere → check 1 short-circuit does NOT trigger).
	git := &fakeGit{}
	err := PreflightWorktreeCreate(context.Background(), tmp, "feat-stale", tgt, git)
	if err == nil {
		t.Fatalf("expected error when target path exists and branch is not attached, got nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected 'already exists' in error, got %q", err.Error())
	}
}

// TestPreflightWorktreeCreate_BranchAttachedElsewhere covers
// the "branch already checked out at another worktree" case.
// We use a real tempdir + real `git init` because the porcelain
// parser is non-trivial to fake accurately.
func TestPreflightWorktreeCreate_BranchAttachedElsewhere(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git binary not available: %v", err)
	}
	tmp := t.TempDir()

	// `git init` + initial commit so worktree add can succeed.
	run := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = tmp
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "test@test")
	run("config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(tmp, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "f")
	run("commit", "-q", "-m", "init")
	// Create feat-x WITHOUT checking it out — `git worktree add`
	// requires the target branch not be currently checked out.
	run("branch", "feat-x")

	// Create a worktree holding feat-x at one path.
	wt1 := filepath.Join(tmp, "wt1")
	run("worktree", "add", wt1, "feat-x")

	// Now ask PreflightWorktreeCreate for a different path with
	// the same branch — it should detect the conflict.
	wt2 := filepath.Join(tmp, "wt2")
	git := ExecGitRunner{}
	err := PreflightWorktreeCreate(context.Background(), tmp, "feat-x", wt2, git)
	if err == nil {
		t.Fatalf("expected conflict error, got nil")
	}
	if !strings.Contains(err.Error(), "already checked out") {
		t.Errorf("expected 'already checked out' in error, got %q", err.Error())
	}
}

// TestPreflightWorktreeCreate_BranchAttachedSamePath covers the
// allowed case: branch is attached at exactly the target
// worktree path (re-entry after daemon recovery). Should NOT
// fail.
func TestPreflightWorktreeCreate_BranchAttachedSamePath(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git binary not available: %v", err)
	}
	tmp := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = tmp
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "test@test")
	run("config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(tmp, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "f")
	run("commit", "-q", "-m", "init")
	run("branch", "feat-x")

	wt1 := filepath.Join(tmp, "wt1")
	run("worktree", "add", wt1, "feat-x")

	// Same path — should pass.
	err := PreflightWorktreeCreate(context.Background(), tmp, "feat-x", wt1, ExecGitRunner{})
	if err != nil {
		t.Errorf("expected same-path to be allowed, got %v", err)
	}
}

// TestPreflightWorktreeCreate_ParentUnwritable covers the
// "parent dir is not writable" case. We use os.Chmod 0o500
// (read+execute, no write) — sufficient on POSIX to make
// os.MkdirTemp fail. The test is skipped on non-POSIX systems
// where chmod 0500 doesn't grant the desired read-only
// behaviour.
func TestPreflightWorktreeCreate_ParentUnwritable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skipf("chmod semantics on Windows don't match POSIX; skipping")
	}
	tmp := t.TempDir()
	readOnlyParent := filepath.Join(tmp, "ro")
	if err := os.MkdirAll(readOnlyParent, 0o755); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	if err := os.Chmod(readOnlyParent, 0o500); err != nil {
		t.Skipf("chmod 0500 not supported: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(readOnlyParent, 0o755) })

	tgt := filepath.Join(readOnlyParent, "new-wt")
	git := &fakeGit{}
	err := PreflightWorktreeCreate(context.Background(), tmp, "feat-x", tgt, git)
	if err == nil {
		t.Fatalf("expected parent-not-writable error, got nil")
	}
	if !strings.Contains(err.Error(), "not writable") {
		t.Errorf("expected 'not writable' in error, got %q", err.Error())
	}
}

// fakeGit is a minimal GitRunner for tests that don't need to
// exercise the worktree-list path. Returns empty for any
// show-ref / worktree-list query.
type fakeGit struct{}

func (f *fakeGit) Run(_ context.Context, _ string, args ...string) (string, string, error) {
	switch {
	case len(args) > 0 && args[0] == "show-ref":
		return "", "fake: branch not found", nil
	case len(args) > 0 && args[0] == "worktree":
		return "", "", nil
	}
	return "", "", nil
}