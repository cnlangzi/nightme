//go:build !windows

// Unix-specific parts of the daemon lifecycle: startDaemon
// (fork + fd inheritance + lock handoff + ready handshake via
// inherited pipe) and runDaemonChild (the daemon process
// itself, which takes over the inherited lock and writes a
// bootstrap message back to the parent).
//
// The cross-platform command tree (newStartCmd / newStopCmd /
// etc.) and helpers (acquireLifecycleLock / stopDaemon / etc.)
// live in daemon_lifecycle.go (no build tag).
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
	"time"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/daemoncontrol"
	nmerrors "github.com/cnlangzi/nightme/internal/errors"
	"github.com/cnlangzi/nightme/internal/version"
)

const (
	daemonLockFDEnv = "NIGHTME_DAEMON_LOCK_FD"
	readyFDEnv      = "NIGHTME_READY_FD"
)

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
	// Panics write to stderr; devNull throws the stack trace away and
	// a daemon crash then looks like "the log just stops". Opt in to
	// capturing it with NIGHTME_STDERR_FILE=<path> when diagnosing.
	child.Stderr = devNull
	if p := os.Getenv("NIGHTME_STDERR_FILE"); p != "" {
		if f, ferr := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); ferr == nil {
			defer f.Close()
			child.Stderr = f
		}
	}
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

func runDaemonChild(cmd *cobra.Command, channelName string) (retErr error) {
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

	deps := withChannel(defaultRunDeps(), channelName)
	deps.onReady = func() {
		server.SetReady()
		writeBootstrap(bootstrapMessage{Ready: true})
	}
	// F-40: the lifecycle layer holds the daemoncontrol server
	// reference. The runDeps.registerHealth callback bridges the
	// post-`newChannel` adapter handle (built inside runRunWith)
	// back here so we can wire the "health" RPC responder.
	deps.registerHealth = func(_ channel.Channel, fn func() (string, json.RawMessage, error)) {
		server.SetHealthProvider(fn)
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
