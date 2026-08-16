// print.go — one-shot print mode for opencode using `opencode run`.
//
// Why this exists (F-OPENCODE-PRINT-001, 2026-08-14):
//
// The opencode bridge historically had ONE spawn recipe for
// RunOnce: the long-lived `opencode serve` HTTP path used by
// Starter.Start. RunOnce would Start + defer Close +
// drain Events() until EventAgentResult. That pattern works but
// carries three costs
// the one-shot use case doesn't need:
//
//  1. ~1s server boot. opencode serve spawns a subprocess, binds
//     a port (random by default), prints the URL to stdout for
//     the bridge to parse. For /gtw commit and buildAgentPrompt
//     this is pure waste.
//
//  2. Session handshake. Create-session + subscribe-SSE round-
//     trips add another ~100-300ms before the prompt can even
//     fire.
//
//  3. ~5s closeDrainTimeout. The HTTP server has no "exit after
//     one turn" protocol flag; Close waits up to 5s for graceful
//     shutdown before falling back to kill.
//
// The pragmatic fix, mirroring codex (F-CODEX-PRINT-001),
// claudecode (F-CLAUDE-PRINT-001) and pi (F-PI-PRINT-001):
//
//	opencode run --format json --dir <workspace> \
//	  [-f <file1> [-f <file2> ...]] \
//	  <prompt>
//
// Note: opencode run also accepts `-m <provider/model>` and
// `--attach <url>`, neither of which is wired through cfg today.
// Model defaults to whatever opencode.json names; per-call model
// overrides would require a `Model string` field on StartConfig
// (intentionally absent — matches codex/claudecode/pi print modes
// which all skip per-call model selection). Attach is for high-
// frequency one-shot callers that want to skip the per-call serve
// boot; the bridge's print mode already pays that boot cost once
// via a fresh spawn, so attach is unnecessary until /gtw commit
// starts running RunOnce many times per second.
//
// Verified against the opencode CLI source (sst/opencode
// packages/opencode/src/cli/cmd/run.ts) for v1.18.14:
//
//   - `--format json` emits one JSON event per stdout line via
//     `process.stdout.write(JSON.stringify({type, timestamp,
//     sessionID, ...data}) + EOL)`. Six event types: tool_use,
//     step_start, step_finish, text, reasoning, error.
//   - `step_finish` is the terminal signal — the CLI process
//     exits with code 0 right after the last step_finish of the
//     turn. There is no separate "session.idle" event in
//     `opencode run --format json` mode (that exists only in
//     the long-lived SSE wire).
//   - Every event carries `sessionID` at the top level, so the
//     audit-side session id is cheap to capture. If a future
//     opencode release ships events without sessionID (e.g. a
//     server-side tool lifecycle event), the field stays
//     empty in RunResult — non-fatal, audit-only.
//   - Exit code semantics: `process.exitCode = 1` is set when
//     the model errors or a CLI arg fails; otherwise 0. Stderr
//     captures the human-readable failure message.
//
// Why we don't reuse the long-lived `translator` from
// translate.go:
//
// translate.go is designed for SSE streams — it has streaming-
// specific state (pendingTools correlation, turnHadContent /
// turnHadStep flags, markContent reset on each prompt). The
// `opencode run --format json` wire is a sequential, single-turn
// delivery — no tool correlation needed, no per-prompt state
// reset, no EventAgent channel. The state surface is much
// smaller (text accumulator + sessionID + done flag), so we
// keep this implementation self-contained rather than force-
// fitting the streaming translator.
//
// If a future migration brings a second bridge into the same
// shape (e.g. another print-mode emitter), this file is the
// place to extract a shared parser.
package opencode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// stderrCapBytes bounds the stderr buffer kept in memory across a
// print-mode run. 64 KiB matches the long-lived bridge's stderr
// tail and the cross-bridge convention (codex, claudecode, pi
// print.go).
const stderrCapBytes = 64 * 1024

