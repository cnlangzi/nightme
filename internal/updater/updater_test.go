package updater

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/version"
)

// fixture serves a synthetic GitHub release payload + the
// SHA256SUMS file + the asset binary. All paths use the same
// /repos/<repo>/... prefix so the production URL builder hits
// the same router.
type fixture struct {
	repo      string
	tag       string
	assetBody []byte
	assetName string
	sums      string
	srv       *httptest.Server

	// ua records the User-Agent of the most recent request to a
	// release-lookup endpoint. atomic.Value because the handler
	// runs on the server's goroutine.
	ua atomic.Value // string

	// assetUA is the same, for the asset-download endpoint. Kept
	// separate from ua so a test can assert on both without one
	// request clobbering the other's record.
	assetUA atomic.Value // string
}

// lastAssetUserAgent returns the User-Agent the fixture saw on the
// most recent asset download, failing the test if none was made.
func (f *fixture) lastAssetUserAgent(t *testing.T) string {
	t.Helper()
	ua, ok := f.assetUA.Load().(string)
	if !ok {
		t.Fatal("no asset-download request reached the fixture")
	}
	return ua
}

// lastUserAgent returns the User-Agent the fixture saw on the most
// recent release-lookup request, failing the test if no such
// request has been made.
func (f *fixture) lastUserAgent(t *testing.T) string {
	t.Helper()
	ua, ok := f.ua.Load().(string)
	if !ok {
		t.Fatal("no release-lookup request reached the fixture")
	}
	return ua
}

