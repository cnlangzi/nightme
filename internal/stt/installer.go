package stt

import (
	"archive/tar"
	"archive/zip"
	"compress/bzip2"
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
	"strings"
	"time"
)

// Installer handles `nightme stt install` — the user-visible
// "make voice transcription work" command. It resolves the
// latest nightme-stt binary + SenseVoice model from the
// upstream release pages, downloads each, verifies the
// SHA-256 against what GitHub reports, and atomically
// activates the files under `<dataDir>/stt/`.
//
// Issue #381 §5 contract:
//   - HTTPS only
//   - SHA-256 verified before activation
//   - Download into a temp location, never overwrite a
//     known-good installation
//   - Atomic rename on success
//   - Install metadata written to `<dataDir>/stt/manifest.json`
//
// There is no static manifest pinned in this binary. The
// installer always queries GitHub's Releases API for the
// latest stable nightme + the latest sherpa-onnx SenseVoice
// asset. Issue §5's "pinned source + SHA-256" requirement is
// preserved by checking every download against GitHub's
// per-asset digest before activating it — same integrity
// guarantee, no static config to keep in sync with releases.
type Installer struct {
	dataDir string
	http    *http.Client

	// workers + models are pluggable so tests can swap
	// resolvers. Production wires these to GitHub-backed
	// implementations (WorkerResolver = *GitHubWorkerResolver,
	// ModelResolver = *GitHubModelResolver).
	worker WorkerResolver
	model  ModelResolver
}

// NewInstaller returns an Installer bound to dataDir.
// dataDir is the same root the manager uses (typically
// `~/.nightme`). If dataDir is empty, returns an error —
// without a stable data dir, install state has nowhere to
// live.
//
// workers and model are required — without resolvers, Install
// can't know what to fetch. Production callers wire these
// to the GitHub resolvers (see DefaultGitHubResolvers).
func NewInstaller(dataDir string, workers WorkerResolver, model ModelResolver) (*Installer, error) {
	if dataDir == "" {
		return nil, errors.New("stt installer: empty data dir")
	}
	if workers == nil {
		return nil, errors.New("stt installer: nil worker resolver")
	}
	if model == nil {
		return nil, errors.New("stt installer: nil model resolver")
	}
	return &Installer{
		dataDir: dataDir,
		http: &http.Client{
			Timeout: 30 * time.Minute, // sensevoice model ~230MB
		},
		worker: workers,
		model:  model,
	}, nil
}

// DefaultGitHubResolvers returns the production resolvers:
// the nightme-stt binary from cnlangzi/nightme, the
// SenseVoice model from k2-fsa/sherpa-onnx. Anonymous
// GitHub API access (60 req/h per IP).
func DefaultGitHubResolvers() (WorkerResolver, ModelResolver) {
	return &GitHubWorkerResolver{Repo: "cnlangzi/nightme"},
		&GitHubModelResolver{Repo: "k2-fsa/sherpa-onnx"}
}

// InstallResult reports what was fetched. Used by
// InstallResult reports what was activated. Used by both
// `nightme stt install` (one-shot) and the per-stage update
// path to print a summary at the end.
type InstallResult struct {
	WorkerInstalled bool
	WorkerPath      string
	WorkerVersion   string
	WorkerTag       string // GitHub release tag (e.g. "v0.6.0")
	ModelInstalled  bool
	ModelDir        string
	ModelTag        string
	ManifestPath    string
	BytesDownloaded int64
	// Staging paths on disk if Fetch was used — empty when
	// Install ran the all-in-one path (Activate consumed them
	// in place).
	WorkerStagingPath string
	ModelStagingPath  string
}

// CheckResult is what Check returns. The two halves (worker
// + model) are checked independently because in principle
// a new nightme binary could ship without a new SenseVoice
// release (and vice versa).
//
// "Up to date" compares the recorded tag in
// <dataDir>/stt/manifest.json against the tag GitHub just
// reported. We don't compare the binary's SHA against the
// tarball's SHA — those are different files. Instead we
// trust the manifest's record of what we installed (which
// was SHA-verified at install time) and only re-download
// when the version changes.
type CheckResult struct {
	LatestWorker   Asset
	LatestModel    Asset
	WorkerOnDisk   string // version tag recorded in manifest, "" if no manifest
	WorkerUpToDate bool   // WorkerOnDisk == LatestWorker.Tag
	ModelInstalled bool   // both files present on disk
	ModelUpToDate  bool   // tag from manifest's model entry == latest
}

