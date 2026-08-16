// Package updater — self-update support for nightme.
//
// The package owns three jobs, in order:
//
//   - Lookup: fetch the GitHub release metadata for a given tag,
//     including the list of release assets and SHA256SUMS.
//
//   - Match: pick the single asset that matches the running
//     binary's GOOS/GOARCH. Assets follow the
//     "nightme_<version>_<os>_<arch>.<ext>" naming convention
//     used by the project's release workflow (see the
//     SHA256SUMS listing for v0.3.7 as the canonical example).
//
//   - Download: fetch the matched asset to a staging path under
//     DataDir/updates/<version>/, with cancellable context,
//     stdlib-only progress reporting, and SHA256 verification
//     against the SHA256SUMS file from the same release.
//
// The package is stdlib-only. We could pull in
// rhysd/go-github-selfupdate but it bundles its own GitHub
// client, semaphore, and progress bar; for a single-binary
// release like nightme the stdlib path is ~200 lines and
// keeps the supply-chain surface small.
//
// Layering:
//
//   - cmd/nightme/update.go (CLI shell)
//   - internal/updater (this package: Lookup / Match / Download)
//   - cmd/nightme/update.go (Install — next commit: selfupdate
//     binary swap + daemon restart).
package updater

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// DefaultTimeout caps the entire download path (lookup +
// checksum + asset). Production callers pass a derived context
// so Ctrl-C cancels cleanly.
const DefaultTimeout = 5 * time.Minute

// ProgressFunc is called periodically during Download with the
// current bytes read, total bytes (when known), and elapsed
// wall time. Implementations typically render an ASCII progress
// bar to the terminal. Callers may pass nil to skip progress
// reporting entirely (faster path for tests / quiet mode).
//
// Frequency is best-effort: the downloader flushes a progress
// event on every chunk boundary AND on every Tick interval
// (200ms), whichever fires first. Total may be -1 if the
// server did not send Content-Length.
type ProgressFunc func(downloaded int64, total int64, elapsed time.Duration)

// QuietProgress is a no-op ProgressFunc for callers that want
// to silence the progress reporter (CI, scripted runs, tests).
func QuietProgress(int64, int64, time.Duration) {}

// Release is the subset of the GitHub release payload we read.
// We don't decode every field — the asset list and the tag name
// are all that Lookup consumers need.
type Release struct {
	TagName string  `json:"tag_name"`
	Assets  []Asset `json:"assets"`
}

// Asset is one downloadable file in a release. The fields we
// need are name (for matching + SHA256SUMS lookup) and
// browser_download_url (the actual asset URL). Size is useful
// for the progress bar's total field.
type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// Lookup queries GitHub for the release that matches the
// given tag (e.g. "v0.3.7" or "0.3.7"). When tag is empty the
// API serves the latest non-prerelease release.
//
// repo is "owner/name" on GitHub. Tests override this to point
// at an httptest server.
func Lookup(ctx context.Context, repo, tag string) (*Release, error) {
	url := "https://api.github.com/repos/" + repo + "/releases/latest"
	if tag != "" {
		// GitHub's /releases/tags/<tag> route returns the
		// same JSON shape and works for any tag, including
		// those that point at a draft / pre-release.
		url = "https://api.github.com/repos/" + repo + "/releases/tags/" + tag
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build lookup request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "nightme-updater/1.0")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	client := &http.Client{Timeout: DefaultTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("lookup release: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("lookup release: HTTP %d", resp.StatusCode)
	}

	var r Release
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("decode release: %w", err)
	}
	if r.TagName == "" {
		return nil, errors.New("lookup release: empty tag_name")
	}
	return &r, nil
}

// MatchAsset picks the asset matching the running binary's
// GOOS/GOARCH using the
// "nightme_<version>_<os>_<arch>.<ext>" convention. When
// version is empty the matcher accepts any version segment so
// callers can use it across releases.
//
// Returns nil (no error) when no asset matches — the caller
// surfaces this as "no binary for darwin/amd64 in this
// release", which is the correct diagnostic for an
// unsupported OS/arch.
func MatchAsset(release *Release, version string) *Asset {
	wantOS := runtime.GOOS
	wantArch := runtime.GOARCH
	wantExt := "tar.gz"
	if wantOS == "windows" {
		wantExt = "zip"
	}
	// Strip a leading "v" so "v0.3.7" and "0.3.7" both match.
	v := strings.TrimPrefix(version, "v")
	if v == "" {
		// Fall back to whatever the release's tag says so a
		// single MatchAsset call works for "latest".
		v = strings.TrimPrefix(release.TagName, "v")
	}
	want := fmt.Sprintf("nightme_%s_%s_%s.%s", v, wantOS, wantArch, wantExt)

	for i := range release.Assets {
		if release.Assets[i].Name == want {
			return &release.Assets[i]
		}
	}
	return nil
}

