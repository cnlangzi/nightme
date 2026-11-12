package me

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/proc"
)

// realGitRunner runs git in a workspace — what the runtime wires in.
type realGitRunner struct{}

func (realGitRunner) Run(ctx context.Context, dir string, args ...string) (string, string, error) {
	c := proc.New(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr strings.Builder
	c.Stdout = &stdout
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		return stdout.String(), stderr.String(), err
	}
	return stdout.String(), stderr.String(), nil
}

// gitOut runs git in dir and returns its output, ignoring failures.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := proc.New(t.Context(), "git", append([]string{"-C", dir}, args...)...)
	out, _ := c.CombinedOutput()
	return strings.TrimSpace(string(out))
}

// gitIn runs git in dir and fails the test on error.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	c := proc.New(t.Context(), "git", append([]string{"-C", dir}, args...)...)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// repoWithOrigin creates a working repository whose origin is a local
// bare repository. The working copy has no refs/remotes/origin/HEAD —
// `git fetch` never creates one.
func repoWithOrigin(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	bare := filepath.Join(base, "remote.git")
	gitIn(t, base, "init", "--bare", bare)
	gitIn(t, bare, "symbolic-ref", "HEAD", "refs/heads/main")

	seed := filepath.Join(base, "seed")
	gitIn(t, seed, "init")
	gitIn(t, seed, "config", "user.email", "test@test.local")
	gitIn(t, seed, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(seed, "a.go"), []byte("package a\n"), 0o600); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	gitIn(t, seed, "add", "a.go")
	gitIn(t, seed, "commit", "-q", "-m", "init")
	gitIn(t, seed, "remote", "add", "origin", bare)
	gitIn(t, seed, "push", "-q", "origin", "HEAD:refs/heads/main")

	work := filepath.Join(base, "work")
	gitIn(t, work, "init")
	gitIn(t, work, "config", "user.email", "test@test.local")
	gitIn(t, work, "config", "user.name", "test")
	gitIn(t, work, "remote", "add", "origin", bare)
	gitIn(t, work, "fetch", "origin")
	removeOriginHead(t, work)
	gitIn(t, work, "checkout", "-B", "main", "origin/main")
	return work
}

// removeOriginHead deletes refs/remotes/origin/HEAD in whichever form
// it exists — symbolic or direct. Some git versions create it on
// fetch; the read-only assertion under test needs it absent.
func removeOriginHead(t *testing.T, repo string) {
	t.Helper()
	gitOut(t, repo, "symbolic-ref", "-d", "refs/remotes/origin/HEAD")
	gitOut(t, repo, "update-ref", "--no-deref", "-d", "refs/remotes/origin/HEAD")
}

// TestRepoFacts_BaseBranchIsResolvedReadOnly covers the upstream base
// branch in a repository that lacks refs/remotes/origin/HEAD: the
// shared resolver finds the remote's default branch, and /me leaves
// the repository alone — it reports facts, it does not change them.
func TestRepoFacts_BaseBranchIsResolvedReadOnly(t *testing.T) {
	dir := repoWithOrigin(t)
	// One commit on top of origin/main.
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n+changed\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	gitIn(t, dir, "add", "a.go")
	gitIn(t, dir, "commit", "-q", "-m", "change")

	svc := New(Config{Git: realGitRunner{}})
	facts, err := svc.repoFacts(context.Background(), dir)
	if err != nil {
		t.Fatalf("repoFacts: %v", err)
	}
	if facts.Base != "origin/main" {
		t.Errorf("Base = %q, want %q", facts.Base, "origin/main")
	}
	if facts.AheadOfBase != 1 {
		t.Errorf("AheadOfBase = %d, want 1", facts.AheadOfBase)
	}
	if facts.BehindBase != 0 {
		t.Errorf("BehindBase = %d, want 0", facts.BehindBase)
	}
	if out := gitOut(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD"); !strings.Contains(out, "not a symbolic ref") {
		t.Errorf("origin/HEAD = %q, want /me to leave the repository untouched", out)
	}
}

// TestRepoFacts_NoRemoteHasNoBase covers a repository without a
// remote: there is no base to report, and none is invented.
func TestRepoFacts_NoRemoteHasNoBase(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init")
	gitIn(t, dir, "config", "user.email", "test@test.local")
	gitIn(t, dir, "config", "user.name", "test")

	svc := New(Config{Git: realGitRunner{}})
	facts, err := svc.repoFacts(context.Background(), dir)
	if err != nil {
		t.Fatalf("repoFacts: %v", err)
	}
	if facts.Base != "" {
		t.Errorf("Base = %q, want empty", facts.Base)
	}
}
