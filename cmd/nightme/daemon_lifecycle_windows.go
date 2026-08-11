//go:build windows

// Windows-specific parts of the daemon lifecycle: startDaemon
// (CreateProcess + DETACHED_PROCESS + CREATE_NEW_PROCESS_GROUP
// + ready handshake via Ping polling) and runDaemonChild (the
// daemon process itself, which takes its own daemoncontrol
// TryLock because Windows LockFileEx doesn't support fd
// inheritance the way Unix flock does).
//
// The cross-platform command tree (newStartCmd / newStopCmd /
// etc.) and helpers (acquireLifecycleLock / stopDaemon / etc.)
// live in daemon_lifecycle.go (no build tag).
//
// Differences from the Unix flow:
//
//   - No parent TryLock. The child acquires its own daemon lock;
//     if another daemon is already up, the child exits with an
//     error and parent.startDaemon reports a timeout (less
//     informative than the Unix "already running" message but
//     correct).
//   - No fd inheritance. Go's os/exec on Windows doesn't expose
//     STARTUPINFO.bInheritHandles + handle table, so the child
//     cannot receive the parent's lock fd or a ready pipe. The
//     lock is OS-level (LockFileEx) so re-acquiring it works;
//     readiness is detected by polling daemoncontrol.Ping until
//     it returns Ready=true.
//   - No "setsid". Windows doesn't have session leaders; the
//     closest equivalent is DETACHED_PROCESS (no console) +
//     CREATE_NEW_PROCESS_GROUP (no Ctrl-C broadcast), which is
//     what we set below.
//   - No "Job Object" for v1. The Unix flow doesn't enforce
//     parent-death cleanup either (no PR_SET_PDEATHSIG); the
//     agentsession.reapOrphan path already cleans up leaked
//     agent processes on next nightme run / start.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"time"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/daemoncontrol"
	nmerrors "github.com/cnlangzi/nightme/internal/errors"
	"github.com/cnlangzi/nightme/internal/version"
	"golang.org/x/sys/windows"
)

// startReadyPollInterval is how often the parent pings the
// daemon to detect readiness. 200ms is a good balance — fast
// enough that `nightme start` feels responsive, slow enough
// that we don't hammer the named pipe before the child has a
// chance to bind it.
const startReadyPollInterval = 200 * time.Millisecond

// startReadyPingTimeout is the per-call timeout on the Ping
// inside each poll tick. It must be shorter than the poll
// interval so a single tick = one Ping attempt + wait for the
// next tick, not one Ping attempt that overruns the tick and
// queues the next one behind it.
const startReadyPingTimeout = 150 * time.Millisecond

