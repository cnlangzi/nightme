//go:build unix

// Regression coverage for the inherited-lock fd leak.
//
// The daemon adopts daemon.lock as fd 3 via ExtraFiles, which
// arrives with FD_CLOEXEC cleared. Before LockFromFile re-armed
// it, every subprocess the daemon exec'd (!cmd shell, gtw hooks,
// agent bridges) inherited the descriptor — and because flock is
// bound to the open file description rather than the process, the
// lock survived the daemon's own exit for as long as any
// descendant lived. `nightme restart` invoked from such a
// descendant (`!make restart`) therefore stopped the daemon
// successfully, then spun the full 15s in stopDaemon waiting for
// a lock it was itself holding, and never reached startDaemon.
package daemoncontrol

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// lockHelperEnv marks the re-exec'd test binary as the helper
// subprocess standing in for the daemon child.
const lockHelperEnv = "NIGHTME_LOCK_CLOEXEC_HELPER"

// grandchildMarker is the on-the-wire handshake between the
// helper subprocess and its parent test. The leading and trailing
// pipes matter: the helper writes via fmt.Printf, so when the
// parent runs with -test.v the framework's own log lines get
// interleaved with the helper's stdout — without delimiters, a
// stray "1234" from a test framework message would be misread as a
// pid. Cutting on the opening marker ensures only this single
// line is parsed.
const grandchildMarker = "<<<NIGHTME_HELPER_GRANDCHILD_PID=%d>>>"

func TestLockFromFileArmsCloseOnExec(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.lock")
	owner, err := TryLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()

	// unix.Dup yields a descriptor with FD_CLOEXEC cleared, which
	// is exactly the shape forkExec hands to the child for each
	// ExtraFiles entry.
	fd, err := unix.Dup(int(owner.File().Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != nil {
		t.Fatal(err)
	} else if flags&unix.FD_CLOEXEC != 0 {
		t.Fatal("precondition: duped fd already has FD_CLOEXEC, test proves nothing")
	}

	if _, err := LockFromFile(os.NewFile(uintptr(fd), "daemon.lock")); err != nil {
		t.Fatal(err)
	}
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Fatal("LockFromFile left FD_CLOEXEC clear; the lock will leak into every exec'd subprocess")
	}
}

// TestLockNotHeldByGrandchildAfterAdopterExits is the end-to-end
// shape of the `!make restart` failure: test → helper (adopts the
// lock like the daemon child does) → sh grandchild that outlives
// the helper. Once the test and the helper have both released
// their descriptors, the lock must be free even though the
// grandchild is still alive.
func TestLockNotHeldByGrandchildAfterAdopterExits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.lock")
	owner, err := TryLock(path)
	if err != nil {
		t.Fatal(err)
	}

	helper := exec.Command(os.Args[0], "-test.run=^TestLockCloexecHelper$")
	helper.Env = append(os.Environ(), lockHelperEnv+"=1")
	helper.ExtraFiles = []*os.File{owner.File()}
	out, err := helper.CombinedOutput()
	if err != nil {
		t.Fatalf("helper failed: %v\n%s", err, out)
	}
	pid := grandchildPID(t, string(out))
	t.Cleanup(func() {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Signal(syscall.SIGKILL)
		}
	})

	// The helper has exited; only this process's descriptor should
	// still be pinning the lock. CloseLocalCopy, not Close: Close
	// issues LOCK_UN, which releases the lock for *every* fd on the
	// shared open file description — including any the grandchild
	// inherited — and would mask the leak under test.
	if err := owner.CloseLocalCopy(); err != nil {
		t.Fatal(err)
	}
	lock, err := TryLock(path)
	if err != nil {
		t.Fatalf("lock still held after adopter exited (grandchild pid %d inherited it): %v", pid, err)
	}
	_ = lock.Close()
}

// TestLockCloexecHelper is not a test: it is the subprocess body
// for TestLockNotHeldByGrandchildAfterAdopterExits, dispatched by
// re-execing the test binary. It skips under a normal `go test`
// run.
//
// It deliberately does NOT Close the adopted lock — Close issues
// LOCK_UN on the shared open file description, which would
// release the lock no matter what FD_CLOEXEC says and mask the
// very leak under test.
func TestLockCloexecHelper(t *testing.T) {
	if os.Getenv(lockHelperEnv) == "" {
		t.Skip("helper subprocess only")
	}
	if _, err := LockFromFile(os.NewFile(3, "daemon.lock")); err != nil {
		t.Fatal(err)
	}
	// Stands in for `sh -c "make restart"`: setsid so it survives
	// this process, and no stdio wired so it can't hold the
	// parent's CombinedOutput pipe open for its whole lifetime.
	sh := exec.Command("sh", "-c", "sleep 30")
	sh.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := sh.Start(); err != nil {
		t.Fatal(err)
	}
	// Reported on stdout rather than via t.Logf so the parent can
	// read it without running the helper under -test.v.
	fmt.Printf(grandchildMarker+"\n", sh.Process.Pid)
	_ = sh.Process.Release()
}

func grandchildPID(t *testing.T, out string) int {
	t.Helper()
	_, rest, ok := strings.Cut(out, "<<<NIGHTME_HELPER_GRANDCHILD_PID=")
	if !ok {
		t.Fatalf("helper did not report a grandchild pid:\n%s", out)
	}
	// Closing delimiter guards against an interleaved log line
	// appending digits after the pid.
	pidStr, _, ok := strings.Cut(rest, ">>>")
	if !ok {
		t.Fatalf("helper pid marker not terminated:\n%s", out)
	}
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		t.Fatalf("parse grandchild pid from %q: %v", pidStr, err)
	}
	return pid
}
