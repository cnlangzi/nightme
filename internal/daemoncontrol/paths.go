// Package daemoncontrol: cross-platform daemon lifecycle.
//
// Paths holds the on-disk + IPC endpoint locations for one
// nightme instance. ResolvePaths (in paths_unix.go /
// paths_windows.go) fills the fields for the current OS:
//
//   - Unix:    Socket = "<dataDir>/daemon.sock" (AF_UNIX socket)
//   - Windows: Socket = \\.\pipe\nightme-{hash(dataDir)[:8]}
//     (named pipe; same field name, different protocol)
//
// All other fields (Dir, DaemonLock, LifecycleLock) are
// filesystem paths and are identical across platforms.
package daemoncontrol

type Paths struct {
	Dir           string
	DaemonLock    string
	LifecycleLock string
	// Socket is the IPC endpoint address. On Unix it is a
	// filesystem path for an AF_UNIX socket; on Windows it is
	// a named-pipe path. Callers pass it opaquely to Ping /
	// GetStatus / GetHealth / Stop / RequestSocket — the
	// implementation knows how to dial it.
	Socket string
}