func newFixture(t *testing.T, tag, version, assetBody string) *fixture {
	t.Helper()

	// SHA256SUMS line for the asset.
	sum := sha256.Sum256([]byte(assetBody))
	sumHex := hex.EncodeToString(sum[:])

	wantOS := runtime.GOOS
	wantArch := runtime.GOARCH
	ext := "tar.gz"
	if wantOS == "windows" {
		ext = "zip"
	}
	assetName := fmt.Sprintf("nightme_%s_%s_%s.%s", version, wantOS, wantArch, ext)

	repo := "cnlangzi/nightme"
	f := &fixture{
		repo:      repo,
		tag:       tag,
		assetBody: []byte(assetBody),
		assetName: assetName,
		sums:      sumHex + "  " + assetName + "\n",
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+repo+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		f.ua.Store(r.Header.Get("User-Agent"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{
			"tag_name": %q,
			"assets": [
				{"name": "SHA256SUMS.txt", "browser_download_url": "%s/repos/%s/asset/sums", "size": %d},
				{"name": %q,             "browser_download_url": "%s/repos/%s/asset/binary", "size": %d}
			]
		}`, tag, f.srvURL(), repo, len(f.sums), assetName, f.srvURL(), repo, len(assetBody))
	})
	mux.HandleFunc("/repos/"+repo+"/releases/tags/"+tag, func(w http.ResponseWriter, r *http.Request) {
		f.ua.Store(r.Header.Get("User-Agent"))
		// Same body as /releases/latest for our tests.
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{
			"tag_name": %q,
			"assets": [
				{"name": "SHA256SUMS.txt", "browser_download_url": "%s/repos/%s/asset/sums", "size": %d},
				{"name": %q,             "browser_download_url": "%s/repos/%s/asset/binary", "size": %d}
			]
		}`, tag, f.srvURL(), repo, len(f.sums), assetName, f.srvURL(), repo, len(assetBody))
	})
	mux.HandleFunc("/repos/"+repo+"/asset/sums", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, f.sums)
	})
	mux.HandleFunc("/repos/"+repo+"/asset/binary", func(w http.ResponseWriter, r *http.Request) {
		f.assetUA.Store(r.Header.Get("User-Agent"))
		_, _ = w.Write(f.assetBody)
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fixture) srvURL() string { return f.srv.URL }

// TestLookup verifies that Lookup decodes the GitHub release
// payload (only the fields we care about) and surfaces the tag.
func TestLookup(t *testing.T) {
	f := newFixture(t, "v9.9.9", "9.9.9", "fake-binary-body")

	// LookupURL exists as a var precisely so tests can point the
	// release API at an httptest server; use it rather than
	// reaching out to api.github.com.
	origURL := LookupURL
	LookupURL = f.srvURL()
	t.Cleanup(func() { LookupURL = origURL })

	release, err := Lookup(context.Background(), f.repo, "")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if release.TagName != "v9.9.9" {
		t.Errorf("TagName = %q, want %q", release.TagName, "v9.9.9")
	}
	if len(release.Assets) != 2 {
		t.Fatalf("got %d assets, want 2", len(release.Assets))
	}

	// The release lookup is a version check, so it must identify
	// itself as nightme — GitHub rate-limits anonymous clients by
	// IP, and a shared "Go-http-client/1.1" is indistinguishable
	// from any other Go program behind the same NAT.
	ua := f.lastUserAgent(t)
	if want := version.UserAgent(); ua != want {
		t.Errorf("User-Agent = %q, want %q", ua, want)
	}
	if !strings.HasPrefix(ua, "nightme/") {
		t.Errorf("User-Agent = %q, want a nightme/ product token", ua)
	}
	if !strings.Contains(ua, runtime.GOOS) || !strings.Contains(ua, runtime.GOARCH) {
		t.Errorf("User-Agent = %q, want it to carry GOOS and GOARCH", ua)
	}
}

// TestMatchAsset_PicksOurOSArch is the core asset-selection
// contract: MatchAsset must pick the asset matching
// runtime.GOOS / runtime.GOARCH.
func TestMatchAsset_PicksOurOSArch(t *testing.T) {
	wantOS, wantArch := runtime.GOOS, runtime.GOARCH
	ext := "tar.gz"
	if wantOS == "windows" {
		ext = "zip"
	}
	v := "9.9.9"

	assetName := fmt.Sprintf("nightme_%s_%s_%s.%s", v, wantOS, wantArch, ext)
	other := []Asset{
		{Name: fmt.Sprintf("nightme_%s_darwin_amd64.%s", v, ext)},
		{Name: fmt.Sprintf("nightme_%s_linux_arm64.%s", v, ext)},
		{Name: "SHA256SUMS.txt"},
		{Name: assetName}, // ← the one we want
	}
	r := &Release{TagName: "v" + v, Assets: other}

	got := MatchAsset(r, v)
	if got == nil {
		t.Fatalf("MatchAsset returned nil; expected %q", assetName)
	}
	if got.Name != assetName {
		t.Errorf("MatchAsset = %q, want %q", got.Name, assetName)
	}
}

// TestMatchAsset_NoMatch covers the diagnostic path: when the
// release has no asset for our OS/arch, MatchAsset returns
// nil (no error). The CLI translates that into "no binary
// for darwin/arm64 in this release".
//
// We list only assets for a DIFFERENT GOOS than the test
// runner's, so the test is platform-independent (CI runners
// on linux/amd64, macos/arm64, etc. all see "no match" for
// the synthetic release).
func TestMatchAsset_NoMatch(t *testing.T) {
	otherOS := "windows"
	if runtime.GOOS == "windows" {
		otherOS = "linux"
	}
	wantExt := "tar.gz"
	if otherOS == "windows" {
		wantExt = "zip"
	}
	r := &Release{
		TagName: "v9.9.9",
		Assets: []Asset{
			{Name: fmt.Sprintf("nightme_9.9.9_%s_amd64.%s", otherOS, wantExt)},
			{Name: fmt.Sprintf("nightme_9.9.9_%s_arm64.%s", otherOS, wantExt)},
		},
	}
	if got := MatchAsset(r, "9.9.9"); got != nil {
		t.Errorf("MatchAsset = %v, want nil", got)
	}
}

// TestFormatBytes / TestFormatSpeed are pure functions and
// worth pinning so the progress reporter never silently
// regresses on its formatter strings.
func TestFormatBytes(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 kB"},
		{1536, "1.5 kB"},
		{1024 * 1024, "1.0 MB"},
		{1024 * 1024 * 1024, "1.0 GB"},
	}
	for _, tt := range tests {
		if got := FormatBytes(tt.in); got != tt.want {
			t.Errorf("FormatBytes(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFormatSpeed(t *testing.T) {
	if got := FormatSpeed(1024*1024, time.Second); got != "1.0 MB/s" {
		t.Errorf("FormatSpeed(1MiB, 1s) = %q, want %q", got, "1.0 MB/s")
	}
	if got := FormatSpeed(0, 0); got != "0 B/s" {
		t.Errorf("FormatSpeed(0, 0) = %q, want %q", got, "0 B/s")
	}
}

// TestStagingDir covers the canonical path computation.
func TestStagingDir(t *testing.T) {
	got, err := StagingDir("/var/lib/nightme", "v0.3.7")
	if err != nil {
		t.Fatalf("StagingDir: %v", err)
	}
	want := filepath.Join("/var/lib/nightme", "updates", "0.3.7")
	if got != want {
		t.Errorf("StagingDir = %q, want %q", got, want)
	}
}

func TestStagingDir_EmptyDataDir(t *testing.T) {
	if _, err := StagingDir("", "v0.3.7"); err == nil {
		t.Errorf("StagingDir(\"\") returned no error; want one")
	}
}

// TestExtractArchive_WritesToStagingDir pins the contract that
// ExtractArchive writes the extracted binary under the supplied
// stagingDir, never under the process CWD.
//
// Regression guard for the Win32 REPL bug: a hand-rolled
// "filepathDir" helper that scanned for '/' returned "." for any
// Windows path with '\' separators. ExtractArchive then joined
// ".\nightme.exe" and opened that for write — which on a REPL
// launched from the install dir is the running exe, triggering
// ERROR_SHARING_VIOLATION:
//
//	install failed: extract: open extract dst: open nightme.exe:
//	  The process cannot access the file because it is being
//	  used by another process.
//
// The test builds an in-memory zip, calls ExtractArchive against
// a chosen stagingDir, and asserts the returned path lives under
// that stagingDir. We call extractZIP directly (rather than going
// through ExtractArchive, which switches on GOOS) so the test
// runs on every platform — the contract is platform-independent.
func TestExtractArchive_WritesToStagingDir(t *testing.T) {
	dir := t.TempDir()
	stagingDir := filepath.Join(dir, "updates", "0.4.4")
	// Production always reaches extractZIP via Download, which
	// MkdirAll's stagingDir first. The test bypasses Download
	// to pin the contract in isolation, so we mirror that
	// MkdirAll here.
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		t.Fatalf("mkdir staging: %v", err)
	}
	const body = "fake-binary-payload-12345"

	// Build an in-memory zip containing nightme.exe.
	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	w, err := zw.Create("nightme.exe")
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatalf("zip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}

	archivePath := filepath.Join(dir, "nightme_0.4.4.zip")
	if err := os.WriteFile(archivePath, zipBuf.Bytes(), 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}

	got, err := extractZIP(archivePath, stagingDir)
	if err != nil {
		t.Fatalf("extractZIP: %v", err)
	}

	want := filepath.Join(stagingDir, "nightme.exe")
	if got != want {
		t.Fatalf("extractZIP returned %q; want %q (cwd-relative bare %q means the dir helper regressed)",
			got, want, "nightme.exe")
	}

	gotBody, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("read extracted: %v", err)
	}
	if string(gotBody) != body {
		t.Fatalf("extracted body mismatch: got %q, want %q", gotBody, body)
	}

	// Belt-and-suspenders: the cwd must NOT contain a stray
	// nightme.exe left behind by a regression. If we ever
	// regress to writing under "." again, t.TempDir's cleanup
	// will at least surface this here.
	if _, err := os.Stat(filepath.Join(".", "nightme.exe")); err == nil {
		t.Fatalf("found stray nightme.exe in cwd — ExtractArchive regressed to writing under \".\"")
	}
}

// TestProgressReader_EmitsProgress ensures the progress
// reader fires the callback on tick intervals (not just chunk
// boundaries). This is the contract our ASCII bar depends on
// for a smooth line.
func TestProgressReader_EmitsProgress(t *testing.T) {
	var calls int32
	progress := func(int64, int64, time.Duration) {
		atomic.AddInt32(&calls, 1)
	}
	pr := &progressReader{
		underlying: strings.NewReader(strings.Repeat("a", 1024)),
		total:      1024,
		start:      time.Now(),
		progress:   progress,
	}
	// Force a tick by setting lastEmit into the past.
	pr.lastEmit = time.Now().Add(-time.Second)
	buf := make([]byte, 16)
	if _, err := pr.Read(buf); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if atomic.LoadInt32(&calls) == 0 {
		t.Errorf("progress not emitted after a forced-tick read")
	}
}

// TestInstall_HappyPath exercises the swap on a fresh target:
// staged → backup → copy. Verifies the on-disk target now
// contains the staged bytes and the .old backup contains the
// pre-install bytes.
func TestInstall_HappyPath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "nightme")
	originalBody := strings.Repeat("OLD-BINARY-", 200) // > 1 KiB
	if err := os.WriteFile(target, []byte(originalBody), 0o755); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	staged := filepath.Join(dir, "staged", "nightme")
	newBody := strings.Repeat("NEW-BINARY-", 250)
	if err := os.MkdirAll(filepath.Dir(staged), 0o700); err != nil {
		t.Fatalf("mkdir staged: %v", err)
	}
	if err := os.WriteFile(staged, []byte(newBody), 0o755); err != nil {
		t.Fatalf("seed staged: %v", err)
	}

	res, err := Install(staged, target)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.NewBinaryPath != target {
		t.Errorf("NewBinaryPath = %q, want %q", res.NewBinaryPath, target)
	}
	if res.OldBinaryPath != target+".old" {
		t.Errorf("OldBinaryPath = %q, want %q", res.OldBinaryPath, target+".old")
	}

	gotTarget, _ := os.ReadFile(target)
	if string(gotTarget) != newBody {
		t.Errorf("target body wrong after Install (got %q, want %q)", string(gotTarget)[:20], newBody[:20])
	}
	gotOld, _ := os.ReadFile(res.OldBinaryPath)
	if string(gotOld) != originalBody {
		t.Errorf("backup body wrong after Install (got %q, want %q)", string(gotOld)[:20], originalBody[:20])
	}

	// Mode must be 0755 (or at least include +x) on POSIX
	// systems. Windows ignores the executable bit (the
	// filesystem has no +x), so we skip the assertion there
	// — production behaviour is unchanged because the
	// install step always issues os.Chmod, which is a
	// no-op on Windows.
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(target)
		if st.Mode()&0o111 == 0 {
			t.Errorf("target mode = %v, want executable", st.Mode())
		}
	}
}

