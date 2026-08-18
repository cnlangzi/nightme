//go:build windows

package main

// addLifecycleCommands is defined in root.go (cross-platform);
// this file's only role is to keep addUnixOnlyCommands as a
// Windows no-op stub so root.go's call site compiles on both
// platforms without conditional logic.

// addUnixOnlyCommands is the Windows no-op for commands that
// depend on Unix-only surfaces (currently just `nightme doctor`).
// The Windows variant of this file does not register doctor.
func addUnixOnlyCommands(_ *cmdRegistry) {}
