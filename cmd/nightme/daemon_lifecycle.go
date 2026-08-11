// Package main — cross-platform daemon lifecycle commands.
//
// This file holds the bits that are identical on Unix and
// Windows: the cobra command tree (start / stop / restart /
// status / _daemon), the path loader, the lifecycle lock
// acquisition, and the user-facing run* wrappers.
//
// Platform-specific bits (startDaemon, runDaemonChild) live in
// daemon_lifecycle_unix.go and daemon_lifecycle_windows.go.
//
// On Unix the parent forks a child and inherits the daemon
// lock via ExtraFiles + an env-var handshake. On Windows the
// child takes its own lock and the parent polls Ping for
// readiness. The external behavior — `nightme start` returns
// once the daemon is up, `nightme status` shows it, `nightme
// stop` shuts it down — is identical.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/daemoncontrol"
	nmerrors "github.com/cnlangzi/nightme/internal/errors"
)

const (
	lifecycleWait = 15 * time.Second
	startupWait   = 15 * time.Second
)

type daemonOptions struct {
	channel string
}

// bootstrapMessage is the unix-only ready-handshake payload that
// the daemon writes to its inherited ready pipe. On Windows the
// handshake is implicit (parent polls Ping until the server is
// ready), so this struct is unused on Windows but harmless.
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
		Short: "Show whether the nightme daemon is running",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStatus(cmd, jsonOutput)
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON instead of a text table")
	return cmd
}

func newStopCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop a running nightme daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStop(cmd)
		},
	}
	return cmd
}

func newRestartCmd() *cobra.Command {
	opts := daemonOptions{channel: "feishu"}
	cmd := &cobra.Command{
		Use:   "restart",
		Short: "Stop the running daemon (if any) and start a fresh one",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRestart(cmd, opts)
		},
	}
	addDaemonOptionFlags(cmd, &opts)
	return cmd
}

func addDaemonOptionFlags(cmd *cobra.Command, opts *daemonOptions) {
	cmd.Flags().StringVar(&opts.channel, "channel", opts.channel,
		"Channel implementation: feishu (default) or echo (smoke test)")
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
	fmt.Fprintln(w, "PID\tSTATE\tUPTIME\tCHANNEL\tVERSION")
	fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", status.PID, status.State,
		(time.Duration(status.UptimeSeconds) * time.Second).Round(time.Second), status.Channel, status.Version)
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

// stopDaemon is cross-platform: both Unix (SIGTERM via socket
// RPC) and Windows (Stop RPC + TerminateProcess fallback via the
// pipe) implement daemoncontrol.Stop, and the lock polling
// detects daemon exit the same way on both.
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
	var channelName string
	cmd := &cobra.Command{
		Use:    "_daemon",
		Short:  "Internal daemon process",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDaemonChild(cmd, channelName)
		},
	}
	cmd.Flags().StringVar(&channelName, "channel", "feishu", "Channel implementation")
	return cmd
}
