// print.go — one-shot print mode for cursor using `agent -p`.
//
// Cursor CLI's `--output-format` accepts `text | json |
// stream-json`; we use `json` so the per-turn usage block is
// captured into RunResult.Usage and the footer can render input /
// output / cache tokens. cursor-agent 2026.08.11+ emits the full
// envelope on stdout:
//
//	{
//	  "type":"result",
//	  "subtype":"success" | "error_max_turns" | "refusal" | ...,
//	  "is_error":false,
//	  "result":"<final assistant text>",
//	  "session_id":"<uuid>",
//	  "usage":{"inputTokens":N,"outputTokens":N,
//	           "cacheReadTokens":N,"cacheWriteTokens":N}
//	}
//
// We capture stdout and stderr separately — a stderr trailer
// (update notice, MCP log) must not contaminate the JSON parse
// and replace the assistant answer with `{...}` plus noise.
//
// Parse failure is a hard error. cursor's `--output-format json`
// has been the supported path since 2026.08.11 (verified
// locally against 2026.10.01-e373342) and any binary running
// through this bridge necessarily carries the full Cursor
// parent flags (`--force --trust --sandbox --approve-mcps`) —
// there is no "older cursor without json" / "third-party
// binary that ignores --output-format" surface to fall back
// to. If the envelope shape changes, the bridge fails loudly
// instead of silently dropping the Usage field.
//
// `is_error: true` envelopes (cursor's "error_max_turns" /
// "refusal" / future error subtypes) are returned as errors so
// `/gtw commit` and friends don't treat the failed turn as a
// successful body. The error message still carries SessionID +
// Usage as audit fields (operators can resume the session and
// see how many tokens the failure spent), matching the
// claudecode print-mode is_error convention
// (internal/bridge/claudecode/print.go:505).
//
// FullAccessArgs are prepended so print-mode matches the ACP
// session's "act without prompting" default. This mirrors the
// codex/claudecode/pi/opencode print-mode pattern: bypass the
// long-lived ACP bridge driver, spawn a fresh process, capture
// stdout, reap on exit.
package cursor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/proc"
)

// cursorResultEnvelope mirrors the single-line JSON that
// `cursor-agent --print --output-format json` emits on stdout
// (cursor-agent 2026.08.11+; verified locally against
// 2026.10.01-e373342). The struct lives here (not in a shared
// protocol package) because the JSON shape is private to
// cursor's print-mode wire — cursor's ACP wire (separate
// `agent acp` subcommand) does NOT emit this envelope, so this
// struct has no callers outside this file.
//
// Field set covers what the channel footer / RunResult audit
// fields consume:
//   - Result  → RunResult.Text
//   - Subtype → RunResult.Subtype (e.g. "success" / "error_max_turns")
//   - SessionID → RunResult.SessionID
//   - Usage  → RunResult.Usage (inputTokens / outputTokens /
//     cacheReadTokens / cacheWriteTokens)
//   - IsError → controls the success-vs-error branch in
//     runPrintMode. When true, even a populated Result.Text is
//     surfaced as an error so /gtw etc. don't commit / publish
//     cursor's failure prose as a successful body.
//
// Other envelope fields (`request_id`, `duration_ms`,
// `duration_api_ms`) are dropped from the struct on purpose —
// encoding/json ignores unknown keys by default, and we have no
// consumer for them. Wall-clock duration is already measured in
// runPrintMode from the spawn wrapper.
type cursorResultEnvelope struct {
	Type      string       `json:"type"`
	Subtype   string       `json:"subtype"`
	IsError   bool         `json:"is_error"`
	Result    string       `json:"result"`
	SessionID string       `json:"session_id"`
	Usage     *cursorUsage `json:"usage,omitempty"`
}

// cursorUsage is the per-turn token usage cursor-agent prints
// under the result envelope. cursor-agent does NOT include
// `costUSD` on the wire (cursor's pricing is flat per-plan, not
// per-token surfaced); we therefore only forward the token
// counts and leave CostUSD at the zero value. Channels that
// render the cost line will omit the cost segment in this case,
// matching the documented "CostUSD == 0 means unknown / not
// reported" contract in agent.UsageInfo.
type cursorUsage struct {
	InputTokens      int `json:"inputTokens"`
	OutputTokens     int `json:"outputTokens"`
	CacheReadTokens  int `json:"cacheReadTokens"`
	CacheWriteTokens int `json:"cacheWriteTokens"`
}

