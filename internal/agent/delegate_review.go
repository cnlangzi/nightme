// delegate_review.go — shared Review body for delegate-tier bridges.
//
// Implements the three-tier dispatch from docs/REVIEW.md §2, aligned
// with ocr's official Delegation Mode SKILL (skills/open-code-review-
// delegate/SKILL.md):
//
//   Tier 1 (native)   — codex/claudecode override Starter.Review; this
//                       helper is NOT called for them.
//   Tier 2 (ocr)     — delegate bridges (dsh/pi/acp/opencode/cursor)
//                       when `ocr` is on $PATH: follow the SKILL's
//                       Step 1-6 verbatim — `ocr delegate preview` for
//                       the authoritative file list + merge-base +
//                       exclusion reasons, `ocr delegate rule` (batched,
//                       per SKILL "fetch rules per-batch") for per-file
//                       rules; diff via the preview's merge-base (a
//                       commit hash, always resolvable).
//   Tier 3 (prompt)   — same delegate bridges when `ocr` is absent:
//                       Go-side best-effort reprise of ocr's engineering
//                       (default-branch detect as a resolvable ref,
//                       merge-base, the three diffs incl. untracked, a
//                       noise-filtered file list, the built-in rubric).
//
// `ocr` is an external tool (like git) — NEVER a bridge/agent. ocr's
// delegation mode is LLM-free: it emits only deterministic engineering
// (file list + rules) as JSON; the host agent (this bridge's agent)
// supplies the LLM via RunOnce. No second LLM config.
//
// Every step degrades gracefully: ocr missing/failed → Tier 3 Go
// reprise; precompute totally failed (no workspace) → StandardPrompt
// verbatim. /review never hard-fails on precompute.

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/proc"
)

// maxDiffLines caps any single diff section fed into the prompt. A huge
// PR's full diff can run tens of thousands of lines and blow the host
// agent's context window; beyond this cap the diff is truncated with a
// pointer that tells the agent to read the affected file directly for
// the omitted context. This is the single lever that keeps /review
// stable on large changesets within a single RunOnce (full per-rule-
// group batching across multiple RunOnce calls is left to Phase C).
const maxDiffLines = 2000

// DelegateReview is the shared Review entry point for delegate-tier
// bridges (dsh / pi / acp / opencode / cursor). It runs the three-tier
// dispatch described in docs/REVIEW.md §2 and returns the RunResult
// from the bridge's own RunOnce — the host agent's LLM drives the
// review. Native bridges (codex / claudecode) bypass this entirely;
// their Starter.Review override invokes the native review command.
//
// opts are forwarded to RunOnce unchanged (event sinks, etc.). The
// error is wrapped with the agent name, matching the previous
// per-bridge Review bodies so the /review dispatcher's error surfacing
// is unchanged.
func DelegateReview(ctx context.Context, s Starter, cfg StartConfig, opts ...RunOnceOption) (RunResult, error) {
	pre := precomputeReview(ctx, cfg.Workspace)

	prompt := assembleReviewPrompt(pre)
	if prompt == "" {
		// Total precompute failure (no workspace): fall back to the
		// legacy static prompt so /review still works, just without
		// the Go-side precompute enhancements.
		prompt = StandardPrompt()
	}

	result, err := s.RunOnce(ctx, cfg, []ContentBlock{{
		Type: ContentText,
		Text: prompt,
	}}, opts...)
	if err != nil {
		return RunResult{}, fmt.Errorf("agent %s: review one-shot failed: %w",
			s.Info().Name, err)
	}
	return result, nil
}

