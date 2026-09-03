// print-mode spawn — the one-shot counterpart to the stream-json
// mode used by the long-lived Start path.
//
// Why this exists (F-CLAUDE-PRINT-001, 2026-08-14):
//
// The claudecode bridge historically had ONE spawn recipe for
// RunOnce: the same `--print --input-format stream-json --output-
// format stream-json --permission-mode bypassPermissions --verbose`
// argv used by Start (see permissions.go DefaultArgs). That recipe
// is designed for chat sessions — claude stays alive across many
// turns as long as the bridge holds stdin open. Reusing it for
// one-shot invocations (/gtw commit, buildAgentPrompt) means the
// bridge has to spawn a long-lived session, send one prompt via
// stream-json stdin, drain one result, then close the process.
//
// That shape carries the same operational risks pi hit before
// F-PI-PRINT-001 (2026-08-13): the long-lived pipe + stdin
// correlation + busy guard add failure modes that don't exist in
// the print-mode path, and the resume-preservation probe in
// Start's path is wasted work for one-shot.
//
// The pragmatic fix, mirroring pi's print-mode spawn:
//
//   claude -p "<prompt>" --output-format stream-json --verbose
//          --permission-mode <mode> [--bare] [--allowedTools ...]
//
// With `-p`, claude treats the prompt as a positional argument
// (no stdin reads), runs the turn, emits the result event on
// stdout, and exits. Process exit is the natural turn-end signal.
//
// Start (the stream-json held-stdin path) is unchanged: chat
// sessions still need the long-lived form across many turns.
// RunOnce and Start share the same Starter; only the spawn path
// differs. The shared stream.go translator is reused as-is
// because print-mode emits the same wire events as stream-json
// held-stdin mode.
//
// One-Shot argv notes:
//
//   -p <prompt>            — positional prompt (per docs: claude
//                            exits after the turn, no stdin).
//   --output-format stream-json
//                          — line-delimited JSON events.
//   --verbose              — required to enable stream-json output
//                            (per official docs).
//   --permission-mode      — bypassPermissions for now; future
//                            cfg.PermissionMode override.
//   --bare                 — NOT passed by default. RunOnce must
//                            behave like the chat session's Start
//                            path: load ~/.claude/CLAUDE.md, fire
//                            user-configured hooks (PreToolUse etc.),
//                            load .mcp.json MCP servers, and read
//                            OAuth / system-keychain credentials.
//                            Skipping any of these would make
//                            `/gtw commit` etc. diverge from the
//                            chat session's behavior, which users
//                            would find confusing. The startup-cost
//                            hit is acceptable for one-shot.
//                            Override with cfg.Bare if a future
//                            caller wants scripted-mode isolation.
//   --allowedTools         — comma-separated pre-approve list. We
//                            don't pass it today (bypassPermissions
//                            is already on); future cfg.AllowedTools
//                            override would slot in here.

package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/proc"
)

// claudeLog is a thin wrapper around slog.Default() that scopes
// log lines to the claudecode bridge with a stable "component"
// key. Mirrors pi/print_unix.go's piLog helper. Defaulting to
// the package-level slog keeps print-mode logs in the same
// MultiWriter sink as the rest of the bridge (see CLAUDE.md
// conventions memory: cmd/nightme/main.go installs the default
// logger once; callers should NOT plumb a logger through).
func claudeLog(msg string, args ...any) {
	all := make([]any, 0, len(args)+2)
	all = append(all, slog.String("component", "claudecode"))
	all = append(all, args...)
	slog.Default().Info(msg, all...)
}

// claudeDiagnostic builds the BridgeDiagnostic payload that the
// chat renderer needs to render an EventAgentError event with
// a non-empty title (translate.go silently drops events whose
// Diagnostic is nil — see codex/print.go:880-886 for the
// documented contract). Mirrors codex/print.go's codexDiagnostic
// helper; the difference is only the AgentName stamp.
//
// We use BridgeExitUnknown here because the review path's only
// failure modes are upstream of cmd.Wait (workspace empty, ctx
// cancelled, RunResult-carrying error from parsePrintStream).
// ClassifyExit is reserved for child-process exit classification
// (codex does that branch in runCodexReviewPlain's waitErr path).
func claudeDiagnostic(err error) *agent.BridgeDiagnostic {
	return &agent.BridgeDiagnostic{
		ExitKind:   agent.BridgeExitUnknown,
		WaitErr:    err,
		StderrTail: "",
		AgentName:  "claudecode",
		KilledAt:   time.Now(),
	}
}

