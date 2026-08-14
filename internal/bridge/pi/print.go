// print-mode spawn — the one-shot counterpart to the RPC mode
// defined in agent.go / rpc.go.
//
// Why this exists (F-PI-PRINT-001 investigation, 2026-08-13):
//
// The pi bridge historically had ONE spawn recipe: --mode rpc.
// It works fine for long-lived chat sessions (multiple turns on
// a single agent session) but is unreliable for one-shot
// invocations like /gtw commit where the agent spawns → does
// the prompt → exits. Specifically:
//
//   - /gtw commit in production saw pi close its stdout pipe
//     2-5 seconds after the prompt RPC ack, with no
//     `agent_start` / `turn_start` / message events ever
//     streamed back through the bridge's readPump. The bridge
//     saw EOF, RunOnceDrain returned "event stream closed
//     without result", and the verify step in commit.go
//     reported "agent finished but no commit happened".
//   - The exact same RunOnce flow passed when driven from a
//     `go test ./internal/bridge/pi -run RealPi -v` smoke
//     test in a fresh process. Pi stayed alive for 40+ seconds,
//     produced events, and committed successfully.
//
// After ruling out timeouts, ctx cancellation, hook execution,
// prober interference, and bridge code paths, the most likely
// remaining cause is some RPC-mode-specific interaction (the
// long-lived pipe, the response-correlation map, the pending
// waiter) that flakily drops events under daemon load.
//
// The pragmatic fix: for RunOnce (one-shot), do NOT use RPC.
// pi exposes `--mode json -p <prompt>` for exactly this case:
// same JSON event stream as RPC, but the process exits when
// the turn completes. No long-lived pipe, no response
// correlation, no pending waiter — just "spawn, stream
// events, wait for exit".
//
// Start (RPC mode) is unchanged: it still drives the long-lived
// chat-session use case where the runtime holds the bridge
// across many turns. The print-mode path is reachable only
// via RunOnce.
//
// Translation: print-mode emits the same JSON event format as
// RPC mode (`agent_start`, `turn_start`, `message_update`,
// `tool_execution_*`, `agent_settled`, etc.), so the existing
// translator (translate.go) works without modification. The
// only thing print-mode doesn't emit is the `response` envelope
// (no RPC requests); the translator already returns early for
// `case "response"` since it doesn't have an id-correlation
// context in this path.

package pi

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// stderrCapBytes bounds the stderr buffer kept in memory across a
// print-mode run. Without this cap a chatty failing child can
// OOM the bridge silently — caller sees "command failed" with
// no clue that the real cause was memory exhaustion. 64 KiB is
// generous for any human-readable diagnostic and matches the
// long-lived bridge's stderr tail.
const stderrCapBytes = 64 * 1024

