package stt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

// FindNightmeSTT locates the installed nightme-stt worker
// binary under <dataDir>/stt/bin/. Returns the absolute path
// on success, or an error wrapping `errNotBuilt` if the
// binary is missing (issue #381 §13 — voice messages will
// surface a user-facing "run `nightme stt install`" prompt).
//
// On Windows the binary carries a .exe suffix; on Unix it
// doesn't. `isWindowsExe` is a build-tag constant set in
// kill_unix.go / kill_windows.go so neither platform
// needs to compile the other.
func FindNightmeSTT(dataDir string) (string, error) {
	if dataDir == "" {
		return "", errors.New("stt: empty data dir")
	}
	binName := "nightme-stt"
	if isWindowsExe {
		binName += ".exe"
	}
	candidate := filepath.Join(dataDir, "stt", "bin", binName)
	if _, err := os.Stat(candidate); err == nil {
		return candidate, nil
	}
	return "", fmt.Errorf("%w: expected at %s", errNotBuilt, candidate)
}

// errNotBuilt is the sentinel returned by FindNightmeSTT
// when the binary is missing. Callers (the Telegram
// adapter's voice handler) detect this via errors.Is and
// render a user-facing "nightme stt install" prompt.
var errNotBuilt = errors.New("stt: nightme-stt runtime not installed")

// IsNotBuilt reports whether err is the "binary missing"
// sentinel. Exported so the Telegram voice handler can
// branch on it without importing the unexported var.
func IsNotBuilt(err error) bool {
	return errors.Is(err, errNotBuilt)
}

// ProductionSpawner exec's nightme-stt as a child process
// and returns a *execHandle that Stop signals. Issue #381 §2:
// nightme-stt is a normal child process — no daemon mode,
// no system service. NightMe owns its lifecycle.
//
// No flags are passed to the child: the worker resolves its
// own dataDir from NIGHTME_PATHS_DATA_DIR (falling back to
// ~/.nightme) and derives the endpoint via stt.DefaultEndpoint.
// nightme core and the worker share internal/version and
// internal/stt.DefaultEndpoint, so they agree on the socket
// path without a flag handshake. The Spawner signature still
// receives endpoint because test spawners (loopback) and the
// Manager's dial() path use it — production just doesn't need
// to forward it.
//
// We deliberately use `exec.Command`, NOT
// `exec.CommandContext`. The Manager's `EnsureReady` ctx
// is cancelled the moment `EnsureReady` returns — if we passed
// it here, the exec watcher would fire Kill on the already-
// cancelled ctx when `execHandle.Stop` calls `Wait`, defeating
// the SIGTERM + shutdownGrace window the Stop code intends.
// Stop owns the kill lifecycle; the spawner just starts the
// process.
func ProductionSpawner(dataDir string) Spawner {
	return func(ctx context.Context, endpoint Endpoint) (Handle, error) {
		bin, err := FindNightmeSTT(dataDir)
		if err != nil {
			return nil, err
		}
		_ = ctx
		_ = endpoint // see comment above; kept in signature for test spawners + Manager dial()
		cmd := exec.Command(bin)
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("stt: start worker: %w", err)
		}
		return &execHandle{cmd: cmd, pid: cmd.Process.Pid}, nil
	}
}

// execHandle is the production Handle backed by an os/exec
// process. Stop sends SIGTERM, waits up to ctx for the worker
// to exit, then SIGKILL as a fallback.
type execHandle struct {
	cmd   *exec.Cmd
	pid   int
	once  sync.Once
	stopE error
}

func (e *execHandle) PID() int { return e.pid }

// Stop sends SIGTERM, waits up to ctx for the worker to
// exit, then SIGKILL as a fallback. Idempotent.
func (e *execHandle) Stop(ctx context.Context) error {
	e.once.Do(func() {
		if e.cmd == nil || e.cmd.Process == nil {
			return
		}
		_ = e.cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() {
			_ = e.cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
			return
		case <-ctx.Done():
		}
		_ = e.cmd.Process.Kill()
		<-done
	})
	return e.stopE
}
