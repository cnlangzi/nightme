package gtw

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gitLockConflictStderr renders the stderr git emits when it cannot
// take the index lock.
func gitLockConflictStderr(lockPath string) string {
	return "fatal: Unable to create '" + lockPath + "': File exists.\n" +
		"\n" +
		"Another git process seems to be running in this repository, e.g.\n" +
		"an editor opened by 'git commit'. Please make sure all processes\n" +
		"are terminated then try again."
}

// writeLock creates an index lock whose mtime is age old.
func writeLock(t *testing.T, path string, age time.Duration) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write lock: %v", err)
	}
	stamp := time.Now().Add(-age)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatalf("chtimes lock: %v", err)
	}
}

// countingRetry returns a retry hook that records how many times it
// ran and answers with the given response.
func countingRetry(calls *int, stdout, stderr string, err error) gitRetryFunc {
	return func(_ context.Context, _ string, _ ...string) (string, string, error) {
		*calls++
		return stdout, stderr, err
	}
}

func TestParseGitLockConflict(t *testing.T) {
	tests := []struct {
		name     string
		stderr   string
		wantPath string
		wantOK   bool
	}{
		{
			name:     "fatal prefix",
			stderr:   "fatal: Unable to create '/repo/.git/index.lock': File exists.",
			wantPath: "/repo/.git/index.lock",
			wantOK:   true,
		},
		{
			name:     "error prefix",
			stderr:   "error: Unable to create 'C:/repo/.git/index.lock': File exists.",
			wantPath: "C:/repo/.git/index.lock",
			wantOK:   true,
		},
		{
			name: "multi-line stderr",
			stderr: "fatal: Unable to create '/repo/.git/index.lock': File exists.\n" +
				"\nAnother git process seems to be running in this repository.",
			wantPath: "/repo/.git/index.lock",
			wantOK:   true,
		},
		{
			// git does not escape an apostrophe in the path.
			name:     "path with an apostrophe",
			stderr:   "fatal: Unable to create '/tmp/apos's repo/.git/index.lock': File exists.",
			wantPath: "/tmp/apos's repo/.git/index.lock",
			wantOK:   true,
		},
		{
			// git ships translations, so the wording is not stable.
			name:     "localized message",
			stderr:   "fatal: 无法创建 '/repo/.git/index.lock'：文件已存在。",
			wantPath: "/repo/.git/index.lock",
			wantOK:   true,
		},
		{
			name:   "other failure",
			stderr: "fatal: not a git repository: '/nope'",
			wantOK: false,
		},
		{
			name:   "empty stderr",
			stderr: "",
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, ok := parseGitLockConflict(tt.stderr)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if path != tt.wantPath {
				t.Errorf("path = %q, want %q", path, tt.wantPath)
			}
		})
	}
}

// TestRecoverGitLockConflict_StaleLockRemovedAndRetriedOnce pins the
// happy path of the recovery: a stale lock is removed, the command
// is retried exactly once, and the retry's output is what the caller
// gets.
func TestRecoverGitLockConflict_StaleLockRemovedAndRetriedOnce(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "index.lock")
	writeLock(t, lock, 31*time.Second)

	calls := 0
	retry := countingRetry(&calls, "staged", "", nil)

	out, _, err := recoverGitLockConflict(context.Background(), dir,
		[]string{"add"}, "", gitLockConflictStderr(lock),
		errors.New("exit status 128"), lock, retry)
	if err != nil {
		t.Fatalf("recoverGitLockConflict: %v", err)
	}
	if out != "staged" {
		t.Errorf("stdout = %q, want %q", out, "staged")
	}
	if calls != 1 {
		t.Errorf("retry calls = %d, want 1", calls)
	}
	if _, statErr := os.Stat(lock); !os.IsNotExist(statErr) {
		t.Errorf("stale lock %s survived the recovery (err=%v)", lock, statErr)
	}
}

