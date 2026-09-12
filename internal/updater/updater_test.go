package updater

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// --- URL composition --------------------------------------

func TestGitHubAssetURL(t *testing.T) {
	got := GitHubAssetURL("v0.5.0", "nightme_0.5.0_linux_amd64.tar.gz")
	want := "https://github.com/cnlangzi/nightme/releases/download/v0.5.0/nightme_0.5.0_linux_amd64.tar.gz"
	if got != want {
		t.Errorf("GitHubAssetURL = %q, want %q", got, want)
	}
}

func TestMirrorAssetURL(t *testing.T) {
	got := MirrorAssetURL("v0.5.0", "nightme_0.5.0_linux_amd64.tar.gz")
	want := "https://nightme.dev/downloads/0.5.0/nightme_0.5.0_linux_amd64.tar.gz"
	if got != want {
		t.Errorf("MirrorAssetURL = %q, want %q", got, want)
	}
	// Strip-v must also work on already-bare versions.
	if got := MirrorAssetURL("0.5.0", "f"); got != "https://nightme.dev/downloads/0.5.0/f" {
		t.Errorf("MirrorAssetURL on bare ver = %q", got)
	}
}

func TestAssetNameForRuntime(t *testing.T) {
	tests := []struct {
		ver, goos, goarch, want string
	}{
		{"0.5.0", "linux", "amd64", "nightme_0.5.0_linux_amd64.tar.gz"},
		{"0.5.0", "darwin", "arm64", "nightme_0.5.0_darwin_arm64.tar.gz"},
		{"0.5.0", "windows", "amd64", "nightme_0.5.0_windows_amd64.zip"},
		{"0.5.0", "windows", "arm64", "nightme_0.5.0_windows_arm64.zip"},
	}
	for _, tt := range tests {
		if got := AssetNameForRuntime(tt.ver, tt.goos, tt.goarch); got != tt.want {
			t.Errorf("AssetNameForRuntime(%q, %q, %q) = %q, want %q",
				tt.ver, tt.goos, tt.goarch, got, tt.want)
		}
	}
}

// --- formatters -------------------------------------------

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

// --- staging ----------------------------------------------

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

// --- extraction -------------------------------------------

func TestExtractArchive_WritesToStagingDir(t *testing.T) {
	dir := t.TempDir()
	stagingDir := filepath.Join(dir, "updates", "0.4.4")
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		t.Fatalf("mkdir staging: %v", err)
	}
	const body = "fake-binary-payload-12345"

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
		t.Fatalf("extractZIP returned %q; want %q", got, want)
	}

	gotBody, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("read extracted: %v", err)
	}
	if string(gotBody) != body {
		t.Fatalf("extracted body mismatch: got %q, want %q", gotBody, body)
	}

	if _, err := os.Stat(filepath.Join(".", "nightme.exe")); err == nil {
		t.Fatalf("found stray nightme.exe in cwd — ExtractArchive regressed to writing under \".\"")
	}
}

// --- progress reader -------------------------------------

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
	pr.lastEmit = time.Now().Add(-time.Second)
	buf := make([]byte, 16)
	if _, err := pr.Read(buf); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if atomic.LoadInt32(&calls) == 0 {
		t.Errorf("progress not emitted after a forced-tick read")
	}
}

// --- install ----------------------------------------------

func TestInstall_HappyPath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "nightme")
	originalBody := strings.Repeat("OLD-BINARY-", 200)
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
		t.Errorf("target body wrong after Install")
	}
	gotOld, _ := os.ReadFile(res.OldBinaryPath)
	if string(gotOld) != originalBody {
		t.Errorf("backup body wrong after Install")
	}

	if runtime.GOOS != "windows" {
		st, _ := os.Stat(target)
		if st.Mode()&0o111 == 0 {
			t.Errorf("target mode = %v, want executable", st.Mode())
		}
	}
}

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

func TestInstall_RefusesSmallStaged(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "nightme")
	staged := filepath.Join(dir, "staged", "nightme")
	if err := os.WriteFile(target, []byte(strings.Repeat("A", 2048)), 0o755); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(staged), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(staged, []byte("tiny"), 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := Install(staged, target)
	if err == nil || !strings.Contains(err.Error(), "suspiciously small") {
		t.Fatalf("expected 'suspiciously small' error, got %v", err)
	}
	body, _ := os.ReadFile(target)
	if len(body) != 2048 {
		t.Errorf("target was modified by failed install: len=%d", len(body))
	}
}