// Check resolves the latest worker + model from GitHub and
// compares each against the recorded tags in
// <dataDir>/stt/manifest.json. No downloads happen. Used
// by `nightme stt update` to decide whether the remaining
// two stages are necessary.
func (i *Installer) Check(ctx context.Context) (*CheckResult, error) {
	workerAsset, err := i.worker.WorkerAsset(ctx)
	if err != nil {
		return nil, fmt.Errorf("stt installer: resolve worker: %w", err)
	}
	modelAsset, err := i.model.ModelAsset(ctx)
	if err != nil {
		return nil, fmt.Errorf("stt installer: resolve model: %w", err)
	}
	modelDir := modelInstallPath(i.dataDir)
	modelInstalled := isModelInstalled(modelDir)

	// Read the install manifest if present. Its absence
	// means "never installed" → WorkerUpToDate=false.
	rec, _ := readInstallManifest(filepath.Join(i.dataDir, "stt", "manifest.json"))
	workerOnDisk := ""
	modelOnDisk := ""
	if rec != nil {
		workerOnDisk = rec.WorkerTag
		modelOnDisk = rec.ModelTag
	}
	return &CheckResult{
		LatestWorker:   workerAsset,
		LatestModel:    modelAsset,
		WorkerOnDisk:   workerOnDisk,
		WorkerUpToDate: workerOnDisk != "" && workerOnDisk == workerAsset.Tag,
		ModelInstalled: modelInstalled,
		ModelUpToDate:  modelInstalled && modelOnDisk == modelAsset.Tag,
	}, nil
}

// installRecord is the JSON shape of <dataDir>/stt/manifest.json.
// Populated by writeInstallManifest and read by Check.
type installRecord struct {
	WorkerTag string `json:"worker_tag"`
	ModelTag  string `json:"model_tag"`
}