// TestRecoverGitLockConflict_YoungLockLeftInPlace pins the other
// half of the contract: a lock that may still be owned by a live git
// is never removed, and the failure names the file, its age and the
// manual remedy.
func TestRecoverGitLockConflict_YoungLockLeftInPlace(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "index.lock")
	writeLock(t, lock, 5*time.Second)

	calls := 0
	retry := countingRetry(&calls, "", "", nil)

	_, _, err := recoverGitLockConflict(context.Background(), dir,
		[]string{"add"}, "", gitLockConflictStderr(lock),
		errors.New("exit status 128"), lock, retry)
	if err == nil {
		t.Fatal("recoverGitLockConflict: expected an error, got nil")
	}
	if calls != 0 {
		t.Errorf("retry calls = %d, want 0 (a young lock is not removed)", calls)
	}
	if _, statErr := os.Stat(lock); statErr != nil {
		t.Errorf("young lock %s was removed: %v", lock, statErr)
	}
	if !strings.Contains(err.Error(), lock) {
		t.Errorf("error should name the lock path %q: %v", lock, err)
	}
	if !strings.Contains(err.Error(), "Unable to create '"+lock+"': File exists.") {
		t.Errorf("error should carry git's own stderr: %v", err)
	}
	if !strings.Contains(err.Error(), "rm '"+lock+"'") {
		t.Errorf("error should say how to clear the lock by hand: %v", err)
	}
}

// TestRecoverGitLockConflict_RetryFailureIsNotRetriedAgain pins the
// single-retry ceiling: a retry that fails is reported as-is and the
// command never runs a third time.
func TestRecoverGitLockConflict_RetryFailureIsNotRetriedAgain(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "index.lock")
	writeLock(t, lock, 31*time.Second)

	calls := 0
	retry := countingRetry(&calls, "",
		"error: pathspec 'missing' did not match any file(s) known to git",
		errors.New("exit status 128"))

	_, stderr, err := recoverGitLockConflict(context.Background(), dir,
		[]string{"add"}, "", gitLockConflictStderr(lock),
		errors.New("exit status 128"), lock, retry)
	if err == nil {
		t.Fatal("recoverGitLockConflict: expected an error, got nil")
	}
	if calls != 1 {
		t.Errorf("retry calls = %d, want 1 (never a third attempt)", calls)
	}
	if !strings.Contains(stderr, "did not match") {
		t.Errorf("the retry's stderr should be reported: %q", stderr)
	}
}

// TestRecoverGitLockConflict_LiveHolderKeepsTheLock pins the guard
// that keeps the recovery from stealing a lock a live git still owns.
//
// Git holds the lock file open from the moment it creates it until it
// commits or rolls the lock back, and it holds it across the whole
// worktree update of a checkout — so a lock can be minutes old while
// its owner is alive. Unlinking it would let two processes write the
// index at once.
func TestRecoverGitLockConflict_LiveHolderKeepsTheLock(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "index.lock")
	writeLock(t, lock, 31*time.Second)

	// Hold the lock open, the way a live git does.
	handle, err := os.OpenFile(lock, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open lock: %v", err)
	}
	defer handle.Close()

	calls := 0
	retry := countingRetry(&calls, "", "", nil)

	_, _, err = recoverGitLockConflict(context.Background(), dir,
		[]string{"add"}, "", gitLockConflictStderr(lock),
		errors.New("exit status 128"), lock, retry)
	if err == nil {
		t.Fatal("expected an error while the lock is held, got nil")
	}
	if calls != 0 {
		t.Errorf("retry calls = %d, want 0 (a held lock is not removed)", calls)
	}
	if _, statErr := os.Stat(lock); statErr != nil {
		t.Errorf("the held lock was removed: %v", statErr)
	}
}

// TestRecoverGitLockConflict_RefLockIsNotRemoved pins that only the
// index lock is ever removed. The other lock files git takes (a ref
// lock, packed-refs.lock) belong to network operations that can run
// for minutes, and removing one of those breaks the ref update.
func TestRecoverGitLockConflict_RefLockIsNotRemoved(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "refs", "heads", "main.lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeLock(t, lock, 31*time.Second)

	calls := 0
	retry := countingRetry(&calls, "", "", nil)

	_, _, err := recoverGitLockConflict(context.Background(), dir,
		[]string{"push"}, "", gitLockConflictStderr(lock),
		errors.New("exit status 128"), lock, retry)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if calls != 0 {
		t.Errorf("retry calls = %d, want 0 (a ref lock is not removed)", calls)
	}
	if _, statErr := os.Stat(lock); statErr != nil {
		t.Errorf("the ref lock was removed: %v", statErr)
	}
}

// TestRecoverGitLockConflict_LockGoneRetriesWithoutRemoval covers the
// transient case: the holder released the lock between our failure
// and our check. Nothing is removed and the command is retried once.
func TestRecoverGitLockConflict_LockGoneRetriesWithoutRemoval(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "index.lock")

	calls := 0
	retry := countingRetry(&calls, "done", "", nil)

	out, _, err := recoverGitLockConflict(context.Background(), dir,
		[]string{"add"}, "", gitLockConflictStderr(lock),
		errors.New("exit status 128"), lock, retry)
	if err != nil {
		t.Fatalf("recoverGitLockConflict: %v", err)
	}
	if out != "done" {
		t.Errorf("stdout = %q, want %q", out, "done")
	}
	if calls != 1 {
		t.Errorf("retry calls = %d, want 1", calls)
	}
	if _, statErr := os.Stat(lock); !os.IsNotExist(statErr) {
		t.Errorf("lock %s appeared out of nowhere (err=%v)", lock, statErr)
	}
}

