package gtw

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// staleGitLockAge is how old an index lock has to be before the git
// runner treats it as abandoned by a killed git process.
//
// Git holds the index lock only while it rewrites the index file —
// the expensive part of `git add` (hashing the working tree) happens
// before the lock is taken — so a live lock normally lives for
// milliseconds to a couple of seconds. Thirty seconds is far above
// any realistic hold time, which keeps the risk of removing a live
// lock negligible, while still recovering quickly.
//
// The bar is deliberately high: removing a lock that a live git
// still owns lets two processes write the index at once, and a
// corrupted index is a worse outcome than the failure this prevents.
const staleGitLockAge = 30 * time.Second

// gitLockConflictRe matches the index-lock conflict git reports:
//
//	fatal: Unable to create '<path>': File exists.
//
// git prefixes the message with `fatal:` or `error:` depending on
// the command, so the match is on the message body. The path comes
// from git rather than from the working directory: git names the
// exact file it could not create, which is correct for the primary
// checkout and for linked worktrees alike, and needs no extra git
// invocation to resolve.
var gitLockConflictRe = regexp.MustCompile(`Unable to create '([^']+)': File exists\.`)

// gitRetryFunc re-runs a failed git invocation. The recovery takes
// it as a parameter so the retry behaviour can be exercised without
// spawning a real git.
type gitRetryFunc func(ctx context.Context, dir string, args ...string) (string, string, error)

// parseGitLockConflict extracts the index-lock path from git's
// stderr. Returns ("", false) when the failure is not a lock
// conflict.
func parseGitLockConflict(stderr string) (string, bool) {
	m := gitLockConflictRe.FindStringSubmatch(stderr)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// recoverGitLockConflict handles a git failure that names an index
// lock as the obstacle.
//
// The recovery never removes a lock before git has reported it, and
// it retries the command at most once:
//
//   - the lock is gone — the holder released it between our failure
//     and the check; retry once, remove nothing;
//   - the lock is younger than staleGitLockAge — a live git may
//     still own it; leave it alone and report the conflict with the
//     manual remedy;
//   - the lock is older — its owner was killed; remove it, log one
//     warning, retry once.
//
// A retry that fails is reported as-is. The command never runs a
// third time: retrying in a loop turns one failure into a hang.
func recoverGitLockConflict(ctx context.Context, dir string, args []string,
	stdout, stderr string, err error, lockPath string, retry gitRetryFunc,
) (string, string, error) {
	info, statErr := os.Stat(lockPath)
	if statErr != nil {
		if !os.IsNotExist(statErr) {
			return stdout, stderr, err
		}
		// The holder released the lock between our failure and this
		// check — retry once without removing anything.
		return retryGitOnce(ctx, dir, args, 0, retry)
	}
	age := time.Since(info.ModTime())
	if age <= staleGitLockAge {
		// Not stale: a live git may still own this lock.
		return stdout, stderr, newGitLockError(err, stderr, lockPath, age)
	}
	if rmErr := os.Remove(lockPath); rmErr != nil && !os.IsNotExist(rmErr) {
		return stdout, stderr, gitErr(fmt.Sprintf(
			"stale git index lock %s could not be removed: %v", lockPath, rmErr), err, stderr)
	}
	slog.Default().Warn("gtw: removed stale git index lock",
		"lock", lockPath, "age_seconds", int(age.Seconds()))
	return retryGitOnce(ctx, dir, args, age, retry)
}

// retryGitOnce re-runs the git command after the lock conflict was
// cleared. It is the last attempt: a failure here is reported to the
// caller, with the lock's path and age when git still names a lock.
func retryGitOnce(ctx context.Context, dir string, args []string,
	age time.Duration, retry gitRetryFunc,
) (string, string, error) {
	out, serr, rerr := retry(ctx, dir, args...)
	if rerr == nil {
		return out, serr, nil
	}
	if path, ok := parseGitLockConflict(serr); ok {
		return out, serr, newGitLockError(rerr, serr, path, age)
	}
	return out, serr, rerr
}

// gitLockError reports an index-lock conflict the runner could not
// clear. It carries git's own stderr, the lock's path, its age and
// the manual remedy, so the user learns which file is in the way
// instead of seeing a bare "exit status 128".
type gitLockError struct {
	// Err is the underlying exec failure ("exit status 128").
	Err error
	// Stderr is git's own diagnostic.
	Stderr string
	// Path is the index lock git could not create.
	Path string
	// Age is how long the lock has been on disk. Zero when the
	// lock was already gone when we looked.
	Age time.Duration
	// Rebase reports an unfinished rebase in the same git
	// directory — the lock then survives `git rebase --abort`.
	Rebase bool
}

func newGitLockError(err error, stderr, path string, age time.Duration) *gitLockError {
	return &gitLockError{
		Err:    err,
		Stderr: stderr,
		Path:   path,
		Age:    age,
		Rebase: unfinishedRebase(filepath.Dir(path)) != "",
	}
}

func (e *gitLockError) Error() string {
	var b strings.Builder
	b.WriteString("git index lock conflict: ")
	b.WriteString(e.Path)
	if e.Age > 0 {
		fmt.Fprintf(&b, " (%s old)", e.Age.Round(time.Second))
	}
	b.WriteString("\n")
	if e.Rebase {
		b.WriteString("unfinished rebase in this repository — run `git rebase --abort` to clear it before retrying")
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "if no git process is running, remove it manually: rm '%s'", e.Path)
	if s := strings.TrimSpace(e.Stderr); s != "" {
		b.WriteString("\ngit stderr: ")
		b.WriteString(s)
	}
	return b.String()
}

func (e *gitLockError) Unwrap() error { return e.Err }

// unfinishedRebase returns the name of the rebase state directory in
// gitDir ("rebase-merge" or "rebase-apply"), or "" when no rebase is
// in progress.
//
// A `git pull --rebase` that was killed leaves one of those
// directories behind. Clearing the index lock alone does not make
// the repository usable: the next rebase refuses to run.
func unfinishedRebase(gitDir string) string {
	for _, name := range []string{"rebase-merge", "rebase-apply"} {
		if _, err := os.Stat(filepath.Join(gitDir, name)); err == nil {
			return name
		}
	}
	return ""
}

// gitErr renders a failed git invocation: the caller's context, the
// runner's error and git's own stderr.
//
// The stderr is appended only when the error does not already carry
// it — a lock-conflict error embeds git's stderr next to the lock's
// path and age, and repeating it would print git's words twice.
func gitErr(op string, err error, stderr string) error {
	if s := strings.TrimSpace(stderr); s != "" && !strings.Contains(err.Error(), s) {
		op += " (stderr: " + s + ")"
	}
	return fmt.Errorf("%s: %w", op, err)
}
