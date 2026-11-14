//go:build darwin

package gtw

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
)

// lockHasLiveHolder reports whether a live process currently has the
// lock file open.
//
// Git holds the lock file open from the moment it creates it until it
// commits or rolls the lock back, so an open lock means a live git
// owns it — the file's age says nothing (a checkout keeps the lock
// open for the whole worktree update, and the lock's mtime stays at
// its creation time). A killed git closes its descriptor, leaving the
// file behind but no longer open.
//
// macOS has no /proc, so the open descriptors are listed with lsof.
// lsof walks every process, which costs a second or so — the check
// only runs on the rare path where a lock is about to be removed.
func lockHasLiveHolder(path string) bool {
	lsof, err := exec.LookPath("lsof")
	if err != nil {
		lsof = "/usr/sbin/lsof"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, lsof, "-t", "--", path).Output()
	if err == nil {
		return strings.TrimSpace(string(out)) != ""
	}
	// lsof exits 1 when it lists nothing. Anything else (the file is
	// gone, lsof is missing) leaves the answer unknown — treat that
	// as "held", so the lock is left alone.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && strings.TrimSpace(string(out)) == "" {
		return false
	}
	return true
}