// cursorUsageToAgentInfo maps cursor's print-mode usage shape
// onto agent.UsageInfo. cursor's "cacheWriteTokens" corresponds
// to the canonical "cacheCreationInputTokens" (the input tokens
// that wrote to the prompt cache this turn). cursor's
// "cacheReadTokens" matches "cacheReadInputTokens" verbatim.
//
// All-zero usage is treated as "no usage reported" and returns
// nil, mirroring the "nil = not reported" semantics the channel
// footer relies on (a synthetic message or a turn where the
// bridge legitimately observed no token data must not render a
// misleading "0 tokens" line). This also matches the
// `appendAuditFields_NilUsageDoesNotPanic` family of tests
// across other bridges.
func cursorUsageToAgentInfo(u *cursorUsage) *agent.UsageInfo {
	if u == nil {
		return nil
	}
	if u.InputTokens|u.OutputTokens|u.CacheReadTokens|u.CacheWriteTokens == 0 {
		return nil
	}
	return &agent.UsageInfo{
		InputTokens:              u.InputTokens,
		OutputTokens:             u.OutputTokens,
		CacheReadInputTokens:     u.CacheReadTokens,
		CacheCreationInputTokens: u.CacheWriteTokens,
	}
}

// runPrintMode spawns `cursor-agent` + FullAccessArgs +
// `-p "prompt" --output-format json` for one-shot invocations
// (/gtw commit, buildAgentPrompt).
//
// stdout and stderr are captured into separate buffers. stdout
// is parsed strictly as the cursorResultEnvelope above; any
// parse failure is returned as a hard error (no text fallback
// — see file doc for why). stderr is only consulted on the
// spawn-error path so update notices / MCP log lines cannot
// break the JSON parse.
//
// opts (when set via WithEventSink) wires a per-call observer
// that receives an up-front Ready + a terminal Result/Done
// (or Error on failure). The Result event carries Usage so
// channels that fold it into the receipt footer (line 2 of
// `📊 in N · out N · …`) light up for cursor one-shot calls
// the same way they already do for codex / claudecode / pi /
// opencode print-modes.
//
// Failure modes:
//   - cmd.Run() != nil          → spawn / model / CLI failure; surface stderr
//   - parseCursorOutput != nil  → malformed envelope (return as hard error)
//   - env.IsError               → wrap as error with audit fields
//   - env.Result empty          → model produced nothing; surface stderr
func runPrintMode(ctx context.Context, s *Starter, cfg agent.StartConfig, blocks []agent.ContentBlock, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	if cfg.Workspace == "" {
		return agent.RunResult{}, fmt.Errorf("cursor: workspace is required")
	}

	sink := agent.ParseRunOnceOptions(opts).OnEvent

	prompt := extractText(blocks)
	if prompt == "" {
		return agent.RunResult{}, fmt.Errorf("cursor: empty prompt")
	}

	startTime := time.Now()

	args := printModeArgs(prompt, cfg.Args)

	cmd := proc.New(ctx, s.command, args...)
	cmd.Dir = cfg.Workspace

	// Forward cfg.Env the same way Start does (append to
	// os.Environ, cfg wins on conflict). Without this, /gtw
	// commit-time env overrides (custom API keys, MCP
	// credentials) are silently dropped on the print-mode path.
	if len(cfg.Env) > 0 {
		cmd.Env = append(os.Environ(), cfg.Env...)
	}

	// Separate stdout / stderr buffers — keep them apart so a
	// stderr trailer (cursor's update notices, MCP log lines)
	// cannot break the JSON parse and replace the assistant
	// reply with a JSON blob plus noise. CombinedOutput() would
	// have concatenated stderr onto stdout for us; we instead
	// wire the buffers explicitly and run cmd.Run() ourselves.
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Up-front Ready so the per-call sink sees the lifecycle
	// start. cursor's wire carries no session / model / branch
	// info, so only the static metadata is populated.
	if sink != nil {
		sink(agent.AgentEvent{
			Kind:      agent.EventAgentReady,
			AgentName: s.Info().Name,
			Workspace: cfg.Workspace,
		})
	}

	err := cmd.Run()

	elapsedMs := time.Since(startTime).Milliseconds()

	cLog("PrintMode Exit",
		"workspace", cfg.Workspace,
		"elapsed_ms", elapsedMs,
		"stdout_bytes", stdout.Len(),
		"stderr_bytes", stderr.Len(),
		"err", errStr(err))

	if err != nil {
		stderrText := strings.TrimSpace(stderr.String())
		wrapped := error(err)
		if stderrText != "" {
			wrapped = fmt.Errorf("cursor run: %w (stderr: %s)", err, stderrText)
		} else {
			wrapped = fmt.Errorf("cursor run: %w", err)
		}
		// Finding 1 from /review: emit EventAgentError to the
		// sink before returning so the aggregator's doneCount
		// reaches expected (otherwise multi-job /review hangs
		// forever because the chat lifecycle never closes).
		if sink != nil {
			sink(agent.AgentEvent{
				Kind:       agent.EventAgentError,
				Err:        wrapped,
				Diagnostic: cursorDiagnostic(agent.ClassifyExit(err, false), stderrText),
			})
		}
		return agent.RunResult{}, wrapped
	}

	env, isEnvelope, parseErr := parseCursorEnvelope(stdout.Bytes())
	if parseErr != nil {
		wrapped := fmt.Errorf("cursor: parse output: %w (stdout: %s)", parseErr, strings.TrimSpace(stdout.String()))
		if sink != nil {
			sink(agent.AgentEvent{
				Kind:       agent.EventAgentError,
				Err:        wrapped,
				Diagnostic: cursorDiagnostic(agent.BridgeExitCleanExit, ""),
			})
		}
		return agent.RunResult{}, wrapped
	}
	_ = isEnvelope // always true on the parse-success path; kept for symmetry with future error-shape envelopes

	text, subtype, sessionID, usage, isError := env.Result, env.Subtype, env.SessionID, cursorUsageToAgentInfo(env.Usage), env.IsError

	if isError {
		// cursor reported the run as a failure. Surface it as
		// an error so /gtw commit etc. don't treat the run as
		// successful — matches the claudecode print-mode
		// is_error convention (internal/bridge/claudecode/
		// print.go:505). The error message carries the subtype
		// (e.g. "error_max_turns") and the result text — the
		// latter is cursor's own description of what failed.
		//
		// The is_error branch runs BEFORE the empty-answer
		// guard below so an is_error envelope with no result
		// text is still surfaced as a typed failure ("cursor
		// reported is_error=true without further detail"),
		// not as a generic "empty answer" (which would imply
		// the model didn't respond at all).
		//
		// We append session_id + usage tokens so a failed
		// turn is still auditable: operators can resume via
		// `agent --resume <sid>`, and the runtime can surface
		// "your last /gtw call spent N tokens before failing"
		// instead of silently underreporting cost on the
		// failure path.
		resultForAudit := agent.RunResult{SessionID: sessionID, Usage: usage}
		msg := strings.TrimSpace(text)
		if subtype != "" {
			msg = subtype + ": " + msg
		}
		if msg == "" {
			msg = "cursor reported is_error=true without further detail"
		}
		wrapped := fmt.Errorf("cursor: result.is_error: %s%s", msg, appendCursorAuditFields(resultForAudit))
		if sink != nil {
			sink(agent.AgentEvent{
				Kind:       agent.EventAgentError,
				Err:        wrapped,
				Diagnostic: cursorDiagnostic(agent.BridgeExitCleanExit, ""),
			})
		}
		return agent.RunResult{}, wrapped
	}

	if text == "" {
		wrapped := fmt.Errorf("cursor: empty answer")
		if sink != nil {
			sink(agent.AgentEvent{
				Kind:       agent.EventAgentError,
				Err:        wrapped,
				Diagnostic: cursorDiagnostic(agent.BridgeExitCleanExit, ""),
			})
		}
		return agent.RunResult{}, wrapped
	}

	result := agent.RunResult{
		Text:       text,
		DurationMs: elapsedMs,
		Subtype:    subtype,
		SessionID:  sessionID,
		Usage:      usage,
	}
	if result.Subtype == "" {
		// Defensive: cursor's documented envelope always
		// carries a subtype, but a future schema change that
		// omits it would otherwise leave Subtype as "". Keep
		// the legacy "completed" semantics so callers that
		// branch on Subtype don't regress on an unusual
		// envelope.
		result.Subtype = "completed"
	}

	// Success path: emit terminal Result + Done to the sink so
	// the chat channel's outer lifecycle closes (Finding 4 from
	// /review). Usage is populated on the Result event so the
	// channel footer renders input / output / cache tokens for
	// cursor one-shot calls. Same minimal-lifecycle shape as
	// opencode / codex / pi — cursor's print-mode is a single
	// terminal emit, not a stream.
	if sink != nil {
		sink(agent.AgentEvent{
			Kind: agent.EventAgentResult,
			Result: &agent.AgentResultEvent{
				Text:       result.Text,
				DurationMs: result.DurationMs,
				Subtype:    result.Subtype,
				Usage:      result.Usage,
			},
		})
		sink(agent.AgentEvent{
			Kind: agent.EventAgentDone,
			Done: &agent.AgentDoneEvent{ExitCode: 0, Reason: "settled"},
		})
	}

	return result, nil
}