// TestInstall_RefusesSamePath covers the safety check: passing
// the same path for staged and target must error, never silently
// zero out the running binary.
func TestInstall_RefusesSamePath(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nightme")
	if err := os.WriteFile(p, []byte(strings.Repeat("X", 2048)), 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := Install(p, p)
	if err == nil {
		t.Fatal("Install(p, p) succeeded; want error")
	}
}

// TestInstall_RefusesSmallStaged pins the lower bound: a
// "binary" under 1 KiB is almost certainly wrong (truncated
// download, wrong asset, etc.) and Install must refuse.
func TestInstall_RefusesSmallStaged(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "nightme")
	staged := filepath.Join(dir, "staged", "nightme")
	if err := os.WriteFile(target, []byte(strings.Repeat("A", 2048)), 0o755); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(staged), 0o700); err != nil {
		t.Fatalf("mkdir staged: %v", err)
	}
	if err := os.WriteFile(staged, []byte("tiny"), 0o755); err != nil {
		t.Fatalf("seed staged: %v", err)
	}
	_, err := Install(staged, target)
	if err == nil || !strings.Contains(err.Error(), "suspiciously small") {
		t.Fatalf("expected 'suspiciously small' error, got %v", err)
	}
	// Target must be untouched.
	body, _ := os.ReadFile(target)
	if len(body) != 2048 {
		t.Errorf("target was modified by failed install: len=%d", len(body))
	}
}

