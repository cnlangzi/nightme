//go:build windows

package daemoncontrol

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
)

// ResolvePaths fills Paths for the Windows daemon lifecycle.
//
// The IPC endpoint is a named pipe at
//
//	\\.\pipe\nightme-{sha256[:8] of dataDir}
//
// The 8-byte hash keeps the pipe name short (Windows limits named
// pipe names to 256 chars; our value lands around 50) while giving
// 2^-64 collision probability — the same family of addresses as
// Unix AF_UNIX sockets under <dataDir>/daemon.sock, just
// expressed differently. The key matches the daemon.lock /
// lifecycle.lock paths: two users sharing a data dir would
// collide on those too, so the pipe name is keyed the same way
// and the "one daemon per data dir" invariant is preserved.
//
// Permissions: Windows chmod is a no-op for the lower bits; we
// still call it for symmetry with the Unix path, but the real
// per-user isolation is enforced by the named-pipe ACL set up
// in server_windows.go (current-user SID only).
func ResolvePaths(dataDir string) (Paths, error) {
	if dataDir == "" {
		return Paths{}, fmt.Errorf("daemon control: data directory is required")
	}
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return Paths{}, fmt.Errorf("daemon control: resolve data directory: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return Paths{}, fmt.Errorf("daemon control: create data directory %s: %w", abs, err)
	}
	if err := os.Chmod(abs, 0o700); err != nil {
		// Windows ignores chmod's mode bits; only the read-only
		// flag is meaningful. Best-effort.
		_ = err
	}

	socket := pipeName(abs)
	return Paths{
		Dir:           abs,
		DaemonLock:    filepath.Join(abs, "daemon.lock"),
		LifecycleLock: filepath.Join(abs, "lifecycle.lock"),
		Socket:        socket,
	}, nil
}

// pipeName returns the full named-pipe path for dataDir.
func pipeName(dataDir string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(dataDir)))
	return fmt.Sprintf(`\\.\pipe\nightme-%x`, sum[:8])
}
