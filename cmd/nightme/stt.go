// Package main — `nightme stt` management subcommands.
//
// Voice transcription runs out-of-process: nightme-stt is
// spawned on demand and shut down with the daemon. The
// commands here are administrative conveniences for users
// who want to inspect or pre-install the runtime; normal
// Voice usage does not require them (issue #381 §3, §16).
//
// install/update query the GitHub Releases API anonymously
// for the latest nightme + the latest k2-fsa/sherpa-onnx
// SenseVoice model, download each, SHA-256-verify against
// GitHub's per-asset digest, and place the files under
// <dataDir>/stt/. See internal/stt/{release,installer}.go
// for the integrity guarantees.
//
// `update` mirrors nightme's `update` flow: three stages
// (check → fetch → activate) with up-to-date short-circuit
// and per-asset atomic activation. The Install() path
// (used by `install`) is the same machinery minus the
// check + up-to-date print.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/stt"
)

func newSTTCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stt",
		Short: "Inspect / manage the local voice transcription worker",
		Long: `Inspect and (optionally) pre-install the nightme-stt worker
process. Voice messages from Telegram are transcribed by a
separate nightme-stt process that NightMe spawns on demand;
these commands exist so users can prepare or inspect the
runtime without waiting for the first voice message.

Subcommands:
  nightme stt status     installation + readiness summary
  nightme stt install    install runtime + model from latest GitHub release
  nightme stt update     replace runtime + model with latest release
  nightme stt uninstall  remove installed runtime + model

Normal voice usage does not require any of these.`,
	}
	cmd.AddCommand(newSTTStatusCmd())
	cmd.AddCommand(newSTTInstallCmd())
	cmd.AddCommand(newSTTUpdateCmd())
	cmd.AddCommand(newSTTUninstallCmd())
	return cmd
}

func newSTTStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Print voice transcription readiness",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSTTStatus(cmd.OutOrStdout())
		},
	}
}

func newSTTInstallCmd() *cobra.Command {
	var (
		workerOnly bool
		modelOnly  bool
	)
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install nightme-stt runtime + SenseVoice model from latest GitHub release",
		Long: "Resolve the latest nightme-stt binary + SenseVoice model\n" +
			"from GitHub, download each, SHA-256 verify, and activate under\n" +
			"<dataDir>/stt/. Idempotent — re-running after a successful install\n" +
			"is a no-op.\n\n" +
			"By default both the worker binary and the SenseVoice model are\n" +
			"installed. --worker-only / --model-only restrict to one.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSTTInstall(cmd.OutOrStdout(), sttOpts{
				workerOnly: workerOnly,
				modelOnly:  modelOnly,
			})
		},
	}
	cmd.Flags().BoolVar(&workerOnly, "worker-only", false,
		"Install only the nightme-stt binary (skip SenseVoice model)")
	cmd.Flags().BoolVar(&modelOnly, "model-only", false,
		"Install only the SenseVoice model (skip nightme-stt binary)")
	return cmd
}

func newSTTUpdateCmd() *cobra.Command {
	var (
		quiet      bool
		workerOnly bool
		modelOnly  bool
	)
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update nightme-stt + SenseVoice model to latest GitHub release",
		Long: "Walk the STT self-update flow end to end:\n" +
			"\n" +
			"  1. check     resolve the latest release; bail if up-to-date\n" +
			"  2. fetch     download the matching assets + SHA256 verify\n" +
			"  3. activate  move verified artifacts to the live install path;\n" +
			"             stop any running nightme-stt so the next Voice\n" +
			"             message spawns the new binary\n" +
			"\n" +
			"All three stages run in a single invocation. Per-asset\n" +
			"atomic activation means a failure in stage 2 leaves the\n" +
			"already-activated artifact in place.\n\n" +
			"--worker-only / --model-only restrict to one artifact.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSTTUpdate(cmd.OutOrStdout(), sttOpts{
				quiet:      quiet,
				workerOnly: workerOnly,
				modelOnly:  modelOnly,
			})
		},
	}
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false,
		"Suppress progress output (still verifies SHA256)")
	cmd.Flags().BoolVar(&workerOnly, "worker-only", false,
		"Update only the nightme-stt binary")
	cmd.Flags().BoolVar(&modelOnly, "model-only", false,
		"Update only the SenseVoice model")
	return cmd
}

func newSTTUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the installed nightme-stt runtime + model",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSTTUninstall(cmd.OutOrStdout())
		},
	}
}

// sttOpts is the parsed-flag bundle shared by install
// and update. Centralising keeps the function signatures
// stable as flags grow.
type sttOpts struct {
	quiet      bool
	workerOnly bool
	modelOnly  bool
}

// installer returns a fresh Installer bound to the
// resolved data dir. Both install and update share this
// helper so config-loading failures land in one place.
func installer() (*stt.Installer, error) {
	cfg, err := config.LoadDefault()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	if cfg == nil || cfg.Paths.DataDir == "" {
		return nil, errors.New("nightme: data dir not configured")
	}
	workerR, modelR := stt.DefaultGitHubResolvers()
	inst, err := stt.NewInstaller(cfg.Paths.DataDir, workerR, modelR)
	if err != nil {
		return nil, err
	}
	return inst, nil
}

