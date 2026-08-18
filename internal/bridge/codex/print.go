// print.go — one-shot print mode for codex using `codex exec`.
//
// Why this exists (F-CODEX-PRINT-001, 2026-08-14):
//
// The codex bridge historically had ONE spawn recipe for RunOnce:
// the long-lived `codex app-server` path used by Starter.Start.
// RunOnce would Start + defer Close + drain Events() until
// EventAgentResult, then Close (which on codex paid a 5s
// closeDrainTimeout because the app-server has no "exit after
// one turn" protocol flag). That
// pattern works but has two costs the one-shot use case doesn't
// need:
//
//  1. 5s close latency. The app-server has no "exit after one
//     turn" protocol flag, so Close() waits up to closeDrainTimeout
//     for cmd.Wait. For /gtw commit and buildAgentPrompt this is
//     pure waste.
//
//  2. Stream churn. The JSON-RPC handshake (initialize +
//     initialized + thread/start) + translator + readPump +
//     stderrLoop + lifecycle goroutine all run for a single turn
//     that we never intend to follow up on. Per-turn overhead
//     shows up in /gtw commit timing logs.
//
// The pragmatic fix, mirroring claudecode (F-CLAUDE-PRINT-001,
// 2026-08-14) and pi (F-PI-PRINT-001, 2026-08-13):
//
//	codex exec --json -o <tmpfile> \
//	  --dangerously-bypass-approvals-and-sandbox \
//	  -C <workspace> \
//	  --skip-git-repo-check \
//	  [-i <image1> [-i <image2> ...]] \
//	  -- <prompt>
//
// Verified on codex-cli 0.145.0 (the binary present on the
// author's machine at the time of this commit):
//
//   - `-o <file>` writes ONLY the final agent_message to the file
//     (no tool calls, no progress, no user/codex markers). The
//     shared codex app-server's `eventBufferSize` (40960) and
//     readPump are entirely bypassed — `cmd.Wait` returns as
//     soon as the process exits.
//   - `--json` emits NDJSON events on stdout. `thread.started`
//     carries `thread_id`; `turn.completed.usage` carries token
//     counts. We consume those for RunResult.SessionID / Usage.
//   - `-i <path>` is repeatable and produces a working image
//     attachment (verified by feeding a 100×100 PNG and asking
//     the model to count pixels — it answered "100" correctly,
//     proving vision content was actually consumed; not just a
//     hallucinated answer based on the path string).
//   - `--dangerously-bypass-approvals-and-sandbox` is the
//     documented one-flag replacement for the app-server's
//     approval_policy="never" + sandbox_mode="danger-full-access"
//     pair (session.go:262-265).
//   - `--` separates flags from the positional prompt. Without
//     it, codex 0.145 sometimes misroutes the prompt to stdin
//     when `-i` is also present — reproducible test in commit
//     history.
//
// Why we use `-i <path>` instead of base64-in-prompt:
//
// `codex exec` exposes image input ONLY as `-i <file>` paths —
// no `--image-base64` / `--image-stdin` alternative exists in
// 0.145.0 (verified via `codex exec --help`). The CLI reads
// the file bytes internally and feeds them to the model as
// vision content. We pass paths; codex does the base64.
//
// Verified end-to-end with a disambiguation test (F-CODEX-PRINT-001
// follow-up): a 100×100 solid-color PNG via `-i <path>` and
// "how many pixels tall is the image?" → model answered "100"
// correctly. This is the only reliable proof — the model had to
// actually see the image to answer a numeric question. Token
// counts vary (17K-36K) for the same image across runs because
// codex CLI 0.145 has unstable vision-token accounting; do NOT
// use token count as a "was the image attached?" signal — only
// the model's content-aware answers are reliable.
//
// JSON stdin is NOT parsed as structured input (verified
// empirically — `codex exec - < json.json` treats stdin as a
// plain `<stdin>` text block; the `path` field in JSON is
// ignored and the model hallucinates).
//
// Contrast with claudecode/pi: both lack a CLI-level image
// flag in their print mode (claude -p / pi --mode json -p)
// and degrade ContentImage to "[image: <path>]"-style text
// annotations. codex exec is the strongest of the three
// bridges on multimodal fidelity.
//
// The app-server path stays for chat-session multi-turn use
// (Starter.Start). This file is the one-shot counterpart
// (Starter.RunOnce). docs/bridge/codex.md §1.2's "不双后端"
// line refers to the chat-session backend choice (app-server
// only, not exec); this print mode is a separate, additive path.
package codex

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
// tail and claudecode/pi's print.go. Without this cap a chatty
// failing child can OOM the bridge silently.
const stderrCapBytes = 64 * 1024

