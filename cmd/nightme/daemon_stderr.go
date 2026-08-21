// Package main — daemon crash-output capture.
//
// `nightme start` / `nightme restart` fork a detached daemon child.
// Historically both of the child's output streams went to
// /dev/null, which is correct for stdout (the daemon logs through
// slog, not stdout) but destroys the one channel the Go runtime
// uses for unrecoverable failures:
//
//   - a panic in ANY goroutine (the runtime prints the stack to
//     stderr and exits — `Recover` in recover.go only wraps the
//     root command's RunE, i.e. one goroutine of one command),
//   - runtime fatals that cannot be recovered at all
//     ("fatal error: concurrent map writes", OOM, deadlock),
//   - the child's own early-exit errors before slog is wired.
//
// When any of those happen the daemon dies without a single line
// in nightme.log, and `start` can only report the symptom it sees:
// `read daemon readiness: EOF`. Observed 5 times in 85 daemon
// launches on the author's machine — rare enough to be hard to
// reproduce on demand, frequent enough that throwing the evidence
// away each time is expensive.
//
// So the child's stderr is captured to a file instead. Policy:
//
//   - Default path: <DataDir>/daemon-stderr.log.
//   - NIGHTME_STDERR_FILE overrides it (pre-existing escape hatch,
//     kept working so existing debugging habits don't break).
//   - One generation is rotated (.1) when the file grows past
//     daemonStderrMaxBytes, so a stack trace is never lost behind
//     megabytes of older noise and the file cannot grow forever.
//   - Any failure to open the capture file is non-fatal: the
//     caller falls back to /dev/null. A diagnostic aid must never
//     be the reason `nightme start` fails.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/pathutil"
)

const (
	// daemonStderrEnv overrides the capture path.
	daemonStderrEnv = "NIGHTME_STDERR_FILE"

	// daemonStderrName is the capture file inside DataDir.
	daemonStderrName = "daemon-stderr.log"

	// daemonStderrMaxBytes is the size at which the existing
	// capture file is rotated to <name>.1 on the next daemon
	// start. 1 MiB holds thousands of stack traces; the daemon
	// writes nothing to stderr in steady state, so this is a
	// safety valve, not a routine event.
	daemonStderrMaxBytes = 1 << 20
)

// daemonStderrPath returns the file the daemon child's stderr is
// captured to. NIGHTME_STDERR_FILE wins when set (and is used
// verbatim, including relative paths — the operator asked for that
// exact file); otherwise the path is <DataDir>/daemon-stderr.log.
func daemonStderrPath(cfg *config.Config) (string, error) {
	if p := strings.TrimSpace(os.Getenv(daemonStderrEnv)); p != "" {
		return p, nil
	}
	dir := "."
	if cfg != nil && cfg.Paths.DataDir != "" {
		dir = cfg.Paths.DataDir
	}
	// F-PATHUTIL-001: cfg.Paths.DataDir is user-supplied YAML
	// and on Windows is commonly written with forward slashes
	// (Git Bash / WSL habits). filepath.Abs converts a relative
	// path to absolute; pathutil.NormalizeForOS then ensures the
	// Windows form uses backslashes — otherwise the daemon-stderr
	// path would land at "F:/nightme\daemon-stderr.log" and
	// Win32 OpenFile rejects the mixed-separator form.
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve data dir %s: %w", dir, err)
	}
	if n, err := pathutil.NormalizeForOS(abs); err == nil {
		abs = n
	}
	return pathutil.Join(abs, daemonStderrName), nil
}

// openDaemonStderr opens the capture file for append, rotating a
// previous oversized generation out of the way first. Returns the
// open file and its path.
//
// The file is 0600: a crash dump can contain workspace paths and
// prompt fragments, so it gets the same permissions as the rest of
// DataDir.
func openDaemonStderr(cfg *config.Config) (*os.File, string, error) {
	path, err := daemonStderrPath(cfg)
	if err != nil {
		return nil, "", err
	}
	if dir := pathutil.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, path, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	rotateDaemonStderr(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, path, fmt.Errorf("open %s: %w", path, err)
	}
	return f, path, nil
}

// rotateDaemonStderr moves an oversized capture file to <path>.1,
// keeping exactly one previous generation. Best-effort: a failure
// to stat or rename just means the file keeps growing, which is
// strictly better than refusing to capture anything.
func rotateDaemonStderr(path string) {
	info, err := os.Stat(path)
	if err != nil || info.Size() < daemonStderrMaxBytes {
		return
	}
	_ = os.Rename(path, path+".1")
}

// openDaemonStderrOrDevNull is the caller-facing helper used by
// startDaemon on both platforms. It never fails: when the capture
// file cannot be opened it falls back to the supplied /dev/null
// handle (which may itself be nil — Windows passes nil because
// Go treats child.Stderr == nil as "discard", same end result) and
// reports the reason through warn (which callers wire to stderr of
// the launching command).
//
// The returned path is EMPTY when capture failed. Callers must not
// interpolate it into diagnostic messages without checking —
// "/dev/null" / "NUL" are valid paths on Unix / Windows respectively
// but are not files the operator can open, so the diagnostic aid
// the error message promises is silently gone. An empty path is
// the contract that says "there is nothing for the operator to read."
//
// The returned closer is nil when the fallback was used, so the
// caller does not close the shared devNull handle twice.
func openDaemonStderrOrDevNull(cfg *config.Config, devNull *os.File, warn io.Writer) (sink *os.File, path string, closer func()) {
	f, path, err := openDaemonStderr(cfg)
	if err != nil {
		if warn != nil {
			fmt.Fprintf(warn, "warning: daemon stderr capture disabled (%v)\n", err)
		}
		return devNull, "", nil
	}
	return f, path, func() { _ = f.Close() }
}

// childExitDetail renders an os.ProcessState for an error message:
// "exit status 2" for a normal exit, "signal: killed" when the
// child was terminated by a signal. The distinction is the whole
// point — it tells an operator whether the daemon crashed on its
// own or something else killed it.
func childExitDetail(state *os.ProcessState) string {
	if state == nil {
		return "exit status unknown"
	}
	return state.String()
}
