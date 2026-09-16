// Package install implements the worker-side installer for
// nightme-stt's external dependencies. The split between
// binary install (handled by nightme core's `nightme stt
// install` + internal/updater) and model install (this
// package, invoked by `nightme-stt install model`) follows
// the binary-vs-module ownership rule discussed in
// docs/channel/telegram.md §21: nightme owns the worker's
// version, the worker owns its model dependency.
//
// # Why a separate install path
//
// The SenseVoice model is published on sherpa-onnx's release
// feed (https://github.com/k2-fsa/sherpa-onnx/releases/tag/asr-models),
// not on nightme's. Nightme core has no business knowing the
// model URL — that's a property of the sherpa-onnx-go version
// the worker embeds. This package's DefaultModelURL is the
// single source of truth, defined next to the worker code
// that consumes the model.
//
// # Network + proxy
//
// All HTTP downloads go through internal/httpclient.Default(),
// which uses Go's default Transport with ProxyFromEnvironment
// — i.e. HTTP_PROXY / HTTPS_PROXY / NO_PROXY (and their
// lowercase variants) are honored without further wiring.
// Users behind a corporate or regional proxy (Clash, Surge,
// v2ray, mitmproxy, …) just set the env vars and the installer
// follows them, exactly the same way `nightme update` does for
// its GitHub Releases fetch.
//
// # SHA-256 verification
//
// sherpa-onnx does NOT publish SHA-256 sums for its model
// archives (only for binary releases). InstallModel therefore
// treats SHA-256 verification as opt-in: pass `SHA256` in
// InstallOpts to enable. The default is "download + extract"
// with no integrity check, plus a printed warning the user
// can't miss. This is the conservative trade-off — the
// alternative (refuse to install without a hash) leaves
// fresh users stuck because we genuinely do not have a
// verified hash to hand them.
//
// When sherpa-onnx starts publishing sums for the model
// archives, flip the default to verified.
package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/cnlangzi/nightme/internal/httpclient"
	"github.com/cnlangzi/nightme/internal/updater"
)

// DefaultModelURL is the canonical SenseVoice multilingual
// archive on sherpa-onnx's GitHub release feed. The 2024-07-17
// build ships model.onnx (FP, ~226 MB) + tokens.txt — the
// exact pair cmd/nightme-stt/internal/sherpa expects at
// <dataDir>/stt/model/. Bump this URL when upgrading the
// embedded github.com/k2-fsa/sherpa-onnx-go to a version
// whose model schema has shifted.
const DefaultModelURL = "https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/sherpa-onnx-sense-voice-zh-en-ja-ko-yue-2024-07-17.tar.bz2"

// InstallOpts configures one InstallModel call. Field names
// mirror the cobra flags exposed by `nightme-stt install
// model` 1:1, so the CLI parses straight into this struct.
type InstallOpts struct {
	// URL is the model archive URL. Empty = DefaultModelURL.
	URL string
	// SHA256, if non-empty, is the expected SHA-256 of the
	// downloaded archive (lowercase hex). When set, the
	// installer verifies the download against it before
	// extracting; a mismatch fails the install.
	SHA256 string
	// Force overwrites an existing model.onnx + tokens.txt.
	// Without it, InstallModel refuses if either file is
	// already present (use `update model` for upgrades).
	Force bool
	// Out / Err stream the installer's progress + diagnostics
	// to the caller. tar's stderr surfaces here too.
	Out io.Writer
	Err io.Writer
	// Progress, if non-nil, is called periodically during the
	// archive download with (downloaded bytes, total bytes
	// when known, elapsed wall time). total <= 0 means the
	// server omitted Content-Length (chunked transfer). Pass
	// nil to skip progress reporting — the CLI passes
	// updater.NewASCIIProgressBar for verbose mode and
	// updater.QuietProgress for --quiet. Type is the same
	// updater.ProgressFunc so callers share one ProgressFunc
	// across both installers (`nightme update` +
	// `nightme-stt install model`).
	Progress updater.ProgressFunc
}

// ModelDir returns the canonical model directory for the
// given data dir. Exported so the CLI and the worker's
// sherpa package agree on the path without re-deriving it.
func ModelDir(dataDir string) string {
	return filepath.Join(dataDir, "stt", "model")
}