// TestInstall_StaleOldRemovedIsNoOp covers the case where a
// previous install left behind a target.old and the new install
// succeeds: the stale .old is removed before the new rename,
// so we don't error with "file exists".
func TestInstall_StaleOldRemovedIsNoOp(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "nightme")
	if err := os.WriteFile(target, []byte(strings.Repeat("CURR", 1024)), 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Stale .old from a previous attempt.
	if err := os.WriteFile(target+".old", []byte("STALE"), 0o600); err != nil {
		t.Fatalf("seed old: %v", err)
	}

	staged := filepath.Join(dir, "staged", "nightme")
	if err := os.MkdirAll(filepath.Dir(staged), 0o700); err != nil {
		t.Fatalf("mkdir staged: %v", err)
	}
	if err := os.WriteFile(staged, []byte(strings.Repeat("NEW", 1024)), 0o755); err != nil {
		t.Fatalf("seed staged: %v", err)
	}

	if _, err := Install(staged, target); err != nil {
		t.Fatalf("Install: %v", err)
	}
	// Old .old should now contain "CURR", not "STALE".
	body, _ := os.ReadFile(target + ".old")
	if string(body) != strings.Repeat("CURR", 1024) {
		t.Errorf("backup body wrong: got %q", string(body)[:8])
	}
}

