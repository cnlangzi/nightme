//go:build windows

package main

import "os"

// shutdownSignals returns the OS-level signals nightme listens for
// to trigger graceful shutdown. Windows has no SIGTERM — the only
// practical signals are os.Interrupt (Ctrl+C / console close) and
// os.Kill (TerminateProcess, sent by task manager / `taskkill /F`).
//
// `signal.Notify` on Windows silently no-ops on SIGTERM, so
// registering it would mislead readers into thinking graceful
// shutdown is wired; we explicitly return only the signals that
// actually fire on this OS.
func shutdownSignals() []os.Signal {
	return []os.Signal{os.Interrupt, os.Kill}
}
