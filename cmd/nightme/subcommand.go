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
// cmdRegistry fixes that by making one call register both surfaces:
//   reg.add(cmd, bannerLine)   — both cobra tree AND banner
//   reg.addHidden(cmd)         — cobra tree only, hidden from help/banner
//
// Adding a new subcommand is now exactly one call; missing the banner
// is no longer expressible.
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
// entries that the REPL prints when it starts. Entries are kept in
// registration order so the banner reflects what newRootCmd decided
// was the user-facing surface.
type cmdRegistry struct {
	root    *cobra.Command
	entries []bannerEntry
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

func newCmdRegistry(root *cobra.Command) *cmdRegistry {
	return &cmdRegistry{root: root}
}

// add wires a cobra command into the tree AND records its REPL banner
// line in a single call. bannerLine is the formatted banner entry,
// e.g. "  config          interactive configuration menu".
func (r *cmdRegistry) add(cmd *cobra.Command, bannerLine string) {
	r.root.AddCommand(cmd)
	r.entries = append(r.entries, bannerEntry{entry: bannerLine})
}

// addHidden registers an internal command (e.g. `_daemon`) that
// should appear in the cobra tree for internal dispatch but be hidden
// from the REPL banner and `nightme help`. Use this sparingly —
// anything user-callable should go through add().
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