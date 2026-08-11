//go:build !windows

package main

import (
	"os"
	"syscall"
)

// shutdownSignals returns the OS-level signals nightme listens for
// to trigger graceful shutdown. On Unix these are SIGINT (Ctrl+C
// in a terminal) and SIGTERM (process supervisor / `kill`).
func shutdownSignals() []os.Signal {
	return []os.Signal{syscall.SIGINT, syscall.SIGTERM}
}