// reviewContext is the Go-side deterministic precompute shared by
// Tier 2 and Tier 3. Every field is best-effort: empty/zero means
// "could not compute; assembleReviewPrompt omits that section".
// assembleReviewPrompt returns "" only when workspace is empty — the
// signal that DelegateReview should fall back to StandardPrompt.
type reviewContext struct {
	workspace     string
	defaultBranch string // resolvable ref "origin/main"; "" = could not detect
	mergeBase     string // commit hash; "" = could not compute
	committedDiff string // git diff <mergeBase>..HEAD (or base...HEAD fallback)
	stagedDiff    string // git diff --staged
	unstagedDiff  string // git diff
	reviewable    []string
	excluded      []string // "path (reason)" entries; Tier 2 only
	// Tier 2 only (populated when ocr is available + produced rules):
	ocrRules string // formatted rule groups from `ocr delegate rule`
}

func precomputeReview(ctx context.Context, workspace string) reviewContext {
	rc := reviewContext{workspace: workspace}
	if workspace == "" {
		return rc
	}

	// Default branch — common to Tier 2 & 3. Returns a resolvable ref
	// ("origin/main"), NOT a bare name ("main"), so the diff/merge-base
	// commands below don't fail on a checkout that only has the remote
	// tracking branch (the common case on a feature branch).
	base := detectDefaultBranch(ctx, workspace)
	rc.defaultBranch = base
	if base != "" {
		// merge-base via the resolvable ref; "" on failure (non-fatal).
		rc.mergeBase = runGit(ctx, workspace, "merge-base", base, "HEAD")
	}

	// Tier 2: ocr delegation (LLM-free). Per ocr's SKILL Step 1, the
	// preview is the authoritative source for the reviewable file
	// list + merge-base + exclusion reasons. We hand it the same
	// resolvable base ref so ocr's FileFilter (more precise than our
	// Go isReviewablePath heuristic) + untracked handling apply.
	// Failure is non-fatal: we fall through to the Tier 3 Go reprise.
	if base != "" && ocrAvailable() {
		if preview, ok := runOcrDelegatePreview(ctx, workspace, base, "HEAD"); ok {
			rc.reviewable = preview.reviewable
			rc.excluded = preview.excluded
			if preview.mergeBase != "" {
				// ocr's merge-base wins: it ran the same resolution
				// but with its own ref-validation, so it's strictly
				// more authoritative than our runGit above.
				rc.mergeBase = preview.mergeBase
			}
			// SKILL Step 2: rules per-batch. ocr groups rules by
			// content; we fetch in ≤50-path batches to stay well
			// under ARG_MAX on large changesets (SKILL: "fetch rules
			// per-batch as you review"). Single-batch failure is
			// non-fatal — other batches still contribute rules.
			rc.ocrRules = runOcrDelegateRulesBatched(ctx, workspace, rc.reviewable)
		}
	}

	// Tier 3 Go reprise: ocr absent, or preview failed. Collect the
	// file list ourselves (incl. untracked — `git diff --name-only`
	// alone misses newly-added files, which SKILL Step 3 workspace
	// mode explicitly reads directly).
	if rc.reviewable == nil {
		rc.reviewable = collectReviewableFiles(ctx, workspace, base)
	}

	// The three diffs whose union = "the diff a PR would have".
	// Prefer the merge-base commit hash (two-dot, always resolvable)
	// over the symbolic base ref — this is the fix for the bare-name
	// resolution failure that previously emptied committedDiff on a
	// feature branch with no local `main`.
	switch {
	case rc.mergeBase != "":
		rc.committedDiff = truncateDiff(runGit(ctx, workspace, "diff", rc.mergeBase+"..HEAD"))
	case base != "":
		rc.committedDiff = truncateDiff(runGit(ctx, workspace, "diff", base+"...HEAD"))
	}
	rc.stagedDiff = truncateDiff(runGit(ctx, workspace, "diff", "--staged"))
	rc.unstagedDiff = truncateDiff(runGit(ctx, workspace, "diff"))

	return rc
}

