//go:build !windows

// Package main — process termination primitives for `nightme kill`
// on POSIX platforms.
package main

import (
	"context"
	"errors"
	"github.com/cnlangzi/nightme/internal/pathutil"
	"github.com/cnlangzi/nightme/internal/proc"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// killProcess terminates pid.
//
// Default policy is SIGTERM → grace → SIGKILL: agent CLIs (claude,
// codex, …) flush their own session state on SIGTERM, so giving them
// killGrace to exit preserves the resume id the CLI itself writes.
// force=true skips straight to SIGKILL for a wedged child.
//
// ESRCH ("no such process") is returned verbatim so the caller can
// classify it via isProcessGone and report "already exited" — it is
// the end state we wanted, not a failure.
func killProcess(pid int, force bool) error {
	if pid <= 0 {
		return syscall.ESRCH
	}
	if force {
		return syscall.Kill(pid, syscall.SIGKILL)
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return err
	}

	deadline := time.Now().Add(killGrace)
	for {
		if !processAlive(pid) {
			return nil
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(killPollInterval)
	}

	// Grace expired. SIGKILL, tolerating a child that died in the
	// gap between the last liveness poll and this call.
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !isProcessGone(err) {
		return err
	}
	return nil
}

// processAlive reports whether pid can still be signalled. Signal 0
// performs the permission + existence checks without delivering
// anything.
//
// Caveat: a child whose parent (the nightme daemon) has not yet
// reaped it stays visible as a zombie and answers signal 0. In that
// case we simply wait out killGrace and send a harmless SIGKILL —
// the entry is marked exited either way.
func processAlive(pid int) bool {
	return syscall.Kill(pid, syscall.Signal(0)) == nil
}

// isProcessGone reports whether err means "the process no longer
// exists", which kill treats as success.
func isProcessGone(err error) bool {
	return errors.Is(err, syscall.ESRCH)
}

// verifyPIDOwner reports whether pid still belongs to wantCommand,
// returning the command it actually found for the caller's message.
//
// Policy is fail-OPEN — ok=true — whenever we cannot get a definite
// contradiction:
//
//   - wantCommand == "" (unknown agent, e.g. a registry entry whose
//     agent was renamed or removed): nothing to compare against.
//   - `ps` missing, or the PID already gone: killProcess handles the
//     gone case as "already exited", and a missing ps must not
//     disable the command.
//
// Only a positive mismatch ("this PID is /usr/bin/vim, you expected
// claude") blocks the kill. Fail-closed would be worse: it would
// silently refuse to clean up real agents on any system where the
// probe doesn't work, which is the command's entire purpose.
func verifyPIDOwner(pid int, wantCommand string) (string, bool) {
	if wantCommand == "" {
		return "", true
	}
	actual, err := processCommand(pid)
	if err != nil || actual == "" || actual == defunctComm {
		return "", true
	}
	return actual, sameCommand(actual, wantCommand)
}

// defunctComm is what `ps` reports for a zombie: a child that has
// exited but whose parent never reaped it.
//
// This is NOT an exotic case for us — it is the signature of the
// exact failure `nightme kill` exists to clean up. When the daemon
// dies without reaping, its agent children become zombies whose
// registry entries still say "running". Their identity is no longer
// recoverable from ps, so verification must fail open: the process
// is already dead (signalling it is a no-op), and refusing here
// would strand the entry as "running" forever — the one outcome the
// command must never produce.
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

// commTruncLen is the length at which Linux truncates a process's
// `comm`: the kernel stores it in TASK_COMM_LEN (16) bytes including
// the NUL, so `ps -o comm=` yields at most 15 characters. macOS does
// not truncate — it reports the full executable path (verified on
// darwin 24: `ps -p <pid> -o comm=` →
// "/Users/.../nightme/bin/nightme").
const commTruncLen = 15

// sameCommand compares a `ps comm` value with a configured agent
// command.
//
// Both sides may be a bare name ("claude") or an absolute path
// (/opt/homebrew/bin/claude), so the comparison is on the base name.
// A Linux-truncated comm is accepted as a match when it is a prefix
// of the expected name at exactly the truncation length: an agent
// configured as "my-long-agent-runner" shows up as "my-long-agent-r"
// there, and rejecting that would make `kill` refuse to clean up a
// perfectly valid session.
//
// This only ever loosens the check, which is the correct direction:
// verification exists to catch a PID that was recycled by something
// DIFFERENT, and a 15-character prefix match is strong evidence it
// was not.
func sameCommand(actual, want string) bool {
	a, w := pathutil.Base(actual), pathutil.Base(want)
	if a == w {
		return true
	}
	return len(a) == commTruncLen && len(w) > commTruncLen && strings.HasPrefix(w, a)
}