// TestInstall_MissingStaged covers the unhappy path: staged
// path doesn't exist. Target must remain untouched.
func TestInstall_MissingStaged(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "nightme")
	if err := os.WriteFile(target, []byte(strings.Repeat("D", 2048)), 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := Install(filepath.Join(dir, "does-not-exist"), target)
	if err == nil {
		t.Fatal("expected error for missing staged, got nil")
	}
	body, _ := os.ReadFile(target)
	if len(body) != 2048 {
		t.Errorf("target body length changed: %d", len(body))
	}
}

// TestDownload_EndToEnd spins up a fixture with a known
// asset body, runs Download, and asserts that:
//   - the staging file exists and matches the asset body
//   - SHA256 matches what the fixture published
//   - progress callback fired at least once
//
// This is the integration test that pins the full
// lookup → SHA256SUMS → asset download path against an
// httptest server mimicking api.github.com.
func TestDownload_EndToEnd(t *testing.T) {
	body := strings.Repeat("nightme-binary-payload-", 4096) // ~110 KiB
	f := newFixture(t, "v9.9.9", "9.9.9", body)

	// Override the Lookup URL builder by routing through
	// an env-style indirection is awkward; we instead
	// re-implement the relevant slice of Lookup inline
	// using a client pointed at the fixture. This pins the
	// contract for what Download consumes (a *Release +
	// asset) without depending on Lookup's hard-coded
	// api.github.com URL.
	rel := &Release{
		TagName: "v9.9.9",
		Assets: []Asset{
			{Name: "SHA256SUMS.txt", BrowserDownloadURL: f.srvURL() + "/repos/" + f.repo + "/asset/sums"},
			{Name: f.assetName, BrowserDownloadURL: f.srvURL() + "/repos/" + f.repo + "/asset/binary", Size: int64(len(body))},
		},
	}
	asset := MatchAsset(rel, "9.9.9")
	if asset == nil {
		t.Fatalf("MatchAsset returned nil for our fixture")
	}

	stagingDir := t.TempDir()
	var progressCalls int32
	progress := func(int64, int64, time.Duration) {
		atomic.AddInt32(&progressCalls, 1)
	}

	res, err := Download(context.Background(), rel, asset, stagingDir, progress)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if res == nil {
		t.Fatal("Download returned nil result")
	}
	if res.Asset.Name != f.assetName {
		t.Errorf("Asset.Name = %q, want %q", res.Asset.Name, f.assetName)
	}
	wantSum := sha256.Sum256([]byte(body))
	wantHex := hex.EncodeToString(wantSum[:])
	if res.SHA256Hex != wantHex {
		t.Errorf("SHA256Hex = %q, want %q", res.SHA256Hex, wantHex)
	}
	if res.Cached {
		t.Errorf("first Download reported Cached=true; want a live fetch")
	}

	// File on disk matches.
	gotBody, err := os.ReadFile(res.StagingPath)
	if err != nil {
		t.Fatalf("read staging file: %v", err)
	}
	if string(gotBody) != body {
		t.Errorf("staged body != fixture body (len=%d vs %d)", len(gotBody), len(body))
	}
	if atomic.LoadInt32(&progressCalls) == 0 {
		t.Errorf("progress never fired; expected at least one event")
	}

	// The asset download identifies itself the same way the
	// version checks do. This request is the one most likely to
	// cross a corporate proxy (GitHub redirects asset URLs to a
	// CDN host), so an anonymous "Go-http-client/1.1" here is the
	// hardest failure to diagnose.
	if got, want := f.lastAssetUserAgent(t), version.UserAgent(); got != want {
		t.Errorf("asset download User-Agent = %q, want %q", got, want)
	}
}

