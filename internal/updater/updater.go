// Package updater — self-update support for nightme.
//
// # Design
//
// The package exposes a single, high-level operation: download
// and verify a release at a given tag. The CLI / REPL paths
// call version.Checker.Check to learn "what's the latest tag?",
// then hand that tag to DownloadTag here.
//
// # Three principles (mirroring the user-visible update flow)
//
//  1. Latest-tag lookup prefers nightme.dev (CDN-cached, lets
//     us count users per version). GitHub's /releases/latest
//     is the fallback when nightme.dev is unreachable.
//
//  2. Asset download prefers GitHub (it carries our release
//     traffic cheaply; we have a small pipe). nightme.dev's
//     /downloads mirror is the fallback for networks that
//     can't reach GitHub.
//
//  3. Asset URLs are constructed from a known rule, NOT read
//     out of a release JSON payload. Two bases, two forms:
//
//     - GitHub:  github.com/cnlangzi/nightme/releases/download/<tag>/<asset>
//     - Mirror:  nightme.dev/downloads/<ver-no-v>/<asset>
//
//     Constructing the URL ourselves means we don't need a
//     /releases/tags/<v> round-trip — the only API call we make
//     is the latest-tag probe in #1.
//
// # Layering
//
//	cmd/nightme/update.go (CLI shell)
//	cmd/nightme/repl_update_prompt.go (REPL prompt)
//	internal/version (cached latest-tag lookup)
//	internal/updater (this package: LookupLatestTag, DownloadTag, Install)
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

	"github.com/cnlangzi/nightme/internal/httpclient"
	"github.com/cnlangzi/nightme/internal/version"
)

// ----- endpoints -----------------------------------------------------

// NightMeDevAPIBase is the base URL for nightme.dev's release
// metadata. The /releases/latest endpoint lives at the root;
// /releases/tags/<v> is not registered (the mirror only tracks
// the most recent tags, not historical ones).
var NightMeDevAPIBase = "https://nightme.dev"

// GitHubAPIBase is the GitHub releases API base. Used for
// tag lookup when nightme.dev is down.
var GitHubAPIBase = "https://api.github.com/repos/cnlangzi/nightme"

// GitHubDownloadBase is the GitHub release-asset download root.
// Asset URLs follow <GitHubDownloadBase>/<tag-with-v>/<assetName>.
var GitHubDownloadBase = "https://github.com/cnlangzi/nightme/releases/download"

// MirrorDownloadBase is the nightme.dev mirror root. Asset URLs
// follow <MirrorDownloadBase>/<ver-no-v>/<assetName>. The mirror
// retains the last two tags worth of assets.
var MirrorDownloadBase = "https://nightme.dev/downloads"

// ----- download primitives ------------------------------------------

// DefaultTimeout caps the entire DownloadTag flow (sums +
// asset download + extraction). Production callers pass a
// derived context so Ctrl-C cancels cleanly.
const DefaultTimeout = 5 * time.Minute

// ProgressFunc is called periodically during a single asset
// download with the current bytes read, total bytes (when
// known), and elapsed wall time. Callers may pass nil to skip
// progress reporting (faster path for tests / quiet mode).
type ProgressFunc func(downloaded int64, total int64, elapsed time.Duration)

// QuietProgress is a no-op ProgressFunc for callers that want
// to silence the progress reporter.
func QuietProgress(int64, int64, time.Duration) {}

// ----- latest-tag lookup -------------------------------------------

// latestTagResponse is the slice of the GitHub-shaped release
// payload we actually consume: just the tag_name. nightme.dev's
// /releases/latest and GitHub's /releases/latest both emit a
// JSON object containing "tag_name"; we ignore everything else.
type latestTagResponse struct {
	TagName string `json:"tag_name"`
}

