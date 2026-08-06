package gtw

import (
	"context"
	"strings"
	"testing"
)

// fakeGit is a GitRunner that returns canned stdout/stderr per argv.
// Same shape as the one in gtw_test.go (kept here separately so
// git_status tests have their own self-contained fixture data
// without depending on the worktree tests' table).
type fakeGitStatus struct {
	responses map[string]fakeGitStatusResp
}

type fakeGitStatusResp struct {
	stdout string
	stderr string
	err    error
}

func (f *fakeGitStatus) Run(_ context.Context, _ string, args ...string) (string, string, error) {
	key := strings.Join(args, " ")
	if r, ok := f.responses[key]; ok {
		return r.stdout, r.stderr, r.err
	}
	return "", "", nil
}

func TestCollectStatus_CleanWorkingTree(t *testing.T) {
	g := &fakeGitStatus{responses: map[string]fakeGitStatusResp{
		"status --porcelain --branch --untracked-files=normal": {
			stdout: "## main...origin/main\n",
		},
	}}
	snap, err := CollectStatus(context.Background(), "/code/nightme", g)
	if err != nil {
		t.Fatalf("CollectStatus: %v", err)
	}
	if snap == nil {
		t.Fatal("expected non-nil snapshot on clean tree")
	}
	if snap.Branch != "main" {
		t.Errorf("Branch = %q, want main", snap.Branch)
	}
	if !snap.HasUpstream {
		t.Error("HasUpstream = false, want true")
	}
	if snap.Uncommitted != 0 || snap.Untracked != 0 || snap.AheadOfRemote != 0 {
		t.Errorf("clean tree should have all zero counts, got %+v", snap)
	}
}

func TestCollectStatus_DirtyAndAhead(t *testing.T) {
	g := &fakeGitStatus{responses: map[string]fakeGitStatusResp{
		"status --porcelain --branch --untracked-files=normal": {
			stdout: "## feat/x...origin/feat/x [ahead 3]\n" +
				" M internal/foo.go\n" +
				"A  internal/bar.go\n" +
				"?? scratch.txt\n" +
				"?? notes.md\n",
		},
	}}
	snap, err := CollectStatus(context.Background(), "/code/nightme", g)
	if err != nil {
		t.Fatalf("CollectStatus: %v", err)
	}
	if snap == nil {
		t.Fatal("expected non-nil snapshot")
	}
	if snap.Branch != "feat/x" {
		t.Errorf("Branch = %q, want feat/x", snap.Branch)
	}
	if !snap.HasUpstream {
		t.Error("HasUpstream = false, want true")
	}
	if snap.AheadOfRemote != 3 {
		t.Errorf("AheadOfRemote = %d, want 3", snap.AheadOfRemote)
	}
	if snap.Uncommitted != 2 {
		t.Errorf("Uncommitted = %d, want 2 (M + A)", snap.Uncommitted)
	}
	if snap.Untracked != 2 {
		t.Errorf("Untracked = %d, want 2 (??)", snap.Untracked)
	}
}

func TestCollectStatus_NoUpstream_OmitsUnpushedSegment(t *testing.T) {
	// "## main" — local branch, no upstream. Footer should
	// omit "⇡ N" because HasUpstream is false.
	g := &fakeGitStatus{responses: map[string]fakeGitStatusResp{
		"status --porcelain --branch --untracked-files=normal": {
			stdout: "## main\n" +
				" M foo.go\n",
		},
	}}
	snap, err := CollectStatus(context.Background(), "/code", g)
	if err != nil {
		t.Fatalf("CollectStatus: %v", err)
	}
	if snap == nil {
		t.Fatal("expected non-nil snapshot")
	}
	if snap.Branch != "main" {
		t.Errorf("Branch = %q, want main", snap.Branch)
	}
	if snap.HasUpstream {
		t.Error("HasUpstream = true, want false (no upstream)")
	}
	if snap.AheadOfRemote != 0 {
		t.Errorf("AheadOfRemote = %d, want 0 (no upstream to compare)", snap.AheadOfRemote)
	}
	if snap.Uncommitted != 1 {
		t.Errorf("Uncommitted = %d, want 1", snap.Uncommitted)
	}
}

func TestCollectStatus_DetachedHead(t *testing.T) {
	// "## (HEAD detached at 1234abc)" — caller asked to render
	// the branch as "?" so users see "branch unknown" without
	// exposing the SHA. HasUpstream is always false for
	// detached HEAD.
	cases := []struct {
		name   string
		header string
	}{
		{"detached at commit", "## (HEAD detached at 1234abc)"},
		{"detached at remote ref", "## (HEAD detached at origin/main)"},
		{"initial commit / no branch", "## HEAD (no branch)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := &fakeGitStatus{responses: map[string]fakeGitStatusResp{
				"status --porcelain --branch --untracked-files=normal": {
					stdout: c.header + "\n M foo.go\n",
				},
			}}
			snap, err := CollectStatus(context.Background(), "/code", g)
			if err != nil {
				t.Fatalf("CollectStatus: %v", err)
			}
			if snap == nil {
				t.Fatal("expected non-nil snapshot")
			}
			if snap.Branch != "" {
				t.Errorf("Branch = %q, want \"\" (footer renders as ?)", snap.Branch)
			}
			if snap.HasUpstream {
				t.Error("HasUpstream = true, want false for detached HEAD")
			}
			if snap.Uncommitted != 1 {
				t.Errorf("Uncommitted = %d, want 1", snap.Uncommitted)
			}
		})
	}
}

