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
	"github.com/cnlangzi/nightme/internal/proc"
)

// stderrCapBytes bounds the stderr buffer kept in memory across a
// print-mode run. 64 KiB matches the long-lived bridge's stderr
// tail and claudecode/pi's print.go. Without this cap a chatty
// failing child can OOM the bridge silently.
const stderrCapBytes = 64 * 1024

// codexSinkContext returns the three static fields that the
// codex bridge stamps on its up-front Ready event and the
// thread.started-driven Ready re-emit (AgentName / Workspace /
// Branch). SessionID is NOT here — it's populated separately
// when the NDJSON thread.started arrives (callers pass the
// fresh thread_id directly to the Ready emit). Splitting
// SessionID out of this helper avoids the "up-front Ready
// without thread.started yet" race where we don't know the
// thread_id at helper-call time.
//
// P2 follow-up: previously the codex bridge only populated
// SessionID + Model on its sink events; AgentName / Workspace /
// Branch were empty. downstream StatusBar (statusbar.go:154-209)
// dropped Line 3 (the "📁 <ws> · ⎇ <branch> · +N · −N · ±N · …"
// workspace / dirty-state summary) on every codex one-shot /
// review receipt. This helper stamps all three so statusbar
// renders the full three-line footer (dsh's drain shape —
// internal/bridge/dsh/session.go:866-873).
func codexSinkContext(s *Starter, cfg agent.StartConfig) (agentName string, workspace string, branch string) {
	return s.name, cfg.Workspace, detectBranch(cfg.Workspace)
}

