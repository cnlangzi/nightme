// Package agent — BridgeDiagnostic: structured payload attached to
// EventAgentError when a bridge child process exits without our
// permission.
//
// Background: every long-lived bridge (codex / dsh / pi / claudecode
// / opencode) spawns a child process and tracks it via cmd.Wait().
// When the child exits for ANY reason we want to know:
//
//   - HOW it exited (graceful close, clean exit, signal, OOM, panic, etc.)
//   - WHAT it last wrote to stderr (capped ring buffer, always on)
//   - WHEN it died (wall clock)
//   - WHICH session it was (so /diagnose can correlate)
//
// This is the "治根" of a class of bugs where the bridge silently
// died mid-turn and the chat user had no visibility. Before this
// struct existed, the same info was either dropped (Debug-gated
// dLog) or stuck in the nightme.log at line 152 with the misleading
// "claude process exited" hardcoded message. With Diagnostic attached
// to EventAgentError, every layer of the pipeline (gateway translate,
// channel renderer, recovery policy) can act on it without having to
// re-derive the info from lower layers.
//
// Design choices:
//
//   - BridgeDiagnostic is a value type (not interface) so callers can
//     inspect fields directly without type assertions. Bridges fill
//     it on the way out of lifecycle(); the runtime treats it as
//     read-only data attached to an event.
//
//   - ExitKind is a small enum (not a free-form string). Forces every
//     bridge to classify via ClassifyExit so the recovery policy can
//     switch on it without parsing error messages.
//
//   - StderrFingerprint is a best-effort signature for "is this the
//     same crash as last time?". It strips stack frame paths, line
//     numbers, and timestamps — leaves the error message + first
//     stable frame. Used by RecoveryPolicy to detect systematic
//     failures (same fingerprint 3+ times within 5min → escalate)
//     without falsely escalating on transient crashes.
//
//   - RingBuffer is a small byte ring kept in every long-lived bridge
//     for the LAST stderrTailBytes of stderr output. Always populated
//     (no Debug gating), so /diagnose can show the user "what did
//     dsh say right before it died" without us having to reproduce
//     the failure.
package agent