// runEvent is the subset of `opencode run --format json` events
// we consume. The full schema is observed in
// packages/opencode/src/cli/cmd/run.ts (sst/opencode@dev); we
// tolerate unknown event types / extra fields by ignoring them.
//
// Wire shape: one JSON object per stdout line:
//
//	{ "type": "<event>", "timestamp": <ms>, "sessionID": "<id>", ... }
//
// `part` carries the per-type payload (text/reasoning/tool/step*);
// `error` is present only on "error" events.
type runEvent struct {
	Type      string          `json:"type"`
	Timestamp int64           `json:"timestamp,omitempty"`
	SessionID string          `json:"sessionID,omitempty"`
	Part      json.RawMessage `json:"part,omitempty"`
	Error     json.RawMessage `json:"error,omitempty"`
}

// printState accumulates the wire events of a single turn into
// the data the runtime wants back. Fields are populated by
// handleRunEvent and read by runPrintMode after cmd.Wait
// returns. All fields are zero-initialized.
type printState struct {
	text       strings.Builder // accumulated text from `text` events
	sessionID  string          // captured from the first parsed event
	errMsg     string          // captured from `error` events
	done       bool            // set true on `step_finish`
	hadContent bool            // set true on the first text/reasoning event
}

// handleRunEvent is the per-event state mutator. Mirrors the
// surface of the long-lived translator's handleEvent but
// stripped to the print-mode subset (no tool correlation, no
// turn-state machine — `step_finish` IS the terminal).
//
// All extra fields on `part` are tolerated (we only read `text`
// for `text` / `reasoning` types). Unknown event types are
// ignored so a future opencode release that adds new types
// (e.g. `compaction_finish`) doesn't break us.
func handleRunEvent(ev runEvent, st *printState) {
	// sessionID is on every event per the source. First event
	// wins — RunOnce is one-shot so there is no resume path
	// where a later event would carry a different id.
	if st.sessionID == "" && ev.SessionID != "" {
		st.sessionID = ev.SessionID
	}

	switch ev.Type {
	case "text":
		var p struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(ev.Part, &p); err != nil {
			return
		}
		if p.Text == "" {
			return
		}
		st.text.WriteString(p.Text)
		st.hadContent = true
	case "reasoning":
		// RunOnce doesn't surface reasoning to callers — but if
		// a future caller wants it, the bytes are here. Drop
		// silently today.
		var p struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(ev.Part, &p); err == nil && p.Text != "" {
			st.hadContent = true
		}
	case "tool_use", "step_start":
		// Bookkeeping — we don't surface tool/start details to
		// RunResult consumers. Tool-call progress is a chat-
		// session concern (Start path), not a one-shot concern.
	case "step_finish":
		// TERMINAL signal for `opencode run --format json`.
		// The CLI process will exit 0 right after this event.
		st.done = true
	case "error":
		// Decode the error payload. opencode stores it as a
		// string or nested object depending on the cause; we
		// accept both and stringify. A literal `null` payload
		// falls through to the "unknown error event" sentinel
		// rather than being silently dropped by the outer
		// `errMsg != ""` check — without this guard, an error
		// event with no payload would set errMsg="" and the
		// failure path would never fire.
		trimmed := bytes.TrimSpace(ev.Error)
		if len(trimmed) == 0 || string(trimmed) == "null" {
			st.errMsg = "unknown error event"
			return
		}
		var asString string
		if err := json.Unmarshal(ev.Error, &asString); err == nil {
			st.errMsg = asString
			return
		}
		st.errMsg = string(ev.Error)
	default:
		// Unknown event types are tolerated — log at debug in
		// the caller via oLog, never kill the stream.
	}
}

// runNDJSON scans r line-by-line, parses each non-empty line as
// a runEvent, and invokes cb for each. Tolerates malformed
// lines by logging + skipping (mirrors the long-lived
// bridge's pumpStream permissiveness).
//
// Returns ctx.Err() if the context is cancelled mid-scan, so the
// caller can distinguish "child closed stdout cleanly" from "we
// gave up". The caller is expected to keep reading until EOF
// when ctx is alive.
func runNDJSON(ctx context.Context, r io.Reader, cb func(runEvent)) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ev runEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			oLog("PrintMode: invalid JSON event",
				"err", err,
				"line", truncatePrintLogLine(string(line)))
			continue
		}
		cb(ev)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}

