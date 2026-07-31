package version

import (
	"strings"
	"testing"
)

// TestVersion_Pinned locks the v0.1.0 marker so a release-bump
// PR accidentally editing this string without bumping Version
// itself fails loudly.
func TestVersion_Pinned(t *testing.T) {
	if Version != "0.1.0" {
		t.Errorf("Version = %q, want 0.1.0", Version)
	}
}

// TestString pins the exact banner printed by `nightme --version`
// so log scrapers / dashboards can rely on the format.
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
