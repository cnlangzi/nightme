// print.go — one-shot print mode for copilot using `copilot -p`.
//
// Copilot CLI has a built-in print-mode: `copilot -p "prompt"`.
// The process outputs the agent's final response to stdout and
// exits — no JSON events, no multi-turn, no events channel.
// FullAccessArgs are prepended so print-mode matches the ACP
// session's "act without prompting" default.
//
// This is simpler than opencode's print-mode (which emits
// NDJSON events). Copilot's wire is just plain text — stdout
// carries the final answer directly, no protocol envelope.
// So we capture stdout verbatim and return it as RunResult.Text.
//
// Mirrors the cursor/print.go shape almost verbatim (cursor's
// `-p` mode is also plain-text stdout): bypass the long-lived
// ACP bridge driver, spawn a fresh process, capture stdout,
// reap on exit. The only differences from cursor are:
//   - argv shape: FullAccessArgs (--allow-all-tools) +
//     `-p "<prompt>"` instead of FullAccessArgs +
//     `-p "<prompt>" --output-format text` (Copilot has no
//     separate --output-format flag — `-p` already implies
//     plain-text stdout for one-shot mode)
//   - bridge name on errors ("copilot" vs "cursor")
//   - log prefix (`cLog` instead of cursor's `cLog`)
package copilot

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/proc"
)

// runPrintMode spawns `copilot` + FullAccessArgs +
// `-p "prompt"` for one-shot invocations (/gtw commit,
// buildAgentPrompt). The process outputs plain text to stdout
// and exits — no JSON events, no structured protocol.
//
// opts (when set via WithEventSink) wires a per-call observer
// that receives an up-front Ready + a terminal Result/Done
// (or Error on failure). Pre-fix opts were silently dropped
// on the print-mode path (Finding 4 from /review), leaving
// the chat sink permanently open. Copilot's plain-text wire
// has no per-event stream to forward — only lifecycle
// markers make sense; intermediate Text/Tool events don't
// exist at the bridge layer.
//
// Failure modes:
//   - waitErr != nil  → model / CLI failure; surface stderr
//   - empty finalText → model produced nothing; surface stderr
func runPrintMode(ctx context.Context, s *Starter, cfg agent.StartConfig, blocks []agent.ContentBlock, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	if cfg.Workspace == "" {
		return agent.RunResult{}, fmt.Errorf("copilot: workspace is required")
	}

	sink := agent.ParseRunOnceOptions(opts).OnEvent

	prompt := extractText(blocks)
	if prompt == "" {
		return agent.RunResult{}, fmt.Errorf("copilot: empty prompt")
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
	// start. Copilot's plain-text wire carries no session /
	// model info, so only the static metadata is populated.
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
			wrapped = fmt.Errorf("copilot run: %w (stderr: %s)", err, stderr)
		} else {
			wrapped = fmt.Errorf("copilot run: %w", err)
		}
		// Finding 1 from /review: emit EventAgentError to the
		// sink before returning so the aggregator's doneCount
		// reaches expected (otherwise multi-job /review hangs
		// forever because the chat lifecycle never closes).
		if sink != nil {
			sink(agent.AgentEvent{
				Kind:       agent.EventAgentError,
				Err:        wrapped,
				Diagnostic: copilotDiagnostic(agent.ClassifyExit(err, false), stderr),
			})
		}
		return agent.RunResult{}, wrapped
	}

	text := strings.TrimSpace(string(output))
	if text == "" {
		wrapped := fmt.Errorf("copilot: empty answer")
		if sink != nil {
			sink(agent.AgentEvent{
				Kind:       agent.EventAgentError,
				Err:        wrapped,
				Diagnostic: copilotDiagnostic(agent.BridgeExitCleanExit, ""),
			})
		}
		return agent.RunResult{}, wrapped
	}

	result := agent.RunResult{
		Text:       text,
		DurationMs: elapsedMs,
		Subtype:    "completed",
	}

	// Success path: emit terminal Result + Done to the sink so
	// the chat channel's outer lifecycle closes (Finding 4 from
	// /review). Same minimal-lifecycle shape as cursor /
	// opencode / codex / pi — copilot's plain-text wire carries
	// no usage / model so those fields stay zero.
	if sink != nil {
		sink(agent.AgentEvent{
			Kind: agent.EventAgentResult,
			Result: &agent.AgentResultEvent{
				Text:       result.Text,
				DurationMs: result.DurationMs,
				Subtype:    result.Subtype,
			},
		})
		sink(agent.AgentEvent{
			Kind: agent.EventAgentDone,
			Done: &agent.AgentDoneEvent{ExitCode: 0, Reason: "settled"},
		})
	}

	return result, nil
}

// printModeArgs is the argv for `copilot -p`.
//
//	copilot --allow-all-tools -p "<prompt>" -s
//
// `--allow-all-tools` is the parent-level permission flag
// (verified against `copilot --help` on 1.0.81). `-p` switches
// to non-interactive mode where the process emits the final
// answer on stdout and exits. `-s` / `--silent` (verified on
// 1.0.81: "Output only the agent response (no stats), useful
// for scripting with -p") suppresses the post-answer "Changes
// / AI Credits / Tokens / Resume" decoration so stdout is just
// the agent's final text — without `-s` the captured stdout
// mixes the answer with metadata that downstream consumers
// (RunResult.Text) would render as junk.
//
// Per docs/bridge/acp.md §1.1 + agent-no-config-tampering
// principle: only permission + transport flags are injected.
// Model / provider / MCP servers flow from the user's
// ~/.copilot/ settings.
func printModeArgs(prompt string, extra []string) []string {
	args := append([]string{}, FullAccessArgs...)
	args = append(args, "-p", prompt, "-s")
	return append(args, extra...)
}

// copilotDiagnostic is the BridgeDiagnostic payload attached to
// EventAgentError events emitted from the copilot print-mode
// failure paths. Mirrors cursor/cursorDiagnostic +
// opencode/opencodeDiagnostic (same shape, AgentName=
// "copilot"). Without this, Err-only events are silently
// dropped by the upstream translate → chat renderer pair.
func copilotDiagnostic(exitKind agent.BridgeExitKind, stderr string) *agent.BridgeDiagnostic {
	return &agent.BridgeDiagnostic{
		ExitKind:   exitKind,
		StderrTail: stderr,
		AgentName:  bridgeName,
		KilledAt:   time.Now(),
	}
}

// extractText concatenates all ContentText blocks into a single prompt.
// Mirrors cursor/print.go::extractText.
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
// the opencode / codex / cursor print.go helper of the same name.
func errStr(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}