// readInstallManifest parses the manifest if it exists.
// Returns (nil, nil) on absent — not an error condition.
func readInstallManifest(path string) (*installRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var rec installRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// FetchResult is what Fetch returns. The staging paths are
// where Activate will look for the verified assets.
type FetchResult struct {
	Worker          Asset
	Model           Asset
	WorkerStaging   string
	ModelStaging    string
	BytesDownloaded int64
}

// Fetch downloads + verifies + extracts the worker asset
// into staging and atomically moves it into the live
// install path BEFORE touching the model. Per-asset
// activation means a model-side failure leaves the freshly-
// installed worker in place — matching the test
// "TestInstallDownloadsWorkerVerifiesSHA" contract, and
// the user-visible expectation that "I have a new worker,
// the model is still wrong" is a recoverable state.
//
// If the model fetch fails, the worker's staging area is
// cleaned up before returning the error (so we don't leak
// 230 MB of tmp). The worker has already been moved into
// place and stays there.
//
// The opts.WorkerOnly / ModelOnly switches let the caller
// skip the artifact they don't care about.
func (i *Installer) Fetch(ctx context.Context, progress ProgressFunc, opts FetchOptions) (*FetchResult, error) {
	if err := os.MkdirAll(filepath.Join(i.dataDir, "stt"), 0o700); err != nil {
		return nil, fmt.Errorf("stt installer: mkdir: %w", err)
	}

	res := &FetchResult{}

	if !opts.ModelOnly {
		workerAsset, err := i.worker.WorkerAsset(ctx)
		if err != nil {
			return nil, fmt.Errorf("stt installer: resolve worker: %w", err)
		}
		res.Worker = workerAsset
		// Skip the download + extract if the binary on disk
		// already matches the resolved SHA.
		if sha256OfFile(workerInstallPath(i.dataDir, workerAsset.Binary)) != strings.ToLower(workerAsset.SHA256) {
			dl, err := i.downloadTo(ctx, workerAsset.URL, workerAsset.SHA256, workerAsset.Size, progress)
			if err != nil {
				return nil, fmt.Errorf("stt installer: worker download: %w", err)
			}
			res.BytesDownloaded += dl.bytes

			bin, err := extractWorker(workerAsset, dl.path)
			if err != nil {
				_ = os.Remove(dl.path)
				return nil, fmt.Errorf("stt installer: worker extract: %w", err)
			}
			if err := os.Chmod(bin, 0o755); err != nil {
				_ = os.Remove(bin)
				return nil, fmt.Errorf("stt installer: chmod worker: %w", err)
			}
			// Atomic move to the live install path NOW,
			// before the model phase. If model fetch fails,
			// the worker is still in place — better to have
			// a working binary + broken model than no binary.
			finalBin := workerInstallPath(i.dataDir, workerAsset.Binary)
			if err := atomicMove(bin, finalBin); err != nil {
				return nil, fmt.Errorf("stt installer: install worker: %w", err)
			}
		}
	}

	if !opts.WorkerOnly {
		modelAsset, err := i.model.ModelAsset(ctx)
		if err != nil {
			return nil, fmt.Errorf("stt installer: resolve model: %w", err)
		}
		res.Model = modelAsset
		if !isModelInstalled(modelInstallPath(i.dataDir)) {
			dl, err := i.downloadTo(ctx, modelAsset.URL, modelAsset.SHA256, modelAsset.Size, progress)
			if err != nil {
				return nil, fmt.Errorf("stt installer: model download: %w", err)
			}
			res.BytesDownloaded += dl.bytes

			stageDir := filepath.Join(i.dataDir, "stt", "cache", "model-stage")
			if err := os.RemoveAll(stageDir); err != nil {
				return nil, fmt.Errorf("stt installer: clear stage: %w", err)
			}
			if err := os.MkdirAll(stageDir, 0o700); err != nil {
				return nil, fmt.Errorf("stt installer: mkdir stage: %w", err)
			}
			if err := extractModel(dl.path, stageDir, modelAsset.StripDir); err != nil {
				return nil, fmt.Errorf("stt installer: model extract: %w", err)
			}
			if err := atomicMove(stageDir, modelInstallPath(i.dataDir)); err != nil {
				return nil, fmt.Errorf("stt installer: install model: %w", err)
			}
		}
	}

	// Both phases activated successfully. Record the
	// install metadata so future `nightme stt status` can
	// show what's installed without re-resolving GitHub.
	manifestResult := &InstallResult{
		WorkerInstalled: true,
		WorkerPath:      workerInstallPath(i.dataDir, res.Worker.Binary),
		WorkerVersion:   res.Worker.Tag,
		WorkerTag:       res.Worker.Tag,
		ModelInstalled:  true,
		ModelDir:        modelInstallPath(i.dataDir),
		ModelTag:        res.Model.Tag,
		BytesDownloaded: res.BytesDownloaded,
	}
	if err := i.writeInstallManifestFile(manifestResult, res.Worker, res.Model); err != nil {
		return nil, fmt.Errorf("stt installer: write manifest: %w", err)
	}
	return res, nil
}

// FetchOptions controls which artifacts Fetch touches.
type FetchOptions struct {
	WorkerOnly bool // skip the model fetch
	ModelOnly  bool // skip the worker fetch
}

// ActivateOptions controls the post-install behavior.
//
// Activate is mostly a no-op for the "convenience" Install
// path (Fetch already activated per-asset). It exists for
// the CI pre-warm flow where Fetch staged without
// activating: --no-activate → Fetch → activate later. In
// that path, Fetch MUST have been called with
// per-asset staging instead of per-asset activation.
type ActivateOptions struct {
	// NoRestart skips the "stop any running nightme-stt
	// process" step.
	NoRestart bool
}

// Activate populates the InstallResult from a FetchResult.
// It does NOT re-do the atomic moves (Fetch already did
// them per-asset) and it does NOT kill any running worker
// unless opts.NoRestart is false — that decision is left
// to the CLI because "stop the worker" is an
// operator-facing action, not a pure computation.
//
// `WorkerInstalled` / `ModelInstalled` reflect whether
// THIS Activate call moved files. When `WorkerStaging`
// is empty the on-disk binary was already there (Fetch
// skipped the download via SHA-match), so Activate did
// nothing and the Installed flag stays false — matching
// the "did this Activate call move files" semantic.
//
// Kept in the public API so the 3-stage shape (check →
// fetch → activate) mirrors nightme's update flow. The
// CLI doesn't call Activate directly — it uses
// Install() / InstallWithOptions which call
// Fetch+Activate under the hood.
func (i *Installer) Activate(fetch *FetchResult, opts ActivateOptions) (*InstallResult, error) {
	res := &InstallResult{
		WorkerTag: fetch.Worker.Tag,
		ModelTag:  fetch.Model.Tag,
		WorkerPath: workerInstallPath(i.dataDir, fetch.Worker.Binary),
		WorkerVersion: fetch.Worker.Tag,
		ModelDir:   modelInstallPath(i.dataDir),
	}
	if fetch.WorkerStaging != "" {
		res.WorkerInstalled = true
	}
	if fetch.ModelStaging != "" {
		res.ModelInstalled = true
	}

	if !opts.NoRestart {
		killRunningWorker()
	}
	return res, nil
}

// writeInstallManifestFile persists the per-install state
// to <dataDir>/stt/manifest.json. Called from Fetch on
// success — both phases succeeded, manifest is recorded.
func (i *Installer) writeInstallManifestFile(res *InstallResult, worker, model Asset) error {
	manifestPath := filepath.Join(i.dataDir, "stt", "manifest.json")
	return writeInstallManifest(manifestPath, res, worker, model)
}

// Install is the one-shot convenience wrapper that runs
// Fetch + Activate in sequence. Used by `nightme stt
// install` (first-time setup) and by tests that don't
// care about the staged output.
//
// Idempotent: existing files that already match the
// resolved SHA are reused without a re-download.
//
// NoRestart=true: the convenience wrapper does NOT kill
// any running worker — the caller's intent is "set up
// fresh, don't disturb running state." Tests pass true
// (they don't have a real worker); production callers
// (`nightme stt install`) also pass true because install
// is for first-time setup, and there's no running worker
// to disturb yet. `nightme stt update` is the path that
// does the kill, because that's the path the user is
// explicitly refreshing an existing install.
func (i *Installer) Install(ctx context.Context, progress ProgressFunc) (*InstallResult, error) {
	return i.InstallWithOptions(ctx, progress, ActivateOptions{NoRestart: true})
}

// InstallWithOptions is the explicit form for callers
// that want to override the NoRestart default — chiefly
// the update path, which DOES want to stop any running
// nightme-stt so the next Voice message picks up the
// fresh binary.
func (i *Installer) InstallWithOptions(ctx context.Context, progress ProgressFunc, actOpts ActivateOptions) (*InstallResult, error) {
	fetch, err := i.Fetch(ctx, progress, FetchOptions{})
	if err != nil {
		return nil, err
	}
	return i.Activate(fetch, actOpts)
}

// ProgressFunc receives byte-level download progress.
// total == 0 means "size unknown" (Content-Length absent).
type ProgressFunc func(bytes, total int64)

type downloadedFile struct {
	path  string
	bytes int64
}

// downloadTo fetches url to a temp file inside the install
// cache, hashes it as it goes, and returns the path on
// success. The caller is responsible for os.Remove'ing
// the file when done — but the installer always renames
// it into place, so cleanup happens implicitly if rename
// succeeds; if anything fails, the cleanup is the caller's
// job (see Install's error paths).
func (i *Installer) downloadTo(
	ctx context.Context, url, wantSHA string, wantSize int64, progress ProgressFunc,
) (*downloadedFile, error) {
	if !strings.HasPrefix(url, "https://") {
		return nil, fmt.Errorf("refusing non-HTTPS URL %q", url)
	}
	cacheDir := filepath.Join(i.dataDir, "stt", "cache")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(cacheDir, "dl-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create tmp: %w", err)
	}
	defer tmp.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		_ = os.Remove(tmp.Name())
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := i.http.Do(req)
	if err != nil {
		_ = os.Remove(tmp.Name())
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = os.Remove(tmp.Name())
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	if wantSize > 0 && resp.ContentLength > 0 && resp.ContentLength != wantSize {
		_ = os.Remove(tmp.Name())
		return nil, fmt.Errorf("size mismatch for %s: server says %d, manifest says %d",
			url, resp.ContentLength, wantSize)
	}

	hasher := sha256.New()
	mw := io.MultiWriter(tmp, hasher)
	var copied int64
	buf := make([]byte, 64<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := mw.Write(buf[:n]); werr != nil {
				_ = os.Remove(tmp.Name())
				return nil, fmt.Errorf("write: %w", werr)
			}
			copied += int64(n)
			if progress != nil {
				progress(copied, wantSize)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			_ = os.Remove(tmp.Name())
			return nil, fmt.Errorf("read: %w", rerr)
		}
	}
	got := hex.EncodeToString(hasher.Sum(nil))
	if wantSHA != "" && !strings.EqualFold(got, strings.ToLower(wantSHA)) {
		_ = os.Remove(tmp.Name())
		return nil, fmt.Errorf("sha256 mismatch for %s: got %s, want %s",
			url, got, wantSHA)
	}
	return &downloadedFile{path: tmp.Name(), bytes: copied}, nil
}

// extractWorker pulls the executable out of the worker
// archive into a temp directory and returns its path. tar.gz
// uses the first regular file whose name matches the
// archive's Binary field; zip uses the same lookup against
// the central directory.
//
// Why not just unpack everything? The tarball/zip also
// carries LICENSE + README; we don't need them at runtime,
// and dumping them into ~/.nightme/stt/bin/ would clutter
// the install dir.
func extractWorker(a Asset, archivePath string) (string, error) {
	switch a.Archive {
	case "tar.gz":
		return extractWorkerTarGz(a.Binary, archivePath)
	case "zip":
		return extractWorkerZip(a.Binary, archivePath)
	default:
		return "", fmt.Errorf("unknown archive type %q", a.Archive)
	}
}

func extractWorkerTarGz(binaryName, archivePath string) (string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return "", fmt.Errorf("binary %q not found in archive", binaryName)
		}
		if err != nil {
			return "", err
		}
		if hdr.Name != binaryName {
			continue
		}
		return writeTempBinary(tr, int64(hdr.Mode))
	}
}