// runPrintMode spawns claude in `-p` print mode for one-shot
// invocations. It owns the process from spawn to exit, streams
// events through the standard translator, and returns a
// RunResult carrying the agent's final text plus per-turn
// metadata (model, usage, duration, subtype) on a clean run. On any failure (spawn / non-zero exit / ctx cancel /
// translator error / missing result event) it returns a wrapped
// error.
//
// The stream.go translator is used unchanged — the event format
// from print-mode is identical to stream-json held-stdin mode's
// output. Only the input side differs: print-mode doesn't expect
// stream-json stdin (prompt is positional `-p`), so the
// translator's user-message code paths never fire here.
// stderrCapBytes bounds the stderr buffer kept in memory across a
// print-mode run. The child CLI can dump megabytes of error context
// to stderr on certain API failures (e.g. multi-MB API error
// bodies). Without this cap a chatty failure can OOM the bridge
// silently — caller sees "command failed" with no clue that the
// real cause was memory exhaustion. 64 KiB is generous for any
// human-readable diagnostic and matches the long-lived bridge's
// stderr tail (stderrTailBytes).
const stderrCapBytes = 64 * 1024

// buildPrintArgs assembles the argv for a one-shot `claude -p`
// call. The prompt portion is delegated to agent.BlocksToPrompt
// (shared by claudecode / pi / dsh; the print-mode bridges whose
// -p flag accepts a single positional string). Returns (args,
// prompt) so callers can log prompt_bytes without re-extracting
// it from argv — mirrors codex/buildPrintArgs + opencode/buildPrintArgs
// signatures so all four print-mode bridges expose the same surface.
func buildPrintArgs(blocks []agent.ContentBlock) (args []string, prompt string) {
	prompt = agent.BlocksToPrompt(blocks)
	args = []string{
		"-p", prompt,
		"--output-format", "stream-json",
		"--verbose",
		"--permission-mode", "bypassPermissions",
	}
	return args, prompt
}

func runPrintMode(ctx context.Context, s *Starter, cfg agent.StartConfig, blocks []agent.ContentBlock, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	if cfg.Workspace == "" {
		return agent.RunResult{}, fmt.Errorf("claudecode: workspace is required")
	}

	startTime := time.Now()

	// Build argv from blocks. -p takes the prompt as a single
	// argv entry; quoting is handled by exec, not by us.
	//
	// --bare is intentionally NOT passed: RunOnce must mirror the
	// chat session's environment (CLAUDE.md / hooks / MCP /
	// OAuth). See the package doc above.
	//
	// isReview=false: buildAgentPrompt / /gtw commit / any
	// non-/code-review caller must not get the assistant-text
	// recovery layer (see parsePrintStream for why).
	args, prompt := buildPrintArgs(blocks)

	// Forward the per-call event sink so /gtw commit, /gtw pr,
	// and any other RunOnce caller sees intermediate progress
	// in the chat channel. Without this, a long-running
	// RunOnce (e.g. a slow Bash tool_use) would render as
	// silent "Working…" until the terminal result lands.
	// See runCodeReviewPrintMode for the matching pattern
	// on the /review path.
	sink := agent.ParseRunOnceOptions(opts).OnEvent
	return runPrintModeWithPrompt(ctx, s, cfg, args, prompt, startTime, false, sink)
}

