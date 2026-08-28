package version

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

// TestUserAgent_Shape pins the wire format for the no-ldflags
// build. Server-side log parsing keys on this shape, so a change
// here is a change to a published contract.
func TestUserAgent_Shape(t *testing.T) {
	got := UserAgent()
	// Version is read rather than hardcoded: its default gets bumped
	// with releases, and that is data, not part of the format this
	// test exists to pin.
	want := "nightme/" + Normalize(Version) + "+local (" +
		runtime.GOOS + "; " + runtime.GOARCH + "; " + runtime.Version() + ")"
	if got != want {
		t.Errorf("UserAgent() = %q, want %q", got, want)
	}
}

// TestUserAgent_IncludesInjectedCommit covers the release-build
// path: with GitCommit injected, the version token carries a
// +<sha> suffix so a development build cannot masquerade as the
// release whose version string it defaults to.
func TestUserAgent_IncludesInjectedCommit(t *testing.T) {
	orig := GitCommit
	GitCommit = "926bbc4"
	t.Cleanup(func() { GitCommit = orig })

	got := UserAgent()
	if !strings.HasPrefix(got, "nightme/0.1.0+926bbc4 (") {
		t.Errorf("UserAgent() = %q, want a nightme/<version>+<commit> prefix", got)
	}
}

// TestUserAgent_LocalBuildMarkedLocal is the no-ldflags path.
// Version defaults to a real release string, so without a marker
// such a build would be indistinguishable on the wire from that
// release.
//
// This deliberately does NOT assert GitCommit's default value:
// TestUserAgent_Shape already pins the whole string including
// "+local", so it fails if version.go's default and UserAgent's
// fallback ever drift apart. Asserting the global here as well
// would only add a dependency on no sibling test having leaked a
// mutation, with a failure message pointing at the wrong file.
func TestUserAgent_LocalBuildMarkedLocal(t *testing.T) {
	if got := UserAgent(); !strings.HasPrefix(got, "nightme/"+Normalize(Version)+"+local (") {
		t.Errorf("UserAgent() = %q, want a +local commit suffix", got)
	}
}

// TestUserAgent_EmptyCommitFallsBackToLocal covers a build that
// injects an explicitly empty GitCommit. Without the guard the
// suffix would degenerate to a dangling "+".
func TestUserAgent_EmptyCommitFallsBackToLocal(t *testing.T) {
	orig := GitCommit
	GitCommit = ""
	t.Cleanup(func() { GitCommit = orig })

	got := UserAgent()
	if !strings.HasPrefix(got, "nightme/0.1.0+local (") {
		t.Errorf("UserAgent() = %q, want a +local commit suffix", got)
	}
	if strings.Contains(got, "+ ") {
		t.Errorf("UserAgent() = %q, want no dangling %q", got, "+")
	}
}

// TestUserAgent_StripsLeadingV keeps the wire identity aligned with
// what the UI prints — 0.3.7, never v0.3.7.
func TestUserAgent_StripsLeadingV(t *testing.T) {
	orig := Version
	Version = "v0.3.7"
	t.Cleanup(func() { Version = orig })

	if got := UserAgent(); !strings.HasPrefix(got, "nightme/0.3.7+local (") {
		t.Errorf("UserAgent() = %q, want the leading %q stripped", got, "v")
	}
}

// TestUserAgent_SurvivesHostileLdflags is the reason uaToken
// exists. Version and GitCommit are -ldflags injected, so a
// newline or a stray paren in either would otherwise either make
// net/http reject the request outright or let the value escape the
// platform comment. Neither may happen: a mangled identity is
// acceptable, a version check that cannot send a request is not.
func TestUserAgent_SurvivesHostileLdflags(t *testing.T) {
	origVersion, origCommit := Version, GitCommit
	Version = "0.3.7\r\nX-Injected: 1"
	GitCommit = "abc(1234);darwin"
	t.Cleanup(func() {
		Version = origVersion
		GitCommit = origCommit
	})

	got := UserAgent()
	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("UserAgent() = %q, want no CR/LF", got)
	}
	// Exactly one comment: the platform one we emit ourselves. And
	// no injected ";" — that is the platform comment's own field
	// separator, so a leaked one would let a value forge a field.
	if strings.Count(got, "(") != 1 || strings.Count(got, ")") != 1 {
		t.Errorf("UserAgent() = %q, want a single parenthesised comment", got)
	}
	if strings.Count(got, ";") != 2 {
		t.Errorf("UserAgent() = %q, want exactly the 2 platform separators", got)
	}
	if strings.Count(got, "+") != 1 {
		t.Errorf("UserAgent() = %q, want a single commit delimiter", got)
	}

	// The real proof: net/http will actually put it on the wire.
	// A bad value fails inside the transport, not at Header.Set,
	// so only a round trip catches it.
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("User-Agent")
	}))
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("User-Agent", got)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("request rejected with the mangled identity: %v", err)
	}
	defer resp.Body.Close()
	if seen != got {
		t.Errorf("server saw User-Agent %q, want %q", seen, got)
	}
}

// TestUserAgent_UnprintableVersionFallsBack guards the degenerate
// case where sanitising removes every byte of the version: the
// product token must still name a version rather than reading
// "nightme/ (darwin; ...)".
func TestUserAgent_UnprintableVersionFallsBack(t *testing.T) {
	orig := Version
	Version = "\x00\x01\x02"
	t.Cleanup(func() { Version = orig })

	if got := UserAgent(); !strings.HasPrefix(got, "nightme/unknown+local (") {
		t.Errorf("UserAgent() = %q, want a nightme/unknown prefix", got)
	}
}
