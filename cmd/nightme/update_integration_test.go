package main

// End-to-end integration test for the nightme update chain.
//
// This test exercises the same calls runUpdate makes in
// production (version.Checker.Check → updater.DownloadTag →
// updater.Install), with the live network endpoints stubbed
// by a single httptest server. It catches regressions where
// the individual pieces still pass their unit tests but the
// integration breaks — URL composition drift, LookupLatestTag
// signature changes, base-URL typos, SHA verification skip,
// tag mismatch, etc.
//
// Lives in cmd/nightme because that's the only package that
// imports both internal/updater and internal/version
// (internal/updater already imports internal/version for
// UserAgent, so the reverse import would cycle).

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/updater"
	"github.com/cnlangzi/nightme/internal/version"
)

// buildArchiveForTest packages body into a tar.gz (unix) or
// zip (windows) archive containing a single `nightme` /
// `nightme.exe` file. Mirrors the structure ExtractArchive
// expects.
func buildArchiveForTest(t *testing.T, wantOS string, body []byte) []byte {
	t.Helper()
	if wantOS == "windows" {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		w, err := zw.Create("nightme.exe")
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		_, _ = w.Write(body)
		_ = zw.Close()
		return buf.Bytes()
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{
		Name: "nightme",
		Mode: 0o755,
		Size: int64(len(body)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("tar body: %v", err)
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// newUpdateFixture serves a single fake release behind a
// single httptest.Server: /releases/latest returns GitHub-shaped
// JSON with the configured tag, plus sums + binary at the
// conventional paths. The test overrides updater.GitHubDownloadBase
// + updater.NightMeDevAPIBase + updater.MirrorDownloadBase to
// point at this server's URL so all three code paths route
// through it.
func newUpdateFixture(t *testing.T, tag, ver, assetBody string) (*httptest.Server, string) {
	t.Helper()
	wantOS, wantArch := runtime.GOOS, runtime.GOARCH
	ext := "tar.gz"
	if wantOS == "windows" {
		ext = "zip"
	}
	assetName := "nightme_" + ver + "_" + wantOS + "_" + wantArch + "." + ext
	archiveBytes := buildArchiveForTest(t, wantOS, []byte(assetBody))

	sum := sha256.Sum256(archiveBytes)
	sumHex := hex.EncodeToString(sum[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/releases/latest":
			fmt.Fprintf(w, `{"tag_name":%q}`, tag)
		case strings.HasSuffix(r.URL.Path, "/SHA256SUMS.txt"):
			fmt.Fprintf(w, "%s  %s\n", sumHex, assetName)
		case strings.HasSuffix(r.URL.Path, "/"+assetName):
			_, _ = w.Write(archiveBytes)
		default:
			http.NotFound(w, r)
		}
	}))

	// Override all three bases so DownloadTag's LookupLatestTag
	// + sums + binary all flow through this single server.
	savedGH := updater.GitHubDownloadBase
	savedND := updater.NightMeDevAPIBase
	savedM := updater.MirrorDownloadBase
	updater.GitHubDownloadBase = srv.URL
	updater.NightMeDevAPIBase = srv.URL
	updater.MirrorDownloadBase = srv.URL
	t.Cleanup(func() {
		updater.GitHubDownloadBase = savedGH
		updater.NightMeDevAPIBase = savedND
		updater.MirrorDownloadBase = savedM
		srv.Close()
	})
	return srv, assetName
}

// TestUpdate_EndToEnd_FixtureServers exercises the version-check
// → download → install chain that runUpdate orchestrates, with
// the live network endpoints stubbed by a single httptest
// server. Catches regressions where the individual unit tests
// still pass but the integration breaks — e.g. URL composition
// drift (stripV changes, base path typos), LookupLatestTag
// signature drift, SHA-verification skip on a misnamed sums
// entry, tag mismatch handling.
//
// Skips runUpdate itself because it has hard-to-mock side
// effects (os.Executable for the install target, daemon
// restart subprocess, re-exec). The chain it orchestrates is
// tested directly here; if these three calls work in concert,
// runUpdate works (modulo CLI parsing, covered by the
// existing TestUpdate_*Flags / TestUpdate_*Verb tests).
func TestUpdate_EndToEnd_FixtureServers(t *testing.T) {
	const (
		tag = "v9.9.9"
		ver = "9.9.9"
	)
	// Install refuses binaries <1KiB as a safety check (real
	// nightme is many MB). Pad with a non-NULL repeating byte
	// so the tar archive round-trips the full size — Go strings
	// preserve embedded NULs but tar tools occasionally treat
	// NUL as EOF in some environments.
	fakeBody := bytes.Repeat([]byte("X"), 2048)

	_, _ = newUpdateFixture(t, tag, ver, string(fakeBody))

	_, _ = newUpdateFixture(t, tag, ver, string(fakeBody))

	dataDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Stage 1: version check. The fixture serves v9.9.9 as
	// the latest; current version "0.0.1" should be outdated.
	checker, _ := version.NewChecker(dataDir, updater.LookupLatestTag)
	res := checker.Check(ctx, "0.0.1", nil)
	if res.Latest != tag {
		t.Errorf("Check Latest = %q, want %q", res.Latest, tag)
	}
	if !res.Outdated {
		t.Errorf("Check Outdated = false, want true (0.0.1 < %s)", tag)
	}

	// Stage 2: download + verify + extract. QuietProgress
	// because we don't want the test output cluttered with
	// progress-bar characters.
	dl, err := updater.DownloadTag(ctx, dataDir, updater.QuietProgress)
	if err != nil {
		t.Fatalf("DownloadTag: %v", err)
	}
	if dl.Tag != tag {
		t.Errorf("DownloadTag.Tag = %q, want %q", dl.Tag, tag)
	}
	if len(dl.SHA256Hex) != 64 {
		t.Errorf("DownloadTag.SHA256Hex = %q, want 64 hex chars (verification ran)", dl.SHA256Hex)
	}
	if _, err := os.Stat(dl.BinaryPath); err != nil {
		t.Fatalf("DownloadTag.BinaryPath missing: %v", err)
	}

	// Stage 3: install. Synthesise a fake "current binary" as
	// the install target; verify the staged binary overwrites
	// it and that the .old sidecar contains the original
	// bytes.
	installDir := t.TempDir()
	target := filepath.Join(installDir, "nightme")
	originalBody := strings.Repeat("ORIGINAL-BINARY-", 200)
	if err := os.WriteFile(target, []byte(originalBody), 0o755); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	res2, err := updater.Install(dl.BinaryPath, target)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res2.NewBinaryPath != target {
		t.Errorf("Install.NewBinaryPath = %q, want %q", res2.NewBinaryPath, target)
	}

	gotBody, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if !bytes.Equal(gotBody, fakeBody) {
		t.Errorf("target body mismatch: len=%d, want len=%d", len(gotBody), len(fakeBody))
	}
	oldBody, err := os.ReadFile(res2.OldBinaryPath)
	if err != nil {
		t.Fatalf("read .old: %v", err)
	}
	if string(oldBody) != originalBody {
		t.Errorf(".old body = %q, want %q", string(oldBody)[:20], originalBody[:20])
	}
}
