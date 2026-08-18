// print.go — print-mode runner. Spawns `dsh --profile headless --
// "<prompt>"` as a child process, captures the final assistant text
// from stdout, and returns it as agent.RunResult.
//
// Compared to codex's runPrintMode (which uses `codex exec --json
// -o <tmpfile>`) and claudecode's runPrintMode (which uses `claude
// -p <prompt>` with stream-json stdout), dsh is the simplest of
// the three: headless writes the final assistant text verbatim to
// stdout, no structured events, no NDJSON, no tmpfile. So the body
// here is correspondingly leaner.
//
// Concurrency model:
//
//   - spawn child with explicit StdoutPipe + StderrPipe
//   - drain stderr in a goroutine (cap 4 KiB tail; surfaced on error)
//   - read stdout to EOF
//   - cmd.Wait for exit
//   - parse exit code → Subtype ("completed" / "failed")
//   - trim stdout → Text
//
// Failure convention (matches codex print-mode): on any error
// (waitErr / non-EOF readErr) the function returns a zero RunResult
// and embeds the last partial stdout + stderr tail in the error
// message. This prevents callers from misreading a partial Text
// value as a successful answer.
//
// On non-zero exit, stderr tail is appended to the returned error so
// the caller (/gtw commit, buildAgentPrompt) can surface model /
// auth / sandbox failures to the user.
package dsh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/proc"
)

// stderrCapBytes bounds the stderr tail we keep on memory and
// include in error messages. dsh's diagnostic output is human-
// readable (a few hundred bytes for "missing API key" type errors,
// a few KB for stack-trace panics); 4 KiB comfortably fits both.
//
// CRITICAL: even after stderrBuf reaches this cap, the drain
// goroutine MUST keep reading the pipe (discarding data). If it
// stops reading, the child blocks on stderr writes once the kernel
// pipe buffer (default 64 KiB on Linux) fills, hanging the entire
// RunOnce call indefinitely. See runPrintMode's stderr goroutine.
const stderrCapBytes = 4 * 1024

// dLog is a thin wrapper around slog.Default so print.go matches
// the package-level log-helper convention used by codex (cLog),
// pi (piLog), and claudecode. Debug level keeps production
// output quiet; tests don't assert on logs.
func dLog(msg string, args ...any) {
	slog.Default().Debug(msg, args...)
}