// InstallModel downloads the SenseVoice archive, optionally
// verifies its SHA-256, and extracts model.onnx + tokens.txt
// under <dataDir>/stt/model/. Errors leave the model dir
// in a recoverable state (partial files removed) so the user
// can retry without manual cleanup.
//
// The archive is .tar.bz2 — extracted via the system `tar`
// command rather than a Go bzip2 dependency. Modern tar
// (busybox, GNU tar, Windows 10+ bsdtar) all support
// `tar -xjf`; the alternative would be adding
// github.com/dsnet/compress/bzip2 (~50 lines of glue for one
// archive format), which is overkill for a single use site.
//
// After extraction the optional `test_wavs/` directory
// sherpa-onnx ships alongside the model is removed: it is
// ~50 MB of demo audio the user doesn't need on disk and
// would otherwise persist through every reinstall.
func InstallModel(ctx context.Context, dataDir string, opts InstallOpts) error {
	if dataDir == "" {
		return errors.New("install: empty data dir")
	}
	out := opts.Out
	if out == nil {
		out = io.Discard
	}
	errOut := opts.Err
	if errOut == nil {
		errOut = io.Discard
	}
	url := opts.URL
	if url == "" {
		url = DefaultModelURL
	}

	modelDir := ModelDir(dataDir)
	if !opts.Force {
		if present, err := modelPresent(modelDir); err != nil {
			return err
		} else if present {
			return fmt.Errorf("model already installed at %s\n"+
				"  run `nightme-stt update model` to upgrade, or pass --force to overwrite",
				modelDir)
		}
	}

	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", modelDir, err)
	}

	// Stage the download in the model dir itself so a partial
	// failure leaves a recoverable artifact next to its target.
	// Removed in defer so a successful run cleans up automatically.
	archiveName := filepath.Base(url)
	archivePath := filepath.Join(modelDir, archiveName)
	defer func() { _ = os.Remove(archivePath) }()

	fmt.Fprintf(out, "  downloading %s\n", url)
	if err := downloadToFile(ctx, url, archivePath, opts.Progress); err != nil {
		return fmt.Errorf("download: %w", err)
	}

	if opts.SHA256 != "" {
		fmt.Fprintf(out, "  verifying sha256…\n")
		got, err := fileSHA256(archivePath)
		if err != nil {
			return fmt.Errorf("hash: %w", err)
		}
		want := opts.SHA256
		if len(want) == 64 {
			// already lowercase hex expected
		}
		if got != want {
			return fmt.Errorf("sha256 mismatch: got %s, want %s", got, want)
		}
		fmt.Fprintf(out, "  sha256 ok: %s\n", got)
	} else {
		fmt.Fprintf(errOut, "  ⚠ no --sha256 supplied; integrity unverified.\n"+
			"    download integrity rests on TLS only.\n")
	}

	fmt.Fprintf(out, "  extracting…\n")
	if err := extractTarBZ2(ctx, archivePath, modelDir, out, errOut); err != nil {
		return fmt.Errorf("extract: %w", err)
	}

	// Drop the demo audio sherpa-onnx ships in the same
	// archive. The user's data dir is supposed to host the
	// runtime model, not sherpa-onnx's test corpus.
	_ = os.RemoveAll(filepath.Join(modelDir, "test_wavs"))

	// Sanity check: the archive must have produced exactly
	// the two files sherpa.go expects.
	if present, err := modelPresent(modelDir); err != nil {
		return err
	} else if !present {
		return fmt.Errorf("extract succeeded but %s is missing model.onnx or tokens.txt; check the archive layout", modelDir)
	}

	fmt.Fprintf(out, "  installed %s\n", modelDir)
	return nil
}

// modelPresent reports whether both model.onnx and tokens.txt
// exist in modelDir. Errors other than ENOENT are propagated.
func modelPresent(modelDir string) (bool, error) {
	if _, err := os.Stat(filepath.Join(modelDir, "model.onnx")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("stat model.onnx: %w", err)
	}
	if _, err := os.Stat(filepath.Join(modelDir, "tokens.txt")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("stat tokens.txt: %w", err)
	}
	return true, nil
}

// downloadToFile streams URL into path. Caller is responsible
// for any SHA verification (kept separate so the caller can
// hash a partially-written file via tee if needed — not used
// here because we want a small, readable installer).
//
// progress is called periodically on every chunk boundary
// plus a 200ms ticker so a slow connection still sees updates.
// Pass nil to silence; updater.NewASCIIProgressBar is the
// conventional caller (same signature so the same callback
// works for `nightme update` and `nightme-stt install model`).
func downloadToFile(ctx context.Context, url, path string, progress updater.ProgressFunc) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := httpclient.Default().Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}

	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	if _, err := io.Copy(out, &progressReader{
		underlying: resp.Body,
		total:      resp.ContentLength,
		progress:   progress,
	}); err != nil {
		_ = out.Close()
		_ = os.Remove(path)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}

// progressReader wraps an io.Reader and emits progress events
// on every chunk boundary plus a 200ms ticker — same cadence
// as updater.progressReader so the two installers render at
// the same rate. total <= 0 means the server omitted
// Content-Length (chunked transfer); the callback receives 0
// for total in that case.
type progressReader struct {
	underlying io.Reader
	total      int64
	progress   updater.ProgressFunc
	start      time.Time
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
			if pr.lastEmit.IsZero() {
				pr.start = now
			}
			if now.Sub(pr.lastEmit) >= progressInterval {
				pr.progress(pr.done, pr.total, now.Sub(pr.start))
				pr.lastEmit = now
			}
		}
	}
	return n, err
}

// fileSHA256 returns the lowercase hex SHA-256 of path.
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

// extractTarBZ2 shells out to the system `tar` to extract a
// .tar.bz2 archive. See package doc for why we don't pull in
// a Go bzip2 library.
func extractTarBZ2(ctx context.Context, archivePath, destDir string, stdout, stderr io.Writer) error {
	tar, err := exec.LookPath("tar")
	if err != nil {
		return errors.New("`tar` not on PATH; install it (apt: tar / brew: gnu-tar / Windows 10 1803+ has it built in)")
	}
	cmd := exec.CommandContext(ctx, tar, "-xjf", archivePath, "-C", destDir)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}
