// Package procutil — shared subprocess liveness helpers for
// bridge drivers that spawn an OS subprocess per AgentSession
// (claudecode, opencode, codex, pi, pty).
//
// Why a separate package: each of those bridges used to keep
// its own copy of the syscall.Kill(pid, 0) wrapper, which meant
// five implementations of the same "is this PID alive?" check
// drifted independently. The bridges that DON'T spawn a
// subprocess (dsh's shared-host model, future ACP-style SDK
// backends) implement their own liveness logic against the
// driver.Keepalive(ctx) interface — procutil is for the
// subprocess tier only.
//
// Cheap: every call is a single syscall + a couple of field
// reads. Keepalive backstops in the bridge layer invoke this
// on every Submit, so cost matters — syscall.Kill(pid, 0)
// takes <10µs in practice.
package procutil

import (
	"errors"
	"syscall"
)

// ErrNoPID is returned when the caller hands procutil.AlivePID
// a pid of 0 or negative — i.e., the bridge never reached
// Running or has already been demoted. The chat layer treats
// this as a "nothing to check" no-op rather than as a dead
// PID; the bridges' Keepalive implementations branch on it
// to skip the syscall entirely.
var ErrNoPID = errors.New("procutil: no PID (detached / never spawned / already demoted)")

// AlivePID reports whether pid is a currently-alive OS process.
//
// Returns nil when the PID is alive. Returns ErrNoPID for
// non-positive pids so callers can distinguish "not tracked"
// from "definitively dead". Returns the underlying syscall
// error (typically ESRCH) for real "process is gone" cases —
// the bridge's Keepalive interprets this as the trigger for
// self-recovery.
//
// TOCTOU: this is a snapshot check; the PID can die between
// this call and the bridge's next operation. Keepalive
// implementations that follow this with a recovery action
// must re-check under the bridge's own lock (or rely on the
// bridge's process-exit handler) to avoid a duplicate respawn
// when the process is killed mid-recovery.
func AlivePID(pid int) error {
	if pid <= 0 {
		return ErrNoPID
	}
	if err := syscall.Kill(pid, syscall.Signal(0)); err != nil {
		return err
	}
	return nil
}