// Package main — `nightme stt` subcommand tree.
//
// `nightme stt status` mirrors `nightme status` in shape but
// reports on a different process: the nightme-stt worker that
// NightMe spawns for Voice transcription. The command is
// standalone — it does NOT require the nightme daemon to be
// running, only the conventional data dir (NIGHTME_PATHS_DATA_DIR
// > ~/.nightme, identical to what nightme core / nightme-stt use).
//
// Two distinct data sources:
//
//	runtime:  direct IPC probe of the worker endpoint
//	          (stt.ProbeWorkerStatus), so the command works
//	          when the nightme daemon is down — the most
//	          common case for "why isn't voice working?"
//	install:  filesystem stat on <dataDir>/stt/bin/nightme-stt
//	          and <dataDir>/stt/model/{model.onnx,tokens.txt}.
//	          These checks are independent of any process.
//
// The combined view answers both "is voice transcription ready
// right now?" and "is my install complete?" in one shot, matching
// the way `nightme status` answers "is the daemon ready right
// now?" + reports its config + state.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/stt"
	"github.com/cnlangzi/nightme/internal/updater"
	"github.com/cnlangzi/nightme/internal/version"
)

// isWindowsExe mirrors the build-tag constant in
// internal/stt/kill_{unix,windows}.go: the worker binary
// carries a .exe suffix on Windows, nothing on Unix. We
// resolve the expected name locally rather than dragging in
// the cross-platform helper to keep this file readable.
func sttBinaryName() string {
	if runtime.GOOS == "windows" {
		return "nightme-stt.exe"
	}
	return "nightme-stt"
}

func sttBinPath(dataDir string) string {
	return filepath.Join(dataDir, "stt", "bin", sttBinaryName())
}

func sttModelDir(dataDir string) string {
	return filepath.Join(dataDir, "stt", "model")
}

// sttProbe is the gathered snapshot `nightme stt status`
// renders. Each field is independently optional so the
// renderer can degrade gracefully when a part is unavailable
// (no worker → runtime.* zero, no binary → install.binary.*
// zero, etc.). Stable field names are part of the public CLI
// contract — the --json output keys off these.
type sttProbe struct {
	Runtime sttRuntime `json:"runtime"`
	Install sttInstall `json:"install"`
}