func TestInstall_StaleOldRemovedIsNoOp(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "nightme")
	if err := os.WriteFile(target, []byte(strings.Repeat("CURR", 1024)), 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.WriteFile(target+".old", []byte("STALE"), 0o600); err != nil {
		t.Fatalf("seed old: %v", err)
	}

	staged := filepath.Join(dir, "staged", "nightme")
	if err := os.MkdirAll(filepath.Dir(staged), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(staged, []byte(strings.Repeat("NEW", 1024)), 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := Install(staged, target); err != nil {
		t.Fatalf("Install: %v", err)
	}
	body, _ := os.ReadFile(target + ".old")
	if string(body) != strings.Repeat("CURR", 1024) {
		t.Errorf("backup body wrong: got %q", string(body)[:8])
	}
}

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

// --- DownloadTag (offline, mock-served) --------------------

// downloadFixture serves a GitHub-shaped release layout:
// /repos/<repo>/releases/tags/<tag> → sums + binary URLs.
// Plus the sums + binary endpoints themselves.
type downloadFixture struct {
	tag       string
	ver       string
	assetBody []byte
	assetName string
	sums      string
	srv       *httptest.Server
}

// buildArchiveForFixture packages assetBody into a tar.gz (unix)
// or zip (windows) archive containing a single `nightme` /
// `nightme.exe` file — matching what ExtractArchive expects.
func buildArchiveForFixture(t *testing.T, wantOS string, assetBody []byte) []byte {
	t.Helper()
	if wantOS == "windows" {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		w, err := zw.Create("nightme.exe")
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		_, _ = w.Write(assetBody)
		_ = zw.Close()
		return buf.Bytes()
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{
		Name: "nightme",
		Mode: 0o755,
		Size: int64(len(assetBody)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(assetBody); err != nil {
		t.Fatalf("tar body: %v", err)
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func newDownloadFixture(t *testing.T, tag, ver, assetBody string) *downloadFixture {
	t.Helper()
	wantOS, wantArch := runtime.GOOS, runtime.GOARCH
	ext := "tar.gz"
	if wantOS == "windows" {
		ext = "zip"
	}
	assetName := "nightme_" + ver + "_" + wantOS + "_" + wantArch + "." + ext
	archiveBytes := buildArchiveForFixture(t, wantOS, []byte(assetBody))

	sum := sha256.Sum256(archiveBytes)
	sumHex := hex.EncodeToString(sum[:])

	f := &downloadFixture{
		tag:       tag,
		ver:       ver,
		assetBody: archiveBytes,
		assetName: assetName,
		sums:      sumHex + "  " + assetName + "\n",
	}

	var srv *httptest.Server
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/releases/latest":
			fmt.Fprintf(w, `{"tag_name":%q}`, tag)
		case strings.HasSuffix(r.URL.Path, "/SHA256SUMS.txt"):
			fmt.Fprint(w, f.sums)
		case strings.HasSuffix(r.URL.Path, "/"+assetName):
			_, _ = w.Write(f.assetBody)
		default:
			http.NotFound(w, r)
		}
	})
	srv = httptest.NewServer(handler)
	f.srv = srv

	savedGH := GitHubDownloadBase
	savedND := NightMeDevAPIBase
	GitHubDownloadBase = srv.URL
	NightMeDevAPIBase = srv.URL
	t.Cleanup(func() {
		GitHubDownloadBase = savedGH
		NightMeDevAPIBase = savedND
		srv.Close()
	})
	return f
}

func TestDownloadTag_HappyPath(t *testing.T) {
	body := "hello-binary-body-" + strings.Repeat("x", 4096)
	f := newDownloadFixture(t, "v9.9.9", "9.9.9", body)

	dataDir := t.TempDir()
	dl, err := DownloadTag(context.Background(), dataDir, nil)
	if err != nil {
		t.Fatalf("DownloadTag: %v", err)
	}
	if dl.Source != "github" {
		t.Errorf("source = %q, want github", dl.Source)
	}
	if dl.Tag != "v9.9.9" {
		t.Errorf("tag = %q, want v9.9.9", dl.Tag)
	}
	if dl.AssetName != f.assetName {
		t.Errorf("AssetName = %q, want %q", dl.AssetName, f.assetName)
	}
	if len(dl.SHA256Hex) != 64 {
		t.Errorf("SHA256Hex = %q, want 64 hex chars", dl.SHA256Hex)
	}

	body2, err := os.ReadFile(dl.BinaryPath)
	if err != nil {
		t.Fatalf("read binary: %v", err)
	}
	if string(body2) != body {
		t.Errorf("binary content mismatch")
	}
}

func TestDownloadTag_GitHubFails_FallsBackToMirror(t *testing.T) {
	body := "small-body"
	f := newDownloadFixture(t, "v9.9.9", "9.9.9", body)

	// GitHub path must fail. Point it at a 503 stub.
	// MirrorDownloadBase must point at the fixture too —
	// otherwise it falls back to real nightme.dev, which
	// serves a v0.5.0 sums that doesn't list v9.9.9.
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ghSrv.Close()
	savedGH := GitHubDownloadBase
	savedM := MirrorDownloadBase
	GitHubDownloadBase = ghSrv.URL
	MirrorDownloadBase = f.srv.URL
	t.Cleanup(func() {
		GitHubDownloadBase = savedGH
		MirrorDownloadBase = savedM
	})

	dl, err := DownloadTag(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("DownloadTag: %v", err)
	}
	if dl.Source != "mirror" {
		t.Errorf("source = %q, want mirror", dl.Source)
	}
}

func TestDownloadTag_NoSumsFile_DegradesToSizeOnly(t *testing.T) {
	// GitHub returns 200 with empty body for sums, mirror
	// returns 404 for sums. Both should fail, and DownloadTag
	// should error rather than install an unverified binary.
	body := "test-body"
	wantOS, wantArch := runtime.GOOS, runtime.GOARCH
	ext := "tar.gz"
	if wantOS == "windows" {
		ext = "zip"
	}
	assetName := "nightme_9.9.9_" + wantOS + "_" + wantArch + "." + ext
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/tags/v9.9.9"),
			strings.HasSuffix(r.URL.Path, "/releases/latest"):
			fmt.Fprintf(w, `{"tag_name":"v9.9.9","assets":[]}`)
		case strings.HasSuffix(r.URL.Path, "/SHA256SUMS.txt"):
			w.WriteHeader(http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, "/"+assetName):
			_, _ = w.Write([]byte(body))
		}
	}))
	defer srv.Close()
	savedGH := GitHubDownloadBase
	savedM := MirrorDownloadBase
	GitHubDownloadBase = srv.URL
	MirrorDownloadBase = srv.URL
	t.Cleanup(func() {
		GitHubDownloadBase = savedGH
		MirrorDownloadBase = savedM
	})

	_, err := DownloadTag(context.Background(), t.TempDir(), nil)
	if err == nil {
		t.Fatal("expected error when sums file is missing on both sources")
	}
	if !strings.Contains(err.Error(), "download sums") {
		t.Errorf("error %q doesn't mention the sums download", err.Error())
	}
}

