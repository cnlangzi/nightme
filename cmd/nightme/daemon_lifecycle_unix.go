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
	"runtime/debug"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/daemoncontrol"
	nmerrors "github.com/cnlangzi/nightme/internal/errors"
	"github.com/cnlangzi/nightme/internal/runtime"
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

	child := exec.Command(executable, daemonChildCommand, "--channel", opts.channel)
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
	// Panics (in ANY goroutine) and runtime fatals write to stderr
	// and nowhere else. Sending stderr to devNull made a daemon
	// crash look like "the log just stops" — see daemon_stderr.go
	// for the full rationale. The capture file is
	// <DataDir>/daemon-stderr.log unless NIGHTME_STDERR_FILE
	// overrides it; if it cannot be opened we degrade to devNull
	// rather than failing the start.
	stderrSink, stderrPath, closeStderr := openDaemonStderrOrDevNull(cfg, devNull, os.Stderr)
	if closeStderr != nil {
		defer closeStderr()
	}
	child.Stderr = stderrSink
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
		// Wait reaps the child AND populates child.ProcessState,
		// which is the only evidence we have about why it went
		// away. Report it: "exit status 2" means the daemon
		// crashed on its own (look at the stderr capture for the
		// stack), "signal: killed" means something else killed it.
		_ = child.Wait()
		if errors.Is(err, io.EOF) {
			// EOF = every write end of the readiness pipe closed
			// without a byte being written = the child exited
			// before reaching onReady. Reporting the bare EOF
			// (the pre-fix behavior) told the operator nothing
			// about the actual failure.
			return nmerrors.New(nmerrors.CodeBridgeError, fmt.Sprintf(
				"daemon exited before signalling readiness (%s); crash output: %s",
				childExitDetail(child.ProcessState), stderrPath))
		}
		return fmt.Errorf("read daemon readiness: %w", err)
	case <-timer.C:
		_ = child.Process.Signal(syscall.SIGTERM)
		_ = child.Wait()
		// Deliberately NOT called "crash output" here: a timeout
		// means the child was alive and simply too slow, so the
		// file may well be empty. It is still the right place to
		// look for whatever it did print.
		return nmerrors.New(nmerrors.CodeBridgeError, fmt.Sprintf(
			"daemon did not become ready within 15s; child stderr: %s", stderrPath))
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
		return fmt.Errorf("%s must be launched by `nightme start` or `nightme restart`", daemonChildCommand)
	}
	readyFD, err := strconv.Atoi(os.Getenv(readyFDEnv))
	if err != nil || readyFD < 3 {
		return fmt.Errorf("%s readiness pipe is missing", daemonChildCommand)
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

	// A panic on this goroutine used to reach the parent as a bare
	// EOF: the deferred readyFile.Close() fired during unwinding, so
	// the pipe closed with nothing written. Turn it into a real
	// handshake failure (the parent then prints the panic message
	// instead of "read daemon readiness: EOF") and log it, then
	// re-panic so the stack still lands in the stderr capture and
	// the exit status stays non-zero.
	//
	// Scope: this goroutine only. A panic in any OTHER goroutine
	// terminates the process immediately with no unwinding — for
	// those, the stderr capture file is the only evidence, which is
	// exactly why it exists.
	defer func() {
		if r := recover(); r != nil {
			writeBootstrap(bootstrapMessage{Error: fmt.Sprintf("daemon panic: %v", r)})
			loggerFromContext(cmd.Context()).Error("daemon panic",
				"err", r, "stack", string(debug.Stack()))
			panic(r)
		}
	}()

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

	deps := runtime.DefaultDeps()
	deps, _ = runtime.WithChannel(deps, channelName)
	deps.OnReady = func() {
		server.SetReady()
		writeBootstrap(bootstrapMessage{Ready: true})
	}
	// F-40: the lifecycle layer holds the daemoncontrol server
	// reference. The runtime.Deps.RegisterHealth callback bridges the
	// post-`newChannel` adapter handle (built inside Runner.Run)
	// back here so we can wire the "health" RPC responder.
	deps.RegisterHealth = func(_ channel.Channel, fn func() (string, json.RawMessage, error)) {
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