// runPrintMode spawns `codex exec` for one-shot invocations
// (/gtw commit, /gtw pr, buildAgentPrompt). Mirrors claudecode/pi
// print mode: bypass the long-lived bridge driver, spawn a fresh
// process, capture stdout (--json) for metadata, capture the
// final answer via -o <tmpfile>, reap on exit.
//
// Per-call sink (opts.OnEvent): when present, the bridge emits
// Ready → Text → Result (or Error on the failure path), matching
// the dsh drain's contract. SessionID + Model are filled in
// progressively: the NDJSON stream's `thread.started` event gives
// us thread_id (we re-emit Ready when it arrives, since the
// up-front Ready can't carry data we don't have yet) and
// `item.completed` (error variant) yields the model name. nil
// sink is fully supported (no-op on every emit), matching
// `outbound.StreamRunOnceToEmitter`'s non-blocking contract.
func runPrintMode(ctx context.Context, s *Starter, cfg agent.StartConfig, blocks []agent.ContentBlock, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	if cfg.Workspace == "" {
		return agent.RunResult{}, fmt.Errorf("codex: workspace is required")
	}

	agentName, workspace, branch := codexSinkContext(s, cfg)

	sink := agent.ParseRunOnceOptions(opts).OnEvent

	// Up-front Ready so the chat channel's StatusBar / receipt
	// header can flip from "agent X" placeholder to "agent X · …"
	// before the long exec run starts. SessionID/Model are
	// empty here; the NDJSON-driven branch below RE-emits Ready
	// once we know them, so any consumer that snapshots on first
	// Ready and ignores later ones will still see this one.
	//
	// Note: dsh emits EventAgentReady ONCE with all four
	// (SessionID/Model/Workspace/Branch) populated — see
	// internal/bridge/dsh/session.go:289-298. The print-mode
	// here is different: `codex exec` exposes thread_id
	// asynchronously via the `thread.started` NDJSON event, so
	// we have to choose between an empty up-front Ready (this
	// code) and a delayed Ready only after thread.started
	// arrives (which would leave the chat StatusBar silent for
	// the first few seconds of a long exec run). We chose the
	// former so the user sees "🤖: codex · …" immediately;
	// the thread.started-driven re-emit below upgrades the
	// session id once it lands.
	if sink != nil {
		sink(agent.AgentEvent{
			Kind:      agent.EventAgentReady,
			AgentName: agentName,
			Workspace: workspace,
			Branch:    branch,
		})
	}

	startTime := time.Now()

	prefixArgs, prompt := buildPrintArgs(cfg, blocks)

	// Create the -o target tempfile early so we can clean up even
	// if spawn fails. codex exec writes ONLY the final agent
	// message here (verified on 0.145.0); tool-call progress and
	// "user / codex" markers go to stderr.
	tmpOut, err := os.CreateTemp("", "codex-print-*.txt")
	if err != nil {
		// Early-return failure: Ready was already emitted above
		// (sink contract requires every Ready to be paired with a
		// terminal event). Fire Error so the sink observes a
		// complete lifecycle. Caller-side dispatcher (gtw/agent_reply.go)
		// also surfaces the error via its own ❌ path; the sink
		// notification is for the chat channel's StatusBar /
		// receipt, independent of the formatted text.
		wrapped := fmt.Errorf("codex: create tempfile: %w", err)
		if sink != nil {
			sink(agent.AgentEvent{
				Kind:       agent.EventAgentError,
				Err:        wrapped,
				Diagnostic: codexDiagnostic(agent.BridgeExitUnknown, ""),
			})
		}
		return agent.RunResult{}, wrapped
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

	child := proc.New(ctx, s.command, args...)
	child.Dir = cfg.Workspace // belt-and-braces with -C

	stdout, err := child.StdoutPipe()
	if err != nil {
		// Same sink contract: Ready was emitted above, so pair it
		// with Error. See CreateTemp-failure branch above for the
		// rationale.
		wrapped := fmt.Errorf("codex: stdout pipe: %w", err)
		if sink != nil {
			sink(agent.AgentEvent{
				Kind:       agent.EventAgentError,
				Err:        wrapped,
				Diagnostic: codexDiagnostic(agent.BridgeExitUnknown, ""),
			})
		}
		return agent.RunResult{}, wrapped
	}
	stderr, err := child.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		wrapped := fmt.Errorf("codex: stderr pipe: %w", err)
		if sink != nil {
			sink(agent.AgentEvent{
				Kind:       agent.EventAgentError,
				Err:        wrapped,
				Diagnostic: codexDiagnostic(agent.BridgeExitUnknown, ""),
			})
		}
		return agent.RunResult{}, wrapped
	}
	if err := child.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		wrapped := fmt.Errorf("codex: start: %w", err)
		if sink != nil {
			sink(agent.AgentEvent{
				Kind:       agent.EventAgentError,
				Err:        wrapped,
				Diagnostic: codexDiagnostic(agent.BridgeExitUnknown, ""),
			})
		}
		return agent.RunResult{}, wrapped
	}
	pid := child.Process.Pid

	cLog("PrintMode Start",
		"command", s.command,
		"mode", "exec",
		"workspace", cfg.Workspace,
		"prompt_bytes", len(prompt),
		"args_count", len(args),
		"image_count", countImageFlags(prefixArgs),
		"pid", pid)

	// Drain stderr concurrently via the shared helper. Caps
	// stderr capture bytes and cancellation in one place so any
	// future tweak applies to the whole bridge at once.
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
				// Re-emit Ready with the now-known session id.
				// The up-front Ready (line above the spawn)
				// arrived with empty fields; this one is the
				// "real" Ready that lets the channel receipt
				// header render "session <id> · …". We do NOT
				// also re-emit on model discovery below —
				// model arrives AFTER the assistant has already
				// started, so re-emitting Ready twice would
				// race the rolling-log; the single
				// session-id-anchored Ready is enough for the
				// /gtw commit + /gtw pr callers (they only
				// care about the session id for correlation).
				if sink != nil {
					sink(agent.AgentEvent{
						Kind:      agent.EventAgentReady,
						SessionID: ev.ThreadID,
						AgentName: agentName,
						Workspace: workspace,
						Branch:    branch,
					})
				}
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

	// Drain stderr BEFORE cmd.Wait: Go stdlib's exec.Cmd.Wait
	// reaps the process and then calls closeDescriptors on the
	// parent ends of StdoutPipe / StderrPipe, which closes our
	// stderr read end. If the drain goroutine hasn't already
	// pulled "boom\n" out of the kernel pipe buffer at that point,
	// the close discards the buffered bytes and Diagnostic.StderrTail
	// lands empty. Race window was ~milliseconds; visible 100% on
	// macOS and intermittently on Linux under `-race`.
	stderrDrain.wait()
	waitErr := child.Wait()

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
		wrapped := fmt.Errorf("codex: read -o file: %w", fileErr)
		if sink != nil {
			// stderrStr isn't computed until later in this
			// function; at this point stderrDrain has captured
			// whatever the child wrote, but we haven't
			// trim-trimmed it yet. Pass raw for this error path
			// — the renderer's first-line trim still applies
			// (translate.go:210-213).
			rawStderr := stderrDrain.bytes()
			sink(agent.AgentEvent{
				Kind:       agent.EventAgentError,
				Err:        wrapped,
				Diagnostic: codexDiagnostic(agent.BridgeExitUnknown, rawStderr),
			})
		}
		return agent.RunResult{}, wrapped
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

	// Surface waitErr first (jsonReadErr is usually just "broken
	// pipe on closed child" noise); stderr is trimmed here once.
	stderrStr := strings.TrimSpace(stderrDrain.bytes())
	if err := formatCodexExitError(waitErr, stderrStr, finalText, "answer"); err != nil {
		// Surface the failure to the sink so the chat channel
		// flips its receipt to an error state. We do this
		// BEFORE returning so the /review dispatcher (which
		// also emits a friendly "❌ /review failed" text via
		// emitter.Send) sees a consistent picture: the sink
		// shows the process state, the formatted text is the
		// deliverable. Same split as the success path above
		// (sink observes lifecycle, the dispatcher owns
		// presentation).
		//
		// Diagnostic carries BridgeExitKind derived from waitErr
		// so chat.translate renders "codex process exited
		// (non-zero-exit)" — without it,
		// outbound.Translate:188-202 silently drops the Event
		// because Err-only events predate the Diagnostic field.
		//
		// Empty-answer special case: when waitErr is nil but
		// formatCodexExitError returned non-nil, the only
		// remaining failure mode is "subprocess exited cleanly
		// but produced no stdout / no -o content". Calling
		// ClassifyExit(nil, false) here would yield
		// BridgeExitCleanExit, which the Feishu renderer
		// (adapter.go:1772-1790) titles as
		// "⚠️ codex bridge died (clean-exit)" — semantically
		// wrong because the bridge didn't die; it just
		// produced no output. Use BridgeExitUnknown so the
		// title/body stay consistent ("codex: empty answer").
		exitKind := agent.BridgeExitUnknown
		if waitErr != nil {
			exitKind = agent.ClassifyExit(waitErr, false)
		}
		if sink != nil {
			sink(agent.AgentEvent{
				Kind: agent.EventAgentError,
				Err:  err,
				Diagnostic: codexDiagnostic(
					exitKind,
					stderrStr,
				),
			})
		}
		return agent.RunResult{}, err
	}
	// NDJSON-specific: a parse error on a clean exit is still a
	// failure (we couldn't extract session/model/usage).
	if jsonReadErr != nil && !errors.Is(jsonReadErr, io.EOF) {
		// The subprocess exited cleanly (waitErr == nil at this
		// point — formatCodexExitError passed above) but the
		// stdout NDJSON stream was unparseable. BridgeExit-
		// Unknown rather than CleanExit because the failure
		// mode here is protocol-level, not exit-level.
		if stderrStr != "" {
			// P1 follow-up: `stderr` here is the io.ReadCloser
			// from child.StderrPipe() — formatting it with %s
			// would render the *os.File pointer as garbage in
			// the visible error body. Use stderrStr (the
			// trim-trimmed captured content), matching the
			// neighbouring codexDiagnostic call below.
			err := fmt.Errorf("codex: stdout: %w (stderr: %s)", jsonReadErr, stderrStr)
			if sink != nil {
				sink(agent.AgentEvent{
					Kind: agent.EventAgentError,
					Err:  err,
					Diagnostic: codexDiagnostic(
						agent.BridgeExitUnknown,
						stderrStr,
					),
				})
			}
			return agent.RunResult{}, err
		}
		err := fmt.Errorf("codex: stdout: %w", jsonReadErr)
		if sink != nil {
			sink(agent.AgentEvent{
				Kind: agent.EventAgentError,
				Err:  err,
				Diagnostic: codexDiagnostic(
					agent.BridgeExitUnknown,
					"",
				),
			})
		}
		return agent.RunResult{}, err
	}

	// Terminal event: hand the assembled RunResult to the sink
	// so the chat channel can render the canonical
	// "📝 <text> (12.3s)" line + footer tokens. Same shape as
	// dsh's drain (starter.go:228-241).
	if sink != nil {
		sink(agent.AgentEvent{
			Kind: agent.EventAgentResult,
			Result: &agent.AgentResultEvent{
				Text:       result.Text,
				DurationMs: result.DurationMs,
				Subtype:    result.Subtype,
				Usage:      result.Usage,
			},
			SessionID: result.SessionID,
			Model:     result.Model,
			AgentName: agentName,
			Workspace: workspace,
			Branch:    branch,
		})
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
// proc.New.
//
// No error return: encoding blocks into argv + prompt has no
// failure mode that isn't a caller bug (empty ContentImage.Path
// is silently dropped, mirroring the long-lived bridge's
// SendBlocks). If a future flag requires validation, add the
// error return then.
// buildPrintArgs assembles argv for `codex exec <prompt>`. Used
// by RunOnce only.
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
	Item     *codexExecItem  `json:"item,omitempty"`      // item.completed
	Usage    *codexExecUsage `json:"usage,omitempty"`     // turn.completed
}

