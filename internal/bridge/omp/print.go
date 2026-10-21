// print.go — one-shot print mode for omp using
// `omp -p --mode json --approval-mode yolo`.
//
// Why this exists: the omp bridge has ONE spawn recipe for
// long-lived chat sessions (`omp --mode rpc`, agent.go). For
// callers that only need a single turn (/gtw commit,
// buildAgentPrompt, /review), opening a long-lived RPC
// session just to drive one prompt + drain one terminal
// event is wasteful. omp exposes the same NDJSON event
// stream over the print-mode `-p` flag, so the print-mode
// path reuses the same translator (translate.go) and only
// changes the spawn args / lifecycle.
//
// omp print-mode emits:
//   - {"type":"session","version":3,"id":"...","timestamp":...,"cwd":...}
//     as the FIRST wire frame (NOT `ready`).
//   - The same message_update / message_end / tool_execution_*
//     / agent_end{isTerminal:true} frames as RPC mode.
//   - No `response` envelope (no RPC commands available).
//   - No `prompt_result` or `session_settled` (those are RPC
//     mode observability noise).
//
// Translation: print-mode uses the same translator as RPC.
// The print path's terminal signal is identical to RPC's
// (agent_end{isTerminal:true}); without the terminal event,
// the run is considered failed even when the process exited
// cleanly — see parsePrintStream's no-terminal check.

package omp

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

// stderrCapBytes bounds the stderr buffer kept in memory
// across a print-mode run. Matches pi/codex/copilot.
const stderrCapBytes = 64 * 1024

// buildPrintArgs assembles the argv for a one-shot
// `omp -p --mode json --approval-mode yolo` call. The prompt
// portion is delegated to agent.BlocksToPrompt (shared by
// claudecode / pi / dsh; the print-mode bridges whose -p
// flag accepts a single positional string).
func buildPrintArgs(blocks []agent.ContentBlock) (args []string, prompt string) {
	prompt = agent.BlocksToPrompt(blocks)
	args = []string{
		"-p", prompt,
		"--mode", "json",
		"--approval-mode", "yolo",
	}
	return args, prompt
}

// peekPrintMeta inspects a single wire line for the metadata
// that omp's print-mode emits but the translator's typed
// AgentEvent surface never carries: the session id
// ({"type":"session","version":3,"id":..}) which is the
// call's SessionID. omp's print-mode opens WITHOUT a get_state
// handshake, so the session id is only obtainable from this
// frame.
//
// First-non-empty wins. The translator drops `session` via
// its default case (no EventEvent mapped to it).
func peekPrintMeta(line []byte, result *agent.RunResult) {
	if result.SessionID != "" {
		return
	}
	var meta sessionMeta
	if err := json.Unmarshal(line, &meta); err != nil {
		return
	}
	if meta.Type == "session" && meta.ID != "" {
		result.SessionID = meta.ID
	}
}

// runPrintMode spawns `omp -p --mode json --approval-mode
// yolo` for one-shot invocations. It owns the process from
// spawn to exit, streams events through the same translator
// the RPC path uses, and returns a RunResult carrying the
// final text + per-turn usage on a clean run.
//
// print-mode has NO get_state handshake and NO `ready` frame;
// the first wire frame is `{"type":"session",..}`. We do NOT
// wait for `ready` here — see parsePrintStream.
//
// Failure paths surface EventAgentError + Diagnostic to the
// sink so downstream callers see a balanced lifecycle.
func runPrintMode(ctx context.Context, s *Starter, cfg agent.StartConfig, blocks []agent.ContentBlock, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	if cfg.Workspace == "" {
		return agent.RunResult{}, fmt.Errorf("omp: workspace is required")
	}

	startTime := time.Now()
	sink := agent.ParseRunOnceOptions(opts).OnEvent

	args, prompt := buildPrintArgs(blocks)

	child := proc.New(ctx, s.command, args...)
	child.Dir = cfg.Workspace
	env := append([]string(nil), cfg.Env...)
	env = append(env, ompHeadlessEnv...)
	child.Env = append(os.Environ(), env...)

	stdout, err := child.StdoutPipe()
	if err != nil {
		return agent.RunResult{}, fmt.Errorf("omp: stdout pipe: %w", err)
	}
	stderr, err := child.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		return agent.RunResult{}, fmt.Errorf("omp: stderr pipe: %w", err)
	}

	if err := child.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		wrapped := fmt.Errorf("omp: start: %w", err)
		if sink != nil {
			sink(agent.AgentEvent{
				Kind:       agent.EventAgentError,
				Err:        wrapped,
				Diagnostic: ompDiagnostic(agent.BridgeExitUnknown, ""),
			})
		}
		return agent.RunResult{}, wrapped
	}
	pid := child.Process.Pid

	oLog("PrintMode Start",
		"command", s.command, "workspace", cfg.Workspace,
		"prompt_bytes", len(prompt), "pid", pid)

	if sink != nil {
		branch := detectBranch(cfg.Workspace)
		sink(agent.AgentEvent{
			Kind:      agent.EventAgentReady,
			AgentName: s.Info().Name,
			Workspace: cfg.Workspace,
			Branch:    branch,
		})
	}

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

	result, translateErr := parsePrintStream(ctx, stdout, cfg.Workspace, sink)

	<-stderrDone
	waitErr := child.Wait()

	oLog("PrintMode Exit",
		"pid", pid,
		"elapsed_ms", time.Since(startTime).Milliseconds(),
		"wait_err", errStr(waitErr),
		"stderr_bytes", stderrBuf.Len(),
		"stderr_truncated", stderrTruncated,
	)

	if sink != nil {
		switch {
		case translateErr == nil && waitErr == nil:
		case translateErr != nil:
			stderr := strings.TrimSpace(stderrBuf.String())
			sink(agent.AgentEvent{
				Kind:       agent.EventAgentError,
				Err:        translateErr,
				Diagnostic: ompDiagnostic(agent.BridgeExitUnknown, stderr),
			})
		default:
			stderr := strings.TrimSpace(stderrBuf.String())
			sink(agent.AgentEvent{
				Kind:       agent.EventAgentError,
				Err:        waitErr,
				Diagnostic: ompDiagnostic(agent.ClassifyExit(waitErr, false), stderr),
			})
		}
	}

	if translateErr != nil {
		stderr := strings.TrimSpace(stderrBuf.String())
		if stderr != "" {
			return agent.RunResult{}, fmt.Errorf("omp: %w (stderr: %s)", translateErr, stderr)
		}
		return agent.RunResult{}, translateErr
	}

	if waitErr != nil {
		stderr := strings.TrimSpace(stderrBuf.String())
		if stderr != "" {
			return agent.RunResult{}, fmt.Errorf("omp: exit: %w (stderr: %s)", waitErr, stderr)
		}
		return agent.RunResult{}, fmt.Errorf("omp: exit: %w", waitErr)
	}

	return result, nil
}

