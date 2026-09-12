// Package main — version-check wiring.
//
// internal/version.Checker.Lookup is a function field that the
// production wiring attaches to internal/updater.LookupLatestTag.
// The version package can't import updater (cycle, since
// updater imports version for UserAgent), so the wiring lives
// here in cmd/nightme where both packages are already imported.
//
// Every site that calls version.DefaultChecker should follow
// up with a call to wireUpdaterLookup before using the
// returned Checker — wiredChecker packages that into one call.
package main

import (
	"github.com/cnlangzi/nightme/internal/updater"
	"github.com/cnlangzi/nightme/internal/version"
)

// wireUpdaterLookup attaches updater.LookupLatestTag to
// Checker.Lookup. Safe to call on a Checker returned by
// version.DefaultChecker (Lookup is nil before this).
func wireUpdaterLookup(c *version.Checker) {
	c.Lookup = updater.LookupLatestTag
}

// wiredChecker returns a *version.Checker with Lookup already
// attached. Tests that want to inject a stub should build the
// Checker themselves (see stubCheckerWithTag in
// repl_update_test.go) — this helper exists for the four
// production call sites that always want the live lookup.
func wiredChecker(dataDir string) (*version.Checker, string) {
	c, path := version.DefaultChecker(dataDir)
	wireUpdaterLookup(c)
	return c, path
}
