// Package main — version-check wiring.
//
// internal/version.Checker.Lookup is a function field that
// production wires to internal/updater.LookupLatestTag. The
// version package can't import updater (cycle, since
// updater imports version for UserAgent), so the wiring lives
// here in cmd/nightme where both packages are already imported.
//
// Every site that calls version.DefaultChecker should follow
// up with a call to wireUpdaterLookup before using the
// returned Checker.
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

// newProductionChecker is a thin convenience used by every
// caller of version.DefaultChecker. Returns a Checker ready
// to call .Check on.
func newProductionChecker(dataDir string) (*version.Checker, string) {
	c, path := version.DefaultChecker(dataDir)
	wireUpdaterLookup(c)
	return c, path
}
