package stt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"
)

// Asset is what a resolver hands back to the installer.
// URL + SHA256 + Size are the integrity triple (issue
// #381 §5); Archive + Binary tell the installer how to
// extract the executable.
type Asset struct {
	Tag      string // GitHub release tag the asset came from (e.g. "v0.6.0")
	Name     string // asset filename in the release
	URL      string // direct download URL
	SHA256   string // hex-encoded, lowercase
	Size     int64
	Archive  string // "tar.gz" or "zip"
	Binary   string // executable name INSIDE the archive
	StripDir string // for tar.bz2 model archives: top-level dir to strip
}

// WorkerResolver returns the nightme-stt binary asset for
// the current GOOS/GOARCH. The production implementation
// queries the GitHub Releases API anonymously (60 req/h per
// IP is enough — install runs once per user).
type WorkerResolver interface {
	WorkerAsset(ctx context.Context) (Asset, error)
}

// ModelResolver returns the SenseVoice model asset.
// Production queries the k2-fsa/sherpa-onnx release page.
type ModelResolver interface {
	ModelAsset(ctx context.Context) (Asset, error)
}

// GitHubRelease is a minimal subset of the GitHub Releases
// API response. We only deserialize what we need; tags and
// assets carry hundreds of fields we ignore, and pulling
// the full struct via `map[string]any` is wasteful.
type GitHubRelease struct {
	TagName string        `json:"tag_name"`
	Name    string        `json:"name"`
	Assets  []GitHubAsset `json:"assets"`
}

// GitHubAsset is one entry in the release's `assets` array.
// Browser_download_url is the canonical direct-download URL
// (resolvable from any IP, no auth required); `url` requires
// the GitHub API token to follow.
type GitHubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	URL                string `json:"url"`
	Size               int64  `json:"size"`
	Digest             string `json:"digest"` // sha256:abc... — GitHub-side pre-computed
}

// GitHubWorkerResolver fetches the nightme-stt binary from
// the cnlangzi/nightme releases. Matches the asset whose
// filename follows the convention
// `nightme-stt_<tag>_<goos>_<goarch>.<ext>` (set in
// .github/workflows/release.yml).
type GitHubWorkerResolver struct {
	// Repo is the GitHub "owner/name" pair, e.g.
	// "cnlangzi/nightme". Tests can substitute a local
	// repo to avoid hitting the real GitHub.
	Repo string
	// HttpClient overrides the default. Tests use this to
	// point at an httptest server with a fixed response;
	// production lets the resolver build a default client.
	HttpClient *http.Client
	// Token, when non-empty, is sent as a Bearer token.
	// Production callers leave this empty and accept the
	// 60 req/h anonymous quota.
	Token string
	// BaseURL overrides the GitHub API root. Tests point
	// this at httptest.NewTLSServer's URL; production
	// leaves it empty so the resolver hits
	// https://api.github.com.
	BaseURL string
}

// WorkerAsset queries GitHub for the latest nightme release,
// finds the asset matching the current GOOS/GOARCH, and
// resolves its SHA-256. The SHA comes from GitHub's `digest`
// field when present, else from the `<asset>.sha256` sidecar
// file uploaded alongside every release artifact.
//
// Matching is by SUFFIX (`_<goos>_<goarch>.<ext>`), not
// exact name. The release workflow bakes the full git
// describe tag (e.g. `v0.5.1-15-g61bc233-dirty`) into the
// filename, so the resolver cannot pre-compute the exact
// name — it has to wait until it sees the release.
func (r *GitHubWorkerResolver) WorkerAsset(ctx context.Context) (Asset, error) {
	rel, err := r.fetchLatestRelease(ctx)
	if err != nil {
		return Asset{}, fmt.Errorf("github: fetch latest nightme release: %w", err)
	}
	suffix := workerAssetSuffix(runtime.GOOS, runtime.GOARCH)
	var match *GitHubAsset
	for i := range rel.Assets {
		if strings.HasSuffix(rel.Assets[i].Name, suffix) {
			match = &rel.Assets[i]
			break
		}
	}
	if match == nil {
		return Asset{}, fmt.Errorf("github: nightme release %s has no asset ending in %q for %s/%s",
			rel.TagName, suffix, runtime.GOOS, runtime.GOARCH)
	}
	sha, err := r.resolveSHA(ctx, match)
	if err != nil {
		return Asset{}, fmt.Errorf("github: resolve sha256 for %s: %w", match.Name, err)
	}
	archive := "tar.gz"
	binary := "nightme-stt"
	if runtime.GOOS == "windows" {
		archive = "zip"
		binary = "nightme-stt.exe"
	}
	return Asset{
		Tag:     rel.TagName,
		Name:    match.Name,
		URL:     match.BrowserDownloadURL,
		SHA256:  sha,
		Size:    match.Size,
		Archive: archive,
		Binary:  binary,
	}, nil
}

