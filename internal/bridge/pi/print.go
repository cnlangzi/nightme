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
//     saw EOF, the bridge's RunOnce returned "event stream closed
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
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/proc"
)

// stderrCapBytes bounds the stderr buffer kept in memory across a
// print-mode run. Without this cap a chatty failing child can
// OOM the bridge silently — caller sees "command failed" with
// no clue that the real cause was memory exhaustion. 64 KiB is
// generous for any human-readable diagnostic and matches the
// long-lived bridge's stderr tail.
const stderrCapBytes = 64 * 1024

// buildPrintArgs assembles the argv for a one-shot `pi --mode json -p`
// call. The prompt portion is delegated to agent.BlocksToPrompt
// (shared by claudecode / pi / dsh; the print-mode bridges whose
// -p flag accepts a single positional string). Returns (args,
// prompt) so callers can log prompt_bytes without re-extracting
// it from argv — mirrors codex/buildPrintArgs + opencode/buildPrintArgs
// signatures so all four print-mode bridges expose the same surface.
func buildPrintArgs(blocks []agent.ContentBlock) (args []string, prompt string) {
	prompt = agent.BlocksToPrompt(blocks)
	args = []string{"--mode", "json", "-p", prompt}
	return args, prompt
}

// peekPrintMeta inspects a single wire line for the metadata that
// pi's print-mode emits but the translator's typed AgentEvent
// surface never carries: the session id ({"type":"session","id":..})
// and the assistant model ({"type":"message_*","message":{"role":"assistant","model":..}}).
//
// Print-mode has no get_state handshake, so these fields live on
// wire events the translator drops via its default case
// (translate.go:533+) or doesn't promote out of the assistant
// message struct. Peeking here lets RunResult carry both fields
// so the AgentBar footer in channel/feishu renders
// "🤖: pi · <model> · <sessionid>" instead of just "🤖: pi".
//
// First-non-empty wins for both — semantically "the session the
// turn ran in" is the only session event on the wire (one per
// process), and "the model that actually produced the turn" is
// the first assistant message's model (later updates within the
// same turn are rare but should not retroactively rewrite the
// AgentBar).
//
// contextWindow is intentionally NOT extracted: pi's wire events
// do not carry the API-reported context window in print-mode
// (no get_state response, no per-message contextWindow field).
// The UsageBar will therefore continue to omit the `X% (window)`
// segment for pi RunOnce outputs — same as before this fix.
// Filling it would require either a per-model catalog or a
// pi-side wire change; both are out of scope for the
// RunOnce footer stamp. Documented in
// docs/feat/F-PI-PRINT-002.md.
//
// `line` is the raw JSONL frame (without trailing newline); the
// helper is silent on malformed input — translator.translate()
// surfaces JSON errors via the wrapped "pi: translate:" path.
func peekPrintMeta(line []byte, result *agent.RunResult) {
	// Short-circuit: once both fields are captured, every
	// subsequent line (~99% of a typical pi session) is a
	// no-op, so skip the json.Unmarshal entirely. Reused the
	// shared eventEnvelope type from protocol.go so the wire
	// shape lives in exactly one place — see the F-PI-PRINT-002
	// note on eventEnvelope.ID / eventEnvelope.Message.
	if result.SessionID != "" && result.Model != "" {
		return
	}
	var env eventEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		return
	}
	if env.Type == "session" && env.ID != "" && result.SessionID == "" {
		result.SessionID = env.ID
	}
	if env.Message.Role == "assistant" &&
		env.Message.Model != "" &&
		result.Model == "" &&
		(env.Type == "message_start" || env.Type == "message_update" || env.Type == "message_end") {
		result.Model = env.Message.Model
	}
}

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
//
// opts (when set via WithEventSink) wires a per-call observer
// that receives the same AgentEvent stream the dsh / codex
// bridges emit — Ready up front, then per-event Text /
// ToolStart / ToolEnd / TaskCreate / TaskUpdate as they're
// translated, then Result / Done at turn end. Without this
// the per-call sink observer was invisible to the print-mode
// path (historical bug fixed in this revision: opts were
// accepted by RunOnce but silently dropped before reaching
// runPrintMode). The aggregator (e.g. /review multi-job) and
// /gtw callers both depend on the sink observing the full
// lifecycle so the chat channel can flip its receipt header
// from "running" to "done".
func runPrintMode(ctx context.Context, s *Starter, cfg agent.StartConfig, blocks []agent.ContentBlock, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	if cfg.Workspace == "" {
		return agent.RunResult{}, fmt.Errorf("pi: workspace is required")
	}

	startTime := time.Now()
	sink := agent.ParseRunOnceOptions(opts).OnEvent

	// Build argv from blocks. -p takes the prompt as a single
	// argv entry; quoting is handled by exec, not by us. --mode
	// json forces structured output (one JSON event per stdout
	// line) so the translator can parse each line.
	args, prompt := buildPrintArgs(blocks)

	child := proc.New(ctx, s.command, args...)
	child.Dir = cfg.Workspace
	// Forward cfg.Env the same way Start does (append to
	// os.Environ, cfg wins on conflict). Without this,
	// /gtw commit-time env overrides (custom API keys, MCP
	// credentials) are silently dropped on the print-mode path.
	if len(cfg.Env) > 0 {
		child.Env = append(os.Environ(), cfg.Env...)
	}

	stdout, err := child.StdoutPipe()
	if err != nil {
		return agent.RunResult{}, fmt.Errorf("pi: stdout pipe: %w", err)
	}
	stderr, err := child.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		return agent.RunResult{}, fmt.Errorf("pi: stderr pipe: %w", err)
	}

	// Pre-spawn failure: any error path here must emit a
	// terminal EventAgentError to the sink so the caller sees
	// a balanced lifecycle (Ready ↔ terminal). The up-front
	// Ready fires only after we've actually spawned the
	// process (so pid is known); on pre-spawn failure we
	// skip the Ready and emit Error directly.
	if err := child.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		wrapped := fmt.Errorf("pi: start: %w", err)
		if sink != nil {
			sink(agent.AgentEvent{
				Kind:       agent.EventAgentError,
				Err:        wrapped,
				Diagnostic: piDiagnostic(agent.BridgeExitUnknown, ""),
			})
		}
		return agent.RunResult{}, wrapped
	}
	pid := child.Process.Pid

	piLog("PrintMode Start",
		"command", s.command, "workspace", cfg.Workspace,
		"prompt_bytes", len(prompt), "pid", pid)

	// Up-front Ready so the sink sees the lifecycle start.
	// SessionID/Model are filled in later by peekPrintMeta as
	// the wire frames arrive; the up-front Ready uses what
	// we know statically (workspace + branch).
	if sink != nil {
		branch := detectBranch(cfg.Workspace)
		sink(agent.AgentEvent{
			Kind:      agent.EventAgentReady,
			AgentName: s.Info().Name,
			Workspace: cfg.Workspace,
			Branch:    branch,
		})
	}

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
	// normally be EOF from a clean exit). Per-event AgentEvents
	// (Text / ToolStart / ToolEnd / Task* / Result / Done) are
	// forwarded to the sink if one is installed.
	result, translateErr := parsePrintStream(ctx, stdout, cfg.Workspace, sink)

	// Always wait for the process to exit so we can capture
	// both the exit code AND stderr. If parsePrintStream
	// errored early (e.g. agent_settled never fired) pi may
	// still be a useful signal via its stderr — model errors,
	// auth errors, etc. land there. The wait+reap path is
	// shared between success and failure so neither path loses
	// diagnostic info.
	waitErr := child.Wait()
	<-stderrDone

	piLog("PrintMode Exit",
		"pid", pid,
		"elapsed_ms", time.Since(startTime).Milliseconds(),
		"wait_err", errStr(waitErr),
		"stderr_bytes", stderrBuf.Len(),
		"stderr_truncated", stderrTruncated,
	)

	// Build the terminal event for the sink regardless of
	// outcome. If parsePrintStream already emitted a Result
	// (success path), we DON'T emit a duplicate — the
	// aggregator would count two terminals. We just emit
	// Done to close the lifecycle. On the failure path we
	// emit Error so the sink flips to an error state.
	if sink != nil {
		switch {
		case translateErr == nil && waitErr == nil:
			// Success — parsePrintStream already emitted
			// Result (text + usage) AND Done via the
			// translator's agent_settled handler. Nothing
			// more to emit.
		case translateErr != nil:
			stderr := strings.TrimSpace(stderrBuf.String())
			sink(agent.AgentEvent{
				Kind:       agent.EventAgentError,
				Err:        translateErr,
				Diagnostic: piDiagnostic(agent.BridgeExitUnknown, stderr),
			})
		default:
			// waitErr != nil, translateErr == nil: pi
			// produced output but exited non-zero (rare —
			// usually an auth/model error captured in
			// stderr). Surface as error so the sink
			// flips.
			stderr := strings.TrimSpace(stderrBuf.String())
			sink(agent.AgentEvent{
				Kind:       agent.EventAgentError,
				Err:        waitErr,
				Diagnostic: piDiagnostic(agent.ClassifyExit(waitErr, false), stderr),
			})
		}
	}

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

