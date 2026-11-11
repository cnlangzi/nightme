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
// Only the process itself is signalled. Callers that positively
// identified the PID's binary should use KillGroup instead: an agent
// CLI leads its own process group, and the group is the unit that
// also covers the tool subprocesses it spawned.
func Kill(pid int, force bool) error {
	if pid <= 0 {
		return syscall.ESRCH
	}
	return terminate(processTarget(pid), force)
}

// KillGroup terminates pid and every other process in its group.
//
// Agent CLIs are session leaders: internal/proc sets Setsid on the
// children it spawns and go-pty does the same for the PTY family, so
// the group covers the tool subprocesses the CLI spawned. Signalling
// only the CLI would leave those behind — the daemon's own graceful
// shutdown already kills the group for the same reason.
//
// The group is signalled only when pid leads it. A PID that was
// recycled onto a process belonging to somebody else's group is
// signalled on its own, so an unrelated group is never taken down.
//
// Use it only for a PID whose binary has been positively verified:
// the group of a recycled PID is not necessarily the agent's.
func KillGroup(pid int, force bool) error {
	if pid <= 0 {
		return syscall.ESRCH
	}
	if pgid, err := syscall.Getpgid(pid); err != nil || pgid != pid {
		return Kill(pid, force)
	}
	return terminate(groupTarget(pid), force)
}

// target is a termination target: a single process, or a process
// group.
type target struct {
	// signal delivers sig to the target.
	signal func(sig syscall.Signal) error
	// alive reports whether anything in the target is still running.
	alive func() bool
}

// terminate signals the target and escalates to SIGKILL once Grace
// expires.
//
// SIGTERM first, always: agent CLIs flush their own session state on
// SIGTERM, so giving them the grace window preserves the resume id
// the CLI itself writes. force=true skips straight to SIGKILL for a
// wedged child.
//
// ESRCH ("no such process") is returned verbatim so the caller can
// classify it via IsProcessGone and report "already exited" — it is
// the end state we wanted, not a failure.
func terminate(t target, force bool) error {
	if force {
		return t.signal(syscall.SIGKILL)
	}
	if err := t.signal(syscall.SIGTERM); err != nil {
		return err
	}

	deadline := time.Now().Add(Grace)
	for {
		if !t.alive() {
			return nil
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(PollInterval)
	}

	// Grace expired. SIGKILL, tolerating a target that died in the
	// gap between the last liveness poll and this call.
	if err := t.signal(syscall.SIGKILL); err != nil && !IsProcessGone(err) {
		return err
	}
	return nil
}

// processTarget targets a single process.
func processTarget(pid int) target {
	return target{
		signal: func(sig syscall.Signal) error { return syscall.Kill(pid, sig) },
		alive:  func() bool { return processAlive(pid) },
	}
}

// groupTarget targets the process group pid leads.
func groupTarget(pid int) target {
	return target{
		signal: func(sig syscall.Signal) error { return syscall.Kill(-pid, sig) },
		alive:  func() bool { return groupAlive(pid) },
	}
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

// VerifyOwner reports whether pid still belongs to wantCommand.
//
// Policy is fail-OPEN — Match is true — whenever we cannot get a
// definite contradiction:
//
//   - wantCommand == "" (unknown agent, e.g. a registry entry whose
//     agent was renamed or removed): nothing to compare against.
//   - `ps` missing, or the PID already gone: Kill handles the
//     gone case as "already exited", and a missing ps must not
//     disable the command.
//   - the PID is a zombie: it has already exited and only its
//     parent's failure to reap it keeps it visible.
//
// Only a positive mismatch ("this PID is /usr/bin/vim, you expected
// claude") blocks the termination. Fail-closed would be worse: it
// would silently refuse to clean up real agents on any system where
// the probe doesn't work, which is the sweep's entire purpose.
//
// Verified is true only when the PID was positively identified as
// the expected binary — it is what tells the caller whether the
// process group can be signalled too.
func VerifyOwner(pid int, wantCommand string) Owner {
	if wantCommand == "" {
		return Owner{Match: true}
	}
	actual, err := processCommand(pid)
	if err != nil || actual == "" || isDefunct(actual) {
		return Owner{Match: true}
	}
	if !sameCommand(actual, wantCommand) {
		return Owner{Binary: actual, Match: false}
	}
	return Owner{Binary: actual, Match: true, Verified: true}
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

// groupAlive reports whether anything in the group pid leads is still
// running.
//
// Signal 0 to the group answers for the whole group, so a group whose
// leader died but whose members live still reports alive — that is
// exactly the case the group-wide SIGKILL has to cover.
func groupAlive(pid int) bool {
	err := syscall.Kill(-pid, syscall.Signal(0))
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}

// defunctComm is the marker `ps` appends to the command of a zombie:
// a child that has exited but whose parent never reaped it.
const defunctComm = "<defunct>"

// isDefunct reports whether a `ps comm` value describes a zombie.
//
// The spelling is platform-specific: Linux prints "<name> <defunct>"
// (the name is still there), macOS prints "<defunct>". Both are
// matched.
//
// This is NOT an exotic case for us — it is the signature of the
// exact failure the sweep exists to clean up. When the daemon dies
// without reaping, its agent children become zombies whose registry
// entries still say "running". Their identity is no longer
// recoverable from ps, so verification must fail open: the process
// is already dead (signalling it is a no-op), and refusing here
// would strand the entry as "running" forever — the one outcome the
// sweep must never produce.
func isDefunct(comm string) bool {
	return comm == defunctComm || strings.HasSuffix(comm, " "+defunctComm)
}

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