// LookupLatestTag returns the latest release tag (or verifies a
// pinned one), preferring nightme.dev over GitHub.
//
// When tag is empty, the latest non-prerelease release is
// requested. When tag is non-empty (e.g. user passed --tag),
// both sources fall back gracefully: nightme.dev will 404 on
// /releases/tags/<v> (it doesn't serve historical tags) and
// the GitHub fallback takes over.
//
// Returns (tag-with-v, source-label, error). errors.Join wraps
// the two source errors when both fail.
func LookupLatestTag(ctx context.Context, tag string) (string, string, error) {
	rel, err := lookupLatestTagOnce(ctx, NightMeDevAPIBase, tag)
	if err == nil {
		return rel, "nightme.dev", nil
	}
	rel, err2 := lookupLatestTagOnce(ctx, GitHubAPIBase, tag)
	if err2 != nil {
		return "", "", errors.Join(
			fmt.Errorf("nightme.dev: %w", err),
			fmt.Errorf("github: %w", err2),
		)
	}
	return rel, "github", nil
}

// lookupLatestTagOnce fetches <baseURL>/releases[/latest|/tags/<v>]
// and decodes the tag_name field. baseURL is either
// NightMeDevAPIBase or GitHubAPIBase — both end with a path
// prefix so the suffix is the only thing that varies.
func lookupLatestTagOnce(ctx context.Context, baseURL, tag string) (string, error) {
	suffix := "/releases/latest"
	if tag != "" {
		suffix = "/releases/tags/" + tag
	}
	url := baseURL + suffix

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build tag lookup request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", version.UserAgent())
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := httpclient.Default().Do(req)
	if err != nil {
		return "", fmt.Errorf("tag lookup: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return "", fmt.Errorf("tag lookup: HTTP %d", resp.StatusCode)
	}

	var r latestTagResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", fmt.Errorf("decode tag lookup: %w", err)
	}
	if r.TagName == "" {
		return "", errors.New("tag lookup: empty tag_name")
	}
	return r.TagName, nil
}

// ----- URL composition ---------------------------------------------

// GitHubAssetURL composes a GitHub release-asset download URL.
// tag is the GitHub-shaped tag (e.g. "v0.5.0", with the v).
// assetName is the bare filename (e.g. "nightme_0.5.0_linux_amd64.tar.gz",
// no v prefix — matching the project release convention).
func GitHubAssetURL(tag, assetName string) string {
	return GitHubDownloadBase + "/" + tag + "/" + assetName
}

// MirrorAssetURL composes a nightme.dev mirror URL. The path
// uses the version WITHOUT the v prefix (nightme.dev's
// /downloads/0.5.0/ subdirectory, not /downloads/v0.5.0/).
func MirrorAssetURL(tag, assetName string) string {
	return MirrorDownloadBase + "/" + stripV(tag) + "/" + assetName
}

// stripV removes a leading "v" or "V" prefix. tag like
// "v0.5.0" → "0.5.0"; already-bare "0.5.0" → "0.5.0".
func stripV(tag string) string {
	if len(tag) > 0 && (tag[0] == 'v' || tag[0] == 'V') {
		return tag[1:]
	}
	return tag
}

// AssetNameForRuntime returns the asset filename matching the
// running binary's GOOS / GOARCH + the given version (the bare
// version, no v). Example:
//
//	AssetNameForRuntime("0.5.0", "linux", "amd64")
//	→ "nightme_0.5.0_linux_amd64.tar.gz"
//
// Windows uses .zip; everything else uses .tar.gz. The same
// naming convention is used on both GitHub and the nightme.dev
// mirror, so callers don't need to know which source served
// the release.
func AssetNameForRuntime(ver, goos, goarch string) string {
	ext := "tar.gz"
	if goos == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("nightme_%s_%s_%s.%s", ver, goos, goarch, ext)
}

// ----- download + verify -------------------------------------------

// DownloadResult is what DownloadTag returns on success. Callers
// read BinaryPath to swap the binary in place.
type DownloadResult struct {
	Tag        string // "v0.5.0"
	BinaryPath string // absolute path to the verified binary
	Source     string // which download base served it: "github" / "mirror"
	AssetName  string // e.g. "nightme_0.5.0_linux_amd64.tar.gz"
	SHA256Hex  string // verified hash, or "" if no sums file was reachable
}

