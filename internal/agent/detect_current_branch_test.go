// detect_current_branch_test.go — unit tests for CurrentBranch's
// three observable states: real branch, detached HEAD, non-git /
// non-existent path. Mirrors detect_default_branch_test.go's
// shape (also three cases, same "absent value = empty string"
// contract).
//
//go:build !windows

package agent

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// TestCurrentBranch_HappyPath pins the basic contract: a normal
// repo on a feature branch returns the branch name. Pre-fix
// runCodeReviewPrintMode used `defaultBase...HEAD` here as a
// ref-range positional; the fix passes the result of this
// function instead. If the contract regresses (e.g. someone
// switches to `rev-parse --abbrev-ref HEAD` which returns the
// literal string "HEAD" on detached HEAD), the review plugin
// silently gets garbage as its anchor.
func TestCurrentBranch_HappyPath(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"commit", "--allow-empty", "-q", "-m", "root"},
		{"checkout", "-q", "-b", "feat-x"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	got := CurrentBranch(context.Background(), dir)
	if got != "feat-x" {
		t.Errorf("happy path: got %q, want %q", got, "feat-x")
	}
}

// TestCurrentBranch_DetachedHead pins the detached-HEAD → ""
// contract. The pre-fix mental model used `rev-parse
// --abbrev-ref HEAD` which returns "HEAD" here; that string
// would then be passed to claude's plugin as if it were a real
// branch refuse, polluting the review anchor. We use
// `symbolic-ref --short HEAD` which exits non-zero on detached
// HEAD → mapped to "".
func TestCurrentBranch_DetachedHead(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"commit", "--allow-empty", "-q", "-m", "root"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	rootSHA, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	if out, err := exec.Command("git", "-C", dir, "checkout", "--detach", strings.TrimSpace(string(rootSHA))).CombinedOutput(); err != nil {
		t.Fatalf("checkout --detach: %v\n%s", err, out)
	}

	got := CurrentBranch(context.Background(), dir)
	if got != "" {
		t.Errorf("detached HEAD: got %q, want \"\" (literal \"HEAD\" would mislead the plugin)", got)
	}
}

// TestCurrentBranch_NonExistentPath pins the non-git / missing
// directory → "" contract. Same path DetectDefaultBranch_NoRepo
// covers; kept here for symmetry so the two helpers' contract
// both directions are covered by matching-shape tests.
func TestCurrentBranch_NonExistentPath(t *testing.T) {
	got := CurrentBranch(context.Background(), "/tmp/this-path-does-not-exist")
	if got != "" {
		t.Errorf("non-existent path: got %q, want \"\"", got)
	}
}

// TestCurrentBranch_EmptyWorkspace pins the empty-workspace
// short-circuit — no git invocation should fire (proc.New with
// empty -C arg could otherwise confuse git's argument parser).
func TestCurrentBranch_EmptyWorkspace(t *testing.T) {
	got := CurrentBranch(context.Background(), "")
	if got != "" {
		t.Errorf("empty workspace: got %q, want \"\"", got)
	}
}