func TestCollectStatus_NotInRepoReturnsNil(t *testing.T) {
	// Non-zero exit + stderr — git invocation failed (not a
	// repo). Footer should silently skip the segment, no error
	// surfaced.
	g := &fakeGitStatus{responses: map[string]fakeGitStatusResp{
		"status --porcelain --branch --untracked-files=normal": {
			stderr: "fatal: not a git repository",
			err:    &fakeExitErrForStatus{code: 128},
		},
	}}
	snap, err := CollectStatus(context.Background(), "/tmp/not-a-repo", g)
	if err != nil {
		t.Fatalf("CollectStatus: expected nil error, got %v", err)
	}
	if snap != nil {
		t.Errorf("expected nil snapshot on git failure, got %+v", snap)
	}
}

func TestCollectStatus_AheadAndBehind(t *testing.T) {
	// Diverged: ahead 3, behind 1 — we surface ahead only
	// (per F-48 spec). Behind is intentionally parsed away.
	g := &fakeGitStatus{responses: map[string]fakeGitStatusResp{
		"status --porcelain --branch --untracked-files=normal": {
			stdout: "## main...origin/main [ahead 3, behind 1]\n",
		},
	}}
	snap, err := CollectStatus(context.Background(), "/code", g)
	if err != nil {
		t.Fatalf("CollectStatus: %v", err)
	}
	if snap == nil {
		t.Fatal("expected non-nil snapshot")
	}
	if snap.AheadOfRemote != 3 {
		t.Errorf("AheadOfRemote = %d, want 3", snap.AheadOfRemote)
	}
	if !snap.HasUpstream {
		t.Error("HasUpstream = false, want true")
	}
}

func TestCollectStatus_ConflictsCountAsUncommitted(t *testing.T) {
	// UU / AA / DD (mid-merge / mid-rebase) — worktree IS
	// dirty; footer should count these as uncommitted so users
	// see the conflict state.
	g := &fakeGitStatus{responses: map[string]fakeGitStatusResp{
		"status --porcelain --branch --untracked-files=normal": {
			stdout: "## main...origin/main\n" +
				"UU foo.go\n" +
				"AA bar.go\n",
		},
	}}
	snap, err := CollectStatus(context.Background(), "/code", g)
	if err != nil {
		t.Fatalf("CollectStatus: %v", err)
	}
	if snap == nil {
		t.Fatal("expected non-nil snapshot")
	}
	if snap.Uncommitted != 2 {
		t.Errorf("Uncommitted = %d, want 2 (UU + AA)", snap.Uncommitted)
	}
}

func TestCollectStatus_IgnoredFilesNotCounted(t *testing.T) {
	// "!!" (ignored) lines are only emitted with --ignored,
	// which we don't pass. Defensive parser ignores them if
	// present.
	g := &fakeGitStatus{responses: map[string]fakeGitStatusResp{
		"status --porcelain --branch --untracked-files=normal": {
			stdout: "## main\n!! ignored.log\n M foo.go\n",
		},
	}}
	snap, err := CollectStatus(context.Background(), "/code", g)
	if err != nil {
		t.Fatalf("CollectStatus: %v", err)
	}
	if snap == nil {
		t.Fatal("expected non-nil snapshot")
	}
	if snap.Uncommitted != 1 {
		t.Errorf("Uncommitted = %d, want 1 (ignored lines skipped)", snap.Uncommitted)
	}
}

func TestCollectStatus_EmptyPorcelainOutputReturnsNil(t *testing.T) {
	// Defensive: empty output should not produce a snapshot
	// with all-zero fields (which would render as a footer
	// line with just "⎇ main").
	g := &fakeGitStatus{responses: map[string]fakeGitStatusResp{
		"status --porcelain --branch --untracked-files=normal": {
			stdout: "",
		},
	}}
	snap, err := CollectStatus(context.Background(), "/code", g)
	if err != nil {
		t.Fatalf("CollectStatus: %v", err)
	}
	if snap != nil {
		t.Errorf("expected nil snapshot on empty porcelain, got %+v", snap)
	}
}

// fakeExitErrForStatus mirrors the fakeExitErr in gtw_test.go —
// kept local to this file so git_status tests don't depend on
// the other file's test fixtures.
type fakeExitErrForStatus struct{ code int }

func (e *fakeExitErrForStatus) Error() string  { return "exit" }
func (e *fakeExitErrForStatus) ExitCode() int { return e.code }