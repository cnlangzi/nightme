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
	want := "nightme version dev (commit: local, built: unknown)"
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

// TestIsRelease — only a build that carries a release version is held
// to the version requirements of a workflow.
func TestIsRelease(t *testing.T) {
	tests := []struct {
		version string
		want    bool
	}{
		{version: "0.9.1", want: true},
		{version: "1.0.0", want: true},
		{version: "v0.9.1", want: true},
		{version: "0.0.0", want: false},
		{version: "0.9.1-dev", want: false},
		{version: "dev", want: false},
		{version: "926bbc4", want: false},
		{version: "", want: false},
		{version: "0.9", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			if got := isRelease(tt.version); got != tt.want {
				t.Errorf("isRelease(%q) = %t, want %t", tt.version, got, tt.want)
			}
		})
	}
}

// TestIsRelease_FollowsTheBuild — the judgment is about the build the
// binary carries.
func TestIsRelease_FollowsTheBuild(t *testing.T) {
	orig := Version
	t.Cleanup(func() { Version = orig })

	Version = "0.9.1"
	if !IsRelease() {
		t.Error("IsRelease() = false, want true")
	}
	Version = "dev"
	if IsRelease() {
		t.Error("IsRelease() = true, want false")
	}
}
