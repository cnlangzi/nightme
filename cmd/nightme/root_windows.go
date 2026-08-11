//go:build windows

package main

import "github.com/spf13/cobra"

// addUnixOnlyCommands is the Windows no-op stub. The unix-only
// commands (start / stop / restart / status / _daemon / doctor)
// are not registered on Windows until the unix-socket daemon
// lifecycle is ported.
func addUnixOnlyCommands(root *cobra.Command) {}