// runPrintMode is the RunOnce implementation. See package doc for
// the full rationale.
func runPrintMode(ctx context.Context, s *Starter, cfg agent.StartConfig, blocks []agent.ContentBlock) (agent.RunResult, error) {
	if cfg.Workspace == "" {
		return agent.RunResult{}, fmt.Errorf("dsh: workspace is required")
	}

	prompt := agent.BlocksToPrompt(blocks)

	startTime := time.Now()

	// Compose argv from s.args (the canonical source — surfaced
	// verbatim via Info().Args), then append `--` (dsh's flag
	// separator) and the positional prompt. Centralising the
	// argv in Starter avoids the Info()/runPrintMode drift that
	// would silently happen if a future flag is added to one
	// place but not the other.
	args := append([]string{}, s.args...)
	args = append(args, "--", prompt)

	cmd := proc.New(ctx, s.command, args...)
	cmd.Dir = cfg.Workspace

	// Inject the one knob nightme is allowed to override:
	// permissions. dsh reads `DSH_PERMISSION_MODE` from the
	// sandbox-policy plugin (`mode: !!js process.env.DSH_PERMISSION_MODE
	// ?? 'workspace-write'`). The default headless profile does
	// NOT mount sandbox-policy today, so this is a no-op for the
	// user's current setup — but it's the documented contract
	// (see doc.go:17-18, F-dsh-bridge.md §2.1) and will take
	// effect for any future headless composition that includes
	// sandbox-policy (e.g. dsh upgrades or user overlays). Per
	// the agent-no-config-tampering principle, this is the ONLY
	// env var nightme injects — model / provider / credentials
	// still flow from `~/.dsh/settings.yaml` + `.credentials.yaml`.
	cmd.Env = append(os.Environ(), "DSH_PERMISSION_MODE=danger-full-access")

	// Use explicit pipes (NOT cmd.Output()) so we can drain stderr
	// concurrently with stdout. cmd.Output() internally takes the
	// stdout pipe itself, leaving no API for stderr; cmd.Stderr
	// is an io.Writer, not an io.Reader, so cmd.Stderr.Read is
	// a compile error. The StderrPipe / StdoutPipe pair is the
	// correct pattern when both streams need handling.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return agent.RunResult{}, fmt.Errorf("dsh: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		return agent.RunResult{}, fmt.Errorf("dsh: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return agent.RunResult{}, fmt.Errorf("dsh: start: %w", err)
	}

	// Drain stderr concurrently so the child doesn't block on a
	// full stderr pipe. Honors ctx between reads so a cancelled
	// call doesn't wait for exec.CommandContext's SIGKILL to land
	// before this goroutine returns.
	//
	// CRITICAL: even after stderrBuf reaches stderrCapBytes, we
	// keep reading the pipe (discarding bytes). If we stopped, the
	// child would block on stderr writes once the kernel pipe
	// buffer (default 64 KiB) fills — hanging RunOnce indefinitely.
	// The `if n > 0 && stderrBuf.Len() < stderrCapBytes` gate
	// bounds memory but does NOT stop the read.
	stderrBuf := &bytes.Buffer{}
	stderrTruncated := false
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		buf := make([]byte, 4096)
		for {
			if ctx.Err() != nil {
				return
			}
			n, rerr := stderr.Read(buf)
			if n > 0 && stderrBuf.Len() < stderrCapBytes {
				room := stderrCapBytes - stderrBuf.Len()
				if n > room {
					stderrBuf.Write(buf[:room])
					stderrTruncated = true
				} else {
					stderrBuf.Write(buf[:n])
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	// Read stdout to EOF.
	stdoutBytes, readErr := io.ReadAll(stdout)

	waitErr := cmd.Wait()
	<-stderrDone

	elapsedMs := time.Since(startTime).Milliseconds()
	stderrTail := strings.TrimSpace(stderrBuf.String())

	// Subtype is "failed" when ANY error path triggered — either
	// the child exited non-zero (waitErr) or the stdout read failed
	// with something other than a clean EOF (rare but possible:
	// pipe closed mid-read). Clean exit + clean EOF → "completed".
	subtype := "completed"
	if waitErr != nil || (readErr != nil && !errors.Is(readErr, io.EOF)) {
		subtype = "failed"
	}

	// Surface the underlying exit code in the error chain so
	// callers can distinguish signal-killed (negative code, no
	// stderr — likely OOM-killer or SIGTERM) from a real failure
	// (positive code, usually has stderr).
	if exitErr, ok := errors.AsType[*exec.ExitError](waitErr); ok {
		waitErr = fmt.Errorf("dsh: exit %d: %w", exitErr.ExitCode(), waitErr)
	}

	// Soft warning when stderr was truncated. Diagnostic only; the
	// failure paths below already include whatever stderr we kept.
	if stderrTruncated {
		dLog("dsh: stderr truncated",
			"cap_bytes", stderrCapBytes,
			"elapsed_ms", elapsedMs)
	}

	// Failure paths: zero RunResult, embed last answer + stderr
	// tail in the error message. Matches codex print-mode
	// convention — callers MUST NOT read result.Text on error.
	if waitErr != nil {
		finalText := strings.TrimSpace(string(stdoutBytes))
		switch {
		case finalText != "" && stderrTail != "":
			return agent.RunResult{}, fmt.Errorf("%w (last answer: %q; stderr: %s)", waitErr, finalText, stderrTail)
		case finalText != "":
			return agent.RunResult{}, fmt.Errorf("%w (last answer: %q)", waitErr, finalText)
		case stderrTail != "":
			return agent.RunResult{}, fmt.Errorf("%w (stderr: %s)", waitErr, stderrTail)
		default:
			return agent.RunResult{}, waitErr
		}
	}
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		if stderrTail != "" {
			return agent.RunResult{}, fmt.Errorf("dsh: stdout: %w (stderr: %s)", readErr, stderrTail)
		}
		return agent.RunResult{}, fmt.Errorf("dsh: stdout: %w", readErr)
	}

	return agent.RunResult{
		Text:       strings.TrimSpace(string(stdoutBytes)),
		DurationMs: elapsedMs,
		Subtype:    subtype,
	}, nil
}