// parseCursorEnvelope parses the captured stdout blob strictly
// as cursor's print-mode JSON envelope. Returns the populated
// envelope on success; an empty envelope + a non-nil error
// otherwise. The error wraps the underlying json.Unmarshal
// error so operators can see what went wrong (cursor schema
// drift, binary not honouring --output-format json, etc.)
// rather than getting a silent "no Usage" footer.
//
// The `isEnvelope bool` return is reserved for future cursor
// envelopes that don't share the `result` shape (e.g. a future
// `error` envelope); for now every parseable envelope we
// expect is a `type:"result"`.
func parseCursorEnvelope(raw []byte) (env cursorResultEnvelope, isEnvelope bool, err error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return cursorResultEnvelope{}, false, fmt.Errorf("empty stdout")
	}
	if err := json.Unmarshal([]byte(trimmed), &env); err != nil {
		return cursorResultEnvelope{}, false, err
	}
	if env.Type != "result" {
		return cursorResultEnvelope{}, false, fmt.Errorf("unexpected envelope type %q (want \"result\")", env.Type)
	}
	return env, true, nil
}

// appendCursorAuditFields mirrors claudecode/print.go:559
// `appendAuditFields` — renders SessionID + Usage as a string
// we can append to the error message on the is_error path so
// the audit data reaches daemon logs even though we return
// RunResult{} (zero value). Nil-safe on both fields.
func appendCursorAuditFields(result agent.RunResult) string {
	return agent.FormatSessionID(result.SessionID) + agent.FormatUsage(result.Usage)
}

