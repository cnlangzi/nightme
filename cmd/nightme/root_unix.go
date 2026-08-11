//go:build !windows

package main

import "github.com/spf13/cobra"

// addUnixOnlyCommands registers commands that depend on the unix
// daemon lifecycle: start/stop/restart/status, the internal
// _daemon subcommand, and `nightme doctor` (which queries the
// unix-socket daemon). On Windows these have no implementation
// yet — see `cmd/nightme/daemon_lifecycle.go` (already
// `//go:build unix`) and `internal/daemoncontrol/` (the package
// is `//go:build !windows`).
func addUnixOnlyCommands(root *cobra.Command) {
	root.AddCommand(newStartCmd())
	root.AddCommand(newStatusCmd())
	root.AddCommand(newStopCmd())
	root.AddCommand(newRestartCmd())
	root.AddCommand(newDaemonCmd())
	root.AddCommand(newDoctorCmd())
}
