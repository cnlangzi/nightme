package gtw

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRefreshDefaultBranch_RealGitEndToEnd covers the full
// happy path against real git: bare-repo origin, an upstream
// commit that the local repo hasn't seen, and the post-refresh
// local HEAD matches the upstream tip.
//
// This is the unit-test companion to the SkipRefreshDefaultBranch
// integration tests — those skip this code path; this test
// covers it for real.
func TestRefreshDefaultBranch_RealGitEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; skipping real-git integration test")
	}

	repoRoot := initTempRepo(t)
	bare := addLocalRemote(t, repoRoot)

	// Push a new commit to the bare repo's main — simulates
	// "upstream moved forward while you were away". We clone
	// the bare's main locally, commit, push back.
	pushWork := t.TempDir()
	mustGit(t, pushWork, "clone", "-q", bare, pushWork)
	mustGit(t, pushWork, "config", "user.email", "upstream@example.com")
	mustGit(t, pushWork, "config", "user.name", "Upstream")
	if err := writeFile(filepath.Join(pushWork, "new.txt"), "upstream\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustGit(t, pushWork, "add", "new.txt")
	mustGit(t, pushWork, "commit", "-q", "-m", "upstream commit")
	mustGit(t, pushWork, "push", "-q", "origin", "main")

	// Local main is still at the original commit (not the
	// upstream one). RefreshDefaultBranch should bring it
	// up to date via `git pull --ff-only`.
	headBefore, _, err := runGit(t, repoRoot, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD before: %v", err)
	}
	upstreamHead, _, err := runGit(t, bare, "rev-parse", "main")
	if err != nil {
		t.Fatalf("rev-parse upstream main: %v", err)
	}
	if headBefore == upstreamHead {
		t.Fatalf("precondition: local HEAD should differ from upstream before refresh")
	}

	got, err := RefreshDefaultBranch(context.Background(), repoRoot, HandlerDeps{
		Git: ExecGitRunner{},
	})
	if err != nil {
		t.Fatalf("RefreshDefaultBranch: %v", err)
	}
	if got != upstreamHead {
		t.Errorf("returned HEAD = %s, want %s", got, upstreamHead)
	}

	// Local HEAD should now match upstream.
	headAfter, _, err := runGit(t, repoRoot, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD after: %v", err)
	}
	if headAfter != upstreamHead {
		t.Errorf("local HEAD after refresh = %s, want %s", headAfter, upstreamHead)
	}
}

// TestRefreshDefaultBranch_RefusesDirtyMain covers the
// failure case where the local main has uncommitted
// changes. The helper must refuse rather than silently
// dropping the user's work.
func TestRefreshDefaultBranch_RefusesDirtyMain(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; skipping real-git integration test")
	}

	repoRoot := initTempRepo(t)
	addLocalRemote(t, repoRoot)

	if err := writeFile(filepath.Join(repoRoot, "WIP.txt"), "in progress\n"); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := RefreshDefaultBranch(context.Background(), repoRoot, HandlerDeps{
		Git: ExecGitRunner{},
	})
	if err == nil {
		t.Fatal("RefreshDefaultBranch: want error on dirty main, got nil")
	}
	if !strings.Contains(err.Error(), "uncommitted") {
		t.Errorf("error should mention uncommitted: %v", err)
	}
}

// TestRefreshDefaultBranch_NoOrigin covers the case where
// the repo has no `origin` remote at all. Should fail with
// a hint about setting up an upstream.
func TestRefreshDefaultBranch_NoOrigin(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; skipping real-git integration test")
	}

	repoRoot := initTempRepo(t)
	// Deliberately do NOT call addLocalRemote — origin stays
	// absent.

	_, err := RefreshDefaultBranch(context.Background(), repoRoot, HandlerDeps{
		Git: ExecGitRunner{},
	})
	if err == nil {
		t.Fatal("RefreshDefaultBranch: want error when no origin, got nil")
	}
	if !strings.Contains(err.Error(), "origin") {
		t.Errorf("error should mention origin: %v", err)
	}
}