type sttRuntime struct {
	Running  bool   `json:"running"`
	PID      int    `json:"pid,omitempty"`
	Uptime   string `json:"uptime,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	Version  int    `json:"version,omitempty"`
	BuildVer string `json:"build_ver,omitempty"`
	// StartedAt is a time.Time (struct), so omitempty has no
	// effect — the JSON encoder always emits the field. We
	// keep it un-tagged rather than wrapping in *time.Time
	// because the CLI renderer doesn't need nil-vs-zero
	// distinction: Running=false already means "no worker",
	// and the renderer never reads StartedAt in that case.
	StartedAt time.Time `json:"started_at"`
}

type sttInstall struct {
	Binary     installEntry `json:"binary"`
	Model      installEntry `json:"model"`
	NightmeVer string       `json:"nightme_version"`
}

type installEntry struct {
	Path    string `json:"path"`
	Present bool   `json:"present"`
	Version string `json:"version,omitempty"`
}

// probeSTT gathers runtime + install state. Returns a populated
// sttProbe even on failure paths: a missing worker, a missing
// binary, and a missing model are all distinct legitimate
// outcomes that the renderer should show as their own rows
// rather than collapsing into one "error" message.
//
// The IPC dial has its own short timeout — a worker that hangs
// in handshake should not block the user's status read.
func probeSTT(ctx context.Context, dataDir string) sttProbe {
	p := sttProbe{
		Install: sttInstall{
			NightmeVer: version.Version,
		},
	}

	// --- install state ---
	binPath := sttBinPath(dataDir)
	if _, err := os.Stat(binPath); err == nil {
		p.Install.Binary = installEntry{
			Path:    binPath,
			Present: true,
			Version: version.Version, // same X.Y.Z — both built from the same -ldflags
		}
	} else {
		p.Install.Binary = installEntry{Path: binPath}
	}

	modelDir := sttModelDir(dataDir)
	modelFile := filepath.Join(modelDir, "model.onnx")
	tokensFile := filepath.Join(modelDir, "tokens.txt")
	if _, err := os.Stat(modelFile); err == nil {
		if _, err := os.Stat(tokensFile); err == nil {
			// Model version follows the nightme-stt build
			// (SenseVoice weights are versioned with
			// sherpa-onnx-go, which is pinned per nightme-stt
			// release). Surface as the same X.Y.Z; if a future
			// split puts model version on the worker OpStatus
			// payload, plug it in here.
			p.Install.Model = installEntry{
				Path:    modelDir,
				Present: true,
				Version: version.Version,
			}
		} else {
			p.Install.Model = installEntry{Path: modelDir}
		}
	} else {
		p.Install.Model = installEntry{Path: modelDir}
	}

	// --- runtime state ---
	ep, err := stt.DefaultEndpoint(dataDir)
	if err != nil {
		return p
	}
	p.Runtime.Endpoint = string(ep)

	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	ws, err := stt.ProbeWorkerStatus(probeCtx, ep)
	if err != nil {
		// ProbeWorkerStatus wraps ErrWorkerNotReachable around
		// the underlying transport error; either way, render
		// "not running" rather than the dial-stack noise.
		return p
	}
	p.Runtime.Running = true
	p.Runtime.PID = ws.PID
	p.Runtime.Uptime = time.Since(ws.StartedAt).Round(time.Second).String()
	p.Runtime.Version = ws.Version
	p.Runtime.BuildVer = ws.BuildVer
	p.Runtime.StartedAt = ws.StartedAt
	return p
}

// newSTTCmd is the `nightme stt` parent command. Mirrors the
// shape of `nightme update`: install / update / status all live
// here so the parent lists "everything you can do with the
// stt worker" in one help screen.
func newSTTCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stt",
		Short: "Manage the nightme-stt voice-transcription worker",
		Long:  "Inspect and manage the nightme-stt worker spawned for\nTelegram Voice transcription. Subcommands are standalone\n(they do not require the nightme daemon to be running) so they\nwork as install / upgrade diagnostics.",
	}
	cmd.AddCommand(
		newSTTStatusCmd(),
		newSTTInstallCmd(),
		newSTTUpdateCmd(),
	)
	return cmd
}

func newSTTStatusCmd() *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show nightme-stt install + runtime state",
		Long:  "Print the nightme-stt worker's runtime state (PID,\nuptime, IPC endpoint, version) alongside its install state\n(binary path + version, model path + version). Mirrors\n`nightme status`'s shape: a table by default, JSON via --json.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSTTStatus(cmd, jsonOutput)
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output as JSON")
	return cmd
}

func runSTTStatus(cmd *cobra.Command, jsonOutput bool) error {
	cfg, err := config.LoadDefault()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	probe := probeSTT(cmd.Context(), cfg.Paths.DataDir)

	if jsonOutput {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(probe)
	}
	renderSTTStatus(cmd.OutOrStdout(), probe)
	return nil
}

// renderSTTStatus prints the human-friendly view. The table
// layout intentionally mirrors `nightme status` (PID / STATE /
// UPTIME / ENDPOINT / VERSION) so a maintainer who learns one
// CLI's shape learns both. Two sections because runtime and
// install fail independently: a worker can be installed but
// not running, or running but with no model on disk. Merging
// them would hide which side is broken.
func renderSTTStatus(w io.Writer, p sttProbe) {
	fmt.Fprintln(w, "runtime")
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if !p.Runtime.Running {
		fmt.Fprintln(tw, "  state\tnot running\t\t\t")
	} else {
		fmt.Fprintf(tw, "  PID\t%d\t\t\t\n", p.Runtime.PID)
		fmt.Fprintf(tw, "  state\trunning\t\t\t\n")
		fmt.Fprintf(tw, "  uptime\t%s\t\t\t\n", p.Runtime.Uptime)
		fmt.Fprintf(tw, "  endpoint\t%s\t\t\t\n", p.Runtime.Endpoint)
		fmt.Fprintf(tw, "  version\t%d\t\t\t\n", p.Runtime.Version)
		fmt.Fprintf(tw, "  build_ver\t%s\t\t\t\n", p.Runtime.BuildVer)
		if p.Runtime.BuildVer != p.Install.NightmeVer {
			fmt.Fprintf(tw, "  mismatch\tworker=%s nightme=%s\t\t\t\n",
				p.Runtime.BuildVer, p.Install.NightmeVer)
		}
	}
	_ = tw.Flush()

	fmt.Fprintln(w, "install")
	tw = tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "  binary\t%s\t%s\t\n", formatInstallEntry(p.Install.Binary),
		mismatchNote(p.Install.Binary.Version, p.Install.NightmeVer))
	fmt.Fprintf(tw, "  model\t%s\t%s\t\n", formatInstallEntry(p.Install.Model),
		modelHint(p.Install.Model))
	_ = tw.Flush()

	// One-line summary so `nightme stt status | grep ready` works.
	switch {
	case !p.Install.Binary.Present:
		fmt.Fprintln(w, "voice transcription: unavailable — run `nightme stt install`")
	case !p.Install.Model.Present:
		fmt.Fprintln(w, "voice transcription: unavailable — run `nightme-stt install model`")
	case !p.Runtime.Running:
		fmt.Fprintln(w, "voice transcription: installed, worker not running")
	default:
		fmt.Fprintln(w, "voice transcription: ready")
	}
}

func formatInstallEntry(e installEntry) string {
	if !e.Present {
		return "missing  " + e.Path
	}
	return "ok      " + e.Path
}

// mismatchNote flags a worker/build_ver != nightme_ver so the
// rendered install row stands out. A mismatch means the user
// ran `nightme update` (which bumped nightme core) but not
// `nightme stt update` (which would re-fetch the matching
// worker); the worker is still functional but the install is
// out of sync. Returned as a parenthetical so the table column
// width is predictable.
func mismatchNote(installed, expected string) string {
	if installed == "" || installed == expected {
		return "v" + installed
	}
	return fmt.Sprintf("v%s (nightme=%s)", installed, expected)
}

// modelHint points the user at the right next command when the
// model is missing. The CLI never assumes the user has the
// nightme-stt binary on PATH yet (they may have just
// reinstalled nightme core without reinstalling the worker),
// so the model-install command is rendered conditionally.
func modelHint(e installEntry) string {
	if e.Present {
		return "v" + e.Version
	}
	return "run `nightme-stt install model`"
}

// ----- nightme stt install / update -----------------------------------
//
// Both commands share the same skeleton (resolve latest
// release → download + SHA256-verify → extract → swap binary
// in <dataDir>/stt/bin/), so the work lives in
// runSTTDownloadAndInstall and the two commands just decide
// whether to refuse when a binary already exists.
//
// `install` is the first-time path: refuse if the worker
// binary is already on disk (the user's mental model is
// "I'm setting this up"), and require --force to overwrite.
// `update` is the upgrade path: always overwrite — that's
// the whole point of an explicit update verb.
//
// Both go through internal/updater.DownloadSTT + Install,
// which mirror the nightme-self-update flow with two
// surgical changes (asset name pattern + archive basename).
// See internal/updater/updater.go's DownloadSTT doc for the
// parameterization rationale.

// newSTTInstallCmd installs the latest nightme-stt release.
// Refuses if the worker binary is already on disk unless
// --force is set, matching the convention `apt install`
// follows: "you probably meant update if it already exists."
func newSTTInstallCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install nightme-stt worker binary from GitHub Releases",
		Long: "Download the latest nightme-stt release from GitHub\n" +
			"(nightme.dev mirror fallback), SHA256-verify against the\n" +
			"release's SHA256SUMS.txt, and place the binary under\n" +
			"~/.nightme/stt/bin/nightme-stt[.exe].\n\n" +
			"Refuses to overwrite an existing install; use --force or\n" +
			"run `nightme stt update` to upgrade.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSTTInstall(cmd, force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false,
		"Overwrite an existing nightme-stt install")
	return cmd
}

// newSTTUpdateCmd upgrades nightme-stt in place. Always
// proceeds — the only way to reach "no change" with this
// verb is if the latest GitHub tag matches whatever's on
// disk, in which case we still re-write the file (the user
// explicitly asked). This matches `nightme update`'s
// "always overwrite" semantics.
func newSTTUpdateCmd() *cobra.Command {
	var quiet bool
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update nightme-stt worker to the latest release",
		Long: "Download the latest nightme-stt release and replace\n" +
			"the binary under ~/.nightme/stt/bin/. Unlike `install`,\n" +
			"this always overwrites the existing binary — use it\n" +
			"after a `nightme update` to keep the worker in sync.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSTTUpdate(cmd, quiet)
		},
	}
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false,
		"Suppress progress bar (still verifies SHA256)")
	return cmd
}

func runSTTInstall(cmd *cobra.Command, force bool) error {
	cfg, err := config.LoadDefault()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	targetPath := sttBinPath(cfg.Paths.DataDir)

	if !force {
		if _, err := os.Stat(targetPath); err == nil {
			return fmt.Errorf("nightme-stt already installed at %s\n"+
				"  run `nightme stt update` to upgrade, or pass --force to overwrite",
				targetPath)
		}
	}

	return runSTTDownloadAndInstall(cmd, cfg.Paths.DataDir, targetPath)
}

func runSTTUpdate(cmd *cobra.Command, quiet bool) error {
	cfg, err := config.LoadDefault()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	_ = quiet // reserved for a future --quiet plumbing the progress bar
	return runSTTDownloadAndInstall(cmd, cfg.Paths.DataDir, sttBinPath(cfg.Paths.DataDir))
}

// runSTTDownloadAndInstall is the shared three-stage path
// install + update both walk: download the latest GitHub
// release, verify SHA256, extract, swap the worker binary.
// Errors before the file replacement are pure (no rollback
// needed); updater.Install handles backup + atomic copy + a
// rollback on copy failure.
//
// Note on Windows file locking: the worker holds an open
// handle on its own executable for the lifetime of the
// process. If it's currently running, copyFile will fail
// with a sharing-violation error. The nightme daemon is the
// worker's parent; stopping it (`nightme stop`) terminates
// the worker too, so users hitting that error path get a
// clear next-step from the Install error message itself.
func runSTTDownloadAndInstall(cmd *cobra.Command, dataDir, targetPath string) error {
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()

	logf := func(format string, args ...any) {
		fmt.Fprintf(errOut, "  %s  %s\n", paintDim(out, "·"), fmt.Sprintf(format, args...))
	}

	logf("fetching latest release via github…")
	progress := updater.NewASCIIProgressBar(out, 0)
	dlRes, err := updater.DownloadSTT(cmd.Context(), dataDir, progress)
	if err != nil {
		fmt.Fprintf(errOut, "  %s  download failed: %v\n", paintRed(out, "✗"), err)
		return err
	}
	if dlRes.Source == "mirror" {
		logf("using mirror (github was unreachable)")
	}
	fmt.Fprintf(out, "  %s  Staged %s  %s\n",
		paintGreen(out, "✓"),
		dlRes.AssetName,
		paintDim(out, "sha256="+dlRes.SHA256Hex))

	installRes, err := updater.Install(dlRes.BinaryPath, targetPath)
	if err != nil {
		fmt.Fprintf(errOut, "  %s  install failed: %v\n", paintRed(out, "✗"), err)
		fmt.Fprintln(errOut, "     if the worker is running, `nightme stop` then retry")
		return err
	}
	fmt.Fprintf(out, "  %s  installed %s\n", paintGreen(out, "✓"), installRes.NewBinaryPath)
	fmt.Fprintf(out, "     %s %s\n", paintDim(out, "backup"), paintDim(out, installRes.OldBinaryPath))
	fmt.Fprintf(out, "  %s  worker will pick up the new binary on next spawn\n",
		paintDim(out, "→"))
	return nil
}
