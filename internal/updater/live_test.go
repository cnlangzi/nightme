package updater

// Live integration tests — only run when NIGHTME_LIVE_TEST=1
// is set. They hit the real nightme.dev and GitHub endpoints
// to verify the dual-source fallback and URL composition
// against the merged deployment.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func skipIfNotLive(t *testing.T) {
	t.Helper()
	if os.Getenv("NIGHTME_LIVE_TEST") != "1" {
		t.Skip("set NIGHTME_LIVE_TEST=1 to run live integration tests")
	}
}

// TestLive_LookupLatestTag_NightMeDev verifies the primary
// detection path against the real nightme.dev mirror.
func TestLive_LookupLatestTag_NightMeDev(t *testing.T) {
	skipIfNotLive(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tag, source, err := LookupLatestTag(ctx, "")
	if err != nil {
		t.Fatalf("LookupLatestTag: %v", err)
	}
	if source != "nightme.dev" {
		t.Errorf("source = %q, want nightme.dev (primary)", source)
	}
	if tag == "" {
		t.Errorf("empty tag")
	}
	if !strings.HasPrefix(tag, "v") {
		t.Errorf("tag %q missing v prefix", tag)
	}
}

// TestLive_LookupLatestTag_FallsBackToGitHub verifies the
// fallback chain when nightme.dev is down. The mock returns
// 503 to mimic a cold cache or unreachable mirror.
func TestLive_LookupLatestTag_FallsBackToGitHub(t *testing.T) {
	skipIfNotLive(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"no release available"}`))
	}))
	defer srv.Close()
	saved := NightMeDevAPIBase
	NightMeDevAPIBase = srv.URL
	t.Cleanup(func() { NightMeDevAPIBase = saved })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tag, source, err := LookupLatestTag(ctx, "")
	if err != nil {
		t.Fatalf("LookupLatestTag with mirror down: %v", err)
	}
	if source != "github" {
		t.Errorf("source = %q, want github (fallback)", source)
	}
	if tag == "" {
		t.Errorf("empty tag from GitHub")
	}
}

// TestLive_LookupLatestTag_PinnedTag exercises the --tag
// path. nightme.dev doesn't serve /releases/tags/<v> (its
// routes.go only registers /releases/latest), so the call
// naturally 404s there and falls through to GitHub.
func TestLive_LookupLatestTag_PinnedTag(t *testing.T) {
	skipIfNotLive(t)

	// Use whatever v0.5.0 actually maps to on GitHub at
	// the time of test. The exact tag doesn't matter as long
	// as GitHub returns a non-empty tag_name.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tag, source, err := LookupLatestTag(ctx, "v0.5.0")
	if err != nil {
		t.Fatalf("LookupLatestTag(v0.5.0): %v", err)
	}
	if source != "github" {
		t.Errorf("source = %q, want github (nightme.dev 404s on /releases/tags/v0.5.0)", source)
	}
	if tag != "v0.5.0" {
		t.Errorf("tag = %q, want v0.5.0", tag)
	}
}

// TestLive_GitHubAssetURL pins the GitHub download URL rule.
func TestLive_GitHubAssetURL(t *testing.T) {
	skipIfNotLive(t)

	got := GitHubAssetURL("v0.5.0", "nightme_0.5.0_linux_amd64.tar.gz")
	want := "https://github.com/cnlangzi/nightme/releases/download/v0.5.0/nightme_0.5.0_linux_amd64.tar.gz"
	if got != want {
		t.Errorf("GitHubAssetURL = %q, want %q", got, want)
	}
}

// TestLive_MirrorAssetURL pins the nightme.dev mirror URL
// rule. Critically, the path uses the version WITHOUT the
// leading v — different from GitHub's convention.
func TestLive_MirrorAssetURL(t *testing.T) {
	skipIfNotLive(t)

	got := MirrorAssetURL("v0.5.0", "nightme_0.5.0_linux_amd64.tar.gz")
	want := "https://nightme.dev/downloads/0.5.0/nightme_0.5.0_linux_amd64.tar.gz"
	if got != want {
		t.Errorf("MirrorAssetURL = %q, want %q", got, want)
	}
}