// GitHubModelResolver fetches the SenseVoice Small INT8
// multilingual model from the k2-fsa/sherpa-onnx release
// page. The release republishes SenseVoice under the
// `asr-models` tag with the canonical filename
// `sherpa-onnx-sense-voice-zh-en-ja-ko-yue-<date>-int8.tar.bz2`.
type GitHubModelResolver struct {
	// Repo is the upstream source (k2-fsa/sherpa-onnx).
	Repo string
	// AssetName is the exact filename within the release.
	// Tests override this; production leaves it empty and
	// the resolver picks the latest matching asset.
	AssetName string
	// StripDir is the top-level directory inside the tar.bz2
	// archive that should be removed during extraction. The
	// canonical layout is
	// `sherpa-onnx-sense-voice-zh-en-ja-ko-yue-<date>-int8/`.
	StripDir   string
	HttpClient *http.Client
	Token      string
	BaseURL    string
}

// ModelAsset queries the k2-fsa/sherpa-onnx release page and
// picks the latest SenseVoice Small INT8 asset. The release
// republishes model artifacts alongside the native libs, so
// we filter by `int8` in the asset name to avoid grabbing
// fp32 by accident.
func (r *GitHubModelResolver) ModelAsset(ctx context.Context) (Asset, error) {
	rel, err := r.fetchLatestRelease(ctx)
	if err != nil {
		return Asset{}, fmt.Errorf("github: fetch latest sherpa-onnx release: %w", err)
	}
	want := r.AssetName
	if want == "" {
		want = "sherpa-onnx-sense-voice-zh-en-ja-ko-yue-2024-07-17-int8.tar.bz2"
	}
	var match *GitHubAsset
	for i := range rel.Assets {
		if rel.Assets[i].Name == want {
			match = &rel.Assets[i]
			break
		}
	}
	if match == nil {
		return Asset{}, fmt.Errorf("github: sherpa-onnx release %s has no asset %q",
			rel.TagName, want)
	}
	sha, err := r.resolveSHA(ctx, match)
	if err != nil {
		return Asset{}, fmt.Errorf("github: resolve sha256 for %s: %w", match.Name, err)
	}
	stripDir := r.StripDir
	if stripDir == "" {
		// Default to deriving from asset name (everything
		// up to and including `.tar.bz2`).
		stripDir = strings.TrimSuffix(match.Name, ".tar.bz2")
	}
	return Asset{
		Tag:      rel.TagName,
		Name:     match.Name,
		URL:      match.BrowserDownloadURL,
		SHA256:   sha,
		Size:     match.Size,
		Archive:  "tar.bz2",
		StripDir: stripDir,
	}, nil
}

// fetchLatestRelease hits `https://api.github.com/repos/<repo>/
// releases/latest` and parses the JSON. Uses the supplied
// HttpClient (or a default with a 30s timeout).
func (r *GitHubWorkerResolver) fetchLatestRelease(ctx context.Context) (*GitHubRelease, error) {
	return fetchLatestRelease(ctx, r.Repo, r.HttpClient, r.Token)
}