func TestDownloadTag_SHA256Mismatch(t *testing.T) {
	body := "real-body"
	ver := "9.9.9"
	tag := "v" + ver
	wantOS, wantArch := runtime.GOOS, runtime.GOARCH
	ext := "tar.gz"
	if wantOS == "windows" {
		ext = "zip"
	}
	assetName := "nightme_" + ver + "_" + wantOS + "_" + wantArch + "." + ext

	// Serve sums with WRONG hash so verify fails.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/tags/"+tag),
			strings.HasSuffix(r.URL.Path, "/releases/latest"):
			fmt.Fprintf(w, `{"tag_name":%q,"assets":[]}`, tag)
		case strings.HasSuffix(r.URL.Path, "/SHA256SUMS.txt"):
			fmt.Fprintf(w, "0000000000000000000000000000000000000000000000000000000000000000  %s\n", assetName)
		case strings.HasSuffix(r.URL.Path, "/"+assetName):
			_, _ = w.Write([]byte(body))
		}
	}))
	defer srv.Close()
	savedGH := GitHubDownloadBase
	savedM := MirrorDownloadBase
	savedND := NightMeDevAPIBase
	GitHubDownloadBase = srv.URL
	MirrorDownloadBase = srv.URL
	NightMeDevAPIBase = srv.URL
	t.Cleanup(func() {
		GitHubDownloadBase = savedGH
		MirrorDownloadBase = savedM
		NightMeDevAPIBase = savedND
	})

	_, err := DownloadTag(context.Background(), t.TempDir(), nil)
	if err == nil {
		t.Fatal("expected sha256 mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Errorf("error %q doesn't mention sha256 mismatch", err.Error())
	}
}

// --- helpers --------------------------------------------------

// ensure errors package is referenced (used by new sentinel below).
var _ = errors.New

// Keep the bytes import live (used elsewhere via fmt in helpers).
var _ = bytes.NewReader
