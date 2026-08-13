package gtw

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestOriginBranchSHA_RecognisesFirstPushStderr enumerates the
// distinct stderr shapes `git rev-parse --verify origin/<branch>`
// emits when the remote-tracking ref does not exist. Each shape
// must be treated as "first push" (originBefore == "", nil err)
// rather than surfaced to the user as a "❌ read origin/..." error.
//
// The canonical case the user hit in production:
//
//	fatal: Needed a single revision
//
// is what `git rev-parse --verify` emits on a missing ref. The
// older / non-verify shapes are still recognised so the code does
// not regress for git versions or invocation styles that the test
// suite mocks.
//
// See push.go originBranchSHA for the full enumeration rationale.
func TestOriginBranchSHA_RecognisesFirstPushStderr(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
	}{
		{"verify-only-needed-a-single-revision", "fatal: Needed a single revision"},
		{"non-verify-unknown-revision", "fatal: ambiguous argument 'origin/wt-fresh-branch': unknown revision or path not in the working tree.\nUse '--' to separate paths from revisions"},
		{"non-verify-with-target", "fatal: unknown revision or path not in the working tree: 'origin/wt-fresh-branch'"},
		{"not-a-valid-ref", "fatal: Not a valid ref"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			git := newPushGit()
			git.onArgs([]string{"rev-parse", "--verify", "origin/wt-fresh-branch"},
				"", tc.stderr, errors.New("exit status 128"))

			sha, err := originBranchSHA(context.Background(), "/tmp", "wt-fresh-branch", HandlerDeps{Git: git})
			if err != nil {
				t.Fatalf("originBranchSHA: unexpected error for first-push stderr %q: %v", tc.stderr, err)
			}
			if sha != "" {
				t.Errorf("originBranchSHA: sha = %q, want empty string for first-push", sha)
			}
		})
	}
}

// TestOriginBranchSHA_PropagatesUnrelatedErrors ensures the helper
// does NOT swallow non-"ref not found" errors. A broken git
// invocation (e.g. permission denied on the worktree path) must
// still surface so the caller can distinguish "first push" from
// "git is broken".
func TestOriginBranchSHA_PropagatesUnrelatedErrors(t *testing.T) {
	git := newPushGit()
	git.onArgs([]string{"rev-parse", "--verify", "origin/wt-fresh-branch"},
		"", "fatal: unable to read current working directory: No such file or directory", errors.New("exit status 128"))

	_, err := originBranchSHA(context.Background(), "/tmp", "wt-fresh-branch", HandlerDeps{Git: git})
	if err == nil {
		t.Fatal("originBranchSHA: expected error for non-ref-not-found stderr, got nil")
	}
	if !strings.Contains(err.Error(), "rev-parse origin/wt-fresh-branch") {
		t.Errorf("originBranchSHA: error = %q, want it to mention the rev-parse invocation", err.Error())
	}
}