// TestRecoverGitLockConflict_RebaseHintOnlyWhenRebasePresent pins the
// rebase hint: it names `git rebase --abort` only when the git
// directory really holds an unfinished rebase.
func TestRecoverGitLockConflict_RebaseHintOnlyWhenRebasePresent(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "index.lock")
	writeLock(t, lock, 5*time.Second)

	calls := 0
	retry := countingRetry(&calls, "", "", nil)

	_, _, err := recoverGitLockConflict(context.Background(), dir,
		[]string{"add"}, "", gitLockConflictStderr(lock),
		errors.New("exit status 128"), lock, retry)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if strings.Contains(err.Error(), "git rebase --abort") {
		t.Errorf("error mentions a rebase that is not in progress: %v", err)
	}

	for _, name := range []string{"rebase-merge", "rebase-apply"} {
		rebaseDir := filepath.Join(dir, name)
		if err := os.MkdirAll(rebaseDir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
		_, _, err := recoverGitLockConflict(context.Background(), dir,
			[]string{"add"}, "", gitLockConflictStderr(lock),
			errors.New("exit status 128"), lock, retry)
		if err == nil {
			t.Fatalf("%s: expected an error, got nil", name)
		}
		if !strings.Contains(err.Error(), "git rebase --abort") {
			t.Errorf("%s: error should name `git rebase --abort`: %v", name, err)
		}
		if err := os.RemoveAll(rebaseDir); err != nil {
			t.Fatalf("rm %s: %v", name, err)
		}
	}
}

// TestExecGitRunner_StaleLockSelfHeals is the end-to-end proof
// against real git: a stale index lock no longer breaks the next git
// operation.
func TestExecGitRunner_StaleLockSelfHeals(t *testing.T) {
	repo := initTempRepo(t)
	lock := filepath.Join(repo, ".git", "index.lock")
	writeLock(t, lock, 31*time.Second)

	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write new.txt: %v", err)
	}
	if _, stderr, err := (ExecGitRunner{}).Run(context.Background(), repo, "add", "new.txt"); err != nil {
		t.Fatalf("Run: %v (stderr=%s)", err, stderr)
	}
	if _, statErr := os.Stat(lock); !os.IsNotExist(statErr) {
		t.Errorf("stale lock %s survived the recovery (err=%v)", lock, statErr)
	}
	out, _, err := (ExecGitRunner{}).Run(context.Background(), repo, "status", "--porcelain")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, "new.txt") {
		t.Errorf("new.txt was not staged; status = %q", out)
	}
}

// TestExecGitRunner_YoungLockBlocks is the counterpart: a lock that
// may still be live is left alone, and the user learns which file is
// in the way.
func TestExecGitRunner_YoungLockBlocks(t *testing.T) {
	repo := initTempRepo(t)
	lock := filepath.Join(repo, ".git", "index.lock")
	writeLock(t, lock, 5*time.Second)

	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write new.txt: %v", err)
	}
	_, stderr, err := (ExecGitRunner{}).Run(context.Background(), repo, "add", "new.txt")
	if err == nil {
		t.Fatal("Run: expected an error while the lock is held, got nil")
	}
	if !strings.Contains(err.Error(), "Unable to create") {
		t.Errorf("error should carry git's stderr: %v (stderr=%s)", err, stderr)
	}
	if !strings.Contains(err.Error(), "index.lock") {
		t.Errorf("error should name the lock file: %v", err)
	}
	if _, statErr := os.Stat(lock); statErr != nil {
		t.Errorf("the young lock was removed: %v", statErr)
	}
}

// TestExecGitRunner_OtherFailureLeavesLocksAlone pins R5: a lock is
// never removed unless git reported that exact file as the obstacle.
func TestExecGitRunner_OtherFailureLeavesLocksAlone(t *testing.T) {
	repo := initTempRepo(t)
	lock := filepath.Join(repo, ".git", "index.lock")
	writeLock(t, lock, 31*time.Second)

	// `git rev-parse --verify` on a ref that does not exist fails
	// for a reason that has nothing to do with the lock — git never
	// looks at the index.
	_, _, err := (ExecGitRunner{}).Run(context.Background(), repo,
		"rev-parse", "--verify", "refs/heads/no-such-branch")
	if err == nil {
		t.Fatal("Run: expected an error, got nil")
	}
	if _, statErr := os.Stat(lock); statErr != nil {
		t.Errorf("the lock was removed although git never named it: %v", statErr)
	}
}