// parsePrintStream drives the JSON event reader from a
// print-mode pi spawn. It runs the existing translator
// (translate.go) over each line and watches for either an
// `agent_settled` event (turn-end marker carrying the final
// text) or a stream error. Returns RunResult on clean
// completion.
//
// sink (when non-nil) receives every AgentEvent the translator
// emits (Text / ToolStart / ToolEnd / TaskCreate / TaskUpdate /
// Result / Done) so per-call observers (e.g. /review's
// aggregator, /gtw's status emitter) see the same lifecycle
// the dsh / codex bridges emit. The sink is invoked
// synchronously from the read loop — bridges are responsible
// for ensuring it is non-blocking (see WithEventSink contract
// in agent.go). nil sink is fully supported (no-op on every
// emit), matching StreamRunOnceToEmitter's no-op contract.
//
// This is the print-mode analogue of the chat-session
// readPump + lifecycle pair, minus the RPC plumbing. The
// translator is reused as-is because the event format is
// shared between print and RPC modes.
func parsePrintStream(ctx context.Context, stdout io.Reader, workspace string, sink func(agent.AgentEvent)) (agent.RunResult, error) {
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
		// proc.New) when ctx is cancelled — we just stop
		// reading here and let runPrintMode's cmd.Wait() reap
		// the SIGKILLed process.
		if err := ctx.Err(); err != nil {
			return agent.RunResult{}, err
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		// F-PI-PRINT-002: surface the session id + assistant
		// model onto RunResult so the AgentBar footer in
		// channel/feishu renders "🤖: pi · <model> · <sid>".
		// Translator.translate() drops these via its default
		// case (session) or doesn't promote them out of the
		// message struct (message_*). Peeking here is cheap
		// (one JSON unmarshal per line) and only fires before
		// SessionID/Model are set, so the steady-state cost is
		// just one struct decode.
		peekPrintMeta(line, &result)

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
				if sink != nil {
					sink(ev)
				}
			case agent.EventAgentDone:
				sawSettled = true
				if sink != nil {
					sink(ev)
				}
			default:
				// Text / ToolStart / ToolEnd / TaskCreate /
				// TaskUpdate / Permission / etc. — forward
				// verbatim so the sink observer sees the
				// full per-turn lifecycle. No Result
				// capture here (handled above).
				if sink != nil {
					sink(ev)
				}
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
		// F-PI-PRINT-002: SessionID + Model are now surfaced
		// onto RunResult via peekPrintMeta above; the audit
		// suffix includes them when present so operators can
		// grep daemon logs by session / model across bridges.
		return agent.RunResult{}, fmt.Errorf("pi: exit without agent_settled%s", appendAuditFields(result, true))
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

// piDiagnostic is the BridgeDiagnostic payload attached to
// EventAgentError events emitted from the print-mode failure
// paths. Stderr tail gives the renderer the model/auth error
// message pi would otherwise leave in /dev/null — translate.go
// silently drops Err-only events without a Diagnostic, so the
// feishu error card would render with an empty body without
// this field. Mirrors codex/codexDiagnostic (same shape, just
// tags AgentName="pi" so cross-bridge log greps stay readable).
func piDiagnostic(exitKind agent.BridgeExitKind, stderr string) *agent.BridgeDiagnostic {
	return &agent.BridgeDiagnostic{
		ExitKind:   exitKind,
		StderrTail: stderr,
		AgentName:  "pi",
		KilledAt:   time.Now(),
	}
}

// appendAuditFields returns the audit-suffix string (empty when
// result has nothing to report) appended to error messages on
// the pi print-mode failure paths. Symmetric with claudecode's
// `[session_id=X] [usage in=N out=N cache_read=N]` formatting.
//
// F-PI-PRINT-002: SessionID is surfaced onto RunResult via
// peekPrintMeta (peeked from the {"type":"session","id":..}
// wire frame), so whenSessionID is true everywhere now. Model
// is also captured but FormatModel is reserved for bridges that
// surface it on the failure path only (acp); pi's failure-path
// audit mirrors the success-path shape (session_id + subtype +
// usage), so Model is folded into the standard chain via
// FormatSessionID instead of FormatModel.
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
	var b strings.Builder
	if whenSessionID {
		b.WriteString(agent.FormatSessionID(result.SessionID))
	}
	b.WriteString(agent.FormatSubtype(result.Subtype))
	b.WriteString(agent.FormatUsage(result.Usage))
	return b.String()
}