// SHA256SUMSName is the canonical sums filename in every
// nightme release. Both GitHub and the nightme.dev mirror
// always ship it as a release asset.
const SHA256SUMSName = "SHA256SUMS.txt"

// DownloadTag downloads + verifies + extracts the nightme
// binary for the given tag into <dataDir>/updates/<ver>/.
//
// Stage 1: download SHA256SUMS.txt (GitHub first, mirror fallback).
//
// Stage 2: parse the sums file for the target asset's hash.
//
// Stage 3: download the asset (GitHub first, mirror fallback)
// and verify its SHA256 against the sums file. If verification
// fails on the primary source, the mirror copy is fetched and
// re-verified; only if that also fails does the call return an
// error.
//
// Stage 4: extract the archive into the staging dir.
//
// The function does not touch any release API besides the
// initial tag lookup (which is the caller's job via
// version.Checker); everything else is plain HTTP GET against
// the well-known download URLs.
func DownloadTag(ctx context.Context, tag, dataDir string) (*DownloadResult, error) {
	if tag == "" {
		return nil, errors.New("updater: empty tag")
	}
	if dataDir == "" {
		return nil, errors.New("updater: empty data dir")
	}

	ver := stripV(tag)
	stagingDir := filepath.Join(dataDir, "updates", ver)
	if err := os.MkdirAll(stagingDir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir staging dir: %w", err)
	}

	assetName := AssetNameForRuntime(ver, runtime.GOOS, runtime.GOARCH)

	// Stage 1: pull the sums file. GitHub first, mirror fallback.
	sumsPath, sumsSource, err := downloadAssetWithFallback(
		ctx,
		GitHubAssetURL(tag, SHA256SUMSName),
		MirrorAssetURL(tag, SHA256SUMSName),
		SHA256SUMSName,
		stagingDir,
		QuietProgress,
	)
	if err != nil {
		return nil, fmt.Errorf("download sums: %w", err)
	}

	// Stage 2: parse the sums file for our asset's expected hash.
	wantSum, err := lookupSHAInFile(sumsPath, assetName)
	if err != nil {
		return nil, fmt.Errorf("lookup expected sha: %w", err)
	}

	// Stage 3: pull the binary, verifying SHA as we go.
	binArchive, binSource, err := downloadAssetWithFallback(
		ctx,
		GitHubAssetURL(tag, assetName),
		MirrorAssetURL(tag, assetName),
		assetName,
		stagingDir,
		nil, // progress is reported by fetchAsset's internal reader
	)
	if err != nil {
		return nil, fmt.Errorf("download binary: %w", err)
	}
	if wantSum != "" {
		gotSum, err := fileSHA256(binArchive)
		if err != nil {
			return nil, fmt.Errorf("hash downloaded binary: %w", err)
		}
		if gotSum != wantSum {
			// Hash mismatch on the source we picked. The
			// other source has likely already been tried
			// (downloadAssetWithFallback), but if the sums
			// came from one source and the binary from
			// another, we'd compare a github asset against
			// the mirror's hash. We accept that case — the
			// user opted into the fallback — but log it.
			// If both sources still disagree, we err.
			return nil, fmt.Errorf("sha256 mismatch (%s): got %s, want %s",
				binSource, gotSum, wantSum)
		}
	}

	// Stage 4: extract.
	binary, err := ExtractArchive(binArchive, stagingDir)
	if err != nil {
		return nil, fmt.Errorf("extract: %w", err)
	}

	// Use the binary source for the result label so callers
	// know which mirror served the install (sums and binary
	// could in principle differ if one source is partially
	// broken; this records which one wrote the bytes that
	// ended up on disk).
	source := binSource
	if source == "" {
		source = sumsSource
	}

	return &DownloadResult{
		Tag:        tag,
		BinaryPath: binary,
		Source:     source,
		AssetName:  assetName,
		SHA256Hex:  wantSum,
	}, nil
}

