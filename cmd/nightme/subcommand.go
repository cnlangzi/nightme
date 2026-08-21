// Package main — single-source-of-truth subcommand registration.
//
// Background. Before this file, subcommand registration lived in two
// places that drifted apart: root.go called root.AddCommand(...) for
// every subcommand, while repl.go hard-coded the REPL banner "Common:"
// list in a separate constant. The first casualty was `nightme
// config`: registered in the cobra tree, missing from the REPL banner
// — users in the REPL could still type `config` and it worked, but
// discoverability was broken.
//
// cmdRegistry fixes that by making one call register all surfaces:
//   reg.add(cmd, bannerLine)   — cobra tree + REPL banner + tray menu
//   reg.addNoTray(cmd, ...)    — cobra tree + REPL banner only
//   reg.addHidden(cmd)         — cobra tree only, hidden everywhere
//
// Adding a new subcommand is now exactly one call; missing the
// banner or the tray entry is no longer expressible for the
// add() path.
//
// Named cmdRegistry (not registry) because internal/registry is
// already imported as `registry` across the cmd/nightme package —
// using a bare `registry` type here would shadow that import and
// the build fails with "registry already declared".

package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// cmdRegistry holds the cobra root plus the ordered list of banner
// entries that the REPL prints when it starts AND the ordered list
// of tray menu items that the systray builder wires up at daemon
// startup. Both are kept in registration order so each surface
// reflects what newRootCmd decided was the user-facing set.
//
// The three slices (cobra children, banner, tray) share ordering
// by construction: add() appends to all three; addNoTray skips the
// tray slice; addHidden skips the banner slice.
type cmdRegistry struct {
	root    *cobra.Command
	entries []bannerEntry
	tray    []trayItem
}

// bannerEntry is one line of the "Common:" section. Format is fixed
// at the call site: two leading spaces, command use, spaces for column
// alignment, one-line description. We do not auto-format from
// cmd.Short because manual descriptions are far tighter than what
// cobra generates, and the banner is meant to be scannable at a
// glance.
type bannerEntry struct {
	entry string
}

// trayItem is one entry in the system-tray "Commands" submenu.
//
// Clicking a tray menu item must produce the same effect as typing
// the corresponding command in the REPL. We don't invoke cmd.RunE
// directly because RunE expects a cobra context (cmd.OutOrStdout,
// cmd.Context, os.Args) — instead we re-dispatch through a fresh
// cobra root with the use string as the only arg, and io.Discard
// as the output writer so the dispatch doesn't pollute the tray
// event loop. See internal/tray/dispatch.go for the implementation.
//
// Tooltip doubles as the macOS help-tag and the Windows
// "What's this?" balloon text. Short, scannable, no trailing
// period (matches banner style).
type trayItem struct {
	Title   string
	Tooltip string
	Command *cobra.Command
}

func newCmdRegistry(root *cobra.Command) *cmdRegistry {
	return &cmdRegistry{root: root}
}

// add wires a cobra command into the tree, the REPL banner, AND
// the tray menu in a single call. This is the default path for
// every user-facing subcommand.
//
// bannerLine is the formatted banner entry, e.g.
// "  config          interactive configuration menu".
func (r *cmdRegistry) add(cmd *cobra.Command, bannerLine string) {
	r.root.AddCommand(cmd)
	r.entries = append(r.entries, bannerEntry{entry: bannerLine})
	r.tray = append(r.tray, trayItem{
		Title:   trayTitle(cmd),
		Tooltip: cmd.Short,
		Command: cmd,
	})
}

// addNoTray wires a cobra command into the tree and the REPL banner
// but NOT the tray menu. Use for commands that, when triggered from
// a tray click, would be either redundant (start / stop / restart
// are already on the tray as primary items) or unsafe (run blocks
// forever and would hang the tray).
func (r *cmdRegistry) addNoTray(cmd *cobra.Command, bannerLine string) {
	r.root.AddCommand(cmd)
	r.entries = append(r.entries, bannerEntry{entry: bannerLine})
}

// addHidden registers an internal command (e.g. `_daemon`) that
// should appear in the cobra tree for internal dispatch but be
// hidden from the REPL banner AND the tray menu. Use this sparingly —
// anything user-callable should go through add() or addNoTray().
func (r *cmdRegistry) addHidden(cmd *cobra.Command) {
	cmd.Hidden = true
	r.root.AddCommand(cmd)
}

// banner renders the "Common:" section of the REPL banner from the
// registered entries. Insertion order is preserved; callers can wrap
// the result with header / shell / prompt text.
func (r *cmdRegistry) banner() string {
	var b strings.Builder
	for _, e := range r.entries {
		fmt.Fprintln(&b, e.entry)
	}
	return b.String()
}

// TrayItems returns the user-visible subcommands that should appear
// in the tray's "Commands" submenu, in registration order. Hidden
// commands and addNoTray entries are excluded.
//
// The returned slice is a fresh copy; callers may mutate freely.
// Order matches reg.add() call order, which matches the REPL banner
// order, so the tray menu reads the same as the REPL "Common:"
// list — a deliberate visual rhyme.
func (r *cmdRegistry) TrayItems() []trayItem {
	out := make([]trayItem, len(r.tray))
	copy(out, r.tray)
	return out
}

// trayTitle extracts the tray menu label from a cobra command's Use
// string. Use is space-separated: "config [key] [value]" → "config",
// "test ... ..." → "test", "name [value]" → "name". The first
// whitespace-separated token is always the invocation name; we
// don't want "[key] [value]" or the variadic "..." suffix in the
// menu, just the verb the user types.
func trayTitle(cmd *cobra.Command) string {
	name, _, _ := strings.Cut(cmd.Use, " ")
	return name
}