// TestRefreshDefaultBranch_RebaseOnDivergedMain covers the
// "user has un-pushed local commits + upstream moved" case.
// RefreshDefaultBranch should REBASE the local commits on top
// of upstream — the user's commits survive (under new SHAs)
// and the local main now includes upstream's new commits.
func TestRefreshDefaultBranch_RebaseOnDivergedMain(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; skipping real-git integration test")
	}

	repoRoot := initTempRepo(t)
	bare := addLocalRemote(t, repoRoot)

	// Push an upstream commit on top of the synced base.
	pushWork := t.TempDir()
	mustGit(t, pushWork, "clone", "-q", bare, pushWork)
	mustGit(t, pushWork, "config", "user.email", "upstream@example.com")
	mustGit(t, pushWork, "config", "user.name", "Upstream")
	if err := writeFile(filepath.Join(pushWork, "upstream.txt"), "u\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustGit(t, pushWork, "add", "upstream.txt")
	mustGit(t, pushWork, "commit", "-q", "-m", "upstream commit")
	mustGit(t, pushWork, "push", "-q", "origin", "main")

	// Make a LOCAL-only commit on a divergent local main.
	if err := writeFile(filepath.Join(repoRoot, "local.txt"), "l\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustGit(t, repoRoot, "add", "local.txt")
	mustGit(t, repoRoot, "commit", "-q", "-m", "local commit")
	localSHA, _, err := runGit(t, repoRoot, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse local: %v", err)
	}

	_, err = RefreshDefaultBranch(context.Background(), repoRoot, HandlerDeps{
		Git: ExecGitRunner{},
	})
	if err != nil {
		t.Fatalf("RefreshDefaultBranch should succeed via rebase, got: %v", err)
	}

	// Local HEAD should now be on top of upstream's tip.
	upstreamHead, _, err := runGit(t, bare, "rev-parse", "main")
	if err != nil {
		t.Fatalf("rev-parse upstream main: %v", err)
	}
	// HEAD should have upstream as its first parent.
	parents, _, err := runGit(t, repoRoot, "log", "--pretty=%P", "-n", "1", "HEAD")
	if err != nil {
		t.Fatalf("log parents: %v", err)
	}
	if !strings.Contains(parents, upstreamHead) {
		t.Errorf("HEAD's first parent should be upstream %s, got %q", upstreamHead, parents)
	}

	// The user's local file should still exist after rebase.
	if _, _, err := runGit(t, repoRoot, "show", "HEAD:local.txt"); err != nil {
		t.Errorf("local.txt should survive rebase: %v", err)
	}

	// Local commit SHA changed (rebase rewrites history) —
	// sanity-check that the old SHA is gone.
	out, _, _ := runGit(t, repoRoot, "rev-parse", "HEAD")
	if out == localSHA {
		t.Errorf("rebase should rewrite local SHA; old %s == new %s", localSHA, out)
	}
}

// TestRefreshDefaultBranch_RebaseConflict covers the rare
// case where upstream and local both modified the same file
// line. RefreshDefaultBranch should report the conflict
// (and the user can `git rebase --abort`).
func TestRefreshDefaultBranch_RebaseConflict(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; skipping real-git integration test")
	}

	repoRoot := initTempRepo(t)
	bare := addLocalRemote(t, repoRoot)

	// Seed a file both sides will modify.
	if err := writeFile(filepath.Join(repoRoot, "shared.txt"), "line1\nline2\nline3\n"); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	mustGit(t, repoRoot, "add", "shared.txt")
	mustGit(t, repoRoot, "commit", "-q", "-m", "seed")

	// Push a diverging upstream that modifies line2.
	pushWork := t.TempDir()
	mustGit(t, pushWork, "clone", "-q", bare, pushWork)
	mustGit(t, pushWork, "config", "user.email", "upstream@example.com")
	mustGit(t, pushWork, "config", "user.name", "Upstream")
	if err := writeFile(filepath.Join(pushWork, "shared.txt"), "line1\nUPSTREAM\nline3\n"); err != nil {
		t.Fatalf("write upstream: %v", err)
	}
	mustGit(t, pushWork, "add", "shared.txt")
	mustGit(t, pushWork, "commit", "-q", "-m", "upstream change")
	mustGit(t, pushWork, "push", "-q", "origin", "main")

	// Local modifies the SAME line.
	if err := writeFile(filepath.Join(repoRoot, "shared.txt"), "line1\nLOCAL\nline3\n"); err != nil {
		t.Fatalf("write local: %v", err)
	}
	mustGit(t, repoRoot, "add", "shared.txt")
	mustGit(t, repoRoot, "commit", "-q", "-m", "local change")

	_, err := RefreshDefaultBranch(context.Background(), repoRoot, HandlerDeps{
		Git: ExecGitRunner{},
	})
	if err == nil {
		t.Fatal("RefreshDefaultBranch: want error on rebase conflict, got nil")
	}
	// Mid-rebase conflict should be visible: .git/rebase-merge or .git/rebase-applied exists.
	if _, statErr := os.Stat(filepath.Join(repoRoot, ".git", "rebase-merge")); statErr != nil {
		if _, statErr2 := os.Stat(filepath.Join(repoRoot, ".git", "rebase-applied")); statErr2 != nil {
			t.Errorf("expected mid-rebase state to exist; neither rebase-merge nor rebase-applied present: %v / %v", statErr, statErr2)
		}
	}

	// Clean up the mid-rebase state so subsequent tests in
	// the same t.TempDir scope don't see a stuck repo.
	mustGit(t, repoRoot, "rebase", "--abort")
}

// --- helpers ---

func writeFile(path, content string) error {
	return exec.Command("sh", "-c",
		"printf '%s' \"$1\" > \"$2\"", "-", content, path).Run()
}

// runGit runs a git command in dir and returns stdout/stderr
// separately. Used by RefreshDefaultBranch tests where we
// need precise stdout/stderr (mustGit joins them).
func runGit(t *testing.T, dir string, args ...string) (string, string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(stdout.String()),
			strings.TrimSpace(stderr.String()),
			err
	}
	return strings.TrimSpace(stdout.String()),
		strings.TrimSpace(stderr.String()),
		nil
}