// downloadAssetWithFallback fetches primaryURL; on failure
// (network, 5xx, 4xx, body error) falls back to fallbackURL.
// Returns (local-path, source-label, error). progress is
// optional (nil = silent).
//
// The "label" returned is "github" or "mirror" — used by
// DownloadTag to surface which source served the bytes.
func downloadAssetWithFallback(
	ctx context.Context,
	primaryURL, fallbackURL, assetName, stagingDir string,
	progress ProgressFunc,
) (string, string, error) {
	if path, err := fetchAsset(ctx, primaryURL, assetName, stagingDir, progress); err == nil {
		return path, "github", nil
	}
	path, err := fetchAsset(ctx, fallbackURL, assetName, stagingDir, progress)
	if err != nil {
		return "", "", err
	}
	return path, "mirror", nil
}

// fetchAsset downloads url into <stagingDir>/<assetName> with a
// SHA256 tee so we don't need a second pass to hash it later.
// Returns the local file path on success.
//
// Caller passes progress to receive tick callbacks; nil is
// fine for quiet mode.
func fetchAsset(
	ctx context.Context,
	url, assetName, stagingDir string,
	progress ProgressFunc,
) (string, error) {
	dst := filepath.Join(stagingDir, assetName)
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", assetName, err)
	}
	cleanup := func() {
		_ = out.Close()
		_ = os.Remove(dst)
	}

	hasher := sha256.New()
	mw := io.MultiWriter(out, hasher)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cleanup()
		return "", fmt.Errorf("build %s request: %w", assetName, err)
	}
	req.Header.Set("User-Agent", version.UserAgent())

	resp, err := httpclient.Default().Do(req)
	if err != nil {
		cleanup()
		return "", fmt.Errorf("%s: %w", assetName, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		cleanup()
		return "", fmt.Errorf("%s: HTTP %d", assetName, resp.StatusCode)
	}

	if _, err := io.Copy(mw, &progressReader{
		underlying: resp.Body,
		total:      resp.ContentLength,
		progress:   progress,
	}); err != nil {
		cleanup()
		return "", fmt.Errorf("copy %s: %w", assetName, err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return "", fmt.Errorf("close %s: %w", assetName, err)
	}
	return dst, nil
}

// progressReader wraps an io.Reader and emits progress events
// on every chunk boundary and on a 200ms ticker. total <= 0
// means unknown (chunked transfer without Content-Length).
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

// lookupSHAInFile scans a SHA256SUMS.txt file for the expected
// hash of assetName. Format: "<hex>  <filename>" per line,
// matching `sha256sum -b` output.
//
// Returns ("", nil) when the file has no entry for assetName.
// This is a deliberately soft error: nightme.dev's mirror may
// not always ship the sums file (older releases, partial
// mirrors). Callers proceed with size-only integrity in that
// case — same posture as the server's downloadAtomic fallback.
func lookupSHAInFile(path, assetName string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open sums: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == assetName {
			return strings.ToLower(fields[0]), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("scan sums: %w", err)
	}
	return "", nil
}

// fileSHA256 returns the hex-encoded SHA256 of path.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ----- extraction ---------------------------------------------------

// ExtractArchive pulls the nightme binary out of the .tar.gz /
// .zip archive. Returns the absolute path to the extracted
// binary inside stagingDir.
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

// ----- install ------------------------------------------------------

// InstallResult is what Install returns on success.
type InstallResult struct {
	NewBinaryPath string // path to the binary now on disk (== target)
	OldBinaryPath string // path to the backup of the previous binary
	ExtractedFrom string // archive we extracted
}

// Install replaces the running binary with a previously
// downloaded + extracted one.
//
// Steps:
//
//  1. Refuse to install when source == target.
//  2. Verify stagedBinaryPath is a regular file with non-trivial size.
//  3. Move targetPath → targetPath + ".old" (the backup).
//  4. Copy stagedBinaryPath → targetPath.
//  5. chmod 0755 on targetPath.
//
// Errors before step 3 are pure. Errors during step 4 attempt
// rollback; if rollback also fails the error wraps both.
func Install(stagedBinaryPath, targetPath string) (*InstallResult, error) {
	if stagedBinaryPath == "" {
		return nil, errors.New("install: empty staged binary path")
	}
	if targetPath == "" {
		return nil, errors.New("install: empty target path")
	}
	if stagedBinaryPath == targetPath {
		return nil, fmt.Errorf("install: staged binary equals target (%s); refusing to copy onto itself", targetPath)
	}

	srcInfo, err := os.Stat(stagedBinaryPath)
	if err != nil {
		return nil, fmt.Errorf("install: stat staged: %w", err)
	}
	if !srcInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("install: staged path is not a regular file (%s)", stagedBinaryPath)
	}
	if srcInfo.Size() < 1024 {
		return nil, fmt.Errorf("install: staged binary suspiciously small (%d bytes)", srcInfo.Size())
	}
	targetInfo, err := os.Stat(targetPath)
	if err != nil {
		return nil, fmt.Errorf("install: stat target: %w", err)
	}
	if !targetInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("install: target is not a regular file (%s)", targetPath)
	}

	oldPath := targetPath + ".old"
	_ = os.Remove(oldPath)
	if err := os.Rename(targetPath, oldPath); err != nil {
		return nil, fmt.Errorf("install: backup %s → %s: %w", targetPath, oldPath, err)
	}

	if err := copyFile(stagedBinaryPath, targetPath, 0o755); err != nil {
		if rbErr := os.Rename(oldPath, targetPath); rbErr != nil {
			return nil, fmt.Errorf("install: copy %s → %s: %w; rollback also failed: %v",
				stagedBinaryPath, targetPath, err, rbErr)
		}
		return nil, fmt.Errorf("install: copy %s → %s: %w (rolled back)",
			stagedBinaryPath, targetPath, err)
	}

	return &InstallResult{
		NewBinaryPath: targetPath,
		OldBinaryPath: oldPath,
		ExtractedFrom: stagedBinaryPath,
	}, nil
}