// runPrintMode spawns `codex exec` for one-shot invocations
// (/gtw commit, /gtw pr, buildAgentPrompt). Mirrors claudecode/pi
// print mode: bypass the long-lived bridge driver, spawn a fresh
// process, capture stdout (--json) for metadata, capture the
// final answer via -o <tmpfile>, reap on exit.
func runPrintMode(ctx context.Context, s *Starter, cfg agent.StartConfig, blocks []agent.ContentBlock) (agent.RunResult, error) {
	if cfg.Workspace == "" {
		return agent.RunResult{}, fmt.Errorf("codex: workspace is required")
	}

	startTime := time.Now()

	prefixArgs, prompt := buildPrintArgs(cfg, blocks)

	// Create the -o target tempfile early so we can clean up even
	// if spawn fails. codex exec writes ONLY the final agent
	// message here (verified on 0.145.0); tool-call progress and
	// "user / codex" markers go to stderr.
	tmpOut, err := os.CreateTemp("", "codex-print-*.txt")
	if err != nil {
		return agent.RunResult{}, fmt.Errorf("codex: create tempfile: %w", err)
	}
	tmpPath := tmpOut.Name()
	_ = tmpOut.Close()
	defer os.Remove(tmpPath)

	// Final argv layout: <prefix> -o <tmpPath> --json -- <prompt>.
	// Order matters: -o and --json go AFTER any -i flags (which
	// buildPrintArgs may have appended) but BEFORE the positional
	// prompt. The `--` separator is mandatory on codex 0.145 when
	// `-i` is present — without it the prompt is sometimes
	// misrouted to stdin.
	args := append([]string{}, prefixArgs...)
	args = append(args,
		"-o", tmpPath,
		"--json",
		"--",
		prompt,
	)

	cmd := agent.NewCmd(ctx, s.command, args...)
	cmd.Dir = cfg.Workspace // belt-and-braces with -C

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return agent.RunResult{}, fmt.Errorf("codex: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		return agent.RunResult{}, fmt.Errorf("codex: stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return agent.RunResult{}, fmt.Errorf("codex: start: %w", err)
	}
	pid := cmd.Process.Pid

	cLog("PrintMode Start",
		"command", s.command,
		"mode", "exec",
		"workspace", cfg.Workspace,
		"prompt_bytes", len(prompt),
		"args_count", len(args),
		"image_count", countImageFlags(prefixArgs),
		"pid", pid)

	// Drain stderr concurrently via the shared helper (matches
	// runCodexReviewPlain's stderr semantics so a future tweak to
	// the cap / cancellation applies to both surfaces at once).
	stderrDrain := startStderrDrain(ctx, stderr)

	// Read stdout NDJSON events + extract metadata. runNDJSON
	// runs concurrently with the stderr drain above; both
	// complete when the child closes its pipes (typically right
	// before exit).
	var sessionID string
	var model string
	var usage *agent.UsageInfo
	jsonReadErr := runNDJSON(ctx, stdout, func(ev codexExecEvent) {
		switch ev.Type {
		case "thread.started":
			if sessionID == "" && ev.ThreadID != "" {
				sessionID = ev.ThreadID
			}
		case "item.completed":
			// The first item.completed error event carries the
			// model name in its message (codex-cli 0.145+):
			//   "Model metadata for `MiniMax-M3` not found. ..."
			// We parse it as a best-effort signal so the AgentBar
			// footer can render "🤖: codex · <model>" instead of
			// just "🤖: codex".
			if model == "" && ev.Item != nil && ev.Item.Type == "error" {
				if m := extractModelFromError(ev.Item.Message); m != "" {
					model = m
				}
			}
		case "turn.completed":
			if ev.Usage != nil {
				usage = codexExecUsageToUsageInfo(ev.Usage)
			}
		}
	})

	waitErr := cmd.Wait()
	stderrDrain.wait()

	elapsedMs := time.Since(startTime).Milliseconds()

	cLog("PrintMode Exit",
		"pid", pid,
		"mode", "exec",
		"elapsed_ms", elapsedMs,
		"wait_err", errStr(waitErr),
		"stderr_bytes", len(stderrDrain.bytes()),
		"stderr_truncated", stderrDrain.truncatedFlag(),
		"session_id", sessionID)

	// Build the result. Subtype comes from exit code; Usage /
	// SessionID come from the NDJSON events when present.
	subtype := "completed"
	if waitErr != nil {
		subtype = "failed"
	}

	// Read the -o file (final message). Missing file means the
	// process died before writing — usually because exit was
	// non-zero and codex exec only writes on a successful turn.
	finalBytes, fileErr := os.ReadFile(tmpPath)
	if fileErr != nil && !errors.Is(fileErr, os.ErrNotExist) {
		return agent.RunResult{}, fmt.Errorf("codex: read -o file: %w", fileErr)
	}
	finalText := strings.TrimSpace(string(finalBytes))

	result := agent.RunResult{
		Text:       finalText,
		Usage:      usage,
		Model:      model,
		SessionID:  sessionID,
		DurationMs: elapsedMs,
		Subtype:    subtype,
	}

	// Shared error formatting with runCodexReviewPlain. waitErr is
	// surfaced first (jsonReadErr is usually just "broken pipe on
	// closed child" noise); stderr is trimmed here once.
	stderrStr := strings.TrimSpace(stderrDrain.bytes())
	if err := formatCodexExitError(waitErr, stderrStr, finalText, "answer"); err != nil {
		return agent.RunResult{}, err
	}
	// NDJSON-specific: a parse error on a clean exit is still a
	// failure (we couldn't extract session/model/usage).
	if jsonReadErr != nil && !errors.Is(jsonReadErr, io.EOF) {
		if stderrStr != "" {
			return agent.RunResult{}, fmt.Errorf("codex: stdout: %w (stderr: %s)", jsonReadErr, stderr)
		}
		return agent.RunResult{}, fmt.Errorf("codex: stdout: %w", jsonReadErr)
	}
	return result, nil
}

