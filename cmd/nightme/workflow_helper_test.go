package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// runRoot invokes the nightme root cobra command with the given
// args and returns the first non-nil error. Used by workflow
// tests to exercise list/show/run without spinning up the
// full daemon. The cobra root is built fresh per call so tests
// don't share state.
func runRoot(args ...string) error {
	root := buildRootForTest()
	root.SetArgs(args)
	return root.Execute()
}

// buildRootForTest builds the cobra root the same way production
// does, but captures only what's needed for workflow tests
// (the subcommand tree + a no-op output sink).
func buildRootForTest() *cobra.Command {
	root, _ := newRootCmd()
	return root
}

// unused import guards (some helpers below may not be used in
// every test file; keeping the import surface stable).
var _ = fmt.Sprint
var _ = strings.TrimSpace
