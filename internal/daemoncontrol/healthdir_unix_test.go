//go:build !windows

package daemoncontrol

import (
	"os"
	"testing"
)

// healthTestDir returns a data dir for the health tests.
//
// The AF_UNIX socket path must fit sockaddr_un.sun_path — 104 bytes
// on macOS — and t.TempDir() under the macOS per-user temp root
// ("/var/folders/…/T/TestName…/001") is far longer than that. A
// short dir under /tmp keeps the socket path in range.
func healthTestDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "nightme-")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
