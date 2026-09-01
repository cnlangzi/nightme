// review.go — native review runner for the cursor bridge.
//
// Cursor ships three review skills in ~/.cursor/skills-cursor/
// (built-in, auto-loaded by cursor-agent — verified against
// cursor-agent 2026.08.11, see docs/REVIEW.md §2.1.1):
//
//   - review           : AskQuestion-driven menu (disable-model-invocation:
//                        true — model can't dispatch it; user picks bugbot /
//                        security interactively). Useless in print mode.
//   - review-bugbot    : Bugbot subagent — general code-change review.
//                        Computes the diff from the workspace's git state
//                        itself; we don't precompute or pass --base.
//   - review-security  : Security Review subagent — security-focused review.
//
// All three are invocable as slash commands in cursor-agent's -p print mode
// (verified empirically — see docs/REVIEW.md §2.1.1 for the test transcripts):
//
//   cursor-agent --force --trust --sandbox disabled --approve-mcps \
//     -p "/review-bugbot" --output-format text
//
// → dispatches Bugbot subagent; plain-text stdout → RunResult.Text.
//
// Per docs/REVIEW.md §2.1 "codex/claude use native review" rule, cursor now
// qualifies — we invoke its native review directly instead of going through
// the Tier 2/3 fan-out path (ReviewWithOcr / ReviewWithPrompt). Symmetric
// with codex's `codex exec review --base <branch>` (print.go::runCodexReview)
// and claudecode's `claude -p code-review <base>...HEAD`
// (print.go::runCodeReviewPrintMode).
//
// Differences from the Tier 2/3 path (now retired for cursor):
//   - ONE subprocess (no multi-job fan-out, no simplifyGroup — per
//     docs/REVIEW.md §2.6, native reviewers don't get simplify).
//   - No precomputed diff — Bugbot computes the diff from cwd's git state
//     (same role as codex's `--base <branch>` flag, except Bugbot infers
//     the base itself).
//   - Plain text stdout → RunResult.Text (same shape as runPrintMode).
//
// Why we don't use --workspace: cursor-agent's --workspace flag picks the
// repo, but Bugbot's diff math fails when --workspace points to a path
// where the diff math doesn't apply (verified: passing --workspace on a
// path Bugbot considers "no branch base" returns an error even when cwd
// would have worked). cmd.Dir is the right way to point Bugbot at the
// target repo — same pattern as runPrintMode.
package cursor

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/proc"
)

// reviewSlashCommand is the slash command cursor-agent dispatches in
// print mode. Verified 2026-09-01: cursor-agent auto-loads
// ~/.cursor/skills-cursor/review-bugbot/SKILL.md and dispatches the
// subagent_type: "bugbot" subagent when /review-bugbot appears as the
// positional -p prompt.
//
// NOT /review: that skill has disable-model-invocation: true and is a
// pure AskQuestion menu — useless in -p mode (would just print the menu
// and exit). We go straight to the runnable variant.
//
// NOT /agent-review: that's the IDE chat UI's name for the same feature;
// cursor-agent's -p dispatch targets the skill names (review-bugbot /
// review-security), not the IDE's /agent-review alias.
const reviewSlashCommand = "/review-bugbot"

// runCursorReview spawns `cursor-agent -p "/review-bugbot"` for native
// review and returns Bugbot's final plain-text review as RunResult.Text.
//
// Per-call sink (opts.OnEvent): emitted events mirror runPrintMode /
// runCodexReview — EventAgentReady up-front, EventAgentResult + Done on
// success, EventAgentError (with BridgeDiagnostic) on failure. This keeps
// the chat channel's StatusBar / receipt rendering consistent with the
// other Tier-1 native reviewers (codex / claudecode).
//
// Failure modes (forwarded verbatim from Bugbot):
//   - "could not compute a branch-changes diff" — workspace isn't a git
//     repo, or no branch base available. Caller (the /review dispatcher)
//     surfaces this in chat; nothing for the bridge to fix.
//   - Empty stdout — Bugbot exited 0 but produced no text. Surface as
//     "cursor review: empty answer" (same shape as runPrintMode).
//   - Non-zero exit — wrapped with stderr tail, classified via
//     agent.ClassifyExit for BridgeDiagnostic.
func runCursorReview(ctx context.Context, s *Starter, cfg agent.StartConfig, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	workspace := cfg.Workspace
	if workspace == "" {
		return agent.RunResult{}, fmt.Errorf("cursor review: workspace is required")
	}

	sink := agent.ParseRunOnceOptions(opts).OnEvent

	// Up-front Ready so the chat channel's StatusBar / receipt header
	// can flip from "cursor" placeholder to the working state. cursor's
	// wire carries no session / model info (plain-text print mode), so
	// only static metadata is populated — same shape as runPrintMode.
	if sink != nil {
		sink(agent.AgentEvent{
			Kind:      agent.EventAgentReady,
			AgentName: s.Info().Name,
			Workspace: workspace,
		})
	}

	startTime := time.Now()

	// argv: FullAccessArgs (parent) + -p "<slash>" + --output-format text.
	// No --workspace: cmd.Dir does the right thing; --workspace can
	// confuse Bugbot's diff math (see package doc).
	args := withFullAccess(
		"-p", reviewSlashCommand,
		"--output-format", "text",
	)
	args = append(args, cfg.Args...)

	cmd := proc.New(ctx, s.command, args...)
	cmd.Dir = workspace
	// Forward cfg.Env the same way RunOnce does (append to os.Environ,
	// cfg wins on conflict). Without this, /review-time env overrides
	// (custom API keys, MCP credentials) are silently dropped.
	if len(cfg.Env) > 0 {
		cmd.Env = append(os.Environ(), cfg.Env...)
	}

	output, err := cmd.CombinedOutput()
	elapsedMs := time.Since(startTime).Milliseconds()

	cLog("ReviewMode Exit",
		"workspace", workspace,
		"mode", "review-bugbot",
		"elapsed_ms", elapsedMs,
		"output_bytes", len(output),
		"err", errStr(err))

	if err != nil {
		stderr := strings.TrimSpace(string(output))
		wrapped := fmt.Errorf("cursor review: %w (stderr: %s)", err, stderr)
		if sink != nil {
			sink(agent.AgentEvent{
				Kind:       agent.EventAgentError,
				Err:        wrapped,
				Diagnostic: cursorDiagnostic(agent.ClassifyExit(err, false), stderr),
			})
		}
		return agent.RunResult{}, wrapped
	}

	text := strings.TrimSpace(string(output))
	if text == "" {
		wrapped := fmt.Errorf("cursor review: empty answer")
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
		Subtype:    "completed",
	}

	// Success path: emit terminal Result + Done to the sink so the chat
	// channel's outer lifecycle closes. Same minimal-lifecycle shape as
	// runPrintMode — cursor's plain-text wire carries no usage / model so
	// those fields stay zero.
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
