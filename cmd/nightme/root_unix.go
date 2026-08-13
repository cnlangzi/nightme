//go:build !windows

package main

import "github.com/spf13/cobra"

// addUnixOnlyCommands registers commands that depend on Unix-only
// surfaces — currently just `nightme doctor`, which queries the
// running daemon via daemoncontrol.GetHealth (that part is
// cross-platform; doctor itself just isn't wired up on Windows
// yet because nothing in the runtime surfaces a useful snapshot
// to it on that OS).
//
// The cross-platform daemon lifecycle commands (start / stop /
// restart / status / _daemon) are registered separately via
// addLifecycleCommands in root.go so Windows can pick them up.
func addUnixOnlyCommands(root *cobra.Command) {
	root.AddCommand(newDoctorCmd())
}
