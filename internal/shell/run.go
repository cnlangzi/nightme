// Package shell: cross-platform synchronous runner for ad-hoc
// callers (gtw hooks, debug commands, scripts).
//
// Run is the package's second public entry point (the first being
// Dispatcher.Handle, which is async + streaming reply card).
// Run is sync: it executes the platform shell, captures
// stdout/stderr, and returns. No Sender involvement, no
// goroutine, no Reply card.
//
// Run is implemented on top of the streaming executeShell core
// by passing onChunk=nil — coalesceLines writes every line to
// sink regardless, and without an onChunk callback the
// coalescer never flushes mid-stream. From the caller's
// perspective the API is identical to the pre-streaming
// buffer-based implementation; internally the bytes flow
// through io.Pipe + drainer goroutines on platforms that need
// them.
//
// Platform dispatch lives in dispatch_unix.go / dispatch_windows.go
// (same files as executeShell). This file is platform-agnostic.
package shell

import (
	"context"
	"strconv"
	"strings"
)

// ExitError is returned by Run when the child shell exits non-zero.
// Callers can use errors.As(err, &se) to extract it; Code carries
// the exit status (Stderr holds the child's captured stderr so the
// caller can surface it without reading the second return value).
type ExitError struct {
	Code   int
	Stderr string
}

func (e *ExitError) Error() string {
	return "shell: exit code " + strconv.Itoa(e.Code)
}

// Run executes cmd in cwd using the platform shell (sh -c on
// Unix, cmd /c on Windows) and returns captured stdout/stderr
// plus exit diagnostics.
//
// extraEnv is appended on top of the parent process's
// environment in "KEY=VALUE" form (the same shape as
// exec.Cmd.Env). Empty / nil is fine — only os.Environ() is
// then inherited. The parent process is never mutated (no
// global os.Setenv); the child gets a one-shot copy via exec.
// Duplicate keys in extraEnv intentionally override the
// parent's value.
//
// Trailing newlines are stripped to match the legacy gtw
// runCmd contract.
//
// On non-zero exit, returns a non-nil *ExitError (use errors.As
// to extract). On success (exit 0) err is nil.
//
// Stderr is the child stderr (best-effort UTF-8 / Windows code-
// page-decoded by the platform dispatcher). The same string is
// also embedded in *ExitError.Stderr for caller convenience.
func Run(ctx context.Context, cwd, cmd string, extraEnv []string) (stdout, stderr string, exitCode int, err error) {
	// onChunk=nil: coalesceLines writes every line to its sink
	// buffer (stdoutBuf / stderrBuf in executeShell) without
	// emitting any chunks. Run reads the populated r.Stdout /
	// r.Stderr after executeShell returns. Behavior is
	// observationally identical to the pre-streaming buffer-
	// based implementation.
	r := executeShell(ctx, cwd, cmd, extraEnv, nil)
	out := strings.TrimRight(r.Stdout, "\n")
	eerr := strings.TrimRight(r.Stderr, "\n")
	if r.ExitCode != 0 {
		return out, eerr, r.ExitCode, &ExitError{Code: r.ExitCode, Stderr: eerr}
	}
	return out, eerr, 0, nil
}
