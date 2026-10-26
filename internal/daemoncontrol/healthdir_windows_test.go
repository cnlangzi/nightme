//go:build windows

package daemoncontrol

import "testing"

// healthTestDir returns a data dir for the health tests. Windows
// addresses the daemon over a named pipe whose name is hashed, so
// the directory length doesn't matter.
func healthTestDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}
