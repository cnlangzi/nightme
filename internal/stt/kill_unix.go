//go:build !windows

package stt

import "os/exec"

// killByName sends SIGTERM to every process whose argv
// contains `name`. Used to stop a running nightme-stt worker
// after a fresh install so the next Voice message picks up
// the new binary.
//
// pkill returns 1 when no process matches; that's fine —
// the worker may already be dead. We don't surface the
// error because killRunningWorker is best-effort.
//
// We deliberately use exec.Command rather than syscall.Kill
// so we don't need to enumerate PIDs ourselves. The cost
// is one shell-out per install, which is acceptable.
func killByName(name string) {
	_ = exec.Command("pkill", "-f", name).Run()
}

// isWindowsExe is true on Windows, where the binary carries
// a .exe suffix; false on Unix. The other half of this
// build-tag pair lives in kill_windows.go.
var isWindowsExe = false