// truncatePrintLogLine shortens a line for inclusion in error /
// log messages. Caps at 200 bytes so a multi-MB garbage frame
// doesn't blow up the log line. Renamed from the cross-bridge
// `truncateForLog` because transport.go already defines a
// single-arg version; this one keeps the explicit cap so it
// can be tuned independently without touching the SSE payload
// truncation.
func truncatePrintLogLine(s string) string {
	const cap = 200
	if len(s) <= cap {
		return s
	}
	return s[:cap] + "..."
}

// buildPrintArgs assembles the full argv (including the trailing
// positional prompt) for `opencode run --format json` from
// blocks + cfg. Extracted so the test can verify argv shape
// without spawning the binary.
//
// Layout:
//
//	opencode
//	run
//	--format json
//	--dir <workspace>
//	[-f <path> [-f <path> ...]] per ContentImage / ContentFile block
//	<prompt>                    positional, last
//
// IMPORTANT: the prompt is appended as the FINAL element of the
// returned slice. The caller (runPrintMode) MUST pass the slice
// to agent.NewCmd as-is — never reach into `args` to drop the
// tail. The previous shape `(args, prompt string)` made the
// "append prompt to args" step a separate concern at the call
// site and was missed in the original implementation; the
// review (F-OPENCODE-PRINT-001) found that prompt was silently
// dropped, leaving the child to hang on stdin. Returning the
// finished argv eliminates that foot-gun.
func buildPrintArgs(cfg agent.StartConfig, blocks []agent.ContentBlock) []string {
	args := []string{
		"run",
		"--format", "json",
		"--dir", cfg.Workspace,
	}
	// Model is intentionally NOT threaded through StartConfig
	// for one-shot — the print-mode path uses whatever model
	// the user's opencode.json config names as default. Same
	// posture as codex/claudecode/pi print modes (none of
	// them have a per-call model override either). If a
	// future caller needs per-call model selection on the
	// print path, add `Model string` to StartConfig and a
	// `-m` branch here.

	// Encode blocks preserving order. Text joins with "\n";
	// images/files do TWO things at once:
	//   1. Emit a `-f <path>` flag so opencode actually
	//      attaches the file (verified shape on 1.18.14).
	//   2. Emit a `[image: ...]` / `[file: ...]` placeholder in
	//      the prompt so the model can see WHERE the file sits
	//      in the message — opencode run's `-f` flag carries
	//      no positional info, unlike codex's stdin-block
	//      ordering. This mirrors codex's [image] + -i
	//      double-encoding.
	var promptParts []string
	for _, b := range blocks {
		switch b.Type {
		case agent.ContentText:
			if b.Text != "" {
				promptParts = append(promptParts, b.Text)
			}
		case agent.ContentImage:
			if b.Path == "" {
				continue
			}
			args = append(args, "-f", b.Path)
			// Distinguish `[image: ...]` from `[file: ...]` so the
			// model reads the user's INTENT from the placeholder
			// even though both reach the model via the same `-f`
			// flag. Mirrors claudecode/pi buildPrintArgs (which
			// now delegates to agent.BlocksToPrompt). MIME is
			// appended when present (claudecode pattern) for
			// diagnostic + format-discrimination value.
			if b.MediaType != "" {
				promptParts = append(promptParts,
					fmt.Sprintf("[image: %s (%s)]", b.Path, b.MediaType))
			} else {
				promptParts = append(promptParts, "[image: "+b.Path+"]")
			}
		case agent.ContentFile:
			if b.Path == "" {
				continue
			}
			args = append(args, "-f", b.Path)
			promptParts = append(promptParts, "[file: "+b.Path+"]")
		default:
			oLog("PrintMode: unknown block type, skipping",
				"type", string(b.Type))
		}
	}
	prompt := strings.Join(promptParts, "\n")
	if prompt == "" {
		// All blocks were images / empty — opencode run still
		// needs SOMETHING as the positional arg (verified on
		// 1.18.14 via `opencode run --help` which lists
		// `message` as `[array] [default: []]`; passing zero
		// positional args falls back to an empty prompt which
		// some builds treat as stdin).
		prompt = "(see attached content)"
	}
	// Append the prompt as the final positional. This is the
	// ONLY place prompt is added to argv; do not split this
	// out into a separate `(args, prompt)` return tuple.
	return append(args, prompt)
}