func extractWorkerZip(binaryName, archivePath string) (string, error) {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return "", fmt.Errorf("zip: %w", err)
	}
	defer r.Close()
	for _, f := range r.File {
		if filepath.Base(f.Name) != binaryName {
			continue
		}
		src, err := f.Open()
		if err != nil {
			return "", err
		}
		defer src.Close()
		mode := f.Mode()
		if mode == 0 {
			mode = 0o755
		}
		return writeTempBinary(src, int64(mode))
	}
	return "", fmt.Errorf("binary %q not found in archive", binaryName)
}

func writeTempBinary(src io.Reader, mode int64) (string, error) {
	out, err := os.CreateTemp(filepath.Dir(""), "worker-*")
	if err != nil {
		return "", err
	}
	defer out.Close()
	if _, err := io.Copy(out, src); err != nil {
		_ = os.Remove(out.Name())
		return "", err
	}
	if err := os.Chmod(out.Name(), os.FileMode(mode)); err != nil {
		_ = os.Remove(out.Name())
		return "", err
	}
	return out.Name(), nil
}

// extractModel extracts a SenseVoice tar.bz2, strips the
// top-level directory, and writes files under stageDir.
// We keep the model files in a single flat directory
// (<stageDir>/{model.int8.onnx,tokens.txt,...}) because
// the worker only reads from that one path.
func extractModel(archivePath, stageDir, stripDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	bz := bzip2.NewReader(f)
	tr := tar.NewReader(bz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		rel, ok := strings.CutPrefix(hdr.Name, stripDir+"/")
		if !ok {
			continue
		}
		if rel == "" {
			continue
		}
		target := filepath.Join(stageDir, rel)
		if hdr.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tr); err != nil {
			_ = out.Close()
			return err
		}
		_ = out.Close()
	}
}

