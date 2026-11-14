package gtw

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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

// gitLockConflictPrefix is the English wording git uses when it
// cannot create a lock file:
//
//	fatal: Unable to create '<path>': File exists.
//
// git prefixes the message with `fatal:` or `error:` depending on the
// command, so the match is on the message body. The path comes from
// git rather than from the working directory: git names the exact
// file it could not create, which is correct for the primary checkout
// and for linked worktrees alike, and needs no extra git invocation
// to resolve.
const gitLockConflictPrefix = "Unable to create '"

// gitLockConflictSuffix separates the quoted path from git's
// (localized) description of the error.
const gitLockConflictSuffix = "': "

// gitRetryFunc re-runs a failed git invocation. The recovery takes
// it as a parameter so the retry behaviour can be exercised without
// spawning a real git.
type gitRetryFunc func(ctx context.Context, dir string, args ...string) (string, string, error)

// parseGitLockConflict extracts the lock path from git's stderr.
// Returns ("", false) when the failure is not a lock conflict.
//
// The English wording is matched first. The path may contain an
// apostrophe (git does not escape it), so the path runs to the last
// "': " rather than to the first quote.
//
// When git runs in a non-English locale the wording is translated and
// the English match fails. The path is still quoted and still names a
// ".lock" file, so the fallback reads it out of the message as well.
func parseGitLockConflict(stderr string) (string, bool) {
	if path, ok := parseEnglishLockConflict(stderr); ok {
		return path, true
	}
	return parseLocalizedLockConflict(stderr)
}

// parseEnglishLockConflict reads the lock path out of git's English
// "Unable to create '<path>': <error>" message.
func parseEnglishLockConflict(stderr string) (string, bool) {
	i := strings.Index(stderr, gitLockConflictPrefix)
	if i < 0 {
		return "", false
	}
	rest := stderr[i+len(gitLockConflictPrefix):]
	j := strings.LastIndex(rest, gitLockConflictSuffix)
	if j < 0 {
		return "", false
	}
	return rest[:j], true
}

// parseLocalizedLockConflict reads the lock path out of a translated
// "Unable to create" message. Git quotes the path and always names a
// ".lock" file, so the path is what sits between the first quote and
// the last ".lock'".
func parseLocalizedLockConflict(stderr string) (string, bool) {
	open := strings.Index(stderr, "'")
	if open < 0 {
		return "", false
	}
	end := strings.LastIndex(stderr, ".lock'")
	if end < 0 || end < open {
		return "", false
	}
	return stderr[open+1 : end+len(".lock")], true
}

// isIndexLock reports whether path is the index lock of a git
// directory. Only the index lock is ever removed: the other lock
// files git takes (a ref lock, packed-refs.lock) are held by network
// operations that can run for minutes, and removing one of those
// breaks the ref update it belongs to.
func isIndexLock(path string) bool {
	return filepath.Base(path) == "index.lock"
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
//   - the lock is older and nothing holds it open — its owner was
//     killed; remove it, log one warning, retry once.
//
// Age alone does not prove the owner is dead: git holds the index
// lock across the whole worktree update of a checkout, a reset or a
// rebase, and the lock's mtime stays at its creation time, so a live
// lock can be minutes old. An open lock is the signal that its owner
// is alive — git keeps the lock file open until it commits or rolls
// the lock back.
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
	if !isIndexLock(lockPath) {
		// Only the index lock is ever removed. The other lock files
		// git takes (a ref lock, packed-refs.lock) belong to network
		// operations that can run for minutes.
		return stdout, stderr, newGitLockError(err, stderr, lockPath, age)
	}
	if lockHasLiveHolder(lockPath) {
		// A live git still owns the lock. Unlinking it would let two
		// processes write the index at once.
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

// indexLockPath returns the path of the repository's index lock when
// it is there, or "" when it is not.
func indexLockPath(gitDir string) string {
	path := filepath.Join(gitDir, "index.lock")
	if _, err := os.Stat(path); err == nil {
		return path
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
