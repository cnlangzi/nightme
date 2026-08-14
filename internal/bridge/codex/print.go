// print.go — one-shot print mode for codex using `codex exec`.
//
// Why this exists (F-CODEX-PRINT-001, 2026-08-14):
//
// The codex bridge historically had ONE spawn recipe for RunOnce:
//
// The codex bridge historically had ONE spawn recipe for RunOnce:
// the long-lived `codex app-server` path used by Starter.Start.
// RunOnce would Start + defer Close + agent.RunOnceDrain. That
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
		"workspace", cfg.Workspace,
		"prompt_bytes", len(prompt),
		"args_count", len(args),
		"image_count", countImageFlags(prefixArgs),
		"pid", pid)

	// Drain stderr in the background (mirrors claudecode/pi).
	// Honors ctx cancellation between reads so a cancelled call
	// doesn't wait for the child to be SIGKILL'd by
	// exec.CommandContext before this goroutine returns —
	// matches runNDJSON's ctx-aware loop below.
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

	// Read stdout NDJSON events + extract metadata. runNDJSON
	// runs concurrently with the stderr drain above; both
	// complete when the child closes its pipes (typically right
	// before exit).
	var sessionID string
	var usage *agent.UsageInfo
	jsonReadErr := runNDJSON(ctx, stdout, func(ev codexExecEvent) {
		switch ev.Type {
		case "thread.started":
			if sessionID == "" && ev.ThreadID != "" {
				sessionID = ev.ThreadID
			}
		case "turn.completed":
			if ev.Usage != nil {
				usage = codexExecUsageToUsageInfo(ev.Usage)
			}
		}
	})

	waitErr := cmd.Wait()
	<-stderrDone

	elapsedMs := time.Since(startTime).Milliseconds()

	cLog("PrintMode Exit",
		"pid", pid,
		"elapsed_ms", elapsedMs,
		"wait_err", errStr(waitErr),
		"stderr_bytes", stderrBuf.Len(),
		"stderr_truncated", stderrTruncated,
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
		SessionID:  sessionID,
		DurationMs: elapsedMs,
		Subtype:    subtype,
	}

	// Failure paths: surface stderr (model / auth / sandbox errors
	// land there) plus the underlying wait / json-read error.
	// Order matters — prefer waitErr first because jsonReadErr is
	// usually just "broken pipe on closed child" noise.
	if waitErr != nil {
		stderr := strings.TrimSpace(stderrBuf.String())
		if finalText != "" {
			// Process died after writing the final answer
			// (rare but possible). Surface both the answer and
			// the failure so the caller can inspect.
			if stderr != "" {
				return agent.RunResult{}, fmt.Errorf("codex: exit: %w (last answer: %q; stderr: %s)", waitErr, finalText, stderr)
			}
			return agent.RunResult{}, fmt.Errorf("codex: exit: %w (last answer: %q)", waitErr, finalText)
		}
		if stderr != "" {
			return agent.RunResult{}, fmt.Errorf("codex: exit: %w (stderr: %s)", waitErr, stderr)
		}
		return agent.RunResult{}, fmt.Errorf("codex: exit: %w", waitErr)
	}
	if jsonReadErr != nil && !errors.Is(jsonReadErr, io.EOF) {
		stderr := strings.TrimSpace(stderrBuf.String())
		if stderr != "" {
			return agent.RunResult{}, fmt.Errorf("codex: stdout: %w (stderr: %s)", jsonReadErr, stderr)
		}
		return agent.RunResult{}, fmt.Errorf("codex: stdout: %w", jsonReadErr)
	}
	if finalText == "" {
		// Process exited 0 but produced no stdout AND no -o file
		// content. Unusual; treat as failure so /gtw commit
		// doesn't silently treat as success.
		stderr := strings.TrimSpace(stderrBuf.String())
		if stderr != "" {
			return agent.RunResult{}, fmt.Errorf("codex: empty answer (stderr: %s)", stderr)
		}
		return agent.RunResult{}, fmt.Errorf("codex: empty answer")
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
func buildPrintArgs(cfg agent.StartConfig, blocks []agent.ContentBlock) (args []string, prompt string) {
	args = []string{
		"exec",
		// Mirror the app-server's two permission defaults
		// (session.go:262-265): never ask + full FS access.
		// Verified on codex 0.145.0 — equivalent combination
		// flag. Avoids the need to pass `-c approval_policy=...
		// -c sandbox_mode=...` separately.
		"--dangerously-bypass-approvals-and-sandbox",
		// Workspace. Both -C and cmd.Dir (set by runPrintMode)
		// for belt-and-braces.
		"-C", cfg.Workspace,
		// Skip the git-repo guard. /gtw commit may run from a
		// freshly-created worktree that isn't a git repo from
		// codex's perspective (e.g. sub-dir of main checkout).
		// App-server mode doesn't have this guard; exec does.
		"--skip-git-repo-check",
	}

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
	Usage    *codexExecUsage `json:"usage,omitempty"`    // turn.completed
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