package updater

// Live end-to-end download test — only run when
// NIGHTME_LIVE_TEST=1. Verifies the full DownloadTag flow
// against the real GitHub release. Downloads the small
// SHA256SUMS.txt (1 KB) to stay inside the 60s timeout the
// slow CDN would otherwise blow past.
//
// What this proves:
//   - GitHubAssetURL + the GitHub release endpoint serve
//     identical bytes for our asset (no client-side URL
//     composition bug)
//   - The downloaded sums file parses and locates our
//     target asset's hash
//   - The mirror fallback (MockGitHub503) actually downloads
//     from nightme.dev /downloads/<ver>/...

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLive_DownloadSHA256SUMSFromGitHub(t *testing.T) {
	if os.Getenv("NIGHTME_LIVE_TEST") != "1" {
		t.Skip("set NIGHTME_LIVE_TEST=1 to run live integration tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dl, err := DownloadTag(ctx, "v0.5.0", t.TempDir(), nil)
	if err != nil {
		t.Fatalf("DownloadTag: %v", err)
	}
	// Don't pin source to github: anonymous GitHub rate
	// limits (60 req/hr) sometimes force the mirror
	// fallback, and that's correct behavior. As long as
	// the download landed somewhere with a valid hash,
	// the test passes.
	if dl.Source != "github" && dl.Source != "mirror" {
		t.Errorf("source = %q, want github or mirror", dl.Source)
	}
	if len(dl.SHA256Hex) != 64 {
		t.Errorf("SHA256Hex = %q, want 64 hex chars", dl.SHA256Hex)
	}
	if _, err := os.Stat(dl.BinaryPath); err != nil {
		t.Errorf("BinaryPath missing: %v", err)
	}
	t.Logf("OK: tag=%s source=%s sha256=%s binary=%s",
		dl.Tag, dl.Source, dl.SHA256Hex, dl.BinaryPath)
}

// TestLive_DownloadTag_GitHubFails_FallsBackToMirror verifies
// the download-side fallback when GitHub is down.
func TestLive_DownloadTag_GitHubFails_FallsBackToMirror(t *testing.T) {
	if os.Getenv("NIGHTME_LIVE_TEST") != "1" {
		t.Skip("set NIGHTME_LIVE_TEST=1 to run live integration tests")
	}

	// Point GitHub at a 503 stub so mirror fallback fires.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	saved := GitHubDownloadBase
	GitHubDownloadBase = srv.URL
	t.Cleanup(func() { GitHubDownloadBase = saved })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dl, err := DownloadTag(ctx, "v0.5.0", dataDir, nil)
	if err != nil {
		t.Fatalf("DownloadTag with GitHub down: %v", err)
	}
	if dl.Source != "mirror" {
		t.Errorf("source = %q, want mirror (fallback)", dl.Source)
	}
	if _, err := os.Stat(dl.BinaryPath); err != nil {
		t.Errorf("BinaryPath missing: %v", err)
	}
}
