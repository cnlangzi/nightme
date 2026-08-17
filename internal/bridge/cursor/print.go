// print.go — one-shot print mode for cursor using `agent -p`.
//
// Cursor CLI has a built-in print-mode: `agent -p "prompt"
// --output-format text`. The process outputs plain text to stdout
// and exits — no JSON events, no multi-turn, no events channel.
//
// This is simpler than opencode's print-mode (which emits NDJSON
// events). The wire is just raw text, so we capture stdout
// directly and return it as RunResult.Text.
//
// Mirrors the codex/claudecode/pi/opencode print-mode pattern:
// bypass the long-lived ACP bridge driver, spawn a fresh process,
// capture stdout, reap on exit.
package cursor

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// runPrintMode spawns `agent -p "prompt" --output-format text`
// for one-shot invocations (/gtw commit, buildAgentPrompt).
// The process outputs plain text to stdout and exits — no JSON
// events, no structured protocol.
//
// Failure modes:
//   - waitErr != nil  → model / CLI failure; surface stderr
//   - empty finalText → model produced nothing; surface stderr
func runPrintMode(ctx context.Context, s *Starter, cfg agent.StartConfig, blocks []agent.ContentBlock) (agent.RunResult, error) {
	if cfg.Workspace == "" {
		return agent.RunResult{}, fmt.Errorf("cursor: workspace is required")
	}

	prompt := extractText(blocks)
	if prompt == "" {
		return agent.RunResult{}, fmt.Errorf("cursor: empty prompt")
	}

	startTime := time.Now()

	args := []string{"-p", prompt, "--output-format", "text"}
	args = append(args, cfg.Args...)

	cmd := agent.NewCmd(ctx, s.command, args...)
	cmd.Dir = cfg.Workspace

	// Forward cfg.Env the same way Start does (append to
	// os.Environ, cfg wins on conflict). Without this, /gtw
	// commit-time env overrides (custom API keys, MCP
	// credentials) are silently dropped on the print-mode path.
	if len(cfg.Env) > 0 {
		cmd.Env = append(os.Environ(), cfg.Env...)
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
		if stderr != "" {
			return agent.RunResult{}, fmt.Errorf("cursor run: %w (stderr: %s)", err, stderr)
		}
		return agent.RunResult{}, fmt.Errorf("cursor run: %w", err)
	}

	text := strings.TrimSpace(string(output))
	if text == "" {
		return agent.RunResult{}, fmt.Errorf("cursor: empty answer")
	}

	return agent.RunResult{
		Text:       text,
		DurationMs: elapsedMs,
		Subtype:    "completed",
	}, nil
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