// codexExecItem is the `item` payload inside item.* events
// (item.started / item.updated / item.completed). Only the
// fields relevant to the current item.type are populated; the
// JSON decoder tolerates missing / extra fields, so different
// item variants coexist without per-type wrapper structs.
//
// Field map (verified against `codex exec --json` 0.145+):
//   - command_execution : Command, AggregatedOutput, ExitCode,
//     Status; emitted on started / completed
//   - file_change       : Changes[]; emitted only on completed
//   - reasoning         : Text; emitted only on completed (when
//     model_reasoning_summary=detailed)
//   - agent_message     : Text; emitted only on completed —
//     suppressed from the sink (P1 fix — single point of
//     prose delivery is EventAgentResult)
//   - mcp_tool_call     : Server, Tool, Arguments; emitted on
//     started / completed
//   - error             : Message; emitted only on completed
type codexExecItem struct {
	ID               string                `json:"id"`
	Type             string                `json:"type"`
	Message          string                `json:"message,omitempty"`           // error
	Command          string                `json:"command,omitempty"`           // command_execution
	AggregatedOutput string                `json:"aggregated_output,omitempty"` // command_execution completed
	ExitCode         *int                  `json:"exit_code,omitempty"`         // command_execution completed (nil while in_progress)
	Status           string                `json:"status,omitempty"`            // command_execution: in_progress | completed | failed
	Text             string                `json:"text,omitempty"`              // agent_message / reasoning
	Changes          []codexExecItemChange `json:"changes,omitempty"`           // file_change
	Server           string                `json:"server,omitempty"`            // mcp_tool_call
	Tool             string                `json:"tool,omitempty"`              // mcp_tool_call
}

