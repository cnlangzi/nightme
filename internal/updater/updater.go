// Package updater — self-update support for nightme.
//
// # Design
//
// The package exposes two operations:
//
//   - LookupLatestTag: probe the release feed for the current
//     latest tag. nightme.dev first (CDN-cached, lets us count
//     users per version), GitHub /releases/latest fallback when
//     nightme.dev is unreachable.
//
//   - DownloadTag: fetch + SHA256-verify + extract the latest
//     binary. URL composition is rule-based, no API call —
//     GitHub first, nightme.dev mirror fallback.
//
// # Why no --tag / pinned versions
//
// nightme.dev only retains the last two tags' worth of
// assets, and the user-facing CLI always upgrades to the
// latest release. There's no production scenario for
// installing a historical version, so the API stays
// pinned-tag-free.
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
	"slices"
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
// LookupLatestTag probes the release feed for the current latest
// tag. nightme.dev first (CDN-cached, lets us count users per
// version); GitHub /releases/latest fallback when nightme.dev is
// unreachable.
//
// Returns the tag (e.g. "v0.5.0", with the v), the source label
// ("nightme.dev" or "github"), and any error. errors.Join wraps
// the two source errors when both fail.
func LookupLatestTag(ctx context.Context) (string, string, error) {
	rel, err := lookupLatestTagOnce(ctx, NightMeDevAPIBase)
	if err == nil {
		return rel, "nightme.dev", nil
	}
	rel, err2 := lookupLatestTagOnce(ctx, GitHubAPIBase)
	if err2 != nil {
		return "", "", errors.Join(
			fmt.Errorf("nightme.dev: %w", err),
			fmt.Errorf("github: %w", err2),
		)
	}
	return rel, "github", nil
}

