//go:build unix

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/daemoncontrol"
	nmerrors "github.com/cnlangzi/nightme/internal/errors"
	"github.com/cnlangzi/nightme/internal/version"
)

const (
	daemonLockFDEnv = "NIGHTME_DAEMON_LOCK_FD"
	readyFDEnv      = "NIGHTME_READY_FD"
	lifecycleWait   = 15 * time.Second
	startupWait     = 15 * time.Second
)

type daemonOptions struct {
	channel string
	cleanup bool
}

type bootstrapMessage struct {
	Ready bool   `json:"ready"`
	Error string `json:"error,omitempty"`
}

func newStartCmd() *cobra.Command {
	opts := daemonOptions{channel: "feishu"}
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the nightme daemon in the background",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStart(cmd, opts)
		},
	}
	addDaemonOptionFlags(cmd, &opts)
	return cmd
}

func newStatusCmd() *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show daemon status",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStatus(cmd, jsonOutput)
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Print status as JSON")
	return cmd
}

func newStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Gracefully stop the nightme daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStop(cmd)
		},
	}
}

func newRestartCmd() *cobra.Command {
	opts := daemonOptions{channel: "feishu"}
	cmd := &cobra.Command{
		Use:   "restart",
		Short: "Gracefully replace the nightme daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRestart(cmd, opts)
		},
	}
	addDaemonOptionFlags(cmd, &opts)
	return cmd
}

func addDaemonOptionFlags(cmd *cobra.Command, opts *daemonOptions) {
	cmd.Flags().StringVar(&opts.channel, "channel", "feishu", "Channel implementation: feishu or echo")
	cmd.Flags().BoolVar(&opts.cleanup, "cleanup", false, "Kill session CLIs on shutdown instead of detaching them")
}

func loadLifecyclePaths() (*config.Config, daemoncontrol.Paths, error) {
	cfg, err := config.LoadDefault()
	if err != nil {
		return nil, daemoncontrol.Paths{}, fmt.Errorf("load config: %w", err)
	}
	paths, err := daemoncontrol.ResolvePaths(cfg.Paths.DataDir)
	if err != nil {
		return nil, daemoncontrol.Paths{}, err
	}
	return cfg, paths, nil
}