type codexExecItemChange struct {
	Path string `json:"path"`
	Kind string `json:"kind"` // "add" | "delete" | "update"
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
// a background goroutine. Shared so runPrintMode has IDENTICAL
// stderr capture semantics across the bridge — extracting here
// means any future tweak to the cap / cancellation applies at once.
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

func (d *stderrDrain) wait()               { <-d.done }
func (d *stderrDrain) bytes() string       { return d.buf.String() }
func (d *stderrDrain) truncatedFlag() bool { return d.truncated }

// formatCodexExitError returns the canonical "codex: exit: ..." /
// "codex: empty <label>" error for runPrintMode (same waitErr /
// stderr / finalText precedence rules). Returns nil when waitErr
// is nil and finalText is non-empty (success path — caller
// builds the result directly).
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

// codexDiagnostic builds the BridgeDiagnostic attached to every
// EventAgentError emitted on the codex bridge. Required because
// outbound.Translate (translate.go:188-202) returns `(zero, false)`
// when ev.Diagnostic is nil — silent-drop by design — so an Error
// event without Diagnostic never reaches the chat channel via the
// sink pipeline (the dispatcher still surfaces a separate ❌
// OutReply, but the chat receipt card stays at 🔄 because nothing
// drives the runtime's endPrompt path for one-shot calls).
//
// exitKind follows agent.ClassifyExit's taxonomy:
//
//   - BridgeExitCleanExit       — subprocess finished cleanly
//     (used when formatCodexExitError failed because finalText
//     was empty but waitErr is nil).
//   - BridgeExitNonZeroExit     — *exec.ExitError with positive
//     code, e.g. codex review --base <branch> when no diff.
//   - BridgeExitSignalKilled    — *exec.ExitError with negative
//     code (SIGKILL from our ctx cancel).
//   - BridgeExitUnknown         — spawn failure (no subprocess
//     started), or NDJSON parse error on otherwise-clean exit.
//     ClassifyExit also returns Unknown for these but we use
//     the literal here to make the call-site intent explicit.
//
// stderr is the trim-trimmed stderr captured by stderrDrain.
// For spawn-failure paths stderr is empty (we never started);
// passing "" lets the renderer fall back to the synthesized
// body (translate.go:216-227) instead of leaving the card
// body blank.
func codexDiagnostic(exitKind agent.BridgeExitKind, stderr string) *agent.BridgeDiagnostic {
	return &agent.BridgeDiagnostic{
		ExitKind:   exitKind,
		StderrTail: stderr,
		AgentName:  "codex",
		KilledAt:   time.Now(),
	}
}

// translateItemStarted translates a `codex exec --json`
// item.started event into one or more AgentEvents delivered to
// the sink. Only `command_execution` and `mcp_tool_call` produce
// a start event worth translating (the start of a shell command
// or an MCP tool call). Other item types (reasoning, agent_message,
// file_change, error) only emit on completed.
//
// Safe on nil sink / nil item — both are no-ops, matching
// runNDJSON's tolerance for malformed lines.
func translateItemStarted(sink func(agent.AgentEvent), item *codexExecItem) {
	if sink == nil || item == nil {
		return
	}
	switch item.Type {
	case "command_execution":
		sink(agent.AgentEvent{
			Kind: agent.EventAgentToolStart,
			ToolStart: &agent.AgentToolStartEvent{
				ID:   item.ID,
				Name: "bash",
				Args: item.Command,
			},
		})
	case "mcp_tool_call":
		name := item.Server
		if item.Tool != "" {
			name = item.Server + "." + item.Tool
		}
		sink(agent.AgentEvent{
			Kind: agent.EventAgentToolStart,
			ToolStart: &agent.AgentToolStartEvent{
				ID:   item.ID,
				Name: name,
				Args: "", // mcp_tool_call arguments are a JSON value; omit for now
			},
		})
	}
}

// translateItemCompleted translates a `codex exec --json`
// item.completed (or item.updated) event into AgentEvents.
// Suppresses agent_message (P1 fix — final text comes from the
// -o tempfile via EventAgentResult, not from the streaming agent_message).
//
// agent_message suppression rationale: emitting both an
// EventAgentText for the agent_message AND an EventAgentResult
// (with the same final text read from -o after turn.completed)
// produces two visible copies via outbound.Translate
// (OutReply from Text + OutResult from Result). Suppressing
// here means the user sees a single copy via Result.
func translateItemCompleted(sink func(agent.AgentEvent), item *codexExecItem) {
	if sink == nil || item == nil {
		return
	}
	switch item.Type {
	case "command_execution":
		// AgentToolEndEvent has no Err field; fold the exit code
		// into the Output string when non-zero so the receipt
		// card's "⎿ output" line shows the failure marker.
		output := item.AggregatedOutput
		if item.ExitCode != nil && *item.ExitCode != 0 {
			output = fmt.Sprintf("[exit %d] %s", *item.ExitCode, output)
		}
		sink(agent.AgentEvent{
			Kind: agent.EventAgentToolEnd,
			ToolEnd: &agent.AgentToolEndEvent{
				ID:     item.ID,
				Name:   "bash",
				Args:   item.Command,
				Output: output,
			},
		})
	case "file_change":
		// Render a short summary as a single Text entry. The
		// chat channel's outbound.Translate maps Text → OutReply,
		// which folds into the receipt card rolling log.
		paths := make([]string, 0, len(item.Changes))
		for _, c := range item.Changes {
			paths = append(paths, c.Path)
		}
		text := fmt.Sprintf("📝 changed %d file(s): %s",
			len(paths), strings.Join(paths, ", "))
		if len(paths) > 8 {
			text = fmt.Sprintf("📝 changed %d file(s) (first 8: %s)",
				len(paths), strings.Join(paths[:8], ", "))
		}
		sink(agent.AgentEvent{
			Kind: agent.EventAgentText,
			Text: text,
		})
	case "reasoning":
		// Emit as EventAgentThinking so the gateway routes it to
		// OutThinking directly — no string-prefix sniffing needed.
		// Channels render this as a 💭 side line, distinct from
		// the 💬 OutReply bubble for the agent's final answer.
		sink(agent.AgentEvent{
			Kind: agent.EventAgentThinking,
			Text: item.Text,
		})
	case "mcp_tool_call":
		name := item.Server
		if item.Tool != "" {
			name = item.Server + "." + item.Tool
		}
		sink(agent.AgentEvent{
			Kind: agent.EventAgentToolEnd,
			ToolEnd: &agent.AgentToolEndEvent{
				ID:     item.ID,
				Name:   name,
				Args:   "",
				Output: item.AggregatedOutput,
			},
		})
	case "agent_message":
		// P1 fix: deliberately dropped. Final prose surfaces once
		// via EventAgentResult after turn.completed (the text
		// comes from the -o tempfile, not from this streaming
		// event). See translateItemCompleted doc.
	case "error":
		// Codex CLI emits "Model metadata for `…` not found" on
		// every run; parse it for the StatusBar model field.
		// The actual error case (review_failed item.completed)
		// also surfaces here — extractModelFromError returns ""
		// for non-matching shapes, so non-model errors are no-op'd.
	}
}
