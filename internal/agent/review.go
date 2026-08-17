// review.go — /review slash command support.
//
// /review runs a code review in a one-shot subprocess (the
// bridge's existing RunOnce path) and injects the captured
// findings back into the main chat session as a user message.
//
// Architecture (F-review.md v6):
//   - RunOnce is the isolation mechanism. The review runs in a
//     fresh subprocess with its own context window — main chat
//     context is NOT polluted by review reasoning (which can
//     otherwise burn tens of thousands of tokens).
//   - The fresh process runs StandardPrompt, reads git diff,
//     and produces a structured review as plain text. We capture
//     that text via RunResult.Text.
//   - We inject the captured text back into the main chat
//     session via rc.Inject, wrapped in a small preamble. The
//     main agent sees the review as a user message — fix
//     follow-up ("fix the blockers") works naturally with full
//     review context visible to the main agent.
//   - pty/bash returns ErrReviewNotSupported (bash isn't a
//     coding agent, can't do review).
//   - The dispatcher wraps the call in timeouts.Review (30 min,
//     same budget as /gtw commit's Agent timeout). RunOnce makes
//     this safe: the deadline only kills the review subprocess.
//   - Zero qualifiers: /review takes no args. Matches /cwd /
//     /think / /use style.
//
// Fix is conversational: after the review appears in chat, the
// user types "fix the blockers" and the main chat agent uses
// its native Edit tools to apply changes. /review does NOT
// auto-apply anything.

package agent

import (
	"context"
	"errors"
	"fmt"
)

// ReviewContext is passed to Starter.Review by the /review dispatcher.
// It carries the workspace path and an Inject callback the bridge
// uses to send the review findings back into the main chat session
// (after the one-shot review completes).
//
// Inject is the chokepoint: dispatcher closes over
// cs.SelectedAgentSession().SendBlocks so the bridge never touches
// AgentSession or chat-session plumbing directly. This keeps
// bridges focused on "given a chat agent, run a review and
// report back" without coupling them to chat-session lifecycle
// details.
type ReviewContext struct {
	// Workspace is the chat session's selected cwd
	// (cs.SelectedCwd()). The one-shot review runs in this
	// directory so it can run `git diff` against the current
	// branch.
	Workspace string

	// Inject sends the given blocks into the main chat session as
	// a user turn. dispatcher wires this to
	// as.SendBlocks(ctx, blocks). The review findings arrive as a
	// user message in main chat, so the main agent sees them and
	// can act on "fix the blockers"-style follow-ups.
	Inject func(ctx context.Context, blocks []ContentBlock) error
}

// ErrReviewNotSupported is returned by Starter.Review when the agent
// type cannot do review (currently pty/bash fallback).
//
// The /review dispatcher surfaces this as a friendly
// "agent X 暂不支持 /review" reply — it doesn't poison the chat
// with a generic bridge error.
var ErrReviewNotSupported = errors.New("agent: /review not supported")

// Review is the canonical /review implementation.
//
// Bridges that don't need custom review behavior should call this
// from their Review method:
//
//	func (s *Starter) Review(ctx context.Context, rc agent.ReviewContext) error {
//	    return agent.Review(ctx, s, rc)
//	}
//
// Review runs the bridge's RunOnce with StandardPrompt and
// rc.Workspace as the working directory. RunOnce is a one-shot
// subprocess that streams agent events and returns when the
// terminal reply arrives. The fresh subprocess:
//
//   - Has its own context window — main chat context is NOT
//     polluted by review reasoning.
//   - Has its own timeouts (the dispatcher wraps us in
//     timeouts.Review = 30 min).
//   - Streams events the same way /gtw commit does; we just
//     capture RunResult.Text at the end.
//
// After RunOnce returns, we inject the captured findings as a
// user message into the main chat session via rc.Inject. The main
// agent sees the review in its context and can act on follow-up
// "fix the blockers" instructions naturally.
//
// Bridges are free to override Starter.Review entirely (e.g.
// claude could use its built-in /code-review slash trigger for
// richer multi-agent review, instead of going through
// StandardPrompt + the bridge's print-mode). v1 ships with all
// 5 coding bridges delegating to Review.
func Review(ctx context.Context, s Starter, rc ReviewContext) error {
	if rc.Inject == nil {
		return errors.New("agent: ReviewContext.Inject is nil")
	}

	result, err := s.RunOnce(ctx, StartConfig{
		Workspace: rc.Workspace,
	}, []ContentBlock{{
		Type: ContentText,
		Text: StandardPrompt(),
	}})
	if err != nil {
		return fmt.Errorf("agent %s: review one-shot failed: %w",
			s.Info().Name, err)
	}

	return rc.Inject(ctx, []ContentBlock{{
		Type: ContentText,
		Text: formatReviewMessage(rc.Workspace, result.Text),
	}})
}

// formatReviewMessage wraps the raw review output in a small
// preamble so the main agent (which receives this as a user
// message) understands the context. The preamble tells the main
// agent where the review came from and what scope it covered —
// important because the main agent then answers follow-up
// questions like "fix the second blocker" using this review
// as its source of truth.
func formatReviewMessage(workspace, review string) string {
	return fmt.Sprintf("## Code review of %s\n\n"+
		"(current branch vs default branch; run via /review)\n\n%s",
		workspace, review)
}