// buildPrintArgs assembles the fixed-prefix argv + the positional
// prompt from blocks. Extracted from runPrintMode so unit tests
// can assert on argv layout without spawning.
//
// Output layout:
//
//	[exec, --dangerously-bypass-approvals-and-sandbox,
//	 -C <workspace>, --skip-git-repo-check,
//	 -i <img1>, -i <img2>, ..., ]
//	prompt = joined text + "@<file>" refs (sentinel if empty)
//
// The caller is responsible for appending `-o`, `--json`, `--`,
// and the prompt at the end (those pieces depend on a runtime
// tempfile path / context state). The Starter is not used here
// because argv is fully determined by cfg + blocks; the cmd
// binary itself (s.command) is supplied by the caller via
// agent.NewCmd.
//
// No error return: encoding blocks into argv + prompt has no
// failure mode that isn't a caller bug (empty ContentImage.Path
// is silently dropped, mirroring the long-lived bridge's
// SendBlocks). If a future flag requires validation, add the
// error return then.
// buildPrintArgs assembles argv for `codex exec <prompt>`. Only
// used by RunOnce. The codex review subcommand lives in
// runCodexReviewPlain and assembles its own argv (the two
// subcommands take disjoint flag sets — see runCodexReview for
// why review doesn't reuse this function).
func buildPrintArgs(cfg agent.StartConfig, blocks []agent.ContentBlock) (args []string, prompt string) {
	args = []string{"exec"}

	// Mirror the app-server's two permission defaults
	// (session.go:262-265): never ask + full FS access. Verified
	// on codex 0.145.0 — equivalent combination flag. Avoids the
	// need to pass `-c approval_policy=... -c sandbox_mode=...`
	// separately.
	args = append(args,
		"--dangerously-bypass-approvals-and-sandbox",
		// Workspace. Both -C and cmd.Dir (set by runPrintMode)
		// for belt-and-braces.
		"-C", cfg.Workspace,
		// Skip the git-repo guard. /gtw commit may run from a
		// freshly-created worktree that isn't a git repo from
		// codex's perspective (e.g. sub-dir of main checkout).
		// App-server mode doesn't have this guard; exec does.
		"--skip-git-repo-check",
	)

	// Encode blocks preserving order. Each block contributes
	// exactly one entry to promptParts (the human-readable
	// prompt slice), and any image additionally contributes
	// a `-i <path>` argv flag for actual vision-token
	// attachment.
	//
	// Why we keep position markers for images even though
	// the model already sees the image via -i:
	//
	//   `codex exec` CLI has no structured input — stdin is
	//   appended as a `<stdin>` text block (verified 0.145.0),
	//   and the only image mechanism is `-i <file>` flags
	//   which carry no positional info. So if blocks are
	//   [text1, image1, text2], naive concatenation would lose
	//   the fact that image1 sits between text1 and text2.
	//
	//   Per F-CODEX-PRINT-001's "faithful forwarding" rule
	//   (token cost is not our concern), we add a one-line
	//   `[image]` placeholder at each image block's position
	//   in the prompt. The placeholder is deliberately
	//   minimal (no path, no @-syntax) to avoid triggering
	//   the model's view_image / read_image tool, which
	//   would either no-op in print mode or attempt a
	//   pointless file inspection. Verified empirically on
	//   0.145.0 that the placeholder does not cause the
	//   model to enter a tool-call loop.
	//
	// The `-i` flags carry the actual vision content; the
	// placeholder carries the position. Combined: model
	// sees the image AND knows where it sits in the user's
	// message.
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
			args = append(args, "-i", b.Path)
			promptParts = append(promptParts, "[image]")
		case agent.ContentFile:
			if b.Path == "" {
				continue
			}
			promptParts = append(promptParts, "@"+b.Path)
		default:
			cLog("PrintMode: unknown block type, skipping",
				"type", string(b.Type))
		}
	}
	prompt = strings.Join(promptParts, "\n")
	if prompt == "" {
		// All blocks were images / empty — codex exec still needs
		// SOMETHING as the positional arg, otherwise it falls
		// back to stdin (verified bug on codex 0.145.0).
		prompt = "(see attached content)"
	}
	return args, prompt
}