// printModeArgs is the argv for `cursor-agent -p`.
// `--output-format json` makes cursor emit the result envelope
// (subtype / usage / session_id / result) so runPrintMode can
// populate the audit fields. Full-access parent flags precede
// `-p` for the same reason they precede `acp` on the
// chat-session path.
func printModeArgs(prompt string, extra []string) []string {
	args := withFullAccess("-p", prompt, "--output-format", "json")
	return append(args, extra...)
}

// cursorDiagnostic is the BridgeDiagnostic payload attached to
// EventAgentError events emitted from the cursor print-mode
// failure paths. Mirrors codex/codexDiagnostic + pi/piDiagnostic
// (same shape, AgentName="cursor"). Without this, Err-only
// events are silently dropped by the upstream translate →
// chat renderer pair.
func cursorDiagnostic(exitKind agent.BridgeExitKind, stderr string) *agent.BridgeDiagnostic {
	return &agent.BridgeDiagnostic{
		ExitKind:   exitKind,
		StderrTail: stderr,
		AgentName:  "cursor",
		KilledAt:   time.Now(),
	}
}

// extractText concatenates all ContentText blocks into a single prompt.
func extractText(blocks []agent.ContentBlock) string {
	var parts []string
	for _, b := range blocks {
		if b.Type == agent.ContentText && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// errStr renders an error's string form, returning "<nil>" for
// the nil case so the log field is always meaningful. Mirrors
// the opencode / codex print.go helper of the same name.
func errStr(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}
