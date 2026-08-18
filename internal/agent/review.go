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
	"errors"
	"fmt"
)

// ErrReviewNotSupported is returned by Starter.Review when the agent
// type cannot do review (currently pty/bash fallback).
//
// The /review dispatcher surfaces this as a friendly
// "agent X does not support /review" reply — it doesn't poison the
// chat with a generic bridge error.
var ErrReviewNotSupported = errors.New("agent: /review not supported")

// FormatReviewMessage wraps the raw review output in a small
// preamble so the main agent (which receives this as a user
// message) understands the context. The preamble tells the main
// agent where the review came from, who ran it, and what scope
// it covered — important because the main agent then answers
// follow-up questions like "fix the second blocker" using this
// review as its source of truth.
//
// "Run by" annotates the agent that produced the review — the
// review goroutine may have run on a different agent (via
// `/review --agent <name>`) than the current selectedAS where
// the findings land. Tracking the runner is essential for
// reproducibility ("which agent's review is this?") and for the
// user when they re-`/use` between review and fix.
//
// Exported so bridges that override Starter.Review (e.g. claudecode
// using its native /code-review command, codex using codex review)
// can produce the same preamble when injecting their native output
// into the main chat session.
func FormatReviewMessage(workspace, agentName, review string) string {
	return fmt.Sprintf("## Code review of %s (run by %q)\n\n"+
		"(current branch vs default branch; run via /review)\n\n%s",
		workspace, agentName, review)
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
7. **Look for simplification opportunities, not just problems.** Code that "works" can still be 2x shorter. The Suggestions section is where you flag dead code, redundant logic, over-abstraction, and clearer names. Don't just complain about what the code does wrong — propose what could be removed or simplified.

# What to look for (priority order)

- **Correctness**: off-by-one, wrong null/nil handling, race conditions, integer overflow, divide-by-zero, error swallowing, panic paths
- **Resource lifetime**: unclosed files / handles / transactions, goroutine / connection leaks, ` + "`defer`" + ` in loops
- **Concurrency**: shared mutable state without locking, channel send without select, deadlock potential
- **Error handling**: errors silently dropped (especially via ` + "`_ =`" + `), errors wrapped without ` + "`%w`" + `, error returned with no context, panic-from-error
- **API surface**: exported functions with unclear contracts, signatures that make correct use impossible (e.g. ` + "`(int, error)`" + ` without error path), types that prevent the caller from handling failure
- **Security**: unsanitised input → shell / SQL / file path, auth checks skipped, secrets in logs
- **Migration risk**: schema changes with no rollback path, config changes that break old clients, deploy ordering hazards
- **Efficiency**: algorithmic slowness (O(n²) when O(n) is one map away), unnecessary repeated work (recompute on every loop iteration, N+1 queries, redundant IO), missed batching opportunities, hot-path allocations (fmt.Sprintf in inner loops, slice growth without preallocation). Distinct from Resource lifetime — Efficiency is "this works but is slow", Lifetime is "this leaks when it shouldn't".
- **Test gaps**: new code path with no test, behavioural change to existing function with no test update
- **Simplification** (covers claude /code-review's "reuse" + "simplification" pillars): redundant code (DRY violations, dead code, unused parameters/imports), **prefer existing helpers/APIs over new code** (the reviewer's first question should be "is there a util for this already?"), over-abstraction (interfaces/types with one impl, factories that always return the same concrete), functions doing too much (SRP — flag if a function has multiple unrelated responsibilities), naming clarity (ambiguous names, names that lie about what they do), unnecessary indirection. **This is the Suggestions section's main fuel** — without it, "## Suggestions" tends to be empty. Code that "works" can still be 2x shorter.

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