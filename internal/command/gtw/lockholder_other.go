//go:build !linux && !darwin && !windows

package gtw

// lockHasLiveHolder reports whether a live process currently has the
// lock file open.
//
// There is no portable way to list another process' open files, and
// the platforms nightme ships for have no implementation here — so
// the answer is "held", and the runner never removes a lock it
// cannot prove is abandoned. The failure then names the lock file and
// how to clear it by hand.
func lockHasLiveHolder(string) bool { return true }