// fetchLatestRelease shared with the model resolver — same
// GitHub API shape, same auth header, same client defaults.
func (r *GitHubModelResolver) fetchLatestRelease(ctx context.Context) (*GitHubRelease, error) {
	return fetchLatestRelease(ctx, r.Repo, r.HttpClient, r.Token)
}

func fetchLatestRelease(ctx context.Context, repo string, client *http.Client, token string) (*GitHubRelease, error) {
	if repo == "" {
		return nil, errors.New("github resolver: empty repo")
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	apiRoot := "https://api.github.com"
	if root := apiRootFromContext(ctx); root != "" {
		apiRoot = root
	}
	url := apiRoot + "/repos/" + repo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// GitHub's API asks for a UA. Anonymous quota is 60 req/h
	// per IP; we don't pretend to be a browser (UA is just
	// "nightme-stt-installer") so a misbehaving CI doesn't
	// get its IP banned for impersonation.
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "nightme-stt-installer")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("github API rate limit hit (HTTP %d) — retry later or set a token", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("github API: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var rel GitHubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("github API: decode: %w", err)
	}
	return &rel, nil
}

// resolveSHA returns the SHA-256 of an asset. GitHub computes
// this server-side and exposes it two ways:
//
//  1. The asset's `digest` field — preferred, single round
//     trip. Most assets have this.
//  2. A sidecar file at <asset URL>.sha256 — fallback for
//     older uploads. We fetch it separately.
//
// Both paths return the bare hex string (no `sha256:` prefix).
func (r *GitHubWorkerResolver) resolveSHA(ctx context.Context, a *GitHubAsset) (string, error) {
	return resolveSHA(ctx, a, r.HttpClient, r.Token)
}

func (r *GitHubModelResolver) resolveSHA(ctx context.Context, a *GitHubAsset) (string, error) {
	return resolveSHA(ctx, a, r.HttpClient, r.Token)
}

func resolveSHA(ctx context.Context, a *GitHubAsset, client *http.Client, token string) (string, error) {
	if sha := parseDigestField(a.Digest); sha != "" {
		return sha, nil
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	url := a.BrowserDownloadURL + ".sha256"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "nightme-stt-installer")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github: %s.sha256: HTTP %d", a.Name, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}

// workerAssetSuffix returns the SUFFIX the resolver matches
// against GitHub asset names — the part AFTER the version
// tag. The release workflow bakes the tag in via git
// describe, so the suffix is the only thing the resolver
// can predict before seeing the release JSON.
func workerAssetSuffix(goos, goarch string) string {
	return "_" + goos + "_" + goarch + "." + archiveExt(goos)
}

// archiveExt picks the matching archive extension for a
// given OS (zip for Windows, tar.gz everywhere else).
func archiveExt(goos string) string {
	if goos == "windows" {
		return "zip"
	}
	return "tar.gz"
}

// apiRootFromContext returns the optional GitHub API base
// URL override from ctx, or "" if not set. Tests use this
// to redirect the resolver at an httptest server without
// hitting the real api.github.com.
type apiRootCtxKey struct{}

func apiRootFromContext(ctx context.Context) string {
	v, _ := ctx.Value(apiRootCtxKey{}).(string)
	return v
}

// WithGitHubAPIRoot returns a derived context whose
// fetchLatestRelease uses the supplied root instead of
// https://api.github.com. Test-only.
func WithGitHubAPIRoot(ctx context.Context, root string) context.Context {
	return context.WithValue(ctx, apiRootCtxKey{}, root)
}

// lowercase the hex body for stable comparison against
// the on-disk SHA we compute during download.
func parseDigestField(d string) string {
	const prefix = "sha256:"
	d = strings.ToLower(d)
	if !strings.HasPrefix(d, prefix) {
		return ""
	}
	return strings.TrimPrefix(d, prefix)
}
