//go:build !windows

package daemoncontrol

import (
	"fmt"
	"os"
	"path/filepath"
)

// ResolvePaths fills Paths for the Unix daemon lifecycle.
//
// dataDir must exist (or be creatable); the function MkdirAll's
// it with 0o700 perms. The Socket field is the AF_UNIX socket
// path the daemon will listen on; macOS/Linux cap sun_path at
// ~108 bytes, so we reject data dirs that would push us over.
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
		return Paths{}, fmt.Errorf("daemon control: chmod data directory %s: %w", abs, err)
	}
	socket := filepath.Join(abs, "daemon.sock")
	// sockaddr_un.sun_path is 104 bytes on macOS and 108 on Linux.
	// Leave room for the terminating NUL on both platforms.
	if len(socket) >= 104 {
		return Paths{}, fmt.Errorf("daemon control: socket path is too long: %s", socket)
	}
	return Paths{
		Dir:           abs,
		DaemonLock:    filepath.Join(abs, "daemon.lock"),
		LifecycleLock: filepath.Join(abs, "lifecycle.lock"),
		Socket:        socket,
	}, nil
}
