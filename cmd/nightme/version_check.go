// Package main — version-check wiring.
//
// internal/version.Checker.Lookup is a function field that
// production wires to internal/updater.LookupForLatest. The
// version package can't import updater (cycle, see
// internal/version/check.go's ReleaseMeta doc), so the
// adapter lives here in cmd/nightme where both packages are
// already imported.
//
// Every site that calls version.DefaultChecker should follow
// up with a call to wireUpdaterLookup before using the
// returned Checker. The helper below centralises that so the
// wiring is impossible to forget.
package main

import (
	"context"

	"github.com/cnlangzi/nightme/internal/updater"
	"github.com/cnlangzi/nightme/internal/version"
)

// wireUpdaterLookup attaches updater.LookupForLatest to
// Checker.Lookup. Safe to call on a Checker returned by
// version.DefaultChecker (Lookup will be nil before this).
//
// LookupForLatest's source order is the OPPOSITE of
// LookupForDownload — see internal/updater/updater.go for the
// rationale. This function is for the version-detection
// path only; download flows call updater.LookupForDownload
// directly without going through the version cache.
func wireUpdaterLookup(c *version.Checker) {
	c.Lookup = func(ctx context.Context, tag string) (version.ReleaseMeta, string, error) {
		rel, source, err := updater.LookupForLatest(ctx, tag)
		if err != nil {
			return version.ReleaseMeta{}, "", err
		}
		if rel == nil {
			return version.ReleaseMeta{}, source, nil
		}
		return version.ReleaseMeta{
			TagName:     rel.TagName,
			PublishedAt: rel.PublishedAt,
		}, source, nil
	}
}

// newProductionChecker is a thin convenience used by every
// caller of version.DefaultChecker. Returns a Checker ready
// to call .Check on.
func newProductionChecker(dataDir string) (*version.Checker, string) {
	c, path := version.DefaultChecker(dataDir)
	wireUpdaterLookup(c)
	return c, path
}