import (
	"errors"
	"os/exec"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// stderrTailBytes is how much of the child's stderr we keep for
// BridgeDiagnostic. 4 KiB matches what codex already used pre-refactor
// (2 KiB) and the cross-bridge convention in opencode's print-mode
// (64 KiB). 4 KiB is enough for one panic stack + a few lines of
// "fatal load failure" output — anything larger gets truncated and
// the [:N bytes dropped] prefix is preserved.
const StderrTailBytes = 4 * 1024

// BridgeExitKind classifies the reason a bridge child process exited.
//
// The lifecycle() of each bridge calls ClassifyExit(waitErr, graceful)
// once cmd.Wait() returns; the resulting enum drives both the
// Diagnostic field and the recovery policy's escalation logic.
type BridgeExitKind int

const (
	// BridgeExitUnknown: defensive default. Should never be returned
	// by ClassifyExit in practice, but the zero value is safe to
	// embed in a Diagnostic.
	BridgeExitUnknown BridgeExitKind = iota

	// BridgeExitGracefulClose: bridge.Close() was called (d.closed
	// was signaled) and the child exited as a result. Not an error
	// from the runtime's perspective — no Diagnostic, no respawn.
	BridgeExitGracefulClose

	// BridgeExitCleanExit: cmd.Wait() returned nil, so the child
	// exited 0, BUT Close() wasn't called. Rare in practice — only
	// happens if the child voluntarily exits (some agents have a
	// /exit command). Treated like graceful for recovery purposes.
	BridgeExitCleanExit

	// BridgeExitNonZeroExit: cmd.Wait() returned an *exec.ExitError
	// with a positive exit code (1..125). The child crashed with an
	// error but did so politely (no signal). Includes crashes from
	// Node.js uncaughtException when the child installs a handler.
	BridgeExitNonZeroExit

	// BridgeExitSignalKilled: cmd.Wait() returned an *exec.ExitError
	// with negative exit code (signals use -SIGNAL). The child was
	// terminated by a signal — usually SIGKILL from the OS (OOM
	// killer, watchdog) or from us via Process.Kill().
	BridgeExitSignalKilled

	// BridgeExitWatchdogTimeout: bridge's own watchdog
	// (lifecycleWatchdogTimeout) fired and SIGKILL'd the child.
	// Distinguished from BridgeExitSignalKilled because the cause
	// is "child wedged past its own grace window", which is a
	// different recovery signal than "OS killed us".
	BridgeExitWatchdogTimeout

	// BridgeExitPanic: a panic propagated up to lifecycle() — either
	// the bridge panicked in a goroutine that wasn't wrapped by
	// SafeGo/PanicEventHandler, or the panic-recovery path itself
	// failed. Almost always means a bug in the bridge.
	BridgeExitPanic
)

// String renders a BridgeExitKind for logs. Stable across versions;
// recovery policy's stderr fingerprint also includes this.
func (k BridgeExitKind) String() string {
	switch k {
	case BridgeExitGracefulClose:
		return "graceful-close"
	case BridgeExitCleanExit:
		return "clean-exit"
	case BridgeExitNonZeroExit:
		return "non-zero-exit"
	case BridgeExitSignalKilled:
		return "signal-killed"
	case BridgeExitWatchdogTimeout:
		return "watchdog-timeout"
	case BridgeExitPanic:
		return "panic"
	}
	return "unknown"
}

// BridgeDiagnostic is attached to EventAgentError.Diagnostic when a
// bridge child exits without our permission. Read-only once delivered.
type BridgeDiagnostic struct {
	// ExitKind classifies the exit reason. See ClassifyExit.
	ExitKind BridgeExitKind

	// WaitErr is the raw error from cmd.Wait(). May be nil for
	// BridgeExitCleanExit. Use errors.As(err, &exitErr) to extract
	// the exit code / signal.
	WaitErr error

	// StderrTail is the LAST stderrTailBytes of the child's stderr,
	// captured by a per-bridge ring buffer that runs unconditionally
	// (no Debug gating). May be empty if the child never wrote to
	// stderr. Suitable for direct inclusion in EventAgentError.Err
	// (truncated by callers to keep messages chat-friendly).
	StderrTail string

	// SessionID is the bridge session id (dsh sessionId, codex
	// threadID, etc.) the child was working on. Lets /diagnose
	// correlate the crash with the JSONL history on disk.
	SessionID string

	// AgentName is the bridge / agent identifier ("dsh", "codex",
	// "pi", "claudecode", "opencode"). Copied from
	// AgentSessionEntry.Agent at deliver time.
	AgentName string

	// KilledAt is wall-clock time at which the exit was observed
	// (i.e. cmd.Wait() returned). Not the time of any underlying
	// signal — those don't surface in Go's exec API.
	KilledAt time.Time
}

// ClassifyExit maps (waitErr, graceful) into a BridgeExitKind.
// Shared across all bridges so the recovery policy sees the same
// taxonomy regardless of which bridge died.
//
// Precedence:
//
//	graceful=true                 → BridgeExitGracefulClose
//	waitErr==nil                  → BridgeExitCleanExit
//	*exec.ExitError with code<0   → BridgeExitSignalKilled
//	*exec.ExitError with code>=0  → BridgeExitNonZeroExit
//	other error (panic recovery)  → BridgeExitPanic
//	anything else                 → BridgeExitUnknown
func ClassifyExit(waitErr error, graceful bool) BridgeExitKind {
	if graceful {
		return BridgeExitGracefulClose
	}
	if waitErr == nil {
		return BridgeExitCleanExit
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		// Go's exec.ExitError: positive code = child exit code,
		// negative code = killed by signal (-SIGNAL).
		// Codes <0 are signals; >=0 are exit codes.
		if exitErr.ExitCode() < 0 {
			return BridgeExitSignalKilled
		}
		return BridgeExitNonZeroExit
	}
	// errors.As failed → not a normal exit error. Most commonly
	// this is the panic-recovery path: lifecycle() caught a panic
	// (SafeGo/PanicEventHandler would have recovered already, but
	// a second-order failure can leak through).
	if strings.Contains(waitErr.Error(), "panic") {
		return BridgeExitPanic
	}
	return BridgeExitUnknown
}

// stderrFrameRegexp matches the volatile part of a Go-style stack
// frame header — the line number and the PC offset:
//
//	/path/to/file.go:123 +0xabc
//	/path/to/file.go:123
//
// We replace the whole `.go:NN +0xABC` group with just `.go`, so the
// same panic from different PIDs / builds still hashes to the same
// signature, but the source filename (e.g. `main.go`) is preserved
// as part of the fingerprint — that's the stable identifier an
// operator uses to triage.
//
// Used as `stderrFrameRegexp.ReplaceAllString(line, ".go")`; the
// replacement literal is co-located with this declaration for
// grep-ability.
var stderrFrameRegexp = regexp.MustCompile(`\.go:\d+(?:\s+\+0x[0-9a-f]+)?`)

// stderrFrameReplacement is what stderrFrameRegexp matches are
// rewritten to in StderrFingerprint. Keep in sync with the regex
// above.
const stderrFrameReplacement = ".go"

// stderrTimestampRegexp matches common timestamp shapes so they
// don't perturb the fingerprint. Specifically targets:
//
//	2026-08-15T09:57:03.909Z
//	2026-08-15T09:57:03.909+08:00
//	[1234567890]
//	time="..." level=... (loki-style)
var stderrTimestampRegexp = regexp.MustCompile(
	`(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?|` +
		`\[\d{10,15}\]|\btime="[^"]*")`,
)

// StderrFingerprint reduces a stderr tail to a stable signature that
// identifies "this is the same crash as last time" regardless of
// PID, timestamp, or stack-frame offsets. Used by RecoveryPolicy to
// detect systematic failures (same fingerprint 3+ in 5min → escalate).
//
// Heuristic: take the first non-empty lines up to a budget, strip
// timestamps + stack frame offsets + paths, join with "|". Output
// is bounded at ~200 bytes so log lines and recovery-policy state
// stay compact.
//
// Empty input → empty output (caller can treat that as "no signature").
func StderrFingerprint(tail string) string {
	if tail == "" {
		return ""
	}
	var sig strings.Builder
	budget := 200
	for _, line := range strings.Split(tail, "\n") {
		line = stderrTimestampRegexp.ReplaceAllString(line, "")
		line = stderrFrameRegexp.ReplaceAllString(line, stderrFrameReplacement)
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if sig.Len() > 0 {
			sig.WriteByte('|')
		}
		sig.WriteString(line)
		if sig.Len() >= budget {
			break
		}
	}
	if sig.Len() > budget {
		s := sig.String()
		sig.Reset()
		sig.WriteString(s[:budget])
	}
	return sig.String()
}

// stderrRingBuffer is a small byte ring kept by each long-lived
// bridge for the last stderrTailBytes of stderr output. NOT
// thread-safe; callers serialize access (each bridge has exactly
// one drainer goroutine writing to it).
//
// Mirrors the local ringBuffer in codex/session.go — kept private
// here so other packages can't accidentally misuse the unsafe
// internal buffer. Use NewStderrRingBuffer to construct, String()
// to read.
type StderrRingBuffer struct {
	mu  sync.Mutex // protects against concurrent String() from lifecycle() vs Write() from drainer
	buf []byte
	max int
}

func NewStderrRingBuffer(max int) *StderrRingBuffer {
	if max <= 0 {
		max = StderrTailBytes
	}
	return &StderrRingBuffer{buf: make([]byte, 0, max), max: max}
}

func (r *StderrRingBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.max {
		// Drop from the front, keep the tail.
		r.buf = r.buf[len(r.buf)-r.max:]
	}
	return len(p), nil
}

// String returns the current ring contents as a string. Safe to
// call concurrently with Write (acquires the same mutex). The
// returned string is a copy; the caller may retain or mutate it.
func (r *StderrRingBuffer) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.buf)
}

// Truncate shortens s to at most max bytes, returning s unchanged
// when it fits. Truncation marker " ...[truncated N bytes]" is
// appended so the user can see that data was cut.
//
// Helper for callers that want to embed stderr tails in chat
// messages or log lines without overflowing.
func TruncateForLog(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	const marker = " ...[truncated]"
	if max <= len(marker) {
		return s[:max]
	}
	return s[:max-len(marker)] + marker
}

// formatDiagnosticString is a helper for tests + humanize paths.
// Pulled out so bridge_diagnostic_test.go can verify behavior
// without duplicating the format string.
func formatDiagnosticString(d *BridgeDiagnostic) string {
	if d == nil {
		return ""
	}
	return fmt.Sprintf("agent=%s exit=%s session=%s stderr_bytes=%d",
		d.AgentName, d.ExitKind, d.SessionID, len(d.StderrTail))
}
