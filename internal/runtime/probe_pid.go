// Package runtime — cross-platform PID liveness probe.
//
// PidAlive is the cross-platform variant of the POSIX `kill -0`
// check: it answers "can a process with this id still be signalled /
// queried" without delivering any signal. The runtime uses it on
// startup to reconcile stale StatusRunning / StatusDetached entries
// in agent_sessions.json whose process died without a chance to
// emit a KindLifecycle event (e.g. daemon crash, `kill -9` of the
// daemon, OS reboot).
//
// This file is the public surface — it owns the PidAlive name and
// the pid<=0 short-circuit. Per-platform implementations of the
// actual OS probe live in probe_pid_unix.go and probe_pid_windows.go.
package runtime

// PidAlive reports whether pid can still be observed as a live
// process by the OS.
//
// pid <= 0 returns false — `agent_sessions.json` reserves PID 0
// for "not running" and a negative PID is never valid.
//
// The probe is intentionally cheap and side-effect free: no signals
// are delivered (POSIX) and no handles are left open (Windows).
func PidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return pidAliveOS(pid)
}
