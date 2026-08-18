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
	"github.com/cnlangzi/nightme/internal/proc"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/daemoncontrol"
	nmerrors "github.com/cnlangzi/nightme/internal/errors"
	"github.com/cnlangzi/nightme/internal/runtime"
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

	child := proc.New(ctx, executable, daemonChildCommand, "--channel", opts.channel)
	// proc.New on Windows already set CreationFlags |= CREATE_NO_WINDOW
	// (so the daemon child does not pop a visible console). Merge
	// rather than replace: DETACHED_PROCESS (no console inheritance)
	// and CREATE_NEW_PROCESS_GROUP (own process group — Ctrl-C from
	// any other console won't reach the daemon). Without the merge,
	// the daemon pops a flashing console window on every `nightme
	// start` from a console.
	if child.SysProcAttr == nil {
		child.SysProcAttr = &windows.SysProcAttr{}
	}
	child.SysProcAttr.CreationFlags |= windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP
	child.Stdin = nil
	child.Stdout = nil
	// Panics (in ANY goroutine) and runtime fatals write to stderr
	// and nowhere else; a nil Stderr discards them and a daemon
	// crash then looks like "the log just stops". Capture to
	// <DataDir>\daemon-stderr.log (NIGHTME_STDERR_FILE overrides) —
	// same helper and same policy as daemon_lifecycle_unix.go. A
	// capture failure degrades to "discarded" rather than failing
	// the start: nil Stderr is Windows' /dev/null equivalent here.
	stderrSink, stderrPath, closeStderr := openDaemonStderrOrDevNull(cfg, nil, os.Stderr)
	if closeStderr != nil {
		defer closeStderr()
	}
	child.Stderr = stderrSink

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
				// Same rationale as daemon_lifecycle_unix.go: the
				// child was alive but slow, so the capture file may
				// be empty — but it's still the right place to look
				// for whatever the daemon printed.
				//
				// stderrPath is "" when openDaemonStderrOrDevNull
				// failed to open the capture file; in that case the
				// diagnostic aid is gone, so don't promise a file the
				// operator cannot open.
				if stderrPath != "" {
					return nmerrors.New(nmerrors.CodeBridgeError, fmt.Sprintf(
						"daemon did not become ready within 15s; child stderr: %s", stderrPath))
				}
				return nmerrors.New(nmerrors.CodeBridgeError,
					"daemon did not become ready within 15s (stderr capture was disabled at start; see warning above)")
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
						fmt.Sprintf("daemon child exited before becoming ready (code=%d); crash output: %s",
							exitCodeOf(state), stderrPath))
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

// runDaemonChild is the Windows daemon process.
//
// Unlike the Unix version it installs no panic recover: there is no
// readiness pipe here (the parent polls Ping), so a panic already
// surfaces to the parent as "daemon child exited before becoming
// ready (code=...)" and the stack lands in the stderr capture file.
// The Unix recover exists only to turn an otherwise information-free
// EOF on that pipe into a real message.
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

	deps := runtime.DefaultDeps()
	deps, _ = runtime.WithChannel(deps, channelName)
	deps.OnReady = func() {
		server.SetReady()
	}
	deps.RegisterHealth = func(fn func() (string, json.RawMessage, error)) {
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
