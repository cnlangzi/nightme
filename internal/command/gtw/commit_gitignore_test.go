package gtw

import (
	"context"
	"strings"
	"testing"
)

// TestCommitGitignore_NoopWhenClean verifies that on a clean
// .gitignore, CommitGitignoreIfDirty returns nil without issuing
// `git add` or `git commit`. Real-git idempotency.
func TestCommitGitignore_NoopWhenClean(t *testing.T) {
	git := &recordingGit{}
	if err := CommitGitignoreIfDirty(context.Background(), "/wt", git); err != nil {
		t.Fatalf("CommitGitignoreIfDirty: %v", err)
	}
	for _, args := range git.calls {
		for _, a := range args {
			if a == "add" || a == "commit" {
				t.Errorf("unexpected %q in calls: %v", a, args)
			}
		}
	}
}

// TestCommitGitignore_StagesAndCommits verifies the full happy
// path: status shows dirty → add .gitignore → commit with the
// tool identity forced.
func TestCommitGitignore_StagesAndCommits(t *testing.T) {
	git := &recordingGit{statusResp: "?? .gitignore\n"}

	if err := CommitGitignoreIfDirty(context.Background(), "/wt", git); err != nil {
		t.Fatalf("CommitGitignoreIfDirty: %v", err)
	}

	// First call: status. Second: add .gitignore.
	if len(git.calls) < 2 {
		t.Fatalf("expected ≥2 calls (status + add), got %d: %v", len(git.calls), git.calls)
	}

	// Find the add call and verify it includes .gitignore.
	var sawAdd bool
	for _, args := range git.calls {
		if len(args) >= 3 && args[0] == "add" {
			sawAdd = true
			if args[len(args)-1] != ".gitignore" {
				t.Errorf("add target = %q, want .gitignore (in args %v)", args[len(args)-1], args)
			}
		}
	}
	if !sawAdd {
		t.Errorf("no `git add` issued; calls=%v", git.calls)
	}

	// Find the commit call and verify it forces the tool identity
	// via inline -c flags (so it works without global git config).
	// args layout: ["-c", "user.name=...", "-c", "user.email=...",
	// "commit", "-m", "msg", "--", ".gitignore"].
	var sawCommit bool
	for _, args := range git.calls {
		if !containsArg(args, "commit") {
			continue
		}
		sawCommit = true
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "user.name="+gitToolIdentityName) {
			t.Errorf("commit missing user.name override: %v", args)
		}
		if !strings.Contains(joined, "user.email="+gitToolIdentityEmail) {
			t.Errorf("commit missing user.email override: %v", args)
		}
		if !strings.Contains(joined, "ignore .nightme") {
			t.Errorf("commit message should mention .nightme ignore: %v", args)
		}
	}
	if !sawCommit {
		t.Errorf("no `git commit` issued; calls=%v", git.calls)
	}
}

// recordingGit is a tiny fakeGit that returns statusResp for
// `status` and ignores everything else. Records every call so
// tests can assert what CommitGitignoreIfDirty issued.
type recordingGit struct {
	statusResp string
	calls      [][]string
}

func (r *recordingGit) Run(_ context.Context, _ string, args ...string) (string, string, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	if len(args) >= 1 && args[0] == "status" {
		return r.statusResp, "", nil
	}
	return "", "", nil
}

// containsArg reports whether any element of args equals target.
func containsArg(args []string, target string) bool {
	for _, a := range args {
		if a == target {
			return true
		}
	}
	return false
}