package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// runRoot invokes the nightme root cobra command with the given
// args and returns the first non-nil error. Used by workflow
// tests to exercise list/show/run/validate without spinning up the
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

// writeBundle writes a workflow file into the bundle of the given
// name under dir and returns its path.
//
// A workflow is a bundle: a directory under the workflows dir that
// holds a workflow.yml. The name may hold a path — "nightme/coder" is
// the bundle of a published workflow.
func writeBundle(t *testing.T, dir, name, body string) string {
	t.Helper()
	return writeBundleFile(t, dir, name, "workflow.yml", body)
}

// writeBundleFile writes a file into the bundle of the given name
// under dir and returns its path.
func writeBundleFile(t *testing.T, dir, name, file, body string) string {
	t.Helper()
	path := filepath.Join(dir, name, file)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// unused import guards (some helpers below may not be used in
// every test file; keeping the import surface stable).
var _ = fmt.Sprint
var _ = strings.TrimSpace
