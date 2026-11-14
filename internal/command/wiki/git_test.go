package wiki

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGitArgs_NoOptionalLocksFirst pins the argv shape: the flag has
// to precede the subcommand for git to accept it, and it precedes
// `-C` so the flag is the first argument after `git`.
func TestGitArgs_NoOptionalLocksFirst(t *testing.T) {
	args := gitArgs("/repo", "status", "--porcelain")
	if len(args) != 5 {
		t.Fatalf("args = %v, want 5 elements", args)
	}
	if args[0] != "--no-optional-locks" {
		t.Errorf("args[0] = %q, want --no-optional-locks", args[0])
	}
	if args[1] != "-C" || args[2] != "/repo" {
		t.Errorf("args = %v, want -C /repo right after the flag", args)
	}
	if args[3] != "status" || args[4] != "--porcelain" {
		t.Errorf("args = %v, want the subcommand last", args)
	}
}

// TestExecGitRunner_RepoRootAndIsClean covers the two git calls /wiki
// makes against a real repository.
func TestExecGitRunner_RepoRootAndIsClean(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; skipping real-git test")
	}
	dir := initWikiRepo(t)
	r := ExecGitRunner{}

	root, err := r.RepoRoot(context.Background(), dir)
	if err != nil {
		t.Fatalf("RepoRoot: %v", err)
	}
	if !sameDir(t, root, dir) {
		t.Errorf("RepoRoot = %q, want %q", root, dir)
	}

	clean, err := r.IsClean(context.Background(), dir)
	if err != nil {
		t.Fatalf("IsClean: %v", err)
	}
	if !clean {
		t.Error("IsClean: want a clean tree, got dirty")
	}

	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("y\n"), 0o644); err != nil {
		t.Fatalf("write b.txt: %v", err)
	}
	clean, err = r.IsClean(context.Background(), dir)
	if err != nil {
		t.Fatalf("IsClean (dirty): %v", err)
	}
	if clean {
		t.Error("IsClean: want a dirty tree, got clean")
	}
}

// TestExecGitRunner_StatusMatchesPlainGit pins that
// `--no-optional-locks` does not change what the status call
// reports.
func TestExecGitRunner_StatusMatchesPlainGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; skipping real-git test")
	}
	dir := initWikiRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("y\n"), 0o644); err != nil {
		t.Fatalf("write b.txt: %v", err)
	}

	got, err := runGit(context.Background(), dir, "status", "--porcelain")
	if err != nil {
		t.Fatalf("runGit: %v", err)
	}
	plain := plainGit(t, dir, "status", "--porcelain")
	if strings.TrimSpace(got) != strings.TrimSpace(plain) {
		t.Errorf("status output = %q, want %q (the flag must not change it)", got, plain)
	}
}

// TestExecGitRunner_NotARepoReportsGitStderr pins that a failure
// carries git's own diagnostic instead of a bare exit status.
func TestExecGitRunner_NotARepoReportsGitStderr(t *testing.T) {
	dir := t.TempDir()
	_, err := (ExecGitRunner{}).RepoRoot(context.Background(), dir)
	if err == nil {
		t.Fatal("RepoRoot: expected an error outside a repository, got nil")
	}
	if !strings.Contains(err.Error(), "not a git repo") {
		t.Errorf("error should explain the situation: %v", err)
	}
}

// initWikiRepo creates a throwaway repository with one commit.
func initWikiRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	plainGit(t, dir, "init", "-q", "-b", "main")
	plainGit(t, dir, "config", "user.email", "test@example.com")
	plainGit(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	plainGit(t, dir, "add", "a.txt")
	plainGit(t, dir, "commit", "-qm", "init")
	return dir
}

// plainGit runs a git command in dir and returns its stdout.
func plainGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return string(out)
}

// sameDir compares two paths after resolving symlinks (macOS's
// t.TempDir() hands out the symlinked form).
func sameDir(t *testing.T, a, b string) bool {
	t.Helper()
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return a == b
	}
	return ra == rb
}