// runPrintModeWithPrompt is the shared implementation: buildPrintArgs
// (or override) has already produced the argv + prompt; this
// function handles the subprocess plumbing (start, drain stderr,
// translate stdout events, capture result). The `startTime`
// parameter is passed in so both callers get a consistent timer
// baseline. The `sink` parameter, when non-nil, receives
// intermediate assistant / tool events for chat-channel progress
// (see parsePrintStream for the exact event mapping); pass nil
// from unit tests that only care about the RunResult.
func runPrintModeWithPrompt(
	ctx context.Context,
	s *Starter,
	cfg agent.StartConfig,
	args []string,
	prompt string,
	startTime time.Time,
	isReview bool,
	sink func(agent.AgentEvent),
) (agent.RunResult, error) {
	child := proc.New(ctx, s.command, args...)
	child.Dir = cfg.Workspace
	// Forward cfg.Env the same way Start does (append to os.Environ,
	// cfg wins on conflict). Without this, /gtw commit-time env
	// overrides (custom API keys, MCP credentials) are silently
	// dropped on the print-mode path.
	if len(cfg.Env) > 0 {
		child.Env = append(os.Environ(), cfg.Env...)
	}

	stdout, err := child.StdoutPipe()
	if err != nil {
		return agent.RunResult{}, fmt.Errorf("claudecode: stdout pipe: %w", err)
	}
	stderr, err := child.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		return agent.RunResult{}, fmt.Errorf("claudecode: stderr pipe: %w", err)
	}

	if err := child.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return agent.RunResult{}, fmt.Errorf("claudecode: start: %w", err)
	}
	pid := child.Process.Pid

	claudeLog("PrintMode Start",
		"command", s.command, "workspace", cfg.Workspace,
		"prompt_bytes", len(prompt), "pid", pid)

	// Drain stderr in the background. Print-mode stderr is
	// mostly empty for a clean run; any non-empty output
	// indicates a setup or model error worth surfacing on
	// non-zero exit (see below). Cap the buffer at
	// stderrCapBytes so a chatty failing child can't OOM the
	// bridge — when we hit the cap, truncate and stop
	// appending (the tail beyond the cap is silently dropped;
	// the log line below records the truncation so operators
	// can see it happened).
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
	// normally be EOF from a clean exit). The sink parameter
	// (when non-nil) receives every intermediate assistant text
	// and tool_use/tool_result event so the chat channel's
	// StatusBar / receipt shows the plugin's progress instead
	// of sitting on "Working…" for the full run.
	result, translateErr := parsePrintStream(ctx, stdout, isReview, sink)

	// Always wait for the process to exit so we can capture
	// both the exit code AND stderr. If parsePrintStream
	// errored early (e.g. result event never fired) claude may
	// still be a useful signal via its stderr — model errors,
	// auth errors, etc. land there. The wait+reap path is
	// shared between success and failure so neither path loses
	// diagnostic info.
	waitErr := child.Wait()
	<-stderrDone

	claudeLog("PrintMode Exit",
		"pid", pid,
		"elapsed_ms", time.Since(startTime).Milliseconds(),
		"wait_err", errStr(waitErr),
		"stderr_bytes", stderrBuf.Len(),
		"stderr_truncated", stderrTruncated,
	)

	if translateErr != nil {
		// Stream reader hit a non-EOF error, OR the JSON
		// stream ended without a result event. Either way
		// the prompt did not complete normally; surface
		// stderr if claude left a hint about why.
		stderr := strings.TrimSpace(stderrBuf.String())
		if stderr != "" {
			return agent.RunResult{}, fmt.Errorf("claudecode: %w (stderr: %s)", translateErr, stderr)
		}
		return agent.RunResult{}, translateErr
	}

	if waitErr != nil {
		// Surface stderr if any — most failures land here
		// (auth errors, model errors, etc.) with a short
		// human-readable message in stderr.
		stderr := strings.TrimSpace(stderrBuf.String())
		if stderr != "" {
			return agent.RunResult{}, fmt.Errorf("claudecode: exit: %w (stderr: %s)", waitErr, stderr)
		}
		return agent.RunResult{}, fmt.Errorf("claudecode: exit: %w", waitErr)
	}

	return result, nil
}

