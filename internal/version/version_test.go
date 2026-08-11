package version

import (
	"strings"
	"testing"
)

// TestVersion_DefaultNonEmpty guarantees the no-ldflags default
// (used by `go run` / `go build` during development) still renders
// a non-empty version banner. Release builds inject Version from
// the git tag via -ldflags, so this only constrains the dev-time
// default.
func TestVersion_DefaultNonEmpty(t *testing.T) {
	if Version == "" {
		t.Errorf("Version default is empty; the --version banner would be blank")
	}
}

// TestString pins the exact banner printed by `nightme --version`
// when built without -ldflags, so log scrapers / dashboards can
// rely on the format.
func TestString(t *testing.T) {
	got := String()
	want := "nightme version 0.1.0 (commit: unknown, built: unknown)"
	if got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

// TestString_IncludesInjectedFields verifies a -ldflags build
// swaps the unknown placeholders for real values.
func TestString_IncludesInjectedFields(t *testing.T) {
	origCommit, origDate := GitCommit, BuildDate
	GitCommit = "abc1234"
	BuildDate = "2026-07-31T00:00:00Z"
	t.Cleanup(func() {
		GitCommit = origCommit
		BuildDate = origDate
	})

	got := String()
	if !strings.Contains(got, "commit: abc1234") {
		t.Errorf("missing injected commit: %q", got)
	}
	if !strings.Contains(got, "built: 2026-07-31T00:00:00Z") {
		t.Errorf("missing injected build date: %q", got)
	}
}
