// nightme-stt is the local voice-to-text worker. NightMe core spawns
// it as a child process and talks to it over a Unix-socket IPC
// endpoint defined by internal/stt. The worker handles the heavy
// native dependencies (sherpa-onnx, ONNX Runtime, SenseVoice model)
// so NightMe core stays pure-Go for users who never send a Telegram
// Voice message.
//
// CLI surface mirrors cmd/nightme (cobra + subcommands + --version),
// so a maintainer who learns one binary's CLI structure learns both:
//
//	nightme-stt                  serve (default)
//	nightme-stt serve            run the worker
//	nightme-stt version          print version + exit
//	nightme-stt --version        print version + exit
//	nightme-stt --help           cobra's auto-generated help
//
// The worker has no runtime flags by design. The data dir and IPC
// endpoint are global conventions shared with nightme core:
//
//   - dataDir: NIGHTME_PATHS_DATA_DIR > ~/.nightme
//   - endpoint: <dataDir>/stt/stt.sock (Unix) / \\.\pipe\nightme-stt (Windows)
//
// Same precedence nightme core uses in internal/config (config.go
// expandHome + applyEnvOverrides), so the worker and core agree on
// where to find the socket without a flag handshake at spawn time.
// The spawner (internal/stt/spawner.go) launches the binary with
// no flags and the worker derives both values from its own env.
//
// See docs/channel/telegram.md §21 for the full architectural
// rationale and issue #381 for the protocol contract.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/cmd/nightme-stt/internal/ffmpeg"
	"github.com/cnlangzi/nightme/cmd/nightme-stt/internal/install"
	"github.com/cnlangzi/nightme/cmd/nightme-stt/internal/sherpa"
	"github.com/cnlangzi/nightme/internal/stt"
	"github.com/cnlangzi/nightme/internal/updater"
	"github.com/cnlangzi/nightme/internal/version"
)

func init() {
	// Mirror cmd/nightme/main.go: disable Cobra's Windows
	// mousetrap so a double-clicked nightme-stt.exe does not
	// print "This is a command line tool" and sleep 5s before
	// exiting. nightme-stt has no interactive mode, but it IS
	// spawned by explorer-launched processes (e.g. a tray menu
	// item on Windows) and the mousetrap check would still fire
	// once per Execute() call from any parent that cobra thinks
	// is explorer.exe.
	cobra.MousetrapHelpText = ""
}

func main() {
	if err := Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

// Execute builds the cobra command tree and runs it. Split out so
// future entry points (tests, alt binaries) can reuse the same tree
// without going through main().
func Execute() error {
	root := newRootCmd()
	return root.Execute()
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "nightme-stt",
		Short: "Local STT worker for NightMe",
		Long: "nightme-stt is the local voice-to-text worker spawned by\n" +
			"NightMe core to transcribe Telegram Voice messages. It loads\n" +
			"the SenseVoice model via sherpa-onnx-go and exposes an IPC\n" +
			"endpoint at the conventional <data-dir>/stt/stt.sock.\n\n" +
			"Bare invocation starts the worker; `serve` is an explicit\n" +
			"alias. There are no runtime flags — the data dir is\n" +
			"resolved from $NIGHTME_PATHS_DATA_DIR (falling back to\n" +
			"~/.nightme) so the spawner and the worker agree on the\n" +
			"socket path without a flag handshake.\n\n" +
			"See docs/channel/telegram.md §21 for the protocol contract.",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version.Version,
		// Bare `nightme-stt` → start the worker. Mirrors how
		// `nightme` enters the REPL on bare invocation: the
		// binary's primary mode IS its no-args form, so root
		// itself runs the action.
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe()
		},
	}
	root.SetVersionTemplate(sttBanner() + "\n")

	root.AddCommand(
		newServeCmd(),
		newVersionCmd(),
		newInstallCmd(),
		newUpdateCmd(),
	)
	return root
}

// newServeCmd is the explicit `serve` subcommand. Same body as the
// root's default RunE — kept as a sibling so docs and `nightme-stt
// --help` both surface it, and so a future "install / update" mode
// can sit alongside without inheriting serve's defaults.
func newServeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the STT worker",
		Long:  "Run the nightme-stt worker. Listens on the conventional\nendpoint (<data-dir>/stt/stt.sock) and serves Decode + Recognize\nover the IPC protocol defined by internal/stt.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe()
		},
	}
}

// newVersionCmd prints the version banner and exits. Cobra's
// built-in --version flag (wired via root.Version) prints the same
// banner via SetVersionTemplate, so the two paths stay in lockstep.
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the nightme-stt version and exit",
		Long:  "Print the nightme-stt version metadata (version, commit,\nbuild date) and exit.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := cmd.OutOrStdout().Write([]byte(sttBanner() + "\n"))
			return err
		},
	}
}