// runPrintMode spawns `opencode run --format json` for one-shot
// invocations (/gtw commit, /gtw pr, buildAgentPrompt). Mirrors
// codex/claudecode/pi print mode: bypass the long-lived bridge
// driver, spawn a fresh process, capture stdout for events +
// final text, capture stderr for failure diagnostics, reap on
// exit.
//
// Failure modes (in priority order, mirroring codex):
//   - waitErr != nil  → model / CLI failure; surface stderr
//   - errMsg set      → captured from `error` wire event
//   - empty finalText → model produced nothing; surface stderr
func runPrintMode(ctx context.Context, s *Starter, cfg agent.StartConfig, blocks []agent.ContentBlock) (agent.RunResult, error) {
	if cfg.Workspace == "" {
		return agent.RunResult{}, fmt.Errorf("opencode: workspace is required")
	}

	startTime := time.Now()
	args := buildPrintArgs(cfg, blocks)
	// prompt is now the last element of args. Logging both
	// fields is useful for ops (size of the prompt vs number
	// of flags) without re-deriving either.
	promptBytes := len(args[len(args)-1])

	cmd := agent.NewCmd(ctx, s.command, args...)
	cmd.Dir = cfg.Workspace // belt-and-braces with --dir

	// Forward cfg.Env the same way Start does (append to
	// os.Environ, cfg wins on conflict). Without this, /gtw
	// commit-time env overrides (custom API keys, MCP
	// credentials) are silently dropped on the print-mode
	// path.
	if len(cfg.Env) > 0 {
		cmd.Env = append(os.Environ(), cfg.Env...)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return agent.RunResult{}, fmt.Errorf("opencode: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		return agent.RunResult{}, fmt.Errorf("opencode: stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return agent.RunResult{}, fmt.Errorf("opencode: start: %w", err)
	}
	pid := cmd.Process.Pid

	oLog("PrintMode Start",
		"command", s.command,
		"workspace", cfg.Workspace,
		"prompt_bytes", promptBytes,
		"flag_count", len(args)-1, // excludes the trailing positional prompt
		"pid", pid)

	// Drain stderr in the background (mirrors codex / cc / pi).
	// Honors ctx cancellation between reads so a cancelled
	// call doesn't wait for the child to be SIGKILL'd by
	// exec.CommandContext before this goroutine returns.
	stderrBuf := &strings.Builder{}
	stderrTruncated := false
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		buf := make([]byte, 4096)
		for {
			if ctx.Err() != nil {
				return
			}
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

	// Read stdout events + populate state. runNDJSON runs
	// concurrently with the stderr drain above; both complete
	// when the child closes its pipes (typically right before
	// exit).
	var state printState
	jsonReadErr := runNDJSON(ctx, stdout, func(ev runEvent) {
		handleRunEvent(ev, &state)
	})

	waitErr := cmd.Wait()
	<-stderrDone

	elapsedMs := time.Since(startTime).Milliseconds()

	oLog("PrintMode Exit",
		"pid", pid,
		"elapsed_ms", elapsedMs,
		"wait_err", errStr(waitErr),
		"stderr_bytes", stderrBuf.Len(),
		"stderr_truncated", stderrTruncated,
		"session_id", state.sessionID,
		"had_content", state.hadContent,
		"done_event", state.done,
	)

	// Build the result. Subtype reflects run outcome:
	//   - "failed"   if exit code != 0 OR an `error` wire event landed
	//   - "completed" otherwise (success path)
	// Usage / Model are intentionally left at zero — opencode
	// run --format json does NOT currently carry tokens or
	// model name on the wire (verified on 1.18.14 source).
	// Callers MUST treat nil/empty as "not reported" per the
	// RunResult doc.
	subtype := "completed"
	if waitErr != nil || state.errMsg != "" {
		subtype = "failed"
	}
	result := agent.RunResult{
		Text:       strings.TrimSpace(state.text.String()),
		SessionID:  state.sessionID,
		DurationMs: elapsedMs,
		Subtype:    subtype,
	}

	// Failure paths: surface stderr (model / auth / API errors
	// land there) plus the underlying wait / json-read / wire
	// error. Order matters — prefer waitErr first because
	// jsonReadErr is usually just "broken pipe on closed
	// child" noise.
	if waitErr != nil {
		stderr := strings.TrimSpace(stderrBuf.String())
		if state.text.Len() > 0 {
			// Process died after writing the final answer
			// (rare but possible). Surface both the answer and
			// the failure so the caller can inspect.
			if stderr != "" {
				return agent.RunResult{}, fmt.Errorf("opencode: exit: %w (last answer: %q; stderr: %s)", waitErr, state.text.String(), stderr)
			}
			return agent.RunResult{}, fmt.Errorf("opencode: exit: %w (last answer: %q)", waitErr, state.text.String())
		}
		if stderr != "" {
			return agent.RunResult{}, fmt.Errorf("opencode: exit: %w (stderr: %s)", waitErr, stderr)
		}
		return agent.RunResult{}, fmt.Errorf("opencode: exit: %w", waitErr)
	}
	if state.errMsg != "" {
		// Captured from the `error` wire event before the
		// process exited cleanly. Per opencode run.ts, this
		// path also sets exitCode=1 but `cmd.Wait` returns
		// nil in some bash-pipe edge cases — defensive
		// capture here. Preserve partial text the same way the
		// waitErr branch does, so the caller sees both the
		// error reason AND whatever the model managed to write
		// before erroring.
		stderr := strings.TrimSpace(stderrBuf.String())
		if state.text.Len() > 0 {
			if stderr != "" {
				return agent.RunResult{}, fmt.Errorf("opencode: error event: %s (last answer: %q; stderr: %s)", state.errMsg, state.text.String(), stderr)
			}
			return agent.RunResult{}, fmt.Errorf("opencode: error event: %s (last answer: %q)", state.errMsg, state.text.String())
		}
		if stderr != "" {
			return agent.RunResult{}, fmt.Errorf("opencode: error event: %s (stderr: %s)", state.errMsg, stderr)
		}
		return agent.RunResult{}, fmt.Errorf("opencode: error event: %s", state.errMsg)
	}
	if jsonReadErr != nil && !errors.Is(jsonReadErr, io.EOF) {
		stderr := strings.TrimSpace(stderrBuf.String())
		if stderr != "" {
			return agent.RunResult{}, fmt.Errorf("opencode: stdout: %w (stderr: %s)", jsonReadErr, stderr)
		}
		return agent.RunResult{}, fmt.Errorf("opencode: stdout: %w", jsonReadErr)
	}
	if !state.hadContent {
		// Process exited 0 but produced nothing visible to
		// RunOnce. The hadContent flag is set on the first
		// `text` or `reasoning` event; a stream that emitted
		// only `tool_use` / `step_start` / `step_finish`
		// without any reasoning or text leaves it false.
		// Could be (a) an empty model reply (rare;
		// `step_finish` still fires with hadContent=false)
		// or (b) a too-fast child that died before any
		// payload-bearing event landed. Surface stderr for
		// diagnostic.
		//
		// Note: this guard deliberately uses hadContent
		// instead of state.text.Len() == 0 so that
		// reasoning-only models (o1, o3, DeepSeek R1 with
		// reasoning_effort > 0) don't get misreported as
		// "empty answer" when they stream only thinking
		// traces without a final text block. Reasoning text
		// is dropped (RunOnce doesn't surface it) but its
		// presence is enough to confirm the model engaged.
		stderr := strings.TrimSpace(stderrBuf.String())
		if stderr != "" {
			return agent.RunResult{}, fmt.Errorf("opencode: empty answer (stderr: %s)", stderr)
		}
		return agent.RunResult{}, fmt.Errorf("opencode: empty answer")
	}
	return result, nil
}

// errStr renders an error's string form, returning "<nil>" for
// the nil case so the log field is always meaningful. Mirrors
// codex/print.go's helper of the same name.
func errStr(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}