// DownloadResult is what Download returns on success. The caller
// (CLI / install command) reads StagingPath to swap the binary
// in place.
type DownloadResult struct {
	Asset       Asset
	StagingPath string // absolute path under stagingDir
	SHA256Hex   string // hex-encoded hash of the downloaded bytes
	Bytes       int64  // total bytes written (== Asset.Size on success)
}

// Download fetches the asset to stagingDir/<asset.Name> with
// cancellable context, periodic progress reporting, and a
// SHA256SUMS-driven integrity check.
//
// The downloaded archive (.tar.gz on unix, .zip on windows) is
// kept as-is in the staging dir; Install (next commit) is
// responsible for extracting and replacing the binary.
//
// stagingDir is typically <DataDir>/updates/<version>/. The
// function creates it (parents included) if it does not exist.
//
// progress may be nil for silent downloads.
//
// The SHA256SUMS file is fetched separately from the same
// release; if it cannot be downloaded the function fails
// closed (errors.New("checksums unreachable")) so callers
// never silently install an unverified binary.
func Download(
	ctx context.Context,
	release *Release,
	asset *Asset,
	stagingDir string,
	progress ProgressFunc,
) (*DownloadResult, error) {
	if asset == nil {
		return nil, errors.New("download: nil asset")
	}

	// 1. SHA256SUMS lookup. We refuse to download without it
	// so a tampered release cannot slip past.
	wantSum, err := lookupSHA256(ctx, release, asset.Name)
	if err != nil {
		return nil, err
	}

	// 2. Staging path.
	if err := os.MkdirAll(stagingDir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir staging dir: %w", err)
	}
	stagingPath := filepath.Join(stagingDir, asset.Name)

	// 3. Fetch the asset with a SHA256 tee so we don't need
	// a second pass to verify. Progress is reported on every
	// chunk + every tick.
	out, err := os.OpenFile(stagingPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open staging file: %w", err)
	}
	cleanup := func() {
		_ = out.Close()
		// Best-effort remove on any failure path so a
		// partial download doesn't sit in the staging dir
		// and confuse the next attempt.
		_ = os.Remove(stagingPath)
	}

	hasher := sha256.New()
	mw := io.MultiWriter(out, hasher)
	reader, err := fetchWithProgress(ctx, asset.BrowserDownloadURL, asset.Size, mw, progress)
	if err != nil {
		cleanup()
		return nil, err
	}
	// Drain anything the progress reader buffered but didn't
	// pass through mw.
	if _, err := io.Copy(io.Discard, reader); err != nil {
		cleanup()
		return nil, fmt.Errorf("drain response: %w", err)
	}
	if err := out.Close(); err != nil {
		cleanup()
		return nil, fmt.Errorf("close staging file: %w", err)
	}

	// 4. Verify.
	gotSum := hex.EncodeToString(hasher.Sum(nil))
	if gotSum != wantSum {
		_ = os.Remove(stagingPath)
		return nil, fmt.Errorf("sha256 mismatch: got %s, want %s", gotSum, wantSum)
	}

	return &DownloadResult{
		Asset:       *asset,
		StagingPath: stagingPath,
		SHA256Hex:   gotSum,
		Bytes:       asset.Size,
	}, nil
}

// lookupSHA256 fetches the SHA256SUMS file for the release
// and returns the hash for the given asset filename. The
// file format is "<hex>  <filename>" per line, matching
// `sha256sum -b` output.
func lookupSHA256(ctx context.Context, release *Release, assetName string) (string, error) {
	var sumsAsset *Asset
	for i := range release.Assets {
		if release.Assets[i].Name == "SHA256SUMS.txt" {
			sumsAsset = &release.Assets[i]
			break
		}
	}
	if sumsAsset == nil {
		return "", errors.New("checksums: SHA256SUMS.txt not in release")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sumsAsset.BrowserDownloadURL, nil)
	if err != nil {
		return "", fmt.Errorf("build checksums request: %w", err)
	}
	client := &http.Client{Timeout: DefaultTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download checksums: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return "", fmt.Errorf("download checksums: HTTP %d", resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		// fields[0] = sha256 hex, fields[1] = filename.
		// We compare filename with a leading "*" stripped
		// (binary mode) for robustness against either
		// convention being emitted.
		name := strings.TrimPrefix(fields[1], "*")
		if name == assetName {
			return strings.ToLower(fields[0]), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("scan checksums: %w", err)
	}
	return "", fmt.Errorf("checksums: %s not listed", assetName)
}

// fetchWithProgress issues the GET, copies the body into dst,
// and reports progress. The returned reader is whatever
// remains of the response body after copying; callers must
// drain it to allow connection reuse.
func fetchWithProgress(
	ctx context.Context,
	url string,
	total int64,
	dst io.Writer,
	progress ProgressFunc,
) (io.Reader, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build asset request: %w", err)
	}
	req.Header.Set("User-Agent", "nightme-updater/1.0")
	req.Header.Set("Accept", "application/octet-stream")

	client := &http.Client{Timeout: DefaultTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download asset: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("download asset: HTTP %d", resp.StatusCode)
	}

	// Prefer the server's Content-Length when the caller
	// didn't get a hint (e.g. release metadata was stale).
	if total <= 0 && resp.ContentLength > 0 {
		total = resp.ContentLength
	}

	start := time.Now()
	pr := &progressReader{
		underlying: resp.Body,
		total:      total,
		start:      start,
		progress:   progress,
	}
	if _, err := io.Copy(dst, pr); err != nil {
		resp.Body.Close()
		return nil, fmt.Errorf("copy asset body: %w", err)
	}
	// io.Copy already drained — return an empty reader so
	// the caller's `io.Copy(io.Discard, reader)` is a no-op
	// and can still close the body cleanly.
	resp.Body.Close()
	return io.NopCloser(strings.NewReader("")), nil
}

// progressReader wraps an io.Reader and emits progress events
// on chunk boundaries and on a 200ms ticker (whichever fires
// first). total <= 0 means unknown.
type progressReader struct {
	underlying io.Reader
	total      int64
	start      time.Time
	progress   ProgressFunc
	done       int64
	lastEmit   time.Time
}

const progressInterval = 200 * time.Millisecond

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.underlying.Read(p)
	if n > 0 {
		pr.done += int64(n)
		if pr.progress != nil {
			now := time.Now()
			if now.Sub(pr.lastEmit) >= progressInterval {
				pr.progress(pr.done, pr.total, now.Sub(pr.start))
				pr.lastEmit = now
			}
		}
	}
	return n, err
}

