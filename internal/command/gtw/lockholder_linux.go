//go:build linux

package gtw

import (
	"os"
	"path/filepath"
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
// Linux: walk /proc/<pid>/fd and compare the open descriptors with
// the lock. Descriptors of other users' processes are unreadable, so
// they are skipped — the git that owns the lock runs as the same
// user as nightme.
func lockHasLiveHolder(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, proc := range procs {
		if !proc.IsDir() {
			continue
		}
		fdDir := filepath.Join("/proc", proc.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			// os.Stat follows the symlink, so this is the stat of
			// the open file, not of the descriptor itself.
			st, err := os.Stat(filepath.Join(fdDir, fd.Name()))
			if err != nil {
				continue
			}
			if os.SameFile(st, info) {
				return true
			}
		}
	}
	return false
}