func acquireLifecycleLock(ctx context.Context, path string) (*daemoncontrol.Lock, error) {
	deadline := time.Now().Add(lifecycleWait)
	for {
		lock, err := daemoncontrol.TryLock(path)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, daemoncontrol.ErrLocked) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, nmerrors.New(nmerrors.CodeBridgeError, "another daemon lifecycle operation is still running")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func runStart(cmd *cobra.Command, opts daemonOptions) error {
	if err := validateDaemonOptions(opts); err != nil {
		return err
	}
	cfg, paths, err := loadLifecyclePaths()
	if err != nil {
		return err
	}
	transition, err := acquireLifecycleLock(cmd.Context(), paths.LifecycleLock)
	if err != nil {
		return err
	}
	defer transition.Close()
	return startDaemon(cmd.Context(), cmd.OutOrStdout(), cfg, paths, opts)
}

func startDaemon(ctx context.Context, out io.Writer, cfg *config.Config, paths daemoncontrol.Paths, opts daemonOptions) error {
	lock, err := daemoncontrol.TryLock(paths.DaemonLock)
	if err != nil {
		if errors.Is(err, daemoncontrol.ErrLocked) {
			return nmerrors.New(nmerrors.CodeBridgeError, "nightme daemon is already running")
		}
		return err
	}
	defer lock.Close()

	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve nightme executable: %w", err)
	}
	readyR, readyW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create readiness pipe: %w", err)
	}
	defer readyR.Close()

	child := exec.Command(executable, "_daemon", "--channel", opts.channel)
	if opts.cleanup {
		child.Args = append(child.Args, "--cleanup")
	}
	child.Env = append(os.Environ(), daemonLockFDEnv+"=3", readyFDEnv+"=4")
	child.ExtraFiles = []*os.File{lock.File(), readyW}
	child.Stdin = nil
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		_ = readyW.Close()
		return fmt.Errorf("open null output: %w", err)
	}
	defer devNull.Close()
	child.Stdout = devNull
	child.Stderr = devNull
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := child.Start(); err != nil {
		_ = readyW.Close()
		return fmt.Errorf("start daemon process: %w", err)
	}
	_ = readyW.Close()
	// Closing only the launcher's descriptor preserves the inherited flock.
	if err := lock.CloseLocalCopy(); err != nil {
		_ = child.Process.Signal(syscall.SIGTERM)
		_ = child.Wait()
		return fmt.Errorf("handoff daemon lock: %w", err)
	}

	msgCh := make(chan bootstrapMessage, 1)
	errCh := make(chan error, 1)
	go func() {
		var msg bootstrapMessage
		err := json.NewDecoder(bufio.NewReader(readyR)).Decode(&msg)
		if err != nil {
			errCh <- err
			return
		}
		msgCh <- msg
	}()

	timer := time.NewTimer(startupWait)
	defer timer.Stop()
	select {
	case msg := <-msgCh:
		if !msg.Ready {
			_ = child.Wait()
			if msg.Error == "" {
				msg.Error = "daemon startup failed"
			}
			return nmerrors.New(nmerrors.CodeBridgeError, msg.Error)
		}
	case err := <-errCh:
		_ = child.Wait()
		return fmt.Errorf("read daemon readiness: %w", err)
	case <-timer.C:
		_ = child.Process.Signal(syscall.SIGTERM)
		_ = child.Wait()
		return nmerrors.New(nmerrors.CodeBridgeError, "daemon did not become ready within 15s")
	case <-ctx.Done():
		_ = child.Process.Signal(syscall.SIGTERM)
		_ = child.Wait()
		return ctx.Err()
	}

	ready, err := daemoncontrol.Ping(paths.Socket, 2*time.Second)
	if err != nil || !ready {
		_ = child.Process.Signal(syscall.SIGTERM)
		_ = child.Wait()
		if err != nil {
			return fmt.Errorf("verify daemon readiness: %w", err)
		}
		return errors.New("verify daemon readiness: daemon is not ready")
	}
	pid := child.Process.Pid
	if err := child.Process.Release(); err != nil {
		return fmt.Errorf("release daemon process handle: %w", err)
	}
	fmt.Fprintf(out, "nightme daemon started (pid=%d, channel=%s)\n", pid, opts.channel)
	if cfg.Logging.File != "" {
		fmt.Fprintf(out, "log: %s\n", cfg.Logging.File)
	}
	return nil
}

func runStatus(cmd *cobra.Command, jsonOutput bool) error {
	_, paths, err := loadLifecyclePaths()
	if err != nil {
		return err
	}
	status, err := daemoncontrol.GetStatus(paths.Socket, 2*time.Second)
	if err != nil {
		if errors.Is(err, daemoncontrol.ErrNotRunning) {
			return nmerrors.New(nmerrors.CodeNotFound, "nightme daemon is not running")
		}
		return err
	}
	if jsonOutput {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(status)
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "PID\tSTATE\tUPTIME\tCHANNEL\tCLEANUP\tVERSION")
	fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%t\t%s\n", status.PID, status.State,
		(time.Duration(status.UptimeSeconds) * time.Second).Round(time.Second), status.Channel, status.Cleanup, status.Version)
	return w.Flush()
}

func runStop(cmd *cobra.Command) error {
	_, paths, err := loadLifecyclePaths()
	if err != nil {
		return err
	}
	transition, err := acquireLifecycleLock(cmd.Context(), paths.LifecycleLock)
	if err != nil {
		return err
	}
	defer transition.Close()
	return stopDaemon(cmd.Context(), cmd.OutOrStdout(), paths)
}