func startDaemon(ctx context.Context, out io.Writer, cfg *config.Config, paths daemoncontrol.Paths, opts daemonOptions) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve nightme executable: %w", err)
	}

	child := exec.Command(executable, "_daemon", "--channel", opts.channel)
	child.SysProcAttr = &windows.SysProcAttr{
		// DETACHED_PROCESS: no console inheritance. Required
		// because `nightme start` was launched from a console;
		// without this the daemon would tie up the user's
		// terminal until it exits.
		//
		// CREATE_NEW_PROCESS_GROUP: the daemon becomes the
		// root of a new process group. Ctrl-C / Ctrl-Break
		// from any other console won't reach it.
		//
		// CREATE_PRESERVE_CODE_AUTHZ_LEVEL / no flags are
		// needed for our use.
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
	child.Stdin = nil
	child.Stdout = nil
	child.Stderr = nil
	// Panics write to stderr; /dev/null throws the stack trace
	// away and a daemon crash then looks like "the log just
	// stops". Opt in to capturing it with
	// NIGHTME_STDERR_FILE=<path> when diagnosing — same
	// convention as daemon_lifecycle_unix.go.
	if p := os.Getenv("NIGHTME_STDERR_FILE"); p != "" {
		if f, ferr := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); ferr == nil {
			defer f.Close()
			child.Stderr = f
		}
	}

	if err := child.Start(); err != nil {
		return fmt.Errorf("start daemon process: %w", err)
	}

	// Poll for ready: daemoncontrol.Listen (called by the child)
	// creates the named pipe as its first action; Ping then
	// succeeds once the server has called SetReady(). The
	// handshake is implicit — no extra pipe plumbing.
	deadline := time.Now().Add(startupWait)
	ticker := time.NewTicker(startReadyPollInterval)
	defer ticker.Stop()

	pid := child.Process.Pid
	for {
		select {
		case <-ctx.Done():
			_ = child.Process.Kill()
			_ = child.Wait()
			return ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				_ = child.Process.Kill()
				_ = child.Wait()
				return nmerrors.New(nmerrors.CodeBridgeError, "daemon did not become ready within 15s")
			}
			ok, err := daemoncontrol.Ping(paths.Socket, startReadyPingTimeout)
			if err == nil && ok {
				// Release the parent-side handle so the kernel does
				// not block daemon shutdown on the parent process's
				// own termination. If Release fails the daemon is
				// still running (Ping just succeeded) — log a
				// warning rather than fail `nightme start`, which
				// would mislead the user into thinking no daemon
				// is up and lead to a follow-up start that races
				// with the running child.
				if err := child.Process.Release(); err != nil {
					slog.Default().Warn("release daemon process handle",
						"pid", pid, "err", err)
				}
				fmt.Fprintf(out, "nightme daemon started (pid=%d, channel=%s)\n", pid, opts.channel)
				if cfg.Logging.File != "" {
					fmt.Fprintf(out, "log: %s\n", cfg.Logging.File)
				}
				return nil
			}
			// Ping can fail with ErrNotRunning in two cases:
			//   1. Pipe doesn't exist yet (child hasn't called Listen)
			//   2. Child exited (e.g. lock contention) — Wait will succeed
			//
			// Detect case 2 so we surface a clearer error.
			if errors.Is(err, daemoncontrol.ErrNotRunning) {
				if state, werr := child.Process.Wait(); werr == nil && state != nil {
					return nmerrors.New(nmerrors.CodeBridgeError,
						fmt.Sprintf("daemon child exited before becoming ready (code=%d)", exitCodeOf(state)))
				}
			}
		}
	}
}

// exitCodeOf extracts a numeric exit code from os.ProcessState
// without forcing the caller to import os/exec. Windows process
// state carries exit codes the same way Unix does; on this OS
// the field is always populated (no signal-based termination).
func exitCodeOf(state *os.ProcessState) int {
	if state == nil {
		return -1
	}
	return state.ExitCode()
}

func runDaemonChild(cmd *cobra.Command, channelName string) (retErr error) {
	// Take the daemon lock ourselves. Unlike Unix, we can't
	// inherit the parent's fd — LockFileEx is per-handle, so
	// the parent closing its copy would release the lock. The
	// child's TryLock is the only owner.
	cfg, err := config.LoadDefault()
	if err != nil {
		return err
	}
	paths, err := daemoncontrol.ResolvePaths(cfg.Paths.DataDir)
	if err != nil {
		return err
	}
	lock, err := daemoncontrol.TryLock(paths.DaemonLock)
	if err != nil {
		if errors.Is(err, daemoncontrol.ErrLocked) {
			return errors.New("nightme daemon is already running")
		}
		return fmt.Errorf("acquire daemon lock: %w", err)
	}
	defer lock.Close()

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
		return fmt.Errorf("daemoncontrol.Listen: %w", err)
	}
	defer server.Close()
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve() }()

	deps := withChannel(defaultRunDeps(), channelName)
	deps.onReady = func() {
		server.SetReady()
	}
	deps.registerHealth = func(_ channel.Channel, fn func() (string, json.RawMessage, error)) {
		server.SetHealthProvider(fn)
	}
	cmd.SetContext(withLogger(ctx, loggerFromContext(cmd.Context())))
	err = runRunWith(cmd, deps)
	_ = server.Close()
	// Give Serve a brief window to unwind before we return; on
	// Windows the process-exit path is what actually unblocks
	// any pending ConnectNamedPipe (the kernel returns
	// ERROR_OPERATION_ABORTED once the handle is closed).
	select {
	case serverErr := <-serveErr:
		if err == nil && serverErr != nil {
			err = serverErr
		}
	case <-time.After(time.Second):
	}
	return err
}
