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
	"os"
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
	if translateErr != nil {
		// Make sure the process is reaped even if the
		// reader errored out before EOF. SIGKILL is the
		// safe fallback when the process is wedged or we
		// lost the pipe.
		_ = cmd.Process.Kill()
		<-stderrDone
		return "", translateErr
	}

	// Wait for the process to fully exit and capture the
	// exit code. Print-mode is supposed to exit 0 after the
	// turn completes; a non-zero exit is a model / setup
	// error worth surfacing.
	waitErr := cmd.Wait()
	<-stderrDone

	piLog("PrintMode Exit",
		"pid", pid,
		"elapsed_ms", time.Since(startTime).Milliseconds(),
		"exit_err", errStr(waitErr),
		"stderr_bytes", stderrBuf.Len(),
	)

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
	// connectedSent=true skips the EventAgentReady path; print-mode
	// doesn't emit a get_state envelope, so the translator's
	// "haven't seen connected yet" guard would otherwise drop
	// every event. The chat-session path calls deliverConnectedLocked
	// after Start's handshake — we have no handshake here, so we
	// mark connected directly.
	translator.connectedSent = true

	var finalText string
	sawSettled := false

	for scanner.Scan() {
		// Honour ctx cancellation between lines so a stuck
		// pi doesn't outlive its caller's deadline. We don't
		// kill the process here — that's streamPrintEvents'
		// caller's job (SIGKILL after reader error) — we
		// just stop reading.
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

// os is imported only for the future image-encoding extension
// point (see comment in RunOnce); keeping the explicit import
// rather than relying on transitive use keeps go vet honest.
var _ = os.Stat