// runGit runs a git command in workspace and returns trimmed stdout.
// "" on any error — callers treat empty as "skip that section". Uses
// proc.New for cross-platform spawn: Windows .cmd shim handling +
// CREATE_NO_WINDOW (no flashing console), Unix Setsid (detaches from
// the daemon's controlling TTY so a hung child can't wedge kqueue).
// `git -C <workspace>` is used instead of cmd.Dir so git's own
// relative-path resolution anchors at the workspace (matches the
// chatsession GitStatus path).
func runGit(ctx context.Context, workspace string, args ...string) string {
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := proc.New(c, "git", append([]string{"-C", workspace}, args...)...)
	out, _ := cmd.Output() // error swallowed: "" = "skip section", by design
	return strings.TrimSpace(string(out))
}

// detectDefaultBranch resolves origin's default branch as a RESOLVABLE
// ref ("origin/main"), not a bare name. This is critical: `git diff
// main...HEAD` fails on a checkout that only has the remote tracking
// branch (the common case on a feature branch), while
// `git diff origin/main...HEAD` resolves. Prefers
// `git symbolic-ref refs/remotes/origin/HEAD` (cheap, local, returns
// "refs/remotes/origin/main"), falls back to `git remote show origin`'s
// "HEAD branch:" line (network round-trip; only on symbolic-ref
// failure). Returns "" if neither works — caller drops to workspace
// mode.
func detectDefaultBranch(ctx context.Context, workspace string) string {
	out := runGit(ctx, workspace, "symbolic-ref", "refs/remotes/origin/HEAD")
	// out: "refs/remotes/origin/main" (symbolic-ref prints the ref
	// name directly; the "ref: " prefix is the .git/HEAD FILE format,
	// not this command's output, but TrimPrefix is a harmless no-op
	// if the prefix is absent).
	out = strings.TrimSpace(strings.TrimPrefix(out, "ref:"))
	if out != "" {
		if ref := stripRefsRemotes(out); ref != "" && ref != "HEAD" {
			return ref
		}
	}
	// Fallback: parse `git remote show origin`.
	out = runGit(ctx, workspace, "remote", "show", "origin")
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "HEAD branch:") {
			branch := strings.TrimSpace(strings.TrimPrefix(line, "HEAD branch:"))
			if branch != "" && branch != "(unknown)" {
				return "origin/" + branch
			}
		}
	}
	return ""
}

// stripRefsRemotes turns "refs/remotes/origin/main" into "origin/main"
// — a ref that resolves on a checkout with only the remote tracking
// branch. Returns s unchanged if it doesn't match the refs/remotes/
// prefix (caller then treats it as already-short).
func stripRefsRemotes(s string) string {
	const prefix = "refs/remotes/"
	if strings.HasPrefix(s, prefix) {
		return strings.TrimPrefix(s, prefix)
	}
	return s
}

// collectReviewableFiles is the Tier 3 Go reprise of ocr's preview
// file selection. Returns the deduped, noise-filtered list of changed
// paths across committed (base...HEAD), staged, unstaged, AND untracked
// (`git ls-files --others --exclude-standard`). The untracked leg is
// required because `git diff --name-only` omits newly-added files that
// have never been staged — ocr's preview includes them, and so must we
// to honour the coverage mandate on the full PR diff.
func collectReviewableFiles(ctx context.Context, workspace, base string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	if base != "" {
		for _, f := range strings.Split(runGit(ctx, workspace,
			"diff", "--name-only", base+"...HEAD"), "\n") {
			add(f)
		}
	}
	for _, f := range strings.Split(runGit(ctx, workspace,
		"diff", "--staged", "--name-only"), "\n") {
		add(f)
	}
	for _, f := range strings.Split(runGit(ctx, workspace,
		"diff", "--name-only"), "\n") {
		add(f)
	}
	// Untracked new files (never staged). `--others` lists files Git
	// doesn't track; `--exclude-standard` honours .gitignore so we
	// don't pull in build output. Mirrors ocr's workspace-mode
	// untracked handling (SKILL Step 3).
	for _, f := range strings.Split(runGit(ctx, workspace,
		"ls-files", "--others", "--exclude-standard"), "\n") {
		add(f)
	}
	filtered := make([]string, 0, len(out))
	for _, f := range out {
		if isReviewablePath(f) {
			filtered = append(filtered, f)
		}
	}
	return filtered
}