// runPrintMode spawns pi in `--mode json -p` mode for one-shot
// invocations. It owns the process from spawn to exit, streams
// events through the standard translator, and returns a
// RunResult carrying the final text + per-turn metadata on
// a clean run. On any failure (spawn / non-zero exit / ctx
// cancel / translator error) it returns a wrapped error.
//
// The translate.go translator is used unchanged — the JSON
// event shape from print-mode is identical to RPC mode's
// server-pushed events. Only the RPC request/response envelope
// is absent (no stdin writes), and the translator never needs
// to see one because the event stream is what produces
// AgentEvents.
func runPrintMode(ctx context.Context, command, prompt, workspace string, env []string) (agent.RunResult, error) {
	if workspace == "" {
		return agent.RunResult{}, fmt.Errorf("pi: workspace is required")
	}

	startTime := time.Now()

	// Build argv. -p takes the prompt as a single argv entry;
	// quoting is handled by exec, not by us. --mode json forces
	// structured output (one JSON event per stdout line) so the
	// translator can parse each line.
	args := []string{"--mode", "json", "-p", prompt}

	cmd := agent.NewCmd(ctx, command, args...)
	cmd.Dir = workspace
	// Forward cfg.Env the same way Start does (append to
	// os.Environ, cfg wins on conflict). Without this,
	// /gtw commit-time env overrides (custom API keys, MCP
	// credentials) are silently dropped on the print-mode path.
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return agent.RunResult{}, fmt.Errorf("pi: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		return agent.RunResult{}, fmt.Errorf("pi: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return agent.RunResult{}, fmt.Errorf("pi: start: %w", err)
	}
	pid := cmd.Process.Pid

	piLog("PrintMode Start",
		"command", command, "workspace", workspace,
		"prompt_bytes", len(prompt), "pid", pid)

	// Drain stderr in the background. Print-mode stderr is
	// mostly empty for a clean run; any non-empty output
	// indicates a setup or model error worth surfacing on
	// non-zero exit (see below). Cap the buffer at
	// stderrCapBytes so a chatty failing child can't OOM the
	// bridge — when we hit the cap, truncate and stop
	// appending (the tail beyond the cap is silently dropped;
	// the log line below records the truncation).
	stderrBuf := &strings.Builder{}
	stderrTruncated := false
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		buf := make([]byte, 4096)
		for {
			n, err := stderr.Read(buf)
			if n > 0 {
				if stderrBuf.Len() < stderrCapBytes {
					room := stderrCapBytes - stderrBuf.Len()
					if n > room {
						stderrBuf.Write(buf[:room])
						stderrTruncated = true
					} else {
						stderrBuf.Write(buf[:n])
					}
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Read stdout events, translate via the shared translator,
	// and capture the final text + per-turn metadata. A reader
	// error here means the pipe broke mid-run (rare; would
	// normally be EOF from a clean exit).
	result, translateErr := streamPrintEvents(ctx, stdout, workspace)

	// Always wait for the process to exit so we can capture
	// both the exit code AND stderr. If streamPrintEvents
	// errored early (e.g. agent_settled never fired) pi may
	// still be a useful signal via its stderr — model errors,
	// auth errors, etc. land there. The wait+reap path is
	// shared between success and failure so neither path loses
	// diagnostic info.
	waitErr := cmd.Wait()
	<-stderrDone

	piLog("PrintMode Exit",
		"pid", pid,
		"elapsed_ms", time.Since(startTime).Milliseconds(),
		"wait_err", errStr(waitErr),
		"stderr_bytes", stderrBuf.Len(),
		"stderr_truncated", stderrTruncated,
	)

	if translateErr != nil {
		// Stream reader hit a non-EOF error, OR the JSON
		// stream ended without an agent_settled event.
		// Either way the prompt did not complete normally;
		// surface stderr if pi left a hint about why.
		stderr := strings.TrimSpace(stderrBuf.String())
		if stderr != "" {
			return agent.RunResult{}, fmt.Errorf("pi: %w (stderr: %s)", translateErr, stderr)
		}
		return agent.RunResult{}, translateErr
	}

	if waitErr != nil {
		// Surface stderr if any — most failures land here
		// (auth errors, model errors, etc.) with a short
		// human-readable message in stderr.
		stderr := strings.TrimSpace(stderrBuf.String())
		if stderr != "" {
			return agent.RunResult{}, fmt.Errorf("pi: exit: %w (stderr: %s)", waitErr, stderr)
		}
		return agent.RunResult{}, fmt.Errorf("pi: exit: %w", waitErr)
	}

	return result, nil
}

// streamPrintEvents drives the JSON event reader from a
// print-mode pi spawn. It runs the existing translator
// (translate.go) over each line and watches for either an
// `agent_settled` event (turn-end marker carrying the final
// text) or a stream error. Returns RunResult on clean
// completion.
//
// This is the print-mode analogue of the chat-session
// readPump + lifecycle pair, minus the RPC plumbing. The
// translator is reused as-is because the event format is
// shared between print and RPC modes.
func streamPrintEvents(ctx context.Context, stdout io.Reader, workspace string) (agent.RunResult, error) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), MaxFrameSize)

	branch := detectBranch(workspace)
	translator := newTranslator("pi-print", workspace, branch)
	// Note: print-mode does NOT call emitConnected (no
	// handshake). connectedSent stays false but that doesn't
	// matter here — translate() never reads it (only
	// emitConnected does, which we don't call). The state_update
	// case in translate.go bypasses the connectedSent check,
	// so a stray state_update event would still surface
	// EventAgentReady. Print-mode doesn't emit state_update,
	// so this is academic — flagged in case a future pi version
	// starts emitting it.

	var result agent.RunResult
	sawSettled := false

	for scanner.Scan() {
		// Honour ctx cancellation between lines so we exit
		// promptly when the caller's deadline fires. The
		// process is killed by exec.CommandContext (used via
		// agent.NewCmd) when ctx is cancelled — we just stop
		// reading here and let runPrintMode's cmd.Wait() reap
		// the SIGKILLed process.
		if err := ctx.Err(); err != nil {
			return agent.RunResult{}, err
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		events, err := translator.translate(line, nil)
		if err != nil {
			return agent.RunResult{}, fmt.Errorf("pi: translate: %w (line=%s)", err, truncateForErr(line))
		}

		for _, ev := range events {
			// We only care about the final result event;
			// everything else (text deltas, tool calls,
			// thinking) is consumed by translate.go for
			// state tracking but doesn't carry the text
			// we want to return.
			switch ev.Kind {
			case agent.EventAgentResult:
				if ev.Result != nil {
					result.Text = ev.Result.Text
					result.Usage = ev.Result.Usage
					result.DurationMs = ev.Result.DurationMs
					result.Subtype = ev.Result.Subtype
				}
			case agent.EventAgentDone:
				sawSettled = true
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return agent.RunResult{}, fmt.Errorf("pi: stdout: %w", err)
	}

	if !sawSettled {
		// Process exited cleanly but we never saw an
		// agent_settled event — unusual for pi but possible
		// if the model exited early or the JSON stream was
		// truncated. Treat as failure so the caller knows
		// the prompt didn't complete.
		//
		// Even though no terminal event fired, an
		// EventAgentResult may have arrived earlier in the
		// stream — pi's translator emits it on turn_end. We
		// already captured its text + usage + subtype onto
		// `result`; surface them as audit fields so the
		// runtime can still report "your last /gtw commit
		// spent 1234 input tokens before the turn went
		// unconfirmed" instead of silently underreporting
		// cost on the failure path. Mirrors the claudecode
		// is_error fix in print.go.
		//
		// Pi's print-mode doesn't expose a session_id (the
		// EventAgentResult translator doesn't surface it on
		// this path) so we only append usage + subtype.
		return agent.RunResult{}, fmt.Errorf("pi: exit without agent_settled%s", appendAuditFields(result, false))
	}

	result.Text = strings.TrimSpace(result.Text)
	return result, nil
}

// truncateForErr shortens a malformed JSONL line for inclusion
// in error messages. Caps at 200 bytes so a multi-MB garbage
// frame doesn't blow up the error string.
func truncateForErr(line []byte) string {
	const cap = 200
	if len(line) <= cap {
		return string(line)
	}
	return string(line[:cap]) + "..."
}

// appendAuditFields returns the audit-suffix string (empty when
// result has nothing to report) appended to error messages on
// the pi print-mode failure paths. Symmetric with claudecode's
// `[session_id=X] [usage in=N out=N cache_read=N]` formatting.
//
// Pi's print-mode doesn't surface SessionID, so that field is
// skipped (whenSessionID is for parity with claudecode; pass
// true to include it on bridges that capture it).
//
// Subtype is included when non-empty — pi uses stopReason strings
// (e.g. "stop", "tool_use", "max_tokens"); claudecode uses the
// result.subtype vocabulary (e.g. "error_max_turns"). Both are
// useful audit info and fit the same bracketed format.
//
// Usage is included when non-nil; "in/out/cache_read" match the
// pi translate.go usage fields (input_tokens / output_tokens /
// cache_read_tokens).
func appendAuditFields(result agent.RunResult, whenSessionID bool) string {
	var audit strings.Builder
	if whenSessionID && result.SessionID != "" {
		audit.WriteString(" [session_id=")
		audit.WriteString(result.SessionID)
		audit.WriteByte(']')
	}
	if result.Subtype != "" {
		audit.WriteString(" [subtype=")
		audit.WriteString(result.Subtype)
		audit.WriteByte(']')
	}
	if result.Usage != nil {
		audit.WriteString(" [usage in=")
		audit.WriteString(strconv.Itoa(result.Usage.InputTokens))
		audit.WriteString(" out=")
		audit.WriteString(strconv.Itoa(result.Usage.OutputTokens))
		audit.WriteString(" cache_read=")
		audit.WriteString(strconv.Itoa(result.Usage.CacheReadInputTokens))
		audit.WriteByte(']')
	}
	return audit.String()
}
