// print.go — one-shot print mode for cursor using `agent -p`.
//
// Cursor CLI's print-mode supports three output formats (`text`,
// `json`, `stream-json`); we use `json` so the per-turn usage block
// is captured into RunResult.Usage and the footer can render
// input / output / cache tokens. cursor-agent 2026.10.01+ emits
// the full envelope:
//
//	{
//	  "type":"result",
//	  "subtype":"success" | "error_max_turns" | "refusal" | ...,
//	  "is_error":false,
//	  "duration_ms":50046,
//	  "duration_api_ms":50046,
//	  "result":"<final assistant text>",
//	  "session_id":"<uuid>",
//	  "request_id":"<uuid>",
//	  "usage":{"inputTokens":N,"outputTokens":N,
//	           "cacheReadTokens":N,"cacheWriteTokens":N}
//	}
//
// Older / unexpected CLIs that ignore `--output-format json` will
// emit plain text on stdout; we tolerate that and fall back to
// treating the whole blob as Text (Usage left nil — same shape
// as the pre-fix behaviour for those rare cases).
//
// FullAccessArgs are prepended so print-mode matches the ACP
// session's "act without prompting" default. This mirrors the
// codex/claudecode/pi/opencode print-mode pattern: bypass the
// long-lived ACP bridge driver, spawn a fresh process, capture
// stdout, reap on exit.
package cursor

import (
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
// (cursor-agent 2026.10.01+). The struct lives here (not in a
// shared protocol package) because the JSON shape is private
// to cursor's print-mode wire — cursor's ACP wire (separate
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
//   - DurationMs / DurationAPIMs: not propagated — wall-clock is
//     already measured from the spawn wrapper and the API time
//     is redundant (matches what the JSON envelope itself shows).
//
// Other fields (IsError, RequestID) are reserved for future use
// (e.g. error-side audit) and not currently surfaced.
type cursorResultEnvelope struct {
	Type          string       `json:"type"`
	Subtype       string       `json:"subtype"`
	IsError       bool         `json:"is_error"`
	Result        string       `json:"result"`
	SessionID     string       `json:"session_id"`
	RequestID     string       `json:"request_id"`
	DurationMs    int64        `json:"duration_ms"`
	DurationAPIMs int64        `json:"duration_api_ms"`
	Usage         *cursorUsage `json:"usage,omitempty"`
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
// stdout is captured into a single blob and parsed as the
// cursorResultEnvelope above. On successful parse,
// RunResult.{Text, Subtype, SessionID, Usage} are populated
// from the envelope; on parse failure (older cursor, non-cursor
// binary, corrupted output), the trimmed blob becomes Text and
// the other audit fields stay empty — same surface as the
// pre-fix text-only path.
//
// opts (when set via WithEventSink) wires a per-call observer
// that receives an up-front Ready + a terminal Result/Done
// (or Error on failure). The Result event now carries Usage so
// channels that fold it into the receipt footer (line 2 of
// `📊 in N · out N · …`) light up for cursor one-shot calls
// the same way they already do for codex / claudecode / pi /
// opencode print-modes.
//
// Failure modes:
//   - waitErr != nil  → model / CLI failure; surface stderr
//   - empty finalText → model produced nothing; surface stderr
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

	output, err := cmd.CombinedOutput()

	elapsedMs := time.Since(startTime).Milliseconds()

	cLog("PrintMode Exit",
		"workspace", cfg.Workspace,
		"elapsed_ms", elapsedMs,
		"output_bytes", len(output),
		"err", errStr(err))

	if err != nil {
		stderr := strings.TrimSpace(string(output))
		wrapped := error(err)
		if stderr != "" {
			wrapped = fmt.Errorf("cursor run: %w (stderr: %s)", err, stderr)
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
				Diagnostic: cursorDiagnostic(agent.ClassifyExit(err, false), stderr),
			})
		}
		return agent.RunResult{}, wrapped
	}

	text, subtype, sessionID, usage := parseCursorOutput(output)

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
		// Fallback for older cursor / non-cursor binaries whose
		// stdout wasn't a JSON envelope — keep the pre-fix
		// "completed" semantics so callers that branch on
		// Subtype don't regress.
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

// parseCursorOutput splits a captured stdout blob into the
// fields RunResult cares about. The happy path is the JSON
// envelope; anything that doesn't parse (older cursor versions
// that emit plain text, third-party binaries that ignore
// --output-format, etc.) falls through to treating the trimmed
// blob as Text with no audit fields — matching the pre-fix
// behaviour for those rare cases.
//
// Returns (text, subtype, sessionID, usage). Callers should
// still check `text == ""` for the empty-answer guard, since
// both the JSON envelope's `result` field and a raw text body
// can be empty.
func parseCursorOutput(raw []byte) (text, subtype, sessionID string, usage *agent.UsageInfo) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return "", "", "", nil
	}

	// Fast path: cursor's print-mode JSON envelope. The
	// envelope's "type" field is "result" so we can disambiguate
	// from any future JSON shape (e.g. cursor adding a JSON
	// `error` envelope) without invasive branching.
	var env cursorResultEnvelope
	if err := json.Unmarshal([]byte(trimmed), &env); err == nil && env.Type == "result" {
		cLog("PrintMode ParseJSON",
			"subtype", env.Subtype,
			"is_error", env.IsError,
			"usage_present", env.Usage != nil,
			"session_id", env.SessionID)
		return env.Result, env.Subtype, env.SessionID, cursorUsageToAgentInfo(env.Usage)
	}

	// Fallback: treat the trimmed blob as plain text. We
	// deliberately do NOT attempt partial JSON extraction
	// (e.g. a malformed object that happens to have a "result"
	// field) — that path is reserved for a future fix if
	// cursor's wire shape changes mid-line. Today the wire is
	// either a clean envelope or plain text; nothing in
	// between.
	cLog("PrintMode ParseFallback", "reason", "json unmarshal failed or type!=result")
	return trimmed, "", "", nil
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