// runServe is the worker entry point — extracted from the inline
// main() body so both root's default RunE and `serve` can call it.
func runServe() error {
	dataDir := resolveDataDir()
	endpoint, err := stt.DefaultEndpoint(dataDir)
	if err != nil {
		return fmt.Errorf("resolve endpoint: %w", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	// Lazy recognizer load: this is where the SenseVoice model
	// weights get read into RAM. If the model is missing or the
	// CGO binding fails to load, surface a clean error so the
	// manager can render "run nightme stt install" to the user
	// instead of crashing with a CGo stack trace.
	rec, err := sherpa.New(dataDir)
	if err != nil {
		return fmt.Errorf("load recognizer: %w", err)
	}
	defer rec.Close()

	dec := ffmpeg.New(ffmpeg.DefaultConfig())

	listener, err := stt.DefaultTransport().Listen(context.Background(), endpoint)
	if err != nil {
		return fmt.Errorf("listen %s: %w", endpoint, err)
	}
	defer listener.Close()

	logger.Info("nightme-stt: listening", "endpoint", string(listener.Endpoint()))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := stt.NewServer(listener, dec, rec, logger)

	// Watchdog: if Serve returns without ctx being cancelled
	// (e.g. the listener was closed from another goroutine),
	// surface the error rather than silently exiting 0 — the
	// manager's spawn cycle expects the worker to stay up
	// until SIGTERM.
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()

	select {
	case <-ctx.Done():
		logger.Info("nightme-stt: signal received, shutting down")
		return nil
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("serve: %w", err)
		}
		return nil
	}
}

// resolveDataDir mirrors the precedence used by nightme core
// (internal/config/config.go:418 + applyEnvOverrides):
// NIGHTME_PATHS_DATA_DIR env > ~/.nightme. nightme core resolves
// the same way at startup, so both binaries agree on the data
// dir without any flag handshake at spawn time. ~ expansion
// matches config.expandHome so the resolved path is identical to
// what nightme core would compute for the same input.
func resolveDataDir() string {
	if v := os.Getenv("NIGHTME_PATHS_DATA_DIR"); v != "" {
		return expandHome(v)
	}
	return expandHome("~/.nightme")
}

// expandHome mirrors internal/config/config.go:expandHome — "~"
// or "~/..." → the user's home dir. Inlined here because the
// config package's helper is unexported, and the logic is small
// enough that adding an exported helper for one extra caller is
// more cost than duplication. If a third caller appears, lift
// this into internal/nightmedir.
func expandHome(p string) string {
	if p == "" || !strings.HasPrefix(p, "~") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// sttBanner is the version banner printed by `nightme-stt
// --version` and `nightme-stt version`. Same shape as
// internal/version.String() but with the "nightme-stt" prefix so
// logs / scrapers reading the worker's banner know which binary
// they came from. The shared internal/version fields guarantee
// the two binaries agree on Version / GitCommit / BuildDate
// byte-for-byte.
func sttBanner() string {
	return fmt.Sprintf("nightme-stt version %s (commit: %s, built: %s)",
		version.Version, version.GitCommit, version.BuildDate)
}

// ----- install / update ---------------------------------------------
//
// The worker owns its model dependency per the binary-vs-module
// ownership split (docs/channel/telegram.md §21): nightme
// manages the worker's binary version, the worker manages its
// own SenseVoice model install. So the CLI surface is:
//
//	nightme-stt install model   download + extract SenseVoice
//	                            model to <dataDir>/stt/model/
//	nightme-stt update model    same flow, always overwrite
//
// install refuses if the model is already present (the user
// probably meant `update model`); update always proceeds. This
// mirrors the binary-side install/update split on
// `nightme stt install` / `nightme stt update`.
//
// `install model` is also the recovery path when the model is
// missing or corrupt — sherpa.go surfaces a typed error when
// model.onnx or tokens.txt is missing, and the manager
// surfaces it to the user as "run `nightme-stt install model`".

// installSharedOpts carries the cobra flag values used by
// both `install model` and `update model`. Bundled so the two
// commands parse into the same struct without copy-paste.
type installSharedOpts struct {
	url    string
	sha256 string
	force  bool
	quiet  bool
}

func (o *installSharedOpts) installOpts(progress updater.ProgressFunc) install.InstallOpts {
	return install.InstallOpts{
		URL:      o.url,
		SHA256:   o.sha256,
		Force:    o.force,
		Progress: progress,
	}
}

// addInstallFlags wires the install/url/sha256/force/quiet
// flags into cmd. `force` and `quiet` defaults vary per
// command — install defaults to force=false / quiet=false;
// update defaults to force=true / quiet=false. The CLI
// always renders the progress bar unless the user opts out
// with --quiet, mirroring `nightme update`'s surface.
func addInstallFlags(cmd *cobra.Command, o *installSharedOpts, defaultForce bool) {
	cmd.Flags().StringVar(&o.url, "url", "",
		"Model archive URL (default: install.DefaultModelURL pinned to the embedded sherpa-onnx-go version)")
	cmd.Flags().StringVar(&o.sha256, "sha256", "",
		"Optional SHA-256 of the archive (lowercase hex); enables integrity verification")
	cmd.Flags().BoolVar(&o.force, "force", defaultForce,
		"Overwrite an existing model install")
	cmd.Flags().BoolVarP(&o.quiet, "quiet", "q", false,
		"Suppress progress bar (still downloads + extracts)")
}

// resolveProgress returns the ProgressFunc for the given
// install/update invocation. quiet=false picks the same
// ASCII bar `nightme update` uses (single-line, 30 chars,
// percent + bytes/total + speed + ETA, total<=0 means
// indeterminate). quiet=true picks a no-op so the only
// output is the post-download "extracting…" / "installed"
// status lines.
func resolveProgress(out io.Writer, quiet bool) updater.ProgressFunc {
	if quiet {
		return updater.QuietProgress
	}
	return updater.NewASCIIProgressBar(out, 0)
}

// newInstallCmd is the `install` parent. Only `model` lives
// underneath today; the parent exists for symmetry with
// `nightme stt install` and to make room for future model-
// independent install steps without re-shaping the tree.
func newInstallCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install worker dependencies",
		Long:  "Install the worker's external dependencies. The only\nsubcommand today is `model` (SenseVoice weights + tokens).\nRun `nightme-stt install model` after a fresh `nightme stt\ninstall` to fetch the model the worker needs at runtime.",
	}
	cmd.AddCommand(newInstallModelCmd())
	return cmd
}

