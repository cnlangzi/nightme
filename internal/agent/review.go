// review.go — /review slash command support: prompt templates and
// the pure-prompt Review runner.
//
// /review runs a code review and injects findings back into the
// main chat session. Three review strategies exist (docs/REVIEW.md §2):
//
//   - ReviewWithNative: per-bridge in bridge packages (claudecode/codex
//     invoke their built-in /codereview / codex review commands).
//   - ReviewWithOcr: in review_with_ocr.go; ocr delegation flow.
//   - ReviewWithPrompt: in this file; pure-prompt path used when ocr
//     isn't installed or workspace precompute fails.
//
// All three fans out via the same multi-job machinery in
// review_with_ocr.go::delegateReviewMultiJob when ≥2 review dimensions
// are involved. simplify is one such dimension.
//
// Architecture invariants:
//   - RunOnce is the isolation mechanism (fresh subprocess, own context).
//   - FormatReviewMessage wraps findings before injection into AS + channel.
//   - pty/bash returns ErrReviewNotSupported.
//   - timeouts.Review (30 min) bounds the whole review.
//   - /review takes no args besides --agent.

package agent

import (
	"context"
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

// BuiltinPrompt is the nightme-owned canonical review prompt. Used by:
//   - ReviewWithPrompt's primary group (as the group's Rule text)
//   - ReviewWithOcr's fallback path when ocr produces no rule groups
//   - ReviewWithPrompt's single-RunOnce fallback when workspace is empty
//
// ~70 lines of markdown, structured as Summary / Findings / Suggestions
// with severity tags. Drawn from Claude Code /code-review plugin shape
// (confidence-based scoring, finding-first), Codex review subcommand
// (severity-grouped findings), and community "senior engineer" prompts.
const BuiltinPrompt = `You are a senior engineer reviewing code changes for an upcoming pull request. Your job is to find real problems, not to lecture about style. Be specific, concrete, and actionable.

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


# Output format

Write your review in this exact structure. Do not add sections, do not reorder them.

` + "```markdown" + `

## Summary

1-3 sentences. What does this change do, and what's the overall risk profile (low / medium / high)?

## Findings

One line per finding, ordered by severity. If there are no findings, write "No blockers; nothing material to flag." and stop.

- **critical**: <file>:<line> — <one-line issue>. <optional: how to verify it>
- **high**:     <file>:<line> — <one-line issue>
- **medium**:   <file>:<line> — <one-line issue>
- **low**:      <file>:<line> — <one-line issue>

Severity rubric (must match the output schema in the Output format section below):
- **critical**: would break in production or lose data. Must fix before merge.
- **high**:     real bug or footgun, would cause user-visible pain. Should fix before merge.
- **medium**:   code-quality or maintainability issue, not user-visible. Nice to fix.
- **low**:      subjective preference, pedantic, or stylistic. Take it or leave it.

Do not pad with generic "consider adding tests" or "consider adding documentation" — only call those out if a specific behaviour is untested and that test would have caught a real failure.

## Suggestions

Concrete next steps, ordered by impact. One bullet per suggestion. Don't restate the findings; link to them by file:line.

- <file>:<line> — <concrete fix, in one sentence>
- ...

` + "```" + `
`

// simplifyPrompt is the nightme-owned simplify review lens — a focused,
// structured set of axes (reuse / simplification / efficiency / altitude)
// complementary to ocr's per-language rules and the builtin rubric.
// Sourced from Claude Code's /simplify skill axes; refined for review
// (not refactor) — produces findings, does not auto-apply.
//
// Wired into both ReviewWithOcr and ReviewWithPrompt as a parallel
// reviewGroup (Pattern = patternSimplify), so it runs as a separate
// RunOnce concurrently with the main dimension(s). Not in
// ReviewWithNative (per-bridge native review handles its own prompt).
const simplifyPrompt = `# Simplify review lens (nightme-owned, complementary)

This lens reviews the diff for **simplification opportunities** that are orthogonal to correctness/performance/security findings: code that works but can be made simpler, smaller, or more idiomatic without changing behavior.

#### Reporting discipline

- Report a finding only when the diff shows the issue directly. Do not speculate about "could be simpler if" without showing the concrete code.
- Prefer one concrete suggestion per finding, not a list of alternatives.
- Cite ` + "`path:start_line-end_line`" + ` for every finding. If the suggestion spans multiple files, list each affected location in a single finding.
- Skip findings that a formatter / linter already catches (gofmt, prettier, ruff --fix, etc.). Simplify is for **structural** cleanups, not whitespace.
- Findings here use category ` + "`maintainability`" + ` (or ` + "`style`" + ` for subjective ones); severity defaults to ` + "`low`" + ` unless the diff introduces the issue at production-critical paths.

## Axis 1 — Reuse (DRY without dogma)

- **Duplicate logic** in the diff that already exists elsewhere in the same package or sibling package. When duplication is local (one file), prefer extracting a private helper. When it spans packages, propose a single canonical location and reference existing call sites.
- **Reinvented helper**: code that re-implements something already exported from an imported package or a sibling internal package. Reference the existing function with ` + "`Use <pkg>.<Func>`" + ` instead of writing a new one.
- **Inline copies of a small constant / message / error string** that exists in one authoritative place nearby. Suggest referencing the source instead of duplicating.
- **Two near-identical branches in an if/else** that differ only in a single parameter or receiver. Suggest a small loop, table, or generic dispatch.
- **Do NOT flag** minor duplication that exists for clarity, for boundary separation between layers, or where deduplication would create a more coupled abstraction than the duplication itself.

## Axis 2 — Simplification (less code, less indirection)

- **Dead code introduced in the diff**: unused variables, unreachable branches, functions exported but never called outside the file, helpers added "just in case" with no caller. Cite the unused symbol and its scope.
- **Over-engineered abstraction**: interfaces with one implementation, generic wrappers around a single concrete type, configurable factories with exactly one option, builder patterns that build a single struct literally. Suggest using the concrete thing directly.
- **Layering leaks**: business logic in transport code, transport concerns in domain logic, SQL strings in HTTP handlers, etc. Suggest the layer that should own the logic.
- **Premature parameterization**: functions that take parameters no caller uses, options structs with optional fields that are never set, pluggable strategies with one strategy. Suggest removing until a second use appears.
- **Manual work that a stdlib or imported helper does**: re-implementing ` + "`strings.HasPrefix`" + ` with manual slicing, custom ` + "`slices.Contains`" + ` loops, hand-rolled ` + "`sync.Once`" + ` patterns when ` + "`defer`" + `-able helpers exist, etc.
- **Boilerplate from a missing language feature**: a 10-line switch that could be a map literal, a verbose if/else that could be a ternary expression in languages that support it, manual null checks where the type system already enforces non-null.
- **Layered error wrapping that adds no information**: wrapping a single error with ` + "`%w`" + ` (or ` + "`%v`" + `) and no extra context. Suggest removing the wrap or moving context into the message itself.
- **Do NOT flag** intentional abstractions that are aligned with project conventions, even if a more concrete version would be shorter.

## Axis 3 — Efficiency (real wins, not micro-opts)

Focus on changes that have **observable** impact in the diff's hot path or that obviously scale poorly. Skip theoretical micro-optimizations.

- **Accidental O(n²) where O(n) is trivial**: nested loops over the same slice, repeated linear lookups in a map that should be built once, repeated ` + "`strings.Contains`" + ` over a long string that could be a precomputed set, etc.
- **Unnecessary allocation in a tight loop**: building a fresh slice/map on every iteration when it could be hoisted, repeated ` + "`[]byte(s)`" + ` conversions inside a loop, repeated ` + "`json.Marshal`" + ` of the same struct per request.
- **Re-reading a stable value**: file I/O, network call, or heavy compute inside a loop where the result is loop-invariant. Suggest hoisting.
- **Defer in a tight loop**: ` + "`defer`" + ` inside a hot loop defers cleanup to function exit and can balloon memory in long loops. Suggest hoisting the resource into a scoped block.
- **Synchronization that is no longer needed**: mutex protecting a value that is now immutable after construction, atomic counter wrapped around a field that is only touched by a single goroutine post-init.
- **Redundant work across requests**: per-request recomputation of values that depend only on config or startup state; reload of a file that is never modified at runtime.
- **Do NOT flag** micro-opts that would clutter the code for < 1% gain, or hot-path changes that the change author cannot validate without profiling.

## Axis 4 — Altitude (structure and pattern)

Higher-altitude observations: when a small change exposes that the surrounding structure is the actual problem.

- **Wrong layering for the change**: a single-line bug fix that requires editing five files because the responsibility is in the wrong layer. Suggest the structural move, even if it is out of scope for this diff.
- **Leaky abstraction**: a new public field, method, or error variant that exposes an internal detail (storage format, transport type, vendor SDK shape) the caller should not see. Suggest a domain-shaped wrapper.
- **Inconsistent style with the surrounding file**: the diff introduces a new error-handling style, logging pattern, or naming convention that differs from the file's neighbors. Suggest matching local convention before the inconsistency calcifies.
- **Mixed abstraction levels**: a function that switches between high-level business steps and low-level byte manipulation within ten lines. Suggest splitting.
- **Tests that test the implementation rather than the behavior**: assertions on internal call counts, on the exact structure of an error message, on private helper invocations. Suggest testing observable behavior.
- **Configuration that should be code or vice versa**: a feature flag for behavior that never changes, or hardcoded constants that vary per environment and should be in config.
- **Do NOT flag** altitude observations for trivial diffs. Altitude findings are valuable when the diff is part of a recurring pattern; they are noise on a one-line typo fix.

## Severity guidance

| Severity | When to use |
| --- | --- |
| ` + "`blocking`" + ` | The current code is actively wrong (bug, race, leak, security) and the only clean fix is the refactor you are proposing. Rare. |
| ` + "`warning`" + `  | Real maintainability / efficiency cost that will compound as the code is touched again. The default for non-trivial findings. |
| ` + "`style`" + `    | Subjective cleanup. Phrase as a question ("could ` + "`X`" + ` be replaced with ` + "`Y`" + `?") rather than a directive. |
| ` + "`suggestion`" + ` | Optional improvement. Mention only if you have high confidence it is genuinely better. |

## Output schema reminder

Findings must conform to the nightme review output schema (see host agent prompt). One finding per ` + "`path:start_line-end_line`" + ` location; cite the concrete code in ` + "`content`" + `; pick exactly one ` + "`category`" + ` (` + "`reuse`" + `, ` + "`simplification`" + `, ` + "`efficiency`" + `, or ` + "`altitude`" + `).
`

// ReviewWithPrompt runs review using only the builtin prompt, no ocr,
// no precompute-from-delegate-review. Used by:
//
//   - ReviewWithOcr's fallback path when ocr isn't installed or returns
//     no rule groups.
//   - Direct callers who want a "no ocr" baseline review.
//
// Always fans out into ≥2 reviewGroups (builtin + simplify) via
// delegateReviewMultiJob, so the per-group prompts render the full
// workspace diff under each lens.
//
// When workspace is empty (no git repo / not a directory), falls back
// to a single RunOnce with BuiltinPrompt alone (no files = no fan-out
// payload to split across goroutines).
// ReviewWithPrompt runs review without ocr (Go-replicated path). It
// calls precomputeReviewWithBuiltin, which populates reviewContext
// using Go-side git commands (collectWorkspaceFiles + a synthesized
// builtin group) instead of delegating to ocr. The output shape
// matches precomputeReviewWithOcr's — so the fan-out machinery is
// identical regardless of path.
//
// Used by:
//   - delegate-tier bridges (dsh/pi/opencode/cursor/acp) when ocr
//     isn't on $PATH. The bridge's Starter.Review dispatches:
//     `if agent.OcrAvailable() { ReviewWithOcr } else { ReviewWithPrompt }`.
//
// Edge case — empty workspace: precomputeReviewWithBuiltin returns an
// empty reviewContext (no reviewable, no ocrGroups). The function still
// calls delegateReviewMultiJob with `groups = append(pre.ocrGroups,
// simplifyGroup(nil))` = `[simplifyGroup(nil)]` — a one-element slice.
// delegateReviewMultiJob does not check len(groups) >= 2; it spawns one
// goroutine, calls assembleGroupPrompt (returns "" because rc.workspace
// is ""), and the goroutine falls back to BuiltinPrompt text via the
// `prompt == "" → builtinPrompt` guard. Net effect: one RunOnce with
// BuiltinPrompt — no fan-out payload to split, no findings possible.
//
// simplify always runs alongside as a parallel dimension (Pattern =
// patternSimplify), appended after pre.ocrGroups — see
// patternSimplify / simplifyGroup in review_with_ocr.go.
func ReviewWithPrompt(ctx context.Context, s Starter, cfg StartConfig, opts ...RunOnceOption) (RunResult, error) {
	pre := precomputeReviewWithBuiltin(ctx, cfg.Workspace)
	// Short-circuit on empty diff: zero reviewable + zero untracked
	// means precompute produced no files for any goroutine to slice.
	// Without this guard, fan-out collapses to [simplifyGroup(nil)]
	// — one RunOnce that ships a prompt with empty file context to
	// the agent, which can only answer "0 covered, 0 findings".
	// Returning ErrNoDiff lets the /review dispatcher surface a
	// clean inline message instead of burning a dsh session spawn.
	if len(pre.reviewable) == 0 && len(pre.untracked) == 0 {
		return RunResult{}, ErrNoDiff
	}
	groups := append(pre.ocrGroups, simplifyGroup(pre.reviewable))
	return delegateReviewMultiJob(ctx, s, cfg, pre, groups, opts...)
}

// (listWorkspaceFiles deleted — precomputeReview already populates
// reviewable via collectReviewableFiles in the no-ocr branch; see
// review_with_ocr.go.)
