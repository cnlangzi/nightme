//go:build !windows

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
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// runPrintMode spawns pi in `--mode json -p` mode for one-shot
// invocations. It owns the process from spawn to exit, streams
// events through the standard translator, and returns the
// agent's final text on a clean run. On any failure (spawn /
// non-zero exit / ctx cancel / translator error) it returns
// a wrapped error.
//
// The translate.go translator is used unchanged — the JSON
// event shape from print-mode is identical to RPC mode's
// server-pushed events. Only the RPC request/response envelope
// is absent (no stdin writes), and the translator never needs
// to see one because the event stream is what produces
// AgentEvents.
func runPrintMode(ctx context.Context, command, prompt, workspace string) (string, error) {
	if workspace == "" {
		return "", fmt.Errorf("pi: workspace is required")
	}

	startTime := time.Now()

	// Build argv. -p takes the prompt as a single argv entry;
	// quoting is handled by exec, not by us. --mode json forces
	// structured output (one JSON event per stdout line) so the
	// translator can parse each line.
	args := []string{"--mode", "json", "-p", prompt}

	cmd := agent.NewCmd(ctx, command, args...)
	cmd.Dir = workspace

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("pi: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		return "", fmt.Errorf("pi: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return "", fmt.Errorf("pi: start: %w", err)
	}
	pid := cmd.Process.Pid

	piLog("PrintMode Start",
		"command", command, "workspace", workspace,
		"prompt_bytes", len(prompt), "pid", pid)

	// Drain stderr in the background. Print-mode stderr is
	// mostly empty for a clean run; any non-empty output
	// indicates a setup or model error worth surfacing on
	// non-zero exit (see below).
	stderrBuf := &strings.Builder{}
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		buf := make([]byte, 4096)
		for {
			n, err := stderr.Read(buf)
			if n > 0 {
				stderrBuf.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	// Read stdout events, translate via the shared translator,
	// and capture the final text. A reader error here means
	// the pipe broke mid-run (rare; would normally be EOF
	// from a clean exit).
	text, translateErr := streamPrintEvents(ctx, stdout, workspace)

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
	)

	if translateErr != nil {
		// Stream reader hit a non-EOF error, OR the JSON
		// stream ended without an agent_settled event.
		// Either way the prompt did not complete normally;
		// surface stderr if pi left a hint about why.
		stderr := strings.TrimSpace(stderrBuf.String())
		if stderr != "" {
			return "", fmt.Errorf("pi: %w (stderr: %s)", translateErr, stderr)
		}
		return "", translateErr
	}

	if waitErr != nil {
		// Surface stderr if any — most failures land here
		// (auth errors, model errors, etc.) with a short
		// human-readable message in stderr.
		stderr := strings.TrimSpace(stderrBuf.String())
		if stderr != "" {
			return "", fmt.Errorf("pi: exit: %w (stderr: %s)", waitErr, stderr)
		}
		return "", fmt.Errorf("pi: exit: %w", waitErr)
	}

	return text, nil
}

// streamPrintEvents drives the JSON event reader from a
// print-mode pi spawn. It runs the existing translator
// (translate.go) over each line and watches for either an
// `agent_settled` event (turn-end marker carrying the final
// text) or a stream error. Returns the final text on clean
// completion.
//
// This is the print-mode analogue of the chat-session
// readPump + lifecycle pair, minus the RPC plumbing. The
// translator is reused as-is because the event format is
// shared between print and RPC modes.
func streamPrintEvents(ctx context.Context, stdout io.Reader, workspace string) (string, error) {
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

	var finalText string
	sawSettled := false

	for scanner.Scan() {
		// Honour ctx cancellation between lines so we exit
		// promptly when the caller's deadline fires. The
		// process is killed by exec.CommandContext (used via
		// agent.NewCmd) when ctx is cancelled — we just stop
		// reading here and let runPrintMode's cmd.Wait() reap
		// the SIGKILLed process.
		if err := ctx.Err(); err != nil {
			return "", err
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		events, err := translator.translate(line, nil)
		if err != nil {
			return "", fmt.Errorf("pi: translate: %w (line=%s)", err, truncateForErr(line))
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
					finalText = ev.Result.Text
				}
			case agent.EventAgentDone:
				sawSettled = true
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("pi: stdout: %w", err)
	}

	if !sawSettled {
		// Process exited cleanly but we never saw an
		// agent_settled event — unusual for pi but possible
		// if the model exited early or the JSON stream was
		// truncated. Treat as failure so the caller knows
		// the prompt didn't complete.
		return "", fmt.Errorf("pi: exit without agent_settled")
	}

	return strings.TrimSpace(finalText), nil
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