// isReviewablePath drops well-known noise directories. Conservative:
// only paths whose normalised form contains a known noise segment are
// excluded; everything else is left to the LLM to judge. Tier 2 uses
// ocr's (richer) FileFilter instead; this is the Tier 3 fallback.
func isReviewablePath(p string) bool {
	norm := "/" + p + "/"
	for _, seg := range []string{
		"/generated/", "/testdata/", "/vendor/", "/third_party/",
		"/node_modules/", "/.git/", "/dist/", "/build/",
	} {
		if strings.Contains(norm, seg) {
			return false
		}
	}
	return true
}

// ocrPreview is the parsed `ocr delegate preview --format json` output
// (only the fields DelegateReview consumes). See delegate_cmd.go's
// delegatePreviewJSON for the full schema.
type ocrPreview struct {
	mode       string
	mergeBase  string
	reviewable []string
	excluded   []string // "path (reason)"
}

// runOcrDelegatePreview runs `ocr delegate preview --format json
// --from <from> --to <to>` in workspace (SKILL Step 1). Returns the
// authoritative reviewable file list + merge-base (a commit hash,
// always resolvable — the fix for the bare-name bug) + excluded
// files with reasons. ok=false on any failure (caller falls through
// to the Tier 3 Go reprise).
func runOcrDelegatePreview(ctx context.Context, workspace, from, to string) (ocrPreview, bool) {
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := []string{"delegate", "preview", "--format", "json"}
	if from != "" && to != "" {
		args = append(args, "--from", from, "--to", to)
	}
	cmd := proc.New(c, "ocr", args...)
	cmd.Dir = workspace
	out, err := cmd.Output()
	if err != nil {
		return ocrPreview{}, false
	}
	var resp struct {
		Mode       string `json:"mode"`
		MergeBase  string `json:"merge_base"`
		Reviewable []struct {
			Path string `json:"path"`
		} `json:"reviewable_files"`
		Excluded []struct {
			Path          string `json:"path"`
			ExcludeReason string `json:"exclude_reason"`
		} `json:"excluded_files"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return ocrPreview{}, false
	}
	p := ocrPreview{mode: resp.Mode, mergeBase: resp.MergeBase}
	for _, f := range resp.Reviewable {
		if f.Path != "" {
			p.reviewable = append(p.reviewable, f.Path)
		}
	}
	for _, f := range resp.Excluded {
		if f.Path != "" {
			reason := f.ExcludeReason
			if reason == "" {
				reason = "excluded"
			}
			p.excluded = append(p.excluded, f.Path+" ("+reason+")")
		}
	}
	if resp.Mode == "" && len(p.reviewable) == 0 {
		return ocrPreview{}, false
	}
	return p, true
}

// ocrAvailable reports whether the `ocr` CLI is on $PATH. Uses
// exec.LookPath (not proc.New) because this is a pure existence check
// — no spawn, no cross-platform spawn recipe needed. LookPath honours
// PATHEXT on Windows, so "ocr" resolves to ocr.cmd / ocr.exe.
func ocrAvailable() bool {
	_, err := exec.LookPath("ocr")
	return err == nil
}

// ruleBatchSize caps the number of file paths passed to a single
// `ocr delegate rule` invocation. ocr's SKILL says "fetch rules
// per-batch"; a large changeset's full path list can exceed ARG_MAX
// (E2BIG) on some platforms. 50 is well under every common ARG_MAX
// (Linux ~128k entries × ~1k path bytes) while keeping the number of
// ocr subprocess spawns small.
const ruleBatchSize = 50

// runOcrDelegateRulesBatched runs `ocr delegate rule --format json
// <paths...>` in ≤ruleBatchSize-path batches (SKILL Step 2) and
// concatenates the rule groups. A single batch failing (ocr error,
// JSON parse error) is non-fatal — other batches still contribute
// their rules, so a transient failure on one slice doesn't blank the
// whole rule section.
func runOcrDelegateRulesBatched(ctx context.Context, workspace string, paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	var all strings.Builder
	for i := 0; i < len(paths); i += ruleBatchSize {
		end := i + ruleBatchSize
		if end > len(paths) {
			end = len(paths)
		}
		if rules, ok := runOcrDelegateRuleBatch(ctx, workspace, paths[i:end]); ok {
			all.WriteString(rules)
		}
	}
	return all.String()
}

// runOcrDelegateRuleBatch runs one `ocr delegate rule` batch and
// returns the rule groups formatted as a markdown section. ok=false
// on any failure for this batch. proc.New routes the Windows .cmd
// shim through cmd.exe /d /c (plain exec.Command would fail with
// ERROR_INVALID_PARAMETER (87) on a .cmd target).
func runOcrDelegateRuleBatch(ctx context.Context, workspace string, paths []string) (string, bool) {
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := append([]string{"delegate", "rule", "--format", "json"}, paths...)
	cmd := proc.New(c, "ocr", args...)
	cmd.Dir = workspace
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	var resp struct {
		Groups []struct {
			Pattern string   `json:"pattern"`
			Files   []string `json:"files"`
			Rule    string   `json:"rule"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return "", false
	}
	if len(resp.Groups) == 0 {
		return "", false
	}
	var b strings.Builder
	for _, g := range resp.Groups {
		fmt.Fprintf(&b, "### Rule (pattern %s) — applies to: %s\n%s\n\n",
			g.Pattern, strings.Join(g.Files, ", "), g.Rule)
	}
	return b.String(), true
}

// truncateDiff caps a diff section at maxDiffLines. Under the cap the
// diff is returned verbatim; over the cap, the head is kept and a
// truncation marker is appended pointing the agent at the affected
// files for full context. Without this, a large PR's full diff can
// exceed the host agent's context window and the review silently
// degrades (agent starts dropping findings / files).
func truncateDiff(s string) string {
	if s == "" {
		return ""
	}
	// Count newlines; +1 because a trailing line without \n still counts.
	lineCount := strings.Count(s, "\n") + 1
	if lineCount <= maxDiffLines {
		return s
	}
	// Keep the first maxDiffLines lines. IndexByte scans for \n and
	// advances idx past each line; the loop runs maxDiffLines times.
	idx := 0
	for i := 0; i < maxDiffLines; i++ {
		next := strings.IndexByte(s[idx:], '\n')
		if next < 0 {
			break
		}
		idx += next + 1
	}
	return s[:idx] + "\n…(diff truncated: " + strconv.Itoa(lineCount-maxDiffLines) +
		" more lines; read the affected files directly for full context)…\n"
}

// assembleReviewPrompt builds the enhanced review prompt from the Go-
// side precompute. Structure mirrors ocr's delegation SKILL:
// context → files → diffs → rules → how-to → output schema. Tier 2
// (ocr present) injects ocr's per-file rule groups and uses ONLY the
// language-agnostic "how to review" methodology (ocr's rules are the
// language-specific guidance). Tier 3 (ocr absent) uses the built-in
// rubric's "what to look for" list INSTEAD of ocr's rules — the two
// are never mixed, matching docs/REVIEW.md §2.4 (rules: Tier 2 ocr,
// Tier 3 built-in).
//
// Returns "" only when workspace is empty — DelegateReview then falls
// back to StandardPrompt verbatim.
func assembleReviewPrompt(rc reviewContext) string {
	if rc.workspace == "" {
		return ""
	}
	var b strings.Builder

	fmt.Fprintln(&b, "You are a senior engineer reviewing code changes for an upcoming pull request. Find real problems, not style lectures. Be specific, concrete, actionable.")

	// --- Context (precomputed; the agent must NOT re-run git) ---
	fmt.Fprintln(&b, "\n# Context (precomputed by /review — do NOT re-run git)")
	fmt.Fprintf(&b, "- workspace: %s\n", rc.workspace)
	if rc.defaultBranch != "" {
		if rc.mergeBase != "" {
			fmt.Fprintf(&b, "- base branch: %s (merge-base: %s)\n", rc.defaultBranch, rc.mergeBase)
		} else {
			fmt.Fprintf(&b, "- base branch: %s\n", rc.defaultBranch)
		}
	} else {
		fmt.Fprintln(&b, "- base branch: unknown (workspace mode — review staged + unstaged + untracked)")
	}

	// --- Files to review (coverage mandatory, SKILL Step 4 & 6) ---
	if len(rc.reviewable) > 0 {
		fmt.Fprintln(&b, "\n# Files to review")
		fmt.Fprintln(&b, "Coverage is MANDATORY: every file below must end as `reviewed` or `skipped` with a concrete reason. Do not silently omit any.")
		for _, f := range rc.reviewable {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}
	if len(rc.excluded) > 0 {
		fmt.Fprintln(&b, "\n# Excluded (do not review)")
		for _, f := range rc.excluded {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}

	// --- Diffs (precomputed) ---
	hasDiff := rc.committedDiff != "" || rc.stagedDiff != "" || rc.unstagedDiff != ""
	if hasDiff {
		fmt.Fprintln(&b, "\n# Diffs (precomputed — the union below is the full diff to review)")
		if rc.committedDiff != "" {
			label := rc.defaultBranch + "...HEAD"
			if rc.mergeBase != "" {
				label = rc.mergeBase + "..HEAD"
			}
			fmt.Fprintf(&b, "## Committed (%s)\n```\n%s\n```\n", label, rc.committedDiff)
		}
		if rc.stagedDiff != "" {
			fmt.Fprintf(&b, "## Staged\n```\n%s\n```\n", rc.stagedDiff)
		}
		if rc.unstagedDiff != "" {
			fmt.Fprintf(&b, "## Unstaged\n```\n%s\n```\n", rc.unstagedDiff)
		}
	}

	// --- Rules + how-to (Tier 2 ocr; Tier 3 built-in — never mixed) ---
	if rc.ocrRules != "" {
		// Tier 2: ocr's per-file rules ARE the language-specific
		// guidance; add only the language-agnostic methodology.
		fmt.Fprintln(&b, "\n# Review rules (matched per file by `ocr delegate` — LLM-free engineering)")
		fmt.Fprintln(&b, rc.ocrRules)
		fmt.Fprintln(&b, "\n# How to review")
		fmt.Fprint(&b, reviewHowTo())
	} else {
		// Tier 3: no ocr rules — the built-in rubric supplies both
		// the methodology and the "what to look for" priority list.
		fmt.Fprintln(&b, "\n# How to review")
		fmt.Fprint(&b, reviewHowTo())
		fmt.Fprint(&b, reviewWhatToLook())
	}

	// --- Output format (structured + coverage, SKILL Step 5 & 6) ---
	fmt.Fprintln(&b, "\n# Output format")
	fmt.Fprintln(&b, "Output a coverage summary, then one block per finding. Do not pad with generic advice.")
	fmt.Fprintln(&b, "\n```")
	fmt.Fprintln(&b, "## Coverage")
	fmt.Fprintf(&b, "- total: %d\n", len(rc.reviewable))
	fmt.Fprintln(&b, "- reviewed: <N>")
	fmt.Fprintln(&b, "- skipped: <N> (each with reason)")
	fmt.Fprintln(&b, "- coverage_rate: <N>/<total>")
	fmt.Fprintln(&b, "\n## Findings")
	fmt.Fprintln(&b, "For each finding (omit fields you cannot determine; start_line/end_line both 0 = positioning failed):")
	fmt.Fprintln(&b, "- path: <file>")
	fmt.Fprintln(&b, "- start_line: <int>")
	fmt.Fprintln(&b, "- end_line: <int>")
	fmt.Fprintln(&b, "- category: bug|security|performance|maintainability|test|style|documentation|other")
	fmt.Fprintln(&b, "- severity: critical|high|medium|low")
	fmt.Fprintln(&b, "- content: <one-line issue + how to verify>")
	fmt.Fprintln(&b, "```")
	fmt.Fprintln(&b, "\nSeverity rubric: critical = breaks in production / loses data; high = real bug, user-visible pain; medium = quality / maintainability, not user-visible; low = nit / style, take-it-or-leave-it.")
	fmt.Fprintln(&b, "If there are no findings, write \"No blockers; nothing material to flag.\" and still report coverage.")

	return b.String()
}

// reviewHowTo is the language-agnostic review methodology, shared by
// Tier 2 and Tier 3. It is the "process" part of the rubric (read
// first, distinguish blockers, cite locations, skip linter territory,
// false-positive filter, simplification) — NOT the language-specific
// "what to look for" list, which lives in reviewWhatToLook (Tier 3
// only) or in ocr's per-file rules (Tier 2).
func reviewHowTo() string {
	return `1. Read the full diff end to end before judging. Form a mental model of what the change does, then look for where that model breaks.
2. Distinguish BLOCKERS from nits. A blocker breaks in production, loses data, leaks permissions, or makes a follow-up change materially harder. Everything else is a nit or a suggestion.
3. Cite file:line for every finding. A finding without a location is unfalsifiable and useless.
4. Skip linter / typechecker territory. Don't flag what CI / gofmt / eslint / tsc / rustfmt would catch.
5. Skip pre-existing issues. Only flag things this diff introduced or makes worse.
6. False-positive filter. If you are not sure something is real, downgrade it to a nit or omit it. A noisy review is worse than a short one.
7. Look for simplification, not just problems. Dead code, redundant logic, over-abstraction, clearer names.
`
}

// reviewWhatToLook is the built-in, language-agnostic "what to look
// for" priority list — Tier 3 ONLY (ocr absent). Tier 2 substitutes
// ocr's per-file rules (which are richer AND language-specific, e.g.
// the Go rule set from `ocr delegate rule` covers errors/panics,
// nil/interfaces, context/goroutines, channels/locks, etc.) so the
// two are never mixed. Aligned with ocr's delegation SKILL categories
// (bug / security / performance / maintainability / test).
func reviewWhatToLook() string {
	return `
## What to look for (priority order)

- **Correctness**: off-by-one, wrong null/nil handling, race conditions, integer overflow, divide-by-zero, error swallowing, panic paths
- **Resource lifetime**: unclosed files / handles / transactions, goroutine / connection leaks, defer in loops
- **Concurrency**: shared mutable state without locking, channel send without select, deadlock potential
- **Error handling**: errors silently dropped (especially via _ =), errors wrapped without %w, error returned with no context, panic-from-error
- **API surface**: exported functions with unclear contracts, signatures that make correct use impossible, types that prevent the caller from handling failure
- **Security**: unsanitised input → shell / SQL / file path, auth checks skipped, secrets in logs
- **Migration risk**: schema changes with no rollback path, config changes that break old clients, deploy ordering hazards
- **Efficiency**: algorithmic slowness (O(n²) when O(n) is one map away), unnecessary repeated work, N+1 queries, hot-path allocations
- **Test gaps**: new code path with no test, behavioural change to existing function with no test update
- **Simplification**: redundant code (DRY violations, dead code, unused params/imports), prefer existing helpers over new code, over-abstraction, naming clarity, unnecessary indirection
`
}