// StandardPrompt is the canonical review prompt used by all
// bridges for /review. The prompt asks the chat agent to:
//
//  1. Detect the default branch (main / master / trunk) via
//     `git symbolic-ref refs/remotes/origin/HEAD` or
//     `git remote show origin`.
//  2. Run the three diff commands that together form "the diff a
//     PR would have":
//       - `git fetch origin` (best-effort, ignore failures)
//       - `git diff <default-branch>...HEAD` (committed on branch)
//       - `git diff --staged` (staged but not committed)
//       - `git diff` (unstaged working-tree changes)
//  3. Output structured `## Summary` / `## Findings` /
//     `## Suggestions` sections, with severity tags on findings.
//
// The prompt is ~70 lines — long enough to enforce structure
// (sections, severity rubric, "how to review" guardrails) but
// short enough that chat agents don't lose focus. It's the only
// prompt every bridge shares — bridges that want custom review
// behavior override Starter.Review.
//
// Prompt content draws from three mature review-prompt shapes:
//
//   - Claude Code /code-review plugin (anthropics/claude-plugins-official):
//     confidence-based scoring, finding-first structure.
//   - Codex `codex review` subcommand (codex 0.145+): severity-grouped
//     findings with actionable suggestions.
//   - Community "senior engineer reviewing PR" prompts: section-based
//     output (Summary / Findings / Suggestions).
//
// We borrow the structure and severity labels, not the multi-agent
// machinery or proprietary confidence scoring.
func StandardPrompt() string {
	return `You are a senior engineer reviewing code changes for an upcoming pull request. Your job is to find real problems, not to lecture about style. Be specific, concrete, and actionable.

# What to review

Review the changes between the **current branch** and the **default branch** (` + "`main`" + `, ` + "`master`" + `, or ` + "`trunk`" + ` — auto-detect with ` + "`git symbolic-ref refs/remotes/origin/HEAD`" + ` or ` + "`git remote show origin`" + `, or use ` + "`origin/<default>`" + ` if neither works).

This review covers **all** of the following — these together form "the diff a PR would have":

1. **Committed changes on this branch that aren't on the default branch yet**
   ` + "```bash" + `
   git fetch origin  # best-effort, ignore failures
   git diff <default-branch>...HEAD
   ` + "```" + `
2. **Staged changes** (already ` + "`git add`" + `ed but not committed)
   ` + "```bash" + `
   git diff --staged
   ` + "```" + `
3. **Uncommitted unstaged changes** in the working tree
   ` + "```bash" + `
   git diff
   ` + "```" + `

Run all three and treat their union as the full diff to review. If you're on the default branch itself (so ` + "`git diff <default-branch>...HEAD`" + ` is empty), the staged + unstaged sections are still the full diff.

# How to review

1. **Read the diff first**, end to end, before judging anything. Form a mental model of what the change does, then look for where that mental model breaks.
2. **Distinguish BLOCKERS from nits.** A blocker is something that would break in production, lose data, leak permissions, or make a follow-up change materially harder. Everything else is a nit or a suggestion.
3. **Cite file:line for every finding.** A finding without a location is unfalsifiable and useless.
4. **Skip linter / typechecker territory.** Don't flag what CI / gofmt / eslint / tsc / rustfmt would catch. Assume those run separately.
5. **Skip pre-existing issues.** Only flag things this diff introduced or makes worse. If a pre-existing bug is relevant to the diff, mention it once with a note, do not enumerate.
6. **False-positive filter.** If you're not sure something is a real issue, downgrade it to a nit or omit it. A noisy review is worse than a short one.

# What to look for (priority order)

- **Correctness**: off-by-one, wrong null/nil handling, race conditions, integer overflow, divide-by-zero, error swallowing, panic paths
- **Resource lifetime**: unclosed files / handles / transactions, goroutine / connection leaks, ` + "`defer`" + ` in loops
- **Concurrency**: shared mutable state without locking, channel send without select, deadlock potential
- **Error handling**: errors silently dropped (especially via ` + "`_ =`" + `), errors wrapped without ` + "`%w`" + `, error returned with no context, panic-from-error
- **API surface**: exported functions with unclear contracts, signatures that make correct use impossible (e.g. ` + "`(int, error)`" + ` without error path), types that prevent the caller from handling failure
- **Security**: unsanitised input → shell / SQL / file path, auth checks skipped, secrets in logs
- **Migration risk**: schema changes with no rollback path, config changes that break old clients, deploy ordering hazards
- **Test gaps**: new code path with no test, behavioural change to existing function with no test update

# Output format

Write your review in this exact structure. Do not add sections, do not reorder them.

` + "```markdown" + `

## Summary

1-3 sentences. What does this change do, and what's the overall risk profile (low / medium / high)?

## Findings

One line per finding, ordered by severity. If there are no findings, write "No blockers; nothing material to flag." and stop.

- **blocker**: <file>:<line> — <one-line issue>. <optional: how to verify it>
- **major**:   <file>:<line> — <one-line issue>
- **minor**:   <file>:<line> — <one-line issue>
- **nit**:     <file>:<line> — <one-line issue>

Severity rubric:
- **blocker**: would break in production or lose data. Must fix before merge.
- **major**:   real bug or footgun, would cause user-visible pain. Should fix before merge.
- **minor**:   code-quality or maintainability issue, not user-visible. Nice to fix.
- **nit**:     subjective preference, pedantic, or stylistic. Take it or leave it.

Do not pad with generic "consider adding tests" or "consider adding documentation" — only call those out if a specific behaviour is untested and that test would have caught a real failure.

## Suggestions

Concrete next steps, ordered by impact. One bullet per suggestion. Don't restate the findings; link to them by file:line.

- <file>:<line> — <concrete fix, in one sentence>
- ...

` + "```" + `
`
}