// lookupLatestTagOnce fetches <baseURL>/releases/latest and
// decodes the tag_name field. baseURL is either
// NightMeDevAPIBase or GitHubAPIBase — both end with the
// /releases path prefix, so the URL is identical between
// the two sources.
func lookupLatestTagOnce(ctx context.Context, baseURL string) (string, error) {
	url := baseURL + "/releases/latest"

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
// uses the tag as-is — nightme.dev serves assets under
// /downloads/vX.Y.Z/, mirroring GitHub's URL convention.
func MirrorAssetURL(tag, assetName string) string {
	return MirrorDownloadBase + "/" + tag + "/" + assetName
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

// STTAssetNameForRuntime is the nightme-stt analog of
// AssetNameForRuntime. The asset layout is identical
// (nightme-stt_<ver>_<os>_<arch>.<ext>) — same naming
// convention, same archive types, same release flow — so
// `nightme stt install` can use the same GitHub / mirror
// fallback chain as `nightme update`.
//
// Example:
//
//	STTAssetNameForRuntime("0.5.0", "linux", "amd64")
//	→ "nightme-stt_0.5.0_linux_amd64.tar.gz"
func STTAssetNameForRuntime(ver, goos, goarch string) string {
	ext := "tar.gz"
	if goos == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("nightme-stt_%s_%s_%s.%s", ver, goos, goarch, ext)
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

// downloadSpec is the per-binary-type configuration that
// DownloadTag / DownloadSTT share. Each release artifact
// (nightme, nightme-stt) has its own asset name pattern
// (nightme_<ver>_<os>_<arch>.<ext> vs nightme-stt_<ver>_<os>_<arch>.<ext>)
// and its own archive's binary basename, but the download /
// verify / extract flow is otherwise identical — so the two
// public entry points are thin wrappers around downloadTag
// (below).
type downloadSpec struct {
	// AssetName returns the asset filename for a given
	// (ver, goos, goarch) triple. Examples: "nightme_0.5.0_linux_amd64.tar.gz",
	// "nightme-stt_0.5.0_windows_amd64.zip".
	AssetName func(ver, goos, goarch string) string
	// Extract pulls the binary out of the staged archive.
	Extract func(archivePath, stagingDir string) (string, error)
	// BinaryLabel is the basename to record in DownloadResult's
	// error messages and progress logs. Lets users grep for
	// the right artifact when something goes wrong.
	BinaryLabel string
}

// DownloadTag downloads + verifies + extracts the latest
// nightme binary into <dataDir>/updates/<ver>/. The tag is
// resolved internally via LookupLatestTag — callers don't
// pin a version.
//
// Stage 1: download SHA256SUMS.txt (GitHub first, mirror fallback).
//
// Stage 2: parse the sums file for the target asset's hash.
// The sums file MUST list our asset — a missing entry is a
// hard error, not a soft fallback. A stripped sums file
// alongside a tampered asset would otherwise install
// silently.
//
// Stage 3: download the asset (GitHub first, mirror fallback)
// and verify its SHA256 against the sums file. The SHA is
// computed inline (no second file read) via fetchAsset's
// tee.
//
// Stage 4: extract the archive into the staging dir.
//
// progress is called periodically during the binary download
// (the largest, slowest transfer). Pass QuietProgress to
// silence; pass nil to skip callbacks entirely.
func DownloadTag(ctx context.Context, dataDir string, progress ProgressFunc) (*DownloadResult, error) {
	return downloadTag(ctx, dataDir, progress, downloadSpec{
		AssetName:   AssetNameForRuntime,
		Extract:     ExtractArchive,
		BinaryLabel: "nightme",
	})
}

// DownloadSTT is the nightme-stt analog of DownloadTag. Same
// three-stage flow (sums → asset → verify → extract) against
// the same GitHub release feed, but the asset name pattern
// (`nightme-stt_*`) and the extracted binary basename
// (`nightme-stt` / `nightme-stt.exe`) differ. The CLI uses
// this for `nightme stt install` / `nightme stt update`.
//
// Staging dir is the same `<dataDir>/updates/<ver>/` so an
// `updater.StagingDir` lookup works for both binaries; the
// filename inside the staging dir is what tells them apart.
func DownloadSTT(ctx context.Context, dataDir string, progress ProgressFunc) (*DownloadResult, error) {
	return downloadTag(ctx, dataDir, progress, downloadSpec{
		AssetName:   STTAssetNameForRuntime,
		Extract:     ExtractSTTArchive,
		BinaryLabel: "nightme-stt",
	})
}

// downloadTag is the shared implementation behind DownloadTag
// and DownloadSTT. Spec.AssetName + spec.Extract carry the
// only per-binary differences; everything else (sums fetch,
// verify, extract, source labeling) is identical.
func downloadTag(ctx context.Context, dataDir string, progress ProgressFunc, spec downloadSpec) (*DownloadResult, error) {
	if dataDir == "" {
		return nil, errors.New("updater: empty data dir")
	}

	tag, _, err := LookupLatestTag(ctx)
	if err != nil {
		return nil, fmt.Errorf("lookup latest tag: %w", err)
	}

	ver := stripV(tag)
	stagingDir := filepath.Join(dataDir, "updates", ver)
	if err := os.MkdirAll(stagingDir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir staging dir: %w", err)
	}

	assetName := spec.AssetName(ver, runtime.GOOS, runtime.GOARCH)

	// Stage 1: pull the sums file. GitHub first, mirror fallback.
	sumsPath, _, sumsSource, err := downloadAssetWithFallback(
		ctx,
		GitHubAssetURL(tag, SHA256SUMSName),
		MirrorAssetURL(tag, SHA256SUMSName),
		SHA256SUMSName,
		stagingDir,
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("download sums: %w", err)
	}

	// Stage 2: parse the sums file for our asset's expected
	// hash. A missing entry is a HARD error: we just fetched
	// the sums file successfully, so a stripped / partial
	// sums alongside a tampered asset would otherwise install
	// silently.
	wantSum, err := lookupSHAInFile(sumsPath, assetName)
	if err != nil {
		return nil, err
	}

	// Stage 3: pull the binary, verifying SHA inline. The
	// SHA is computed during the file write via fetchAsset's
	// tee — no second pass over the bytes.
	binArchive, gotSum, binSource, err := downloadAssetWithFallback(
		ctx,
		GitHubAssetURL(tag, assetName),
		MirrorAssetURL(tag, assetName),
		assetName,
		stagingDir,
		progress,
	)
	if err != nil {
		return nil, fmt.Errorf("download %s binary: %w", spec.BinaryLabel, err)
	}
	if gotSum != wantSum {
		return nil, fmt.Errorf("sha256 mismatch (%s): got %s, want %s",
			binSource, gotSum, wantSum)
	}

	// Stage 4: extract.
	binary, err := spec.Extract(binArchive, stagingDir)
	if err != nil {
		return nil, fmt.Errorf("extract %s: %w", spec.BinaryLabel, err)
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
		SHA256Hex:  gotSum,
	}, nil
}

// downloadAssetWithFallback fetches primaryURL; on failure
// (network, 5xx, 4xx, body error) falls back to fallbackURL.
// Returns (local-path, sha256hex, source-label, error). progress
// is optional (nil = silent). The sha256hex is computed inline
// during the file write — no second pass over the bytes.
//
// The "label" returned is "github" or "mirror" — used by
// DownloadTag to surface which source served the bytes.
func downloadAssetWithFallback(
	ctx context.Context,
	primaryURL, fallbackURL, assetName, stagingDir string,
	progress ProgressFunc,
) (string, string, string, error) {
	if path, sum, err := fetchAsset(ctx, primaryURL, assetName, stagingDir, progress); err == nil {
		return path, sum, "github", nil
	}
	path, sum, err := fetchAsset(ctx, fallbackURL, assetName, stagingDir, progress)
	if err != nil {
		return "", "", "", err
	}
	return path, sum, "mirror", nil
}

// fetchAsset downloads url into <stagingDir>/<assetName> with a
// SHA256 tee so we don't need a second pass to hash it later.
// Returns (local-path, sha256hex, error). Caller passes
// progress to receive tick callbacks; nil is fine for quiet
// mode.
func fetchAsset(
	ctx context.Context,
	url, assetName, stagingDir string,
	progress ProgressFunc,
) (string, string, error) {
	dst := filepath.Join(stagingDir, assetName)
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", "", fmt.Errorf("open %s: %w", assetName, err)
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
		return "", "", fmt.Errorf("build %s request: %w", assetName, err)
	}
	req.Header.Set("User-Agent", version.UserAgent())

	resp, err := httpclient.Default().Do(req)
	if err != nil {
		cleanup()
		return "", "", fmt.Errorf("%s: %w", assetName, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		cleanup()
		return "", "", fmt.Errorf("%s: HTTP %d", assetName, resp.StatusCode)
	}

	if _, err := io.Copy(mw, &progressReader{
		underlying: resp.Body,
		total:      resp.ContentLength,
		progress:   progress,
	}); err != nil {
		cleanup()
		return "", "", fmt.Errorf("copy %s: %w", assetName, err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return "", "", fmt.Errorf("close %s: %w", assetName, err)
	}
	return dst, hex.EncodeToString(hasher.Sum(nil)), nil
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
// Returns an error when the file has no entry for assetName.
// The caller (DownloadTag) treats this as a hard failure:
// we've successfully fetched the sums file, so a missing
// entry means a broken or tampered release, not a soft-degrade
// case. A stripped sums file alongside a tampered asset
// would otherwise install silently.
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
	return "", fmt.Errorf("sha256 sums: %s not listed", assetName)
}

// ----- extraction ---------------------------------------------------

// ExtractArchive pulls the nightme binary out of the .tar.gz /
// .zip archive. Returns the absolute path to the extracted
// binary inside stagingDir.
func ExtractArchive(archivePath, stagingDir string) (string, error) {
	return extractArchive(archivePath, stagingDir, []string{"nightme", "nightme.exe"})
}

// ExtractSTTArchive is the nightme-stt analog of ExtractArchive.
// Same archive format, same staging layout — only the binary
// basename inside the archive differs (`nightme-stt` /
// `nightme-stt.exe` instead of `nightme` / `nightme.exe`).
func ExtractSTTArchive(archivePath, stagingDir string) (string, error) {
	return extractArchive(archivePath, stagingDir, []string{"nightme-stt", "nightme-stt.exe"})
}

// extractArchive picks the right format for the current GOOS
// and dispatches. allowedBaseNames is the set of binary
// filenames we accept from the archive — both `nightme` and
// `nightme-stt` share this code path so each archive type
// supplies its own basename list.
func extractArchive(archivePath, stagingDir string, allowedBaseNames []string) (string, error) {
	if runtime.GOOS == "windows" {
		return extractZIP(archivePath, stagingDir, allowedBaseNames)
	}
	return extractTARGZ(archivePath, stagingDir, allowedBaseNames)
}

// allowedBaseName reports whether base is in the allow-list.
// Case-sensitive — archives always ship lowercase basenames.
func allowedBaseName(base string, allowed []string) bool {
	return slices.Contains(allowed, base)
}

func extractTARGZ(archivePath, stagingDir string, allowedBaseNames []string) (string, error) {
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
			return "", errors.New("extract: target binary not found in archive")
		}
		if err != nil {
			return "", fmt.Errorf("read tar header: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		base := filepath.Base(hdr.Name)
		if !allowedBaseName(base, allowedBaseNames) {
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

func extractZIP(archivePath, stagingDir string, allowedBaseNames []string) (string, error) {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return "", fmt.Errorf("open zip: %w", err)
	}
	defer r.Close()

	for _, f := range r.File {
		base := filepath.Base(f.Name)
		if !allowedBaseName(base, allowedBaseNames) {
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
	return "", errors.New("extract: target binary not found in zip")
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