// TestExecGitRunner_RetryRunsGitExactlyTwice pins the retry ceiling
// against real git: the conflict costs one run, the retry costs one
// more, and there is no third.
//
// GIT_TRACE points git at a file, so every invocation appends one
// "built-in:" line to it — that is the run counter.
func TestExecGitRunner_RetryRunsGitExactlyTwice(t *testing.T) {
	repo := initTempRepo(t)
	lock := filepath.Join(repo, ".git", "index.lock")
	writeLock(t, lock, 31*time.Second)

	trace := filepath.Join(t.TempDir(), "git-trace")
	t.Setenv("GIT_TRACE", trace)

	// The lock conflict fires first; the retry (lock gone) fails on
	// the unknown branch instead.
	_, stderr, err := (ExecGitRunner{}).Run(context.Background(), repo,
		"checkout", "no-such-branch")
	if err == nil {
		t.Fatal("Run: expected an error, got nil")
	}
	if !strings.Contains(stderr, "did not match") {
		t.Errorf("the retry's stderr should be reported: %q", stderr)
	}
	if n := countGitRuns(t, trace); n != 2 {
		t.Errorf("git ran %d times, want 2 (one conflict + one retry)", n)
	}
}

// countGitRuns counts the git invocations recorded in a GIT_TRACE
// file.
func countGitRuns(t *testing.T, tracePath string) int {
	t.Helper()
	raw, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("read git trace: %v", err)
	}
	n := 0
	for line := range strings.SplitSeq(string(raw), "\n") {
		if strings.Contains(line, "built-in:") {
			n++
		}
	}
	return n
}

// TestRefreshDefaultBranch_RefusesUnfinishedRebase pins R8: the
// refresh refuses to run while a rebase is unfinished, names the
// remedy, and leaves the repository untouched.
func TestRefreshDefaultBranch_RefusesUnfinishedRebase(t *testing.T) {
	for _, name := range []string{"rebase-merge", "rebase-apply"} {
		t.Run(name, func(t *testing.T) {
			repo := initTempRepo(t)
			addLocalRemote(t, repo)

			rebaseDir := filepath.Join(repo, ".git", name)
			if err := os.MkdirAll(rebaseDir, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", name, err)
			}

			// A killed `git pull --rebase` leaves the index lock
			// behind too, and `git rebase --abort` fails while it is
			// there — so the message has to name it.
			lock := filepath.Join(repo, ".git", "index.lock")
			if err := os.WriteFile(lock, nil, 0o600); err != nil {
				t.Fatalf("write index.lock: %v", err)
			}

			_, _, err := RefreshDefaultBranch(context.Background(), repo, HandlerDeps{
				Git: ExecGitRunner{},
			})
			if err == nil {
				t.Fatalf("RefreshDefaultBranch: want an error while %s exists, got nil", name)
			}
			if !strings.Contains(err.Error(), "git rebase --abort") {
				t.Errorf("error should name `git rebase --abort`: %v", err)
			}
			if !strings.Contains(err.Error(), lock) {
				t.Errorf("error should name the index lock %q: %v", lock, err)
			}
			if _, statErr := os.Stat(rebaseDir); statErr != nil {
				t.Errorf("the rebase state was removed: %v", statErr)
			}
			if _, statErr := os.Stat(lock); statErr != nil {
				t.Errorf("the index lock was removed: %v", statErr)
			}
		})
	}
}

// TestCollectReadiness_UsesNoOptionalLocks pins the argv of the chat
// footer path: it must not take the index lock.
func TestCollectReadiness_UsesNoOptionalLocks(t *testing.T) {
	g := &probeGit{}
	g.on(statusCmd, "## main\n", "", nil)

	snap, err := CollectReadiness(context.Background(), "/w", g)
	if err != nil {
		t.Fatalf("CollectReadiness: %v", err)
	}
	if snap == nil {
		t.Fatal("CollectReadiness: expected a snapshot")
	}
	if len(g.calls) != 1 {
		t.Fatalf("git calls = %d, want 1: %v", len(g.calls), g.calls)
	}
	if g.calls[0][0] != "--no-optional-locks" {
		t.Errorf("argv = %v, want --no-optional-locks first", g.calls[0])
	}
}