// runSTTInstall is the first-time-setup path. It does
// not call Check — install always downloads and installs.
// If everything is already on disk at the right SHA, the
// download + extract phases no-op and the call returns
// quickly.
//
// NoRestart=true: install does not stop any running
// nightme-stt because there shouldn't be one yet (the
// first Voice message that triggers EnsureReady will
// pick up the freshly-activated binary).
func runSTTInstall(out io.Writer, opts sttOpts) error {
	inst, err := installer()
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "🎙️ Resolving latest nightme-stt + SenseVoice from GitHub…")
	res, err := inst.InstallWithOptions(context.Background(),
		func(bytes, total int64) {
			if total > 0 && !opts.quiet {
				fmt.Fprintf(out, "\r   %d / %d bytes (%d%%)",
					bytes, total, bytes*100/total)
			}
		},
		stt.ActivateOptions{NoRestart: true},
	)
	if err != nil {
		return fmt.Errorf("install: %w", err)
	}
	fmt.Fprintln(out, "")
	fmt.Fprintf(out, "✓ Worker binary: %s (version %s, installed=%v)\n",
		res.WorkerPath, res.WorkerVersion, res.WorkerInstalled)
	fmt.Fprintf(out, "✓ SenseVoice model: %s (installed=%v)\n", res.ModelDir, res.ModelInstalled)
	fmt.Fprintf(out, "✓ Downloaded %.1f MB total\n", float64(res.BytesDownloaded)/(1<<20))
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Voice transcription is ready. Send a Telegram Voice message to test.")
	return nil
}

// runSTTUpdate mirrors nightme's `nightme update` flow.
// Three observable stages, each printed with a status
// indicator mirroring the nightme update CLI style:
//
//	✓ Already up to date        (skip; nothing to do)
//	⚠ Update available           (GitHub has newer)
//	✓ Staged <asset>  sha256=…   (downloaded + verified)
//	✓ Installed <asset>          (atomic move done)
//	✓ Done.                       (worker restart signaled)
//
// Per-asset atomic activation: a model-side failure
// leaves the already-activated worker in place and
// returns a non-nil error so the user knows to retry.
func runSTTUpdate(out io.Writer, opts sttOpts) error {
	inst, err := installer()
	if err != nil {
		return err
	}
	ctx := context.Background()

	// Stage 1: check
	check, err := inst.Check(ctx)
	if err != nil {
		return fmt.Errorf("update: check: %w", err)
	}
	fmt.Fprintln(out)
	if check.WorkerUpToDate && check.ModelUpToDate {
		fmt.Fprintf(out, "  %s  Already up to date\n", paintGreen(out, "✓"))
		fmt.Fprintf(out, "     worker %s  model up-to-date\n",
			paintDim(out, check.LatestWorker.Tag))
		return nil
	}
	fmt.Fprintf(out, "  %s  Update available\n", paintYellow(out, "▲"))
	if !check.WorkerUpToDate {
		fmt.Fprintf(out, "     worker:  %s\n", paintDim(out, check.LatestWorker.Tag))
	}
	if !check.ModelUpToDate {
		fmt.Fprintf(out, "     model:   %s\n", paintDim(out, check.LatestModel.Tag))
	}

	// Stage 2 + 3: fetch + activate (combined in Install's
	// per-asset path). Print progress as it happens.
	fmt.Fprintf(out, "  %s  fetching + activating…\n", paintDim(out, "·"))
	progress := func(bytes, total int64) {
		if total > 0 && !opts.quiet {
			fmt.Fprintf(out, "\r   %d / %d bytes (%d%%)",
				bytes, total, bytes*100/total)
		}
	}
	res, err := inst.InstallWithOptions(ctx, progress, stt.ActivateOptions{})
	if err != nil {
		fmt.Fprintln(out)
		fmt.Fprintf(out, "  %s  install failed: %v\n", paintRed(out, "✗"), err)
		return err
	}
	fmt.Fprintln(out)
	if res.WorkerInstalled {
		fmt.Fprintf(out, "  %s  installed worker %s\n",
			paintGreen(out, "✓"), res.WorkerVersion)
	}
	if res.ModelInstalled {
		fmt.Fprintf(out, "  %s  installed model  %s\n",
			paintGreen(out, "✓"), res.ModelTag)
	}
	fmt.Fprintf(out, "  %s  Worker SHA: %s\n",
		paintDim(out, "·"), res.WorkerTag)
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  %s  Updated to %s — restart the daemon to pick up the new worker.\n",
		paintGreen(out, "✓"), res.WorkerVersion)
	return nil
}

func runSTTStatus(out io.Writer) error {
	cfg, err := config.LoadDefault()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if cfg == nil || cfg.Paths.DataDir == "" {
		return errors.New("nightme: data dir not configured")
	}
	bin, err := stt.FindNightmeSTT(cfg.Paths.DataDir)
	if err != nil {
		fmt.Fprintln(out, "STT runtime: not installed")
		fmt.Fprintln(out, "STT model:   not installed")
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "Voice messages will prompt to install on first use.")
		return nil
	}
	fmt.Fprintf(out, "STT runtime: %s\n", bin)
	fmt.Fprintf(out, "STT model:   %s/models/sensevoice\n", cfg.Paths.DataDir)
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Voice transcription: ready when worker is running.")
	return nil
}

func runSTTUninstall(out io.Writer) error {
	cfg, err := config.LoadDefault()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if cfg == nil || cfg.Paths.DataDir == "" {
		return errors.New("nightme: data dir not configured")
	}
	sttDir := cfg.Paths.DataDir + "/stt"
	if err := os.RemoveAll(sttDir); err != nil {
		return fmt.Errorf("remove %s: %w", sttDir, err)
	}
	fmt.Fprintf(out, "Removed %s\n", sttDir)
	return nil
}