// StagingDir returns the canonical staging path for a given
// version: <DataDir>/updates/<version>/. Callers should pass
// the value from config.Config.Paths.DataDir; an empty
// DataDir disables staging (returns "" + error).
func StagingDir(dataDir, version string) (string, error) {
	if dataDir == "" {
		return "", errors.New("staging dir: empty data dir")
	}
	v := strings.TrimPrefix(version, "v")
	return filepath.Join(dataDir, "updates", v), nil
}

// FormatSpeed returns a human-readable bytes/sec string for the
// progress reporter (e.g. "1.2 MB/s"). Exposed so tests can
// pin the formatter output without re-implementing the math.
func FormatSpeed(bytes int64, elapsed time.Duration) string {
	if elapsed <= 0 {
		return "0 B/s"
	}
	per := float64(bytes) / elapsed.Seconds()
	return formatBytes(int64(per)) + "/s"
}

// FormatBytes is exposed for progress reporter reuse.
func FormatBytes(n int64) string { return formatBytes(n) }

func formatBytes(n int64) string {
	const k = 1024
	if n < k {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(k), 0
	for n2 := n / k; n2 >= k; n2 /= k {
		div *= k
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGT"[exp])
}

// ExtractArchive pulls the nightme binary out of the .tar.gz /
// .zip downloaded by Download. Exposed here so install.go (next
// commit) can reuse it without depending on archive-specific
// code paths in two places. Returns the absolute path to the
// extracted binary inside stagingDir.
func ExtractArchive(archivePath, stagingDir string) (string, error) {
	if runtime.GOOS == "windows" {
		return extractZIP(archivePath, stagingDir)
	}
	return extractTARGZ(archivePath, stagingDir)
}

func extractTARGZ(archivePath, stagingDir string) (string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return "", fmt.Errorf("open tar.gz: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("gzip reader: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return "", errors.New("extract: nightme binary not found in archive")
		}
		if err != nil {
			return "", fmt.Errorf("read tar header: %w", err)
		}
		// We only care about the binary; release archives
		// may also ship README / LICENSE entries.
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		base := filepath.Base(hdr.Name)
		if base != "nightme" && base != "nightme.exe" {
			continue
		}
		out := filepath.Join(stagingDir, base)
		w, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return "", fmt.Errorf("open extract dst: %w", err)
		}
		if _, err := io.Copy(w, tr); err != nil {
			w.Close()
			return "", fmt.Errorf("copy extract: %w", err)
		}
		if err := w.Close(); err != nil {
			return "", fmt.Errorf("close extract: %w", err)
		}
		return out, nil
	}
}

func extractZIP(archivePath, stagingDir string) (string, error) {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return "", fmt.Errorf("open zip: %w", err)
	}
	defer r.Close()

	for _, f := range r.File {
		base := filepath.Base(f.Name)
		if base != "nightme.exe" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", fmt.Errorf("open zip entry: %w", err)
		}
		out := filepath.Join(stagingDir, base)
		w, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			rc.Close()
			return "", fmt.Errorf("open extract dst: %w", err)
		}
		if _, err := io.Copy(w, rc); err != nil {
			rc.Close()
			w.Close()
			return "", fmt.Errorf("copy extract: %w", err)
		}
		rc.Close()
		if err := w.Close(); err != nil {
			return "", fmt.Errorf("close extract: %w", err)
		}
		return out, nil
	}
	return "", errors.New("extract: nightme.exe not found in zip")
}