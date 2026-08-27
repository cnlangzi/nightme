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

func runPrintMode(ctx context.Context, s *Starter, cfg agent.StartConfig, blocks []agent.ContentBlock) (agent.RunResult, error) {
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
	args, prompt := buildPrintArgs(blocks)
	return runPrintModeWithPrompt(ctx, s, cfg, args, prompt, startTime)
}

// runPrintModeWithPrompt is the shared implementation: buildPrintArgs
// (or override) has already produced the argv + prompt; this
// function handles the subprocess plumbing (start, drain stderr,
// translate stdout events, capture result). The `startTime`
// parameter is passed in so both callers get a consistent timer
// baseline.
func runPrintModeWithPrompt(
	ctx context.Context,
	s *Starter,
	cfg agent.StartConfig,
	args []string,
	prompt string,
	startTime time.Time,
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
	// normally be EOF from a clean exit).
	result, translateErr := parsePrintStream(ctx, stdout)

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
// readPump + pumpStream pair, minus the stdin plumbing. We
// intentionally bypass stream.go::translate here because
// print-mode RunOnce only needs the terminal result event —
// system/init, assistant text, and tool events are not
// rendered by RunOnce callers. Skipping translate avoids
// carrying events through a channel + drain goroutine just
// to discard them.
//
// Error surfacing contract:
//   - result.is_error=true → wrap as error with subtype + text
//   - result event missing  → "exit without result event" error
//   - reader / process errors → wrapped exit error
//
// Usage info is captured onto RunResult.Usage when present;
// the structured log line below carries the same payload for
// operators chasing per-turn costs.
func parsePrintStream(ctx context.Context, stdout io.Reader) (agent.RunResult, error) {
	scanner := bufio.NewScanner(stdout)
	// Allow long lines (Claude Code may emit large content blocks).
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	var (
		result     agent.RunResult
		isError    bool
		sawInit    bool
		sawResult  bool
		// largestAssistant accumulates the longest text block
		// across all assistant messages observed this turn.
		// /code-review's plugin prints the multi-agent review
		// findings in one large assistant message, then closes
		// the turn with a follow-up "Want me to apply…?" — in
		// `-p` mode that question becomes the terminal result
		// event's Text, hiding the actual review. Tracking the
		// largest block here lets us recover the review below.
		largestAssistant string
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
			// Track the largest text block across all assistant
			// messages. The plugin's review content typically
			// lives in one large block (the multi-agent pipeline's
			// final composed review); smaller blocks are usually
			// progress pings like "Reviewer 3 finished".
			if ev.Message != nil {
				for _, block := range ev.Message.Content {
					if block.Type == "text" && len(block.Text) > len(largestAssistant) {
						largestAssistant = block.Text
					}
				}
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

	// Publish the largest assistant text alongside Text so the
	// dispatcher (and any future caller / log inspector) can
	// see both pieces independently. Trimmed for symmetry with
	// the trailing TrimSpace applied to Text below.
	result.AssistantText = strings.TrimSpace(largestAssistant)

	// /code-review plugin's follow-up question ("Want me to
	// apply the suggested patch?", "Anything to change?", …)
	// shows up as the terminal result event's Text in `-p`
	// mode, with the actual review sitting in AssistantText.
	// Swap Text → AssistantText when result.Text looks like a
	// follow-up AND we have a meaningful assistant review to
	// fall back to. Other bridges leave AssistantText empty
	// and the swap is a no-op.
	if isFollowupQuestion(result.Text) && len(result.AssistantText) > len(result.Text)*2 {
		result.Text = result.AssistantText
		claudeLog("PrintMode: recovered review from assistant stream",
			"result_chars", len(result.Text),
			"followup_chars", len(result.AssistantText))
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

// longestText returns the longest string in the slice, with ties
// broken by first occurrence. Empty when the slice is empty.
// Used by the /code-review recovery path to pick the largest
// assistant text block (review) over the trailing follow-up.
func longestText(texts []string) string {
	var best string
	for _, t := range texts {
		if len(t) > len(best) {
			best = t
		}
	}
	return best
}

// isFollowupQuestion reports whether `text` looks like a
// plugin-generated closing remark rather than a substantive
// review.
//
// The /code-review plugin (allowed-tools=gh-only) tries step 8
// `gh pr comment` first; in `-p` mode that path produces one
// of three short closing texts depending on outcome:
//
//   1. gh succeeded: "Posted review to PR #42." / "Comment
//      added to PR #42."
//   2. gh failed, plugin asks the user: "Want me to apply
//      the suggested patch?" / "Should I proceed?" /
//      "Anything else?"
//   3. gh failed, plugin signals nothing further: a short
//      status line.
//
// In every case the review content lives in an earlier
// `assistant` text block, NOT the terminal result event, so we
// want to swap Text → AssistantText. A real review, by
// contrast, always contains `## ` markdown headings
// (`## Summary`, `## Findings`, `## Recommendations`) — the
///// structural marker we use to distinguish the two.
//
// The short-text gate (`reviewLikeLength`) is the primary
// filter: real reviews are typically >1 KiB. The heading gate
// (`## `) is the secondary filter for the rare edge case of a
// short text that's NOT a closing remark (e.g. an actually-
// brief review of a one-line fix). The phrase gate is a
/// tertiary check that catches closing remarks the heading gate
// would miss because they happen to contain `## ` somewhere
// (extremely rare but observed in plugin updates).
func isFollowupQuestion(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return false
	}
	// reviewLikeLength: anything longer than ~600 chars is very
	// unlikely to be a plugin follow-up (those are <200 chars
	// in every observed case). Combined with the structural
	// gates below, this filters out genuine questions that
	// happen to appear inside a long review.
	const reviewLikeLength = 600
	if len(t) > reviewLikeLength {
		return false
	}
	// Real reviews from /code-review always contain `## `
	// markdown headings. Closing remarks never do. Negative-
	// structured: absence-of-heading is a strong follow-up
	// signal.
	if !strings.Contains(t, "## ") {
		return true
	}
	// Belt-and-braces: even when the text contains `## `, catch
	// plugin-specific phrases that the heading check would
	// otherwise miss. New phrases added by future Claude Code
	// /code-review plugin versions should be appended here AND
	// logged via the recovery branch in parsePrintStream so the
	// heuristic stays current.
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

// runCodeReviewPrintMode runs `claude -p "/code-review"` against
// the workspace. This is the bridge's native review path
// (F-review.md §13 "codex/claude use native review" rule): we
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
func runCodeReviewPrintMode(ctx context.Context, s *Starter, cfg agent.StartConfig) (agent.RunResult, error) {
	if cfg.Workspace == "" {
		return agent.RunResult{}, fmt.Errorf("claudecode: workspace is required")
	}

	// /code-review takes an optional positional [target] argument
	// (file path / PR # / branch / ref range). The plugin's step-1
	// "is this a PR?" check is hard-coded to use `gh pr view`; in
	// `-p` mode with no real PR on the branch, that path falls back
	// to AskUserQuestion ("Which one do you want to review?") — which
	// becomes the terminal result event's Text and hides the actual
	// review (see parsePrintStream's isFollowupQuestion recovery).
	//
	// Passing `<defaultBranch>...HEAD` as the positional gives the
	// plugin an explicit ref-range target so it skips the
	// "which target?" step entirely. This is the ref-range syntax
	// documented in `claude code-review --help`'s source-of-truth
	// and confirmed empirically on claude-code 2.1.220.
	//
	// When `detectDefaultBranch` fails (no origin remote, shallow
	// clone, etc.) we fall back to bare `code-review` with no target
	// — same as pre-fix v1. The print-stream parser's follow-up
	// recovery handles the no-PR AskUserQuestion gracefully either
	// way; this positional is a *quality* improvement, not a hard
	// requirement.
	//
	// Pass the command WITHOUT the leading slash — see doc above.
	defaultBase := detectDefaultBranch(ctx, cfg.Workspace)
	args := []string{
		"-p", "code-review",
	}
	if defaultBase != "" {
		args = append(args, defaultBase+"...HEAD")
	}
	args = append(args,
		"--output-format", "stream-json",
		"--verbose",
		"--permission-mode", "bypassPermissions",
	)

	startTime := time.Now()
	return runPrintModeWithPrompt(ctx, s, cfg, args, "code-review", startTime)
}

// detectDefaultBranch finds the repo's default branch name
// (main / master / trunk). Returns "" if it can't be detected —
// caller falls back to `code-review` with no positional target.
//
// Mirrors internal/bridge/codex/print.go::detectDefaultBranch
// (kept duplicated rather than promoted to a shared package so
// each bridge can evolve its detection independently — the
// codex copy is wired for `codex review --base` while this
// one feeds `code-review <base>...HEAD`). Three-tier fallback:
//
//  1. `git symbolic-ref refs/remotes/origin/HEAD` (most reliable
//     on cloned repos).
//  2. `git remote show origin` (shallow-clone fallback).
//  3. Return "" so the caller skips the positional.
func detectDefaultBranch(ctx context.Context, workspace string) string {
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := proc.New(c, "git",
		"-C", workspace, "symbolic-ref", "refs/remotes/origin/HEAD").Output()
	if err == nil {
		ref := strings.TrimSpace(string(out))
		if strings.HasPrefix(ref, "refs/remotes/origin/") {
			return strings.TrimPrefix(ref, "refs/remotes/origin/")
		}
	}
	c2, cancel2 := context.WithTimeout(ctx, 5*time.Second)
	defer cancel2()
	out, err = proc.New(c2, "git",
		"-C", workspace, "remote", "show", "origin").Output()
	if err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "HEAD branch: ") {
				return strings.TrimPrefix(line, "HEAD branch: ")
			}
		}
	}
	return ""
}