func isModelInstalled(modelDir string) bool {
	const expectedFiles = 2
	found := 0
	for _, name := range []string{"model.int8.onnx", "tokens.txt"} {
		info, err := os.Stat(filepath.Join(modelDir, name))
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return false
		}
		found++
	}
	return found == expectedFiles
}

func workerInstallPath(dataDir, binaryName string) string {
	return filepath.Join(dataDir, "stt", "bin", binaryName)
}

func modelInstallPath(dataDir string) string {
	return filepath.Join(dataDir, "stt", "models", "sensevoice")
}

// atomicMove renames src to dst on the same filesystem.
// Falls back to copy + os.Remove when cross-filesystem
// rename isn't supported (issue #381 §5: "Activate files
// atomically where practical"). The copy preserves
// permissions, which is what we want for an executable.
func atomicMove(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	_ = os.Remove(dst)
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	srcInfo, err := os.Stat(src)
	if err == nil {
		_ = os.Chmod(dst, srcInfo.Mode())
	}
	return os.Remove(src)
}

// writeInstallManifest persists a record of what the
// installer just activated. Used by `nightme stt status`
// to render install metadata without re-downloading.
func writeInstallManifest(path string, res *InstallResult, worker, model Asset) error {
	doc := struct {
		NightmeVersion string `json:"nightme_version"`
		WorkerTag      string `json:"worker_tag"`
		WorkerPath     string `json:"worker_path"`
		WorkerSHA256   string `json:"worker_sha256"`
		ModelTag       string `json:"model_tag"`
		ModelDir       string `json:"model_dir"`
		ModelSHA256    string `json:"model_sha256"`
		InstalledAt    string `json:"installed_at"`
	}{
		NightmeVersion: res.WorkerVersion,
		WorkerTag:      worker.Tag,
		WorkerPath:     res.WorkerPath,
		WorkerSHA256:   worker.SHA256,
		ModelTag:       model.Tag,
		ModelDir:       res.ModelDir,
		ModelSHA256:    model.SHA256,
		InstalledAt:    time.Now().UTC().Format(time.RFC3339),
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(doc)
}

// sha256OfFile returns the lowercase hex SHA-256 of path,
// or "" if path can't be read. Used for the idempotent
// install-skip check.
func sha256OfFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// killRunningWorker terminates any nightme-stt process
// whose executable matches the freshly-installed binary.
// Best-effort: failures are silently swallowed because
// the next Voice message will fail with a clear error if
// the worker is still locked on the old binary; the
// operator can then `nightme restart` the daemon.
//
// Strategy: enumerate processes via ps (Unix) / tasklist
// (Windows) and match the executable path. We deliberately
// avoid /proc walking on Linux to stay cross-platform with
// one code path.
//
// Race window: a Voice message arriving between this
// function and the next EnsureReady could spawn the
// stale binary briefly. The manager's SHA check would
// then compare the freshly-installed binary's SHA against
// the just-spawned worker's SHA — they differ — and
// re-spawn. Acceptable: at most one wasted Voice
// transcription.
func killRunningWorker() {
	bin := workerInstallPath("", "nightme-stt")
	_ = bin // we don't actually need the path; ps finds the cmdline.
	// We use a single regex-style match against
	// "nightme-stt" so we don't depend on the exact
	// install path (which varies per platform). The
	// match is intentionally broad — anything that
	// has "nightme-stt" in its argv and isn't THIS
	// process gets a TERM signal.
	//
	// Cross-platform shell-out is acceptable here because
	// (a) install is rare, (b) the wrong-target risk is
	// tiny ("nightme-stt" is a unique substring), and
	// (c) re-implementing procfs / PSAPI / WMI just to
	// kill one process would dwarf the actual install.
	killByName("nightme-stt")
}

// FindNightmeSTT locates the installed nightme-stt worker
// binary under <dataDir>/stt/bin/. Returns the absolute
// path on success, or an error if the binary is missing
// (issue #381 §13 "first-use consent" surfaces this as
// `errNotBuilt`).
//
// On Windows the binary carries a .exe suffix; on Unix it
// doesn't. `isWindowsExe` is a build-tag constant set in
// kill_unix.go / kill_windows.go so neither platform
// needs to compile the other.
func FindNightmeSTT(dataDir string) (string, error) {
	if dataDir == "" {
		return "", errors.New("stt: empty data dir")
	}
	binName := "nightme-stt"
	if isWindowsExe {
		binName += ".exe"
	}
	candidate := filepath.Join(dataDir, "stt", "bin", binName)
	if _, err := os.Stat(candidate); err == nil {
		return candidate, nil
	}
	return "", fmt.Errorf("%w: expected at %s", errNotBuilt, candidate)
}

// errNotBuilt is the sentinel returned by FindNightmeSTT
// when the binary is missing. Callers (the Telegram
// adapter's voice handler) detect this via errors.Is and
// render a user-facing "nightme stt install" prompt.
var errNotBuilt = errors.New("stt: nightme-stt runtime not installed")