// newUpdateCmd is the `update` parent. Symmetric to install;
// only `model` lives underneath.
func newUpdateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update worker dependencies",
		Long:  "Update the worker's external dependencies in place. The\nonly subcommand today is `model` (re-fetch SenseVoice weights\n+ tokens). Always overwrites the existing install — use it\nwhen sherpa-onnx upstream ships a new model version.",
	}
	cmd.AddCommand(newUpdateModelCmd())
	return cmd
}

// newInstallModelCmd installs the model. Refuses if already
// present unless --force is set, matching the convention
// `apt install` follows: "if it already exists you probably
// meant update."
func newInstallModelCmd() *cobra.Command {
	var o installSharedOpts
	cmd := &cobra.Command{
		Use:   "model",
		Short: "Install the SenseVoice model + tokens",
		Long: "Download the SenseVoice multilingual model from\n" +
			"sherpa-onnx's GitHub release feed, verify SHA-256 if\n" +
			"--sha256 is supplied, and extract model.onnx + tokens.txt\n" +
			"under <dataDir>/stt/model/. Refuses if the model is\n" +
			"already installed; pass --force or run `update model`.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInstallModel(cmd, &o)
		},
	}
	addInstallFlags(cmd, &o, false)
	return cmd
}

// newUpdateModelCmd always overwrites. The flag default for
// `--force` is true here so the user can confirm "yes, I
// really mean replace" without re-typing it; they can pass
// --force=false if they want to gate on no-existing (rare).
func newUpdateModelCmd() *cobra.Command {
	var o installSharedOpts
	cmd := &cobra.Command{
		Use:   "model",
		Short: "Re-download the SenseVoice model + tokens",
		Long: "Re-fetch the SenseVoice model and overwrite\n" +
			"<dataDir>/stt/model/. Use this after bumping the\n" +
			"embedded sherpa-onnx-go version, or to repair a corrupt\n" +
			"install. Defaults to --force=true.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInstallModel(cmd, &o)
		},
	}
	addInstallFlags(cmd, &o, true)
	return cmd
}

// runInstallModel is the shared body of install model +
// update model. Delegates to install.InstallModel which owns
// the download + extract + verify pipeline; this wrapper
// just sets up the installOpts and routes the installer's
// progress output through cobra's stdout/stderr streams.
func runInstallModel(cmd *cobra.Command, o *installSharedOpts) error {
	dataDir := resolveDataDir()
	progress := resolveProgress(cmd.OutOrStdout(), o.quiet)
	opts := o.installOpts(progress)
	opts.Out = cmd.OutOrStdout()
	opts.Err = cmd.ErrOrStderr()
	return install.InstallModel(cmd.Context(), dataDir, opts)
}

// ensure io is used; the variable assignment above uses io.Writer
// for the Out/Err fields, and we want a compile-time check that
// the imports stay correct if the helper is later restructured.
var _ io.Writer = (*os.File)(nil)