// parsePrintStream drives the JSON event reader from a
// print-mode omp spawn. It runs the existing translator
// (translate.go) over each line and watches for either an
// `agent_end{isTerminal:true}` event (turn-end marker
// carrying the final text) or a stream error. Returns
// RunResult on clean completion.
//
// print-mode has no `ready` frame; the first wire frame is
// `{"type":"session",..}`. We do NOT wait for ready — the
// translator's default case drops `session` silently, and
// peekPrintMeta surfaces the id onto RunResult.
func parsePrintStream(ctx context.Context, stdout io.Reader, workspace string, sink func(agent.AgentEvent)) (agent.RunResult, error) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), MaxFrameSize)

	branch := detectBranch(workspace)
	translator := newTranslator(bridgeName+"-print", workspace, branch)

	var result agent.RunResult
	sawSettled := false

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return agent.RunResult{}, err
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		peekPrintMeta(line, &result)

		events, err := translator.translate(line, nil)
		if err != nil {
			return agent.RunResult{}, fmt.Errorf("omp: translate: %w (line=%s)", err, truncateForErr(line))
		}

		for _, ev := range events {
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
				if sink != nil {
					sink(ev)
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return agent.RunResult{}, fmt.Errorf("omp: stdout: %w", err)
	}

	if !sawSettled {
		// Process exited cleanly but we never saw an
		// agent_end{isTerminal:true} event. Surface a typed
		// error containing the agent name (R10 acceptance:
		// "退出但无终态事件时返回错误且错误信息包含 agent 名").
		return agent.RunResult{}, fmt.Errorf("omp: exit without agent_end isTerminal=true%s",
			appendAuditFields(result, true))
	}

	result.Text = strings.TrimSpace(result.Text)
	return result, nil
}

// truncateForErr shortens a malformed JSONL line for
// inclusion in error messages.
func truncateForErr(line []byte) string {
	const cap = 200
	if len(line) <= cap {
		return string(line)
	}
	return string(line[:cap]) + "..."
}

// ompDiagnostic is the BridgeDiagnostic payload attached to
// EventAgentError events emitted from the print-mode failure
// paths. Mirrors codexDiagnostic / piDiagnostic.
func ompDiagnostic(exitKind agent.BridgeExitKind, stderr string) *agent.BridgeDiagnostic {
	return &agent.BridgeDiagnostic{
		ExitKind:   exitKind,
		StderrTail: stderr,
		AgentName:  bridgeName,
		KilledAt:   time.Now(),
	}
}

// appendAuditFields returns the audit-suffix string appended
// to error messages on the omp print-mode failure paths.
func appendAuditFields(result agent.RunResult, whenSessionID bool) string {
	var b strings.Builder
	if whenSessionID {
		b.WriteString(agent.FormatSessionID(result.SessionID))
	}
	b.WriteString(agent.FormatSubtype(result.Subtype))
	b.WriteString(agent.FormatUsage(result.Usage))
	return b.String()
}