// parsePrintStream drives the JSON event reader from a
// print-mode claude spawn. It decodes each line into a
// streamEvent and watches for the terminal `result` event
// (which carries the final response text, is_error flag, and
// usage info) and the leading `system/init` event (which
// carries the model). Returns RunResult on clean completion.
//
// This is the print-mode analogue of the chat-session
// readPump + pumpStream pair, minus the stdin plumbing. The
// sink parameter, when non-nil, receives intermediate
// AgentEvents (Text / ToolStart / ToolEnd) for every
// assistant / user-role wire event. The chat channel's
// StatusBar / receipt renders those via gateway.Translate
// and the policy gate, so the user sees the plugin's
// progress during long print-mode runs (the v10 fix; pre-v10
// dropped opts at the dispatcher boundary and the chat sat
// on "Working…" for the full review duration).
//
// Sink MUST be non-blocking on the bridge's side; pass nil
// when no sink is wired (e.g. unit tests that just want the
// RunResult).
//
// Error surfacing contract:
//   - result.is_error=true → wrap as error with subtype + text
//   - result event missing  → "exit without result event" error
//   - reader / process errors → wrapped exit error
//
// Usage info is captured onto RunResult.Usage when present;
// the structured log line below carries the same payload for
// operators chasing per-turn costs.
func parsePrintStream(ctx context.Context, stdout io.Reader, isReview bool, sink func(agent.AgentEvent)) (agent.RunResult, error) {
	scanner := bufio.NewScanner(stdout)
	// Allow long lines (Claude Code may emit large content blocks).
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	var (
		result     agent.RunResult
		isError    bool
		sawInit    bool
		sawResult  bool
		// largestAssistant is only populated when isReview=true;
		// the off-mode path skips both tracking and swap, so
		// result.RecoveredText stays empty for non-review runs.
		largestAssistant string
		// toolUseArgs correlates assistant `tool_use` blocks with
		// their matching user-role `tool_result` blocks so the
		// emitted ToolEnd.Args can carry the same input the
		// ToolStart advertised. stream.go's chat-session path
		// does the same via state.toolUseArgs (line 387); print
		// mode is one-shot so we keep the map local to this
		// function instead of plumbing a state struct through.
		toolUseArgs = map[string]string{}
	)

	for scanner.Scan() {
		// Honour ctx cancellation between lines so we exit
		// promptly when the caller's deadline fires. The
		// process is killed by proc.New (which wraps
		// exec.CommandContext) when ctx is cancelled — we
		// just stop reading here and let runPrintMode's
		// cmd.Wait() reap the SIGKILLed process.
		if err := ctx.Err(); err != nil {
			return agent.RunResult{}, err
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var ev streamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			// Mirror pumpStream's permissiveness: malformed
			// lines are logged + skipped, never abort the run.
			claudeLog("PrintMode: invalid json event",
				"err", err,
				"line", truncateForLog(string(line), 200))
			continue
		}

		// Single hoist of ev.Message != nil: the assistant and
		// user branches both gate on it, and re-checking per
		// branch would mask that "message present" is a
		// wire-level invariant for those event types (v12.1
		// cleanup; pre-v12.1 had two redundant checks).
		switch ev.Type {
		case "system":
			// system/init carries the session id + model.
			// Capture once (first init wins) so the result
			// can attribute the turn to the right session.
			if ev.Subtype == "init" && !sawInit {
				sawInit = true
				result.SessionID = ev.SessionID
				result.Model = ev.Model
			}
		case "assistant":
			if ev.Message == nil {
				continue
			}
			for _, block := range ev.Message.Content {
				// Track the largest text block across all assistant
				// messages ONLY when this is a review-mode spawn.
				// The plugin's review content typically lives in
				// one large block (the multi-agent pipeline's final
				// composed review); smaller blocks are usually
				// progress pings like "Reviewer 3 finished".
				if isReview && block.Type == "text" && len(block.Text) > len(largestAssistant) {
					largestAssistant = block.Text
				}
				// Forward intermediate progress to the sink so the
				// chat channel's StatusBar / receipt shows the
				// plugin's activity. Both text blocks (model's
				// streamed reasoning) and tool_use blocks (Skill,
				// Read, Bash, …) are emitted; the downstream
				// policy gate decides what's visible. Reviews'
				// multi-agent pipeline emits many such events
				// across an 8-minute run — without this the chat
				// sat on "Working…" with no feedback (F-print-
				// forward fix).
				if sink == nil {
					continue
				}
				switch block.Type {
				case "text":
					if block.Text != "" {
						sink(agent.AgentEvent{
							Kind:      agent.EventAgentText,
							Text:      block.Text,
							SessionID: result.SessionID,
							Model:     result.Model,
						})
					}
				case "tool_use":
					toolUseArgs[block.ID] = string(block.Input)
					sink(agent.AgentEvent{
						Kind: agent.EventAgentToolStart,
						ToolStart: &agent.AgentToolStartEvent{
							ID:   block.ID,
							Name: block.Name,
							Args: string(block.Input),
						},
						SessionID: result.SessionID,
						Model:     result.Model,
					})
				}
			}
		case "user":
			if ev.Message == nil {
				continue
			}
			for _, block := range ev.Message.Content {
				if block.Type != "tool_result" {
					continue
				}
				if sink == nil {
					continue
				}
				// tool_result blocks ride in user-role messages.
				// Pair them with the matching tool_use ID so the
				// sink can drive ToolStart / ToolEnd pairing in
				// the chat renderer's rolling log. Args comes from
				// the toolUseArgs map keyed by the result's
				// tool_use_id; output goes through stringifyToolResult
				// so the chat renderer sees "review finished"
				// instead of the raw 16-char JSON string
				// `"review finished"` with literal quotes.
				sink(agent.AgentEvent{
					Kind: agent.EventAgentToolEnd,
					ToolEnd: &agent.AgentToolEndEvent{
						ID:     block.ToolUseID,
						Name:   "", // filled below if we correlated
						Args:   toolUseArgs[block.ToolUseID],
						Output: stringifyToolResult(block.Content),
					},
					Err: func() error {
						if block.IsError {
							return fmt.Errorf("tool reported error")
						}
						return nil
					}(),
					SessionID: result.SessionID,
					Model:     result.Model,
				})
			}
		case "result":
			sawResult = true
			result.Text = ev.Result
			result.DurationMs = ev.DurationMs
			result.Subtype = ev.Subtype
			isError = ev.IsError

			// "有则拿,没有则忽略": capture usage if present, log
			// it for operators chasing per-turn costs, surface
			// it through RunResult so callers can audit
			// cost / tokens without re-querying the bridge.
			result.Usage = decodeUsage(ev.Usage, ev.ModelUsage)
			if result.Usage != nil {
				claudeLog("PrintMode: usage",
					"input_tokens", result.Usage.InputTokens,
					"output_tokens", result.Usage.OutputTokens,
					"cache_read", result.Usage.CacheReadInputTokens,
					"is_error", isError,
					"subtype", result.Subtype)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return agent.RunResult{}, fmt.Errorf("claudecode: stdout: %w", err)
	}

	if !sawResult {
		// Process exited cleanly but no terminal `result`
		// event ever arrived — unusual for claude but possible
		// if the model exited early or the JSON stream was
		// truncated. Treat as failure so the caller knows
		// the prompt didn't complete.
		return agent.RunResult{}, fmt.Errorf("claudecode: exit without result event")
	}

	if isError {
		// claude reported the run as a failure. Surface it as
		// an error so /gtw commit etc. don't treat the run as
		// successful. The error message carries the subtype
		// (e.g. "error_max_turns") and the result text — the
		// latter is claude's own description of what failed.
		//
		// We also append session_id + usage tokens so a
		// failed turn is still auditable: operators can
		// resume the session via `claude --resume <sid>`,
		// and the runtime can surface "your last commit
		// spent 1234 input tokens before failing" instead
		// of silently underreporting cost on the failure
		// path.
		msg := strings.TrimSpace(result.Text)
		if result.Subtype != "" {
			msg = result.Subtype + ": " + msg
		}
		if msg == "" {
			msg = "claude reported is_error=true without further detail"
		}
		return agent.RunResult{}, fmt.Errorf("claudecode: result.is_error: %s%s", msg, appendAuditFields(result))
	}

	result.Text = strings.TrimSpace(result.Text)

	// Review-mode recovery layer. Runs AFTER the isError branch
	// above so error paths never have their Text overwritten
	// by an assistant block — and only when isReview was set,
	// keeping RecoveredText off the wire for every other
	// claudecode print-mode call.
	if isReview && largestAssistant != "" {
		result.RecoveredText = strings.TrimSpace(largestAssistant)
		// /code-review plugin's follow-up question ("Want me
		// to apply the suggested patch?", "Anything to
		// change?", ...) shows up as the terminal result
		// event's Text in `-p` mode, with the actual review
		// sitting in the assistant stream. Swap Text to
		// RecoveredText when result.Text looks like a plugin
		// closing remark AND we have a meaningfully larger
		// assistant block to fall back to.
		if isFollowupQuestion(result.Text) && len(result.RecoveredText) > len(result.Text)*2 {
			claudeLog("PrintMode: recovered review from assistant stream",
				"result_chars", len(result.Text),
				"recovered_chars", len(result.RecoveredText))
			result.Text = result.RecoveredText
		}
	}

	return result, nil
}

// errStr renders an error's string form, returning "<nil>" for
// the nil case so the log field is always meaningful. Matches
// pi/print_unix.go's helper of the same name.
func errStr(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// appendAuditFields returns the audit-suffix string (empty when
// result has nothing to report) appended to error messages on
// the claudecode is_error path. Symmetric with pi's helper of
// the same name in print.go — keeps the cross-bridge error
// format grep-friendly.
//
// The session_id field is included when non-empty (claudecode
// captures it from system/init). Subtype is omitted on this
// path because the is_error branch has already folded Subtype
// into the leading `subtype: text` portion of the error message
// (a re-emit would be redundant).
//
// Usage is included when non-nil; the "in/out/cache_read"
// labels match the wire-format field names so operators
// grepping daemon logs can correlate.
func appendAuditFields(result agent.RunResult) string {
	return agent.FormatSessionID(result.SessionID) + agent.FormatUsage(result.Usage)
}

// isFollowupQuestion reports whether `text` looks like a
// /code-review plugin closing remark rather than a substantive
// review. Positive-match only — returns true ONLY when text
// matches one of the known plugin output shapes. A short
// non-review result that doesn't match any phrase (e.g.
// "✅ Looks clean.", "No issues found.", "All good.")
// returns false, so the recovery layer doesn't accidentally
// overwrite a clean outcome with a progress ping from the
// assistant stream.
//
// The /code-review plugin (allowed-tools=gh-only) produces
// three short closing texts in `-p` mode:
//
//  1. gh succeeded: "Posted review to PR #42." / "Comment
//     added to PR #42." — captured by phrase list.
//  2. gh failed, plugin asks the user: "Want me to apply
//     the suggested patch?" / "Should I proceed?" /
//     "Anything else?" — captured by phrase list.
//  3. gh failed, plugin signals nothing further: a short
//     status line. These are not matches today; if the
//     plugin adds new ones, append to the phrase list AND
//     log via the recovery branch in parsePrintStream.
//
// In cases (1) and (2) the review content lives in an
// earlier `assistant` text block, NOT the terminal result
// event, so the dispatcher would otherwise see the closing
// remark instead of the review. parsePrintStream uses this
// heuristic (gated by RecoveredText being substantially
// larger) to promote the review from the assistant stream.
//
// The short-text gate (`reviewLikeLength`) is the primary
// filter: real reviews are typically >1 KiB. A genuine
// question that happens to appear inside a long review is
// filtered out by the length check before the phrase scan
// even runs.
func isFollowupQuestion(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return false
	}
	// reviewLikeLength: anything longer than ~600 chars is very
	// unlikely to be a plugin closing remark (those are <200
	// chars in every observed case). The length gate also
	// protects against an inline question inside a long review
	// accidentally matching a phrase below.
	const reviewLikeLength = 600
	if len(t) > reviewLikeLength {
		return false
	}
	lower := strings.ToLower(t)
	phrases := []string{
		"want me to",
		"should i ",
		"would you like",
		"anything else",
		"anything to change",
		"apply the",
		"posted review",
		"posted a comment",
		"comment added",
		"comment posted",
	}
	for _, p := range phrases {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// runCodeReviewPrintMode runs `claude -p code-review [<branch>]`
// against the workspace and forwards the per-call event sink (if
// any) to the chat channel's StatusBar / receipt. The full
// rationale for why this passes the local branch name (and not
// a ref-range like `<defaultBase>...HEAD`) is in the function
// body below.
//
// F-review.md §13 "codex/claude use native review" rule: we
// invoke Claude Code's built-in slash command instead of running
// our generic builtinPrompt via `claude -p "<prompt>"`. The chat
// agent already has a multi-agent review pipeline tuned for this
// task; reusing it is strictly better than reverse-engineering
// the same prompt into a generic prompt-mode call.
//
// IMPORTANT — the positional `[command]` slot in
// `claude [options] [command] [prompt]` does NOT take a leading slash.
// Verified on Claude Code 2.1.220:
//
//   - `claude -p /code-review …` → claude treats `/code-review` as a
//     regular prompt, runs zero turns, returns empty `result`
//     (the slash command is never invoked).
//   - `claude -p code-review …` → claude dispatches to the
//     `code-review` slash command; the multi-agent pipeline fires
//     (observed 36+ turns reading repo files, writing findings, etc.).
//
// The pre-v9 code passed `"/code-review"` (with slash) which made every
// /review run on claude return silently empty — exactly the "review 跑
// claude 没结果" symptom. Strip the slash before passing (see args
// below).
//
// Output: the standard claude stream-json transcript. The shared
// print-stream parser extracts the final text into RunResult.Text
// (same path as runPrintMode, so output handling is identical).
func runCodeReviewPrintMode(ctx context.Context, s *Starter, cfg agent.StartConfig, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	if cfg.Workspace == "" {
		return agent.RunResult{}, fmt.Errorf("claudecode: workspace is required")
	}

	// Per-call sink (opts.OnEvent): when present, the chat
	// channel's StatusBar / receipt header flips from "agent X"
	// placeholder to "agent X · …" within the first few seconds
	// of the long review run. Without this forward, /review on
	// claude renders 30s of silence and then dumps the final
	// text — see codex/print.go's runCodexReviewPlain for the
	// matching pattern (same Ready/Result/Error envelope).
	sink := agent.ParseRunOnceOptions(opts).OnEvent

	// /code-review takes an optional positional [target] argument.
	// Per nightme's /review policy we pass the LOCAL branch name
	// (e.g. "fix-review-on-claude"), NOT a ref-range like
	// `<defaultBase>...HEAD` and NOT a PR number — those two
	// forms are what made the v1 implementation fall into
	// `gh pr list` → AskUserQuestion → "Which PR would you like
	// to review?" instead of reviewing the local diff.
	//
	// PR association is deliberately ignored: the user's common
	// case is "I have local commits not pushed yet, review
	// them against the default branch". The branch name is what
	// anchors the plugin to the right working tree; the
	// default-branch comparison happens internally.
	//
	// On detached HEAD / non-git dirs / git error, CurrentBranch
	// returns "" and we fall through to bare `code-review` with
	// no positional — same shape as the no-default-branch path.
	//
	// Pass the command WITHOUT the leading slash — see doc above.
	branch := agent.CurrentBranch(ctx, cfg.Workspace)

	// Up-front Ready so the chat channel's StatusBar / receipt
	// header can flip from "agent X" placeholder to "agent X · …"
	// before the long review run starts. SessionID/Model are
	// empty here; the stream-json parser RE-emits Result with
	// them once known, so a consumer that snapshots on first
	// Ready and ignores later ones still sees this one. Matches
	// codex/print.go::runPrintMode's Ready pattern. Branch is
	// stamped here so the StatusBar's "⎇ <branch>" footer
	// segment renders (v12.1 fix; pre-v12 the field was empty
	// because we computed branch only for the argv positional).
	if sink != nil {
		sink(agent.AgentEvent{
			Kind:      agent.EventAgentReady,
			AgentName: s.Info().Name,
			Workspace: cfg.Workspace,
			Branch:    branch,
		})
	}

	args := []string{
		"-p", "code-review",
	}
	if branch != "" {
		args = append(args, branch)
	}
	args = append(args,
		"--output-format", "stream-json",
		"--verbose",
		"--permission-mode", "bypassPermissions",
	)

	startTime := time.Now()
	result, err := runPrintModeWithPrompt(ctx, s, cfg, args, "code-review", startTime, true, sink)

	// Terminal event: pair the up-front Ready with a Result or
	// Error so the sink observes a complete lifecycle (same
	// contract as codex's runCodexReviewPlain success / error
	// branches). Without this, the StatusBar sits on "Working…"
	// forever even after the review text has landed in the
	// channel via the dispatcher's emitter path.
	//
	// Error events MUST carry a populated Diagnostic; outbound.
	// translate.go:200 silently drops EventAgentError events
	// whose Diagnostic is nil, so the chat receipt card would
	// stay at 🔄 on /review failure (v12.1 fix; pre-v12 the
	// silent drop was harmless because no sink was wired).
	if sink != nil {
		if err != nil {
			sink(agent.AgentEvent{
				Kind:       agent.EventAgentError,
				Err:        err,
				AgentName:  s.Info().Name,
				Workspace:  cfg.Workspace,
				Branch:     branch,
				Diagnostic: claudeDiagnostic(err),
			})
		} else {
			sink(agent.AgentEvent{
				Kind:      agent.EventAgentResult,
				Result:    &agent.AgentResultEvent{Text: result.Text, DurationMs: result.DurationMs, Subtype: result.Subtype, Usage: result.Usage},
				SessionID: result.SessionID,
				Model:     result.Model,
				AgentName: s.Info().Name,
				Workspace: cfg.Workspace,
				Branch:    branch,
			})
		}
	}
	return result, err
}