// countImageFlags returns how many `-i <path>` pairs the prefix
// already contains. Used only for log lines.
func countImageFlags(args []string) int {
	n := 0
	for i, a := range args {
		if a == "-i" && i+1 < len(args) {
			n++
		}
	}
	return n
}

// codexExecEvent is the subset of `codex exec --json` events we
// consume. The full schema is observed on codex 0.145.0; see
// print_real_unix_test.go for examples. We tolerate unknown
// event types / extra fields by ignoring them.
type codexExecEvent struct {
	Type     string          `json:"type"`
	ThreadID string          `json:"thread_id,omitempty"` // thread.started
	Item     *codexExecItem  `json:"item,omitempty"`     // item.completed
	Usage    *codexExecUsage `json:"usage,omitempty"`    // turn.completed
}

// codexExecItem is the `item` payload inside item.completed events.
// We only care about the error variant that carries the model name.
type codexExecItem struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Message string `json:"message,omitempty"` // error message
}

type codexExecUsage struct {
	InputTokens       int `json:"input_tokens"`
	CachedInputTokens int `json:"cached_input_tokens"`
	OutputTokens      int `json:"output_tokens"`
}

// codexExecUsageToUsageInfo maps the codex exec usage shape onto
// the agent-level UsageInfo. Field names differ slightly from
// appServerUsageToUsageInfo (internal/bridge/codex/translate.go:
// cached_input_tokens vs cachedInputTokens) because the JSON
// wire shape is slightly different between app-server and exec.
func codexExecUsageToUsageInfo(u *codexExecUsage) *agent.UsageInfo {
	if u == nil {
		return nil
	}
	return &agent.UsageInfo{
		InputTokens:          u.InputTokens,
		OutputTokens:         u.OutputTokens,
		CacheReadInputTokens: u.CachedInputTokens,
	}
}