func stopDaemon(ctx context.Context, out io.Writer, paths daemoncontrol.Paths) error {
	if err := daemoncontrol.Stop(paths.Socket, 2*time.Second); err != nil && !errors.Is(err, daemoncontrol.ErrNotRunning) {
		return err
	}
	deadline := time.Now().Add(lifecycleWait)
	for {
		lock, err := daemoncontrol.TryLock(paths.DaemonLock)
		if err == nil {
			_ = lock.Close()
			fmt.Fprintln(out, "nightme daemon stopped")
			return nil
		}
		if !errors.Is(err, daemoncontrol.ErrLocked) {
			return err
		}
		if time.Now().After(deadline) {
			return nmerrors.New(nmerrors.CodeBridgeError, "daemon did not stop within 15s")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func runRestart(cmd *cobra.Command, opts daemonOptions) error {
	cfg, paths, err := loadLifecyclePaths()
	if err != nil {
		return err
	}
	transition, err := acquireLifecycleLock(cmd.Context(), paths.LifecycleLock)
	if err != nil {
		return err
	}
	defer transition.Close()

	if current, err := daemoncontrol.GetStatus(paths.Socket, time.Second); err == nil {
		if !cmd.Flags().Changed("channel") {
			opts.channel = current.Channel
		}
		if !cmd.Flags().Changed("cleanup") {
			opts.cleanup = current.Cleanup
		}
	}
	if err := validateDaemonOptions(opts); err != nil {
		return err
	}
	if err := stopDaemon(cmd.Context(), io.Discard, paths); err != nil {
		return err
	}
	return startDaemon(cmd.Context(), cmd.OutOrStdout(), cfg, paths, opts)
}

func validateDaemonOptions(opts daemonOptions) error {
	if opts.channel != "feishu" && opts.channel != "echo" {
		return nmerrors.New(nmerrors.CodeValidationError,
			fmt.Sprintf("unknown channel %q (want feishu or echo)", opts.channel))
	}
	return nil
}

func newDaemonCmd() *cobra.Command {
	var cleanup bool
	var channelName string
	cmd := &cobra.Command{
		Use:    "_daemon",
		Short:  "Internal daemon process",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDaemonChild(cmd, cleanup, channelName)
		},
	}
	cmd.Flags().BoolVar(&cleanup, "cleanup", false, "Kill session CLIs on shutdown")
	cmd.Flags().StringVar(&channelName, "channel", "feishu", "Channel implementation")
	return cmd
}

func runDaemonChild(cmd *cobra.Command, cleanup bool, channelName string) (retErr error) {
	lockFD, err := strconv.Atoi(os.Getenv(daemonLockFDEnv))
	if err != nil || lockFD < 3 {
		return errors.New("_daemon must be launched by `nightme start` or `nightme restart`")
	}
	readyFD, err := strconv.Atoi(os.Getenv(readyFDEnv))
	if err != nil || readyFD < 3 {
		return errors.New("_daemon readiness pipe is missing")
	}
	lock, err := daemoncontrol.LockFromFile(os.NewFile(uintptr(lockFD), "daemon.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	readyFile := os.NewFile(uintptr(readyFD), "daemon-ready")
	if readyFile == nil {
		return errors.New("open daemon readiness pipe")
	}
	defer readyFile.Close()
	writeBootstrap := func(msg bootstrapMessage) {
		_ = json.NewEncoder(readyFile).Encode(msg)
		_ = readyFile.Close()
	}

	cfg, err := config.LoadDefault()
	if err != nil {
		writeBootstrap(bootstrapMessage{Error: err.Error()})
		return err
	}
	paths, err := daemoncontrol.ResolvePaths(cfg.Paths.DataDir)
	if err != nil {
		writeBootstrap(bootstrapMessage{Error: err.Error()})
		return err
	}
	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()
	status := daemoncontrol.Status{
		PID:       os.Getpid(),
		StartedAt: time.Now().UTC(),
		Channel:   channelName,
		Cleanup:   cleanup,
		Version:   version.String(),
		LogPath:   cfg.Logging.File,
	}
	server, err := daemoncontrol.Listen(paths.Socket, status, cancel)
	if err != nil {
		writeBootstrap(bootstrapMessage{Error: err.Error()})
		return err
	}
	defer server.Close()
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve() }()

	deps := withCleanup(withChannel(defaultRunDeps(), channelName), cleanup)
	deps.onReady = func() {
		server.SetReady()
		writeBootstrap(bootstrapMessage{Ready: true})
	}
	cmd.SetContext(withLogger(ctx, loggerFromContext(cmd.Context())))
	err = runRunWith(cmd, deps)
	if err != nil && server.Status().State == "starting" {
		writeBootstrap(bootstrapMessage{Error: err.Error()})
	}
	_ = server.Close()
	select {
	case serverErr := <-serveErr:
		if err == nil && serverErr != nil {
			err = serverErr
		}
	case <-time.After(time.Second):
	}
	return err
}