// TestDownload_SHA256Mismatch verifies the safety property:
// when the asset on disk does not match the published
// SHA256, Download cleans up the staging file and returns an
// error rather than silently installing a tampered binary.
func TestDownload_SHA256Mismatch(t *testing.T) {
	body := "real-body"
	f := newFixture(t, "v9.9.9", "9.9.9", body)

	rel := &Release{
		TagName: "v9.9.9",
		Assets: []Asset{
			{Name: "SHA256SUMS.txt", BrowserDownloadURL: f.srvURL() + "/repos/" + f.repo + "/asset/sums"},
			{Name: f.assetName, BrowserDownloadURL: f.srvURL() + "/repos/" + f.repo + "/asset/binary", Size: int64(len(body))},
		},
	}
	asset := MatchAsset(rel, "9.9.9")
	if asset == nil {
		t.Fatalf("MatchAsset returned nil")
	}

	// Replace the sums endpoint with a deliberately wrong
	// checksum so the post-download verify fails.
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/asset/sums"):
			_, _ = io.WriteString(w, "0000000000000000000000000000000000000000000000000000000000000000  "+f.assetName+"\n")
		case strings.HasSuffix(r.URL.Path, "/asset/binary"):
			_, _ = io.WriteString(w, body)
		default:
			http.NotFound(w, r)
		}
	})

	stagingDir := t.TempDir()
	_, err := Download(context.Background(), rel, asset, stagingDir, QuietProgress)
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("expected sha256 mismatch error, got %v", err)
	}
	// Staging file should have been removed.
	entries, _ := os.ReadDir(stagingDir)
	if len(entries) != 0 {
		t.Errorf("staging dir not cleaned up after mismatch: %v", entries)
	}
}

// TestDownload_ReusesVerifiedCache: a second Download of the
// same asset must hash the staged file against SHA256SUMS and
// skip the archive GET when they match.
func TestDownload_ReusesVerifiedCache(t *testing.T) {
	body := "cached-archive-body"
	f := newFixture(t, "v9.9.9", "9.9.9", body)
	rel := &Release{
		TagName: "v9.9.9",
		Assets: []Asset{
			{Name: "SHA256SUMS.txt", BrowserDownloadURL: f.srvURL() + "/repos/" + f.repo + "/asset/sums"},
			{Name: f.assetName, BrowserDownloadURL: f.srvURL() + "/repos/" + f.repo + "/asset/binary", Size: int64(len(body))},
		},
	}
	asset := MatchAsset(rel, "9.9.9")
	if asset == nil {
		t.Fatal("MatchAsset returned nil")
	}
	stagingDir := t.TempDir()

	first, err := Download(context.Background(), rel, asset, stagingDir, QuietProgress)
	if err != nil {
		t.Fatalf("first Download: %v", err)
	}
	if first.Cached {
		t.Fatal("first Download reported Cached=true")
	}

	var binaryHits int32
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/asset/sums"):
			_, _ = io.WriteString(w, f.sums)
		case strings.HasSuffix(r.URL.Path, "/asset/binary"):
			atomic.AddInt32(&binaryHits, 1)
			http.Error(w, "should not re-download", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	})

	second, err := Download(context.Background(), rel, asset, stagingDir, QuietProgress)
	if err != nil {
		t.Fatalf("second Download: %v", err)
	}
	if !second.Cached {
		t.Fatal("second Download should reuse the staged archive")
	}
	if second.SHA256Hex != first.SHA256Hex {
		t.Errorf("cached SHA256Hex = %q, want %q", second.SHA256Hex, first.SHA256Hex)
	}
	if atomic.LoadInt32(&binaryHits) != 0 {
		t.Errorf("archive was re-downloaded (%d GETs); sha256 match should skip it", binaryHits)
	}
}

// TestDownload_RedownloadsOnBadCache: a staged file whose
// hash does not match SHA256SUMS is overwritten, not trusted.
func TestDownload_RedownloadsOnBadCache(t *testing.T) {
	body := "real-body"
	f := newFixture(t, "v9.9.9", "9.9.9", body)
	rel := &Release{
		TagName: "v9.9.9",
		Assets: []Asset{
			{Name: "SHA256SUMS.txt", BrowserDownloadURL: f.srvURL() + "/repos/" + f.repo + "/asset/sums"},
			{Name: f.assetName, BrowserDownloadURL: f.srvURL() + "/repos/" + f.repo + "/asset/binary", Size: int64(len(body))},
		},
	}
	asset := MatchAsset(rel, "9.9.9")
	if asset == nil {
		t.Fatal("MatchAsset returned nil")
	}
	stagingDir := t.TempDir()
	stagingPath := filepath.Join(stagingDir, asset.Name)
	if err := os.MkdirAll(stagingDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(stagingPath, []byte("tampered-or-partial"), 0o600); err != nil {
		t.Fatalf("seed bad cache: %v", err)
	}

	res, err := Download(context.Background(), rel, asset, stagingDir, QuietProgress)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if res.Cached {
		t.Fatal("bad cache must not be reused")
	}
	got, err := os.ReadFile(res.StagingPath)
	if err != nil {
		t.Fatalf("read staged: %v", err)
	}
	if string(got) != body {
		t.Errorf("staged body = %q, want %q", got, body)
	}
}