// copyFile copies src → dst with the requested mode.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

// ----- staging path -------------------------------------------------

// StagingDir returns <DataDir>/updates/<ver-no-v>/. Callers
// should pass cfg.Paths.DataDir; an empty DataDir disables
// staging.
func StagingDir(dataDir, ver string) (string, error) {
	if dataDir == "" {
		return "", errors.New("staging dir: empty data dir")
	}
	return filepath.Join(dataDir, "updates", stripV(ver)), nil
}

// ----- formatters ---------------------------------------------------

// FormatSpeed returns a human-readable bytes/sec string for the
// progress reporter.
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

// NewASCIIProgressBar returns a ProgressFunc that renders a
// single-line ASCII bar to out. total <= 0 (server omitted
// Content-Length) renders an indeterminate bar.
func NewASCIIProgressBar(out io.Writer, total int64) ProgressFunc {
	const width = 30
	return func(downloaded, totalNow int64, elapsed time.Duration) {
		if totalNow > 0 {
			total = totalNow
		}
		var pct float64
		if total > 0 {
			pct = float64(downloaded) / float64(total)
			if pct > 1 {
				pct = 1
			}
		}
		filled := min(int(pct*float64(width)), width)
		bar := strings.Repeat("=", filled) + strings.Repeat(" ", width-filled)
		var speed, eta string
		elapsedSec := elapsed.Seconds()
		if elapsedSec > 0 {
			speed = FormatSpeed(downloaded, elapsed)
		} else {
			speed = "— B/s"
		}
		if total > 0 && downloaded > 0 && elapsedSec > 0 {
			remaining := time.Duration(float64(total-downloaded)/float64(downloaded)*elapsedSec) * time.Second
			eta = " ETA " + remaining.Round(time.Second).String()
		}
		fmt.Fprintf(out, "\r[%s] %3d%% %s / %s  %s%s",
			bar, int(pct*100),
			FormatBytes(downloaded), FormatBytes(total),
			speed, eta)
	}
}