// runNDJSON scans r line-by-line, parses each non-empty line as
// a codexExecEvent, and invokes cb for each. Tolerates malformed
// lines by logging + skipping (mirrors pumpStream's permissiveness
// in the long-lived bridge).
func runNDJSON(ctx context.Context, r io.Reader, cb func(codexExecEvent)) error {
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
		var ev codexExecEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			cLog("PrintMode: invalid NDJSON event",
				"err", err,
				"line", truncateForLog(string(line), 200))
			continue
		}
		cb(ev)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}

// extractModelFromError parses the model name from the
// `item.completed` error message that codex exec emits on every
// run. The canonical format (codex-cli 0.145+) is:
//
//	Model metadata for `MiniMax-M3` not found. Defaulting to...
//
// We extract the text between the first pair of backticks.
// Returns "" when the message doesn't match the expected shape.
func extractModelFromError(msg string) string {
	const prefix = "Model metadata for `"
	i := strings.Index(msg, prefix)
	if i < 0 {
		return ""
	}
	rest := msg[i+len(prefix):]
	j := strings.Index(rest, "`")
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// stderrDrain captures a subprocess's stderr to a capped buffer in
// a background goroutine. Shared between runPrintMode (exec) and
// runCodexReviewPlain (review) so both surfaces have IDENTICAL
// stderr capture semantics — the prior duplication (verified by
// the bug where `codex review` was routed through runPrintMode's
// NDJSON parser and its plain-text output was silently dropped)
// is what made the surfaces drift; extracting here means any
// future cap / cancellation tweak applies to both at once.
type stderrDrain struct {
	buf       *strings.Builder
	truncated bool
	done      chan struct{}
}

// startStderrDrain launches the capture goroutine. Caller MUST
// call wait() after cmd.Wait to ensure no bytes are lost before
// reading bytes().
func startStderrDrain(ctx context.Context, r io.Reader) *stderrDrain {
	d := &stderrDrain{
		buf:  &strings.Builder{},
		done: make(chan struct{}),
	}
	go func() {
		defer close(d.done)
		chunk := make([]byte, 4096)
		for {
			if ctx.Err() != nil {
				return
			}
			n, err := r.Read(chunk)
			if n > 0 {
				if d.buf.Len() < stderrCapBytes {
					room := stderrCapBytes - d.buf.Len()
					if n > room {
						d.buf.Write(chunk[:room])
						d.truncated = true
					} else {
						d.buf.Write(chunk[:n])
					}
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return d
}

func (d *stderrDrain) wait()             { <-d.done }
func (d *stderrDrain) bytes() string     { return d.buf.String() }
func (d *stderrDrain) truncatedFlag() bool { return d.truncated }

// formatCodexExitError returns the canonical "codex: exit: ..." /
// "codex: empty <label>" error for runPrintMode (exec) and
// runCodexReviewPlain (review). Shared so both surfaces report
// identical failure shape (same waitErr/stderr/finalText
// precedence rules). Returns nil when both waitErr is nil and
// finalText is non-empty (success path — caller builds the result
// directly).
//
// emptyLabel distinguishes the two paths in error messages:
// "answer" (exec) or "review answer" (review).
func formatCodexExitError(waitErr error, stderr, finalText, emptyLabel string) error {
	if waitErr != nil {
		if finalText != "" {
			if stderr != "" {
				return fmt.Errorf("codex: exit: %w (last answer: %q; stderr: %s)", waitErr, finalText, stderr)
			}
			return fmt.Errorf("codex: exit: %w (last answer: %q)", waitErr, finalText)
		}
		if stderr != "" {
			return fmt.Errorf("codex: exit: %w (stderr: %s)", waitErr, stderr)
		}
		return fmt.Errorf("codex: exit: %w", waitErr)
	}
	if finalText == "" {
		if stderr != "" {
			return fmt.Errorf("codex: empty %s (stderr: %s)", emptyLabel, stderr)
		}
		return fmt.Errorf("codex: empty %s", emptyLabel)
	}
	return nil
}

// truncateForLog shortens a line for inclusion in error / log
// messages. Caps at 200 bytes so a multi-MB garbage frame
// doesn't blow up the log line.
func truncateForLog(s string, cap int) string {
	if len(s) <= cap {
		return s
	}
	return s[:cap] + "..."
}

// errStr renders an error's string form, returning "<nil>" for
// the nil case so the log field is always meaningful. Mirrors
// claudecode/print.go's helper of the same name.
func errStr(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}
// runCodexReview runs `codex review --base <default>` against the
// workspace. F-review.md §13 "codex/claude use native review" rule:
// we invoke codex's built-in `review` subcommand instead of running
// our generic StandardPrompt via `codex exec`.
//
// --base <default> gives PR-mode review (current branch vs default
// branch). If the default branch can't be detected (no origin remote),
// we fall back to --uncommitted (working-tree scan only) and log a
// warning so the user knows the coverage is reduced.
//
// Output: stdout is the codex review text (plain text, NOT NDJSON —
// `codex review` is a non-interactive CLI tool; its --help lists no
// --json / -o flag). The bridge's Review method passes it through
// FormatReviewMessage for the canonical preamble.
//
// Plumbing: `runCodexReviewPlain` (this file) is the right shape for
// `review` — spawn with the review flags, read stdout to EOF, return.
// `runPrintMode` is the `exec` shape — spawn with `--json -o <tmp>
// runCodexReview assembles argv for `codex review` and spawns
// the subprocess. We do NOT reuse runPrintMode's plumbing
// because:
//   - `codex review` rejects every exec-only flag (`--json`,
//     `-o`, `--dangerously-bypass-…`, `--skip-git-repo-check`)
//     with exit 2 (verified on codex-cli 0.145.0).
//   - `codex review` outputs plain text on stdout (no NDJSON
//     events, no `-o` tempfile write). The shared stderr-drain
//     + exit-error formatting is the only thing the two paths
//     have in common (handled by stderrDrain + formatCodexExitError
//     in print.go).
//
// argv layout (verified on codex-cli 0.145.0):
//   `codex review
//      -c approval_policy=never
//      -c sandbox_mode=danger-full-access
//      --base <defaultBranch>          ← OR --uncommitted fallback
//      [-- <prompt>]                   ← review has no positional,
//                                          but `--` is harmless
//
// F-review.md §13 "codex/claude use native review" rule: invoking
// the native subcommand instead of our generic StandardPrompt.
func runCodexReview(ctx context.Context, s *Starter, cfg agent.StartConfig) (agent.RunResult, error) {
	// Build the review-specific extra flags. --base <default> is
	// the important one; we detect <default> via git commands.
	var extra []string
	if defaultBase := detectDefaultBranch(ctx, cfg.Workspace); defaultBase != "" {
		extra = []string{"--base", defaultBase}
	} else {
		cLog("codex review: no default branch detected, falling back to --uncommitted",
			"workspace", cfg.Workspace)
		extra = []string{"--uncommitted"}
	}
	return runCodexReviewPlain(ctx, s.command, cfg.Workspace, extra)
}

// runCodexReviewPlain is the review-specific runner: spawns
// `codex review` with the argv the caller assembled, drains
// stdout to EOF as plain text, returns. No NDJSON parser, no
// `-o` tempfile, no `--json` flag.
//
// Mirrors runPrintMode's stderr-drain + ctx-cancel + exit-error
// surfacing shape (via stderrDrain + formatCodexExitError) so the
// two surfaces report the same failure shape.
func runCodexReviewPlain(ctx context.Context, command, workspace string, reviewFlags []string) (agent.RunResult, error) {
	if workspace == "" {
		return agent.RunResult{}, fmt.Errorf("codex: workspace is required")
	}

	startTime := time.Now()

	// codex review argv — see runCodexReview doc.
	//
	// We DO NOT use buildPrintArgs here: the two subcommands have
	// disjoint flag sets (exec uses --dangerously-bypass-… / -C /
	// --skip-git-repo-check; review uses -c approval_policy=… /
	// -c sandbox_mode=…). Sharing argv-assembly would force one
	// or the other to express itself in the wrong dialect.
	//
	// `-c approval_policy=never` + `-c sandbox_mode=danger-full-access`
	// is the review-side equivalent of the exec flag set — both
	// land the same "never ask + full FS" posture so review
	// doesn't prompt the user for every file read on a multi-file
	// PR (fatal for a non-interactive subprocess whose stdin
	// is closed).
	args := append([]string{
		"review",
		"-c", "approval_policy=never",
		"-c", "sandbox_mode=danger-full-access",
	}, reviewFlags...)

	cmd := agent.NewCmd(ctx, command, args...)
	cmd.Dir = workspace // review has no -C; runs in cwd

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return agent.RunResult{}, fmt.Errorf("codex: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		return agent.RunResult{}, fmt.Errorf("codex: stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return agent.RunResult{}, fmt.Errorf("codex: start: %w", err)
	}
	pid := cmd.Process.Pid

	cLog("ReviewMode Start",
		"command", command,
		"mode", "review",
		"workspace", workspace,
		"args_count", len(args),
		"pid", pid)

	// Shared stderr capture with runPrintMode — same semantics,
	// same cap, same ctx-cancel behaviour. See stderrDrain doc.
	stderrDrain := startStderrDrain(ctx, stderr)

	// Drain stdout to EOF (synchronously). Review output is the
	// review text itself — unlike `codex exec`, there's no NDJSON
	// event stream + -o tempfile split. The whole stdout IS the
	// final answer.
	stdoutBuf := &strings.Builder{}
	{
		buf := make([]byte, 4096)
		for {
			if ctx.Err() != nil {
				break
			}
			n, rErr := stdout.Read(buf)
			if n > 0 {
				stdoutBuf.Write(buf[:n])
			}
			if rErr != nil {
				break
			}
		}
	}

	waitErr := cmd.Wait()
	stderrDrain.wait()

	elapsedMs := time.Since(startTime).Milliseconds()
	finalText := strings.TrimSpace(stdoutBuf.String())

	subtype := "completed"
	if waitErr != nil {
		subtype = "failed"
	}

	cLog("ReviewMode Exit",
		"pid", pid,
		"mode", "review",
		"elapsed_ms", elapsedMs,
		"wait_err", errStr(waitErr),
		"stderr_bytes", len(stderrDrain.bytes()),
		"stderr_truncated", stderrDrain.truncatedFlag(),
		"stdout_bytes", stdoutBuf.Len())

	result := agent.RunResult{
		Text:       finalText,
		DurationMs: elapsedMs,
		Subtype:    subtype,
	}

	// Shared error formatting with runPrintMode (see
	// formatCodexExitError doc). Identical waitErr/stderr/
	// finalText precedence rules so the two surfaces report the
	// same failure shape.
	stderrStr := strings.TrimSpace(stderrDrain.bytes())
	if err := formatCodexExitError(waitErr, stderrStr, finalText, "review answer"); err != nil {
		return agent.RunResult{}, err
	}
	return result, nil
}

// detectDefaultBranch finds the repo's default branch name
// (main / master / trunk). Returns "" if it can't be detected —
// the caller should fall back to --uncommitted.
func detectDefaultBranch(ctx context.Context, workspace string) string {
	// git symbolic-ref refs/remotes/origin/HEAD — most reliable on
	// cloned repos.
	cmd := agent.NewCmd(ctx, "git",
		"-C", workspace, "symbolic-ref", "refs/remotes/origin/HEAD")
	out, err := cmd.Output()
	if err == nil {
		ref := strings.TrimSpace(string(out))
		if strings.HasPrefix(ref, "refs/remotes/origin/") {
			return strings.TrimPrefix(ref, "refs/remotes/origin/")
		}
	}
	// git remote show origin — fallback for shallow clones.
	cmd = agent.NewCmd(ctx, "git", "-C", workspace, "remote", "show", "origin")
	out, err = cmd.Output()
	if err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "HEAD branch: ") {
				return strings.TrimPrefix(line, "HEAD branch: ")
			}
		}
	}
	// Final fallback: return "" so the caller falls back to
	// --uncommitted per the documented contract (F-review.md §13).
	// Returning a hard-coded "main" here would shadow the caller's
	// else-branch: codex review would then try --base main on
	// master-only / no-remote repos and fail instead of gracefully
	// scanning the working tree.
	return ""
}
