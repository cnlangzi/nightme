//go:build !windows

// procsweep — process termination primitives on POSIX platforms.
package procsweep

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cnlangzi/nightme/internal/proc"
)

// Kill terminates pid.
//
// Default policy is SIGTERM → grace → SIGKILL: agent CLIs (claude,
// codex, …) flush their own session state on SIGTERM, so giving them
// Grace to exit preserves the resume id the CLI itself writes.
// force=true skips straight to SIGKILL for a wedged child.
//
// ESRCH ("no such process") is returned verbatim so the caller can
// classify it via IsProcessGone and report "already exited" — it is
// the end state we wanted, not a failure.
func Kill(pid int, force bool) error {
	if pid <= 0 {
		return syscall.ESRCH
	}
	if force {
		return syscall.Kill(pid, syscall.SIGKILL)
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return err
	}

	deadline := time.Now().Add(Grace)
	for {
		if !processAlive(pid) {
			return nil
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(PollInterval)
	}

	// Grace expired. SIGKILL, tolerating a child that died in the
	// gap between the last liveness poll and this call.
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !IsProcessGone(err) {
		return err
	}
	return nil
}

// IsProcessGone reports whether err means "the process no longer
// exists", which the sweep treats as success.
func IsProcessGone(err error) bool {
	return errors.Is(err, syscall.ESRCH)
}

// ImageName returns the executable name of pid (e.g. "claude"), or ""
// when it cannot be determined.
//
// The name is platform-native: a bare `ps comm` value on Unix, the
// image file name on Windows.
func ImageName(pid int) string {
	name, _ := processCommand(pid)
	return name
}

// VerifyOwner reports whether pid still belongs to wantCommand,
// returning the command it actually found for the caller's message.
//
// Policy is fail-OPEN — ok=true — whenever we cannot get a definite
// contradiction:
//
//   - wantCommand == "" (unknown agent, e.g. a registry entry whose
//     agent was renamed or removed): nothing to compare against.
//   - `ps` missing, or the PID already gone: Kill handles the
//     gone case as "already exited", and a missing ps must not
//     disable the command.
//
// Only a positive mismatch ("this PID is /usr/bin/vim, you expected
// claude") blocks the termination. Fail-closed would be worse: it
// would silently refuse to clean up real agents on any system where
// the probe doesn't work, which is the sweep's entire purpose.
func VerifyOwner(pid int, wantCommand string) (string, bool) {
	if wantCommand == "" {
		return "", true
	}
	actual, err := processCommand(pid)
	if err != nil || actual == "" || actual == defunctComm {
		return "", true
	}
	return actual, sameCommand(actual, wantCommand)
}

// processAlive reports whether pid can still be signalled. Signal 0
// performs the permission + existence checks without delivering
// anything.
//
// Caveat: a child whose parent (the nightme daemon) has not yet
// reaped it stays visible as a zombie and answers signal 0. In that
// case we simply wait out Grace and send a harmless SIGKILL — the
// entry is marked exited either way.
func processAlive(pid int) bool {
	return syscall.Kill(pid, syscall.Signal(0)) == nil
}

// defunctComm is what `ps` reports for a zombie: a child that has
// exited but whose parent never reaped it.
//
// This is NOT an exotic case for us — it is the signature of the
// exact failure the sweep exists to clean up. When the daemon dies
// without reaping, its agent children become zombies whose registry
// entries still say "running". Their identity is no longer
// recoverable from ps, so verification must fail open: the process
// is already dead (signalling it is a no-op), and refusing here
// would strand the entry as "running" forever — the one outcome the
// sweep must never produce.
const defunctComm = "<defunct>"

// processCommand returns the executable name of pid via `ps`, which
// is available on every unix nightme targets. We ask for `comm=`
// (no header) so the output is a single bare name or path.
func processCommand(pid int) (string, error) {
	out, err := proc.New(context.Background(), "ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
