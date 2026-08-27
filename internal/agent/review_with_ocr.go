// review_with_ocr.go — ReviewWithOcr runner + multi-job fan-out
// machinery for the ocr delegation flow (docs/REVIEW.md §2.2).
//
// Three review runners exist in package agent (and bridge packages):
//   - ReviewWithNative: per-bridge in bridge packages (claudecode/codex
//     invoke their built-in /codereview / codex review commands).
//   - ReviewWithOcr: this file; ocr delegation flow with multi-job
//     fan-out via delegateReviewMultiJob.
//   - ReviewWithPrompt: in review.go; pure-prompt path used when ocr
//     isn't installed or workspace precompute fails.
//
// simplify runs as a parallel reviewGroup inside both ReviewWithOcr and
// ReviewWithPrompt (see patternSimplify / simplifyGroup below) — never
// in ReviewWithNative (per-bridge native review handles its own prompt).
//
// ocr is an external CLI (like git), not a bridge/agent. Its delegation
// mode is LLM-free: it emits only deterministic engineering (file list
// + rules) as JSON; the host agent (this bridge's agent) supplies the
// LLM via RunOnce. No second LLM config.
//
// Every step degrades gracefully: ocr missing/failed → ReviewWithPrompt
// fallback; precompute totally failed (no workspace) → BuiltinPrompt
// verbatim. /review never hard-fails on precompute.

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
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

// ReviewWithOcr runs review using the ocr delegation flow (docs/REVIEW.md
// §2.2 / §2.5). ocr provides deterministic engineering (file selection +
// rule matching, LLM-free); the host agent does the actual review
// reasoning with its own LLM.
//
// Pre-conditions:
//   - The caller (bridge Starter.Review) has already checked OcrAvailable
//     and routed here. ReviewWithOcr does NOT re-check — single
//     responsibility: "given ocr, do the ocr flow".
//
// Flow:
//  1. precomputeReview fills reviewable / ocrGroups / diffs.
//  2. Fan out via delegateReviewMultiJob with [ocrGroups..., simplifyGroup].
//     simplify always runs as a parallel dimension (its own RunOnce) alongside
//     the ocr-sourced groups.
//
// If precomputeReview returns empty (no workspace, no ocr groups), the
// fan-out degrades gracefully to [simplifyGroup(nil)] — one goroutine,
// one RunOnce. No internal fallback to ReviewWithPrompt; if ocr isn't
// actually available, that's a caller bug, not this function's problem.
func ReviewWithOcr(ctx context.Context, s Starter, cfg StartConfig, opts ...RunOnceOption) (RunResult, error) {
	pre := precomputeReviewWithOcr(ctx, cfg.Workspace)
	// Short-circuit on empty diff — see ReviewWithPrompt for the
	// rationale. Both runners must agree on the contract: zero
	// reviewable + zero untracked → ErrNoDiff, no agent spawn.
	if pre.isEmptyDiff() {
		return RunResult{}, ErrNoDiff
	}
	groups := append(pre.ocrGroups, simplifyGroup(pre.reviewable))
	return delegateReviewMultiJob(ctx, s, cfg, pre, groups, opts...)
}

// delegateReviewMultiJob runs one RunOnce per ocr rule group, in
// parallel (sem-capped at maxConcurrentReviewJobs), each with a
// per-group prompt + aggregator-wrapped sink. The aggregator collapses
// N per-job Ready/terminal streams into one outer lifecycle so the
// chat channel sees a single review run, not N interleaved ones.
//
// All jobs share ctx (= the parent /review revCtx): chat-session
// close or /review timeout cancels every in-flight job at once. Per-
// job errors are non-fatal — mergeRunResults records the failed
// group's status and the surviving groups' findings still flow through
// to the final deliverable.
func delegateReviewMultiJob(
	ctx context.Context,
	s Starter,
	cfg StartConfig,
	pre reviewContext,
	groups []reviewGroup,
	opts ...RunOnceOption,
) (RunResult, error) {
	// Extract the outer sink (if any) and rebuild per-job opts with
	// the aggregator's per-job sink replacing it. ParseRunOnceOptions
	// is the single source of truth for option resolution; today the
	// only RunOnceOption is WithEventSink, so this round-trip is
	// lossless. Future options would need to be re-threaded here.
	cfgOpts := ParseRunOnceOptions(opts)
	var agg *eventAggregator
	if cfgOpts.OnEvent != nil {
		agg = newEventAggregator(cfgOpts.OnEvent, len(groups))
	}

	results := make([]RunResult, len(groups))
	errs := make([]error, len(groups))
	sem := make(chan struct{}, maxConcurrentReviewJobs)

	var wg sync.WaitGroup
	for i := range groups {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			// Completion-driven counting (refactor from /review):
			// every per-job goroutine contributes exactly one
			// markJobDone, regardless of RunOnce outcome or
			// sink behavior. The last goroutine to defer runs
			// markJobDone FIRST (LIFO), so the very last one to
			// call drives doneCount to expected and fires the
			// synthetic outer Result. Without this defer,
			// doneCount is at the mercy of which sinks fire
			// which terminals -- a single broken sink strands
			// the chat lifecycle (Findings 1 & 2 from /review).
			if agg != nil {
				defer agg.markJobDone()
			}
			defer func() { <-sem }()

			prompt := assembleGroupPrompt(ctx, pre, &groups[i])
			if prompt == "" {
				prompt = BuiltinPrompt
			}

			var runOpts []RunOnceOption
			if agg != nil {
				runOpts = []RunOnceOption{WithEventSink(agg.wrapJob(fmt.Sprintf("group-%d", i+1)))}
			}

			res, err := s.RunOnce(ctx, cfg, []ContentBlock{{
				Type: ContentText,
				Text: prompt,
			}}, runOpts...)
			results[i] = res
			errs[i] = err
		}(i)
	}
	wg.Wait()

	// Finding 3 from /review: safety net for the case where
	// some per-job RunOnce never emitted a terminal event
	// (spawn failure before the sink was wired, error path
	// that bypassed the sink on some bridge, ctx cancellation
	// that orphaned the subprocess). finalize synthesizes
	// the missing terminals so doneCount reaches expected
	// and the aggregator's outer Result fires — otherwise the
	// chat lifecycle hangs at "review running…" until the
	// 30-min revCtx timeout. Idempotent.
	if agg != nil {
		agg.finalize()
	}

	return mergeRunResults(s.Info().Name, groups, results, errs)
}

// reviewContext is the Go-side deterministic precompute shared by
// Tier 2 and Tier 3. Every field is best-effort: empty/zero means
// "could not compute; assembleReviewPrompt omits that section".
// assembleReviewPrompt returns "" only when workspace is empty — the
// signal that DelegateReview should fall back to BuiltinPrompt.
type reviewContext struct {
	workspace     string
	defaultBranch string // resolvable ref "origin/main"; "" = could not detect
	mergeBase     string // commit hash; "" = could not compute
	committedDiff string // git diff <mergeBase>..HEAD (or base...HEAD fallback)
	stagedDiff    string // git diff --staged
	unstagedDiff  string // git diff
	reviewable    []string
	untracked     []string // new files never committed/staged; can't be diffed (no baseline)
	excluded      []string // "path (reason)" entries; Tier 2 only
	// Tier 2 only (populated when ocr is available + produced rules):
	ocrRules  string        // formatted rule groups from `ocr delegate rule` (markdown, single-job path)
	ocrGroups []reviewGroup // parsed structured groups (multi-job path; parallel to ocrRules)
}

// reviewGroup is one rule group from `ocr delegate rule`. Pattern is
// the glob ocr grouped by (e.g. "**/*.go"); Files is the per-group
// file list (the rule explicitly enumerates them); Rule is the rule
// text (the language-specific guidance for the group).
//
// Used by the multi-job path (docs/REVIEW.md §2.5): when ocr returns
// ≥2 groups, DelegateReview runs one RunOnce per group with this
// group's files + diff + rule — each in its own fresh subprocess /
// independent context.
type reviewGroup struct {
	Pattern string
	Files   []string
	Rule    string
}

// isEmptyDiff reports whether the precomputed context has no files
// for any goroutine to slice. Single source of truth for the
// "ErrNoDiff short-circuit" — ReviewWithPrompt and ReviewWithOcr both
// call this so the contract lives next to the field definitions. A
// nil receiver counts as empty (defensive: tests occasionally pass
// zero-value reviewContext{}).
func (rc *reviewContext) isEmptyDiff() bool {
	if rc == nil {
		return true
	}
	return len(rc.reviewable) == 0 && len(rc.untracked) == 0
}

// Sentinel Patterns for non-ocr review groups. Used by assembleGroupPrompt
// to pick the right header text (ocr groups use "Review rules (matched per
// file by `ocr delegate`...)"; builtin/simplify use their own headers).
// Real ocr groups come with their own glob (e.g. "**/*.go"); sentinels
// use the underscore prefix to avoid any future glob collision.
const (
	patternBuiltin  = "_nightme_builtin"
	patternSimplify = "_nightme_simplify"
)

// simplifyGroup builds a reviewGroup carrying the nightme-owned simplify
// lens (review.go::simplifyPrompt). Always appended to the fan-out group
// list of ReviewWithOcr and ReviewWithPrompt; runs as a parallel
// RunOnce concurrent with the main dimension(s).
func simplifyGroup(files []string) reviewGroup {
	return reviewGroup{Pattern: patternSimplify, Files: files, Rule: simplifyPrompt}
}

// precomputeReviewWithOcr populates the reviewContext using the ocr CLI
// delegation flow (ocr delegate preview + ocr delegate rule). The
// caller (ReviewWithOcr) is responsible for verifying OcrAvailable()
// before calling this — this function assumes ocr is on $PATH and
// won't fall back to git-based collection if ocr subprocess fails.
//
// Returns a reviewContext with ocr-populated fields on success
// (reviewable via ocr's FileFilter, ocrGroups per-pattern rule
// grouping, excluded + reasons, ocr-validated mergeBase). On ocr
// subprocess failure, returns a workspace-only reviewContext (no
// fallback) — callers must detect the empty ocrGroups and decide
// how to proceed (currently: degrade gracefully to simplifyGroup-only
// fan-out).
func precomputeReviewWithOcr(ctx context.Context, workspace string) reviewContext {
	rc := reviewContext{workspace: workspace}
	if workspace == "" {
		return rc
	}

	base := resolvableDefaultBranch(ctx, workspace)
	rc.defaultBranch = base
	if base != "" {
		rc.mergeBase = runGit(ctx, workspace, "merge-base", base, "HEAD")
	}

	if base != "" {
		if preview, ok := runOcrDelegatePreview(ctx, workspace, base, "HEAD"); ok {
			rc.reviewable = preview.reviewable
			rc.excluded = preview.excluded
			if preview.mergeBase != "" {
				rc.mergeBase = preview.mergeBase
			}
			rulesMarkdown, groups := runOcrDelegateRulesBatched(ctx, workspace, rc.reviewable)
			rc.ocrRules = rulesMarkdown
			rc.ocrGroups = groups
		}
	}

	fillDiffs(ctx, &rc, workspace, base)
	return rc
}

// precomputeReviewWithBuiltin populates the reviewContext without ocr,
// using Go-side git commands to replicate what ocr's preview would
// have produced. Used by ReviewWithPrompt on delegate-tier bridges
// when ocr isn't installed.
//
// The output has the same shape as precomputeReviewWithOcr — but
// `ocrGroups` is synthesized as a single builtin group (Pattern =
// patternBuiltin, Rule = BuiltinPrompt) instead of N per-pattern ocr
// groups. `excluded` is nil (Go heuristic doesn't track exclusion
// reasons). File-list precision is lower than ocr's FileFilter.
//
// File enumeration (inlined — this is the only caller, no need for
// a separate helper):
//   - reviewable: files with at least one diff (committed/staged/
//     unstaged). Safe to render with `git diff -- <file>`.
//   - untracked:   new files never committed/staged — they have no
//     baseline to diff against, so they CANNOT appear in any diff.
//     Listed separately so the LLM prompt can call them out as
//     "new file additions" instead of pretending a diff exists.
//
// `git diff <base>...HEAD -- <untracked>` returns empty (file not in
// any commit yet). `git diff -- <untracked>` also returns empty. So
// merging untracked into reviewable would silently violate the
// coverage mandate (file listed but no content for the LLM to
// review). Keeping them separate is the only honest shape.
//
// 4 git sources, dedup'd:
//   1. git diff --name-only <base>...HEAD    (committed on branch)
//   2. git diff --staged --name-only        (staged, not committed)
//   3. git diff --name-only                 (unstaged working tree)
//   4. git ls-files --others --exclude-standard  (untracked, .gitignore)
func precomputeReviewWithBuiltin(ctx context.Context, workspace string) reviewContext {
	rc := reviewContext{workspace: workspace}
	if workspace == "" {
		return rc
	}

	base := resolvableDefaultBranch(ctx, workspace)
	rc.defaultBranch = base
	if base != "" {
		rc.mergeBase = runGit(ctx, workspace, "merge-base", base, "HEAD")
	}

	// Collect reviewable + untracked from 4 git sources.
	seen := map[string]bool{}
	var changed []string
	addChanged := func(s string) {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			changed = append(changed, s)
		}
	}
	if base != "" {
		for _, f := range strings.Split(runGit(ctx, workspace,
			"diff", "--name-only", base+"...HEAD"), "\n") {
			addChanged(f)
		}
	}
	for _, f := range strings.Split(runGit(ctx, workspace,
		"diff", "--staged", "--name-only"), "\n") {
		addChanged(f)
	}
	for _, f := range strings.Split(runGit(ctx, workspace,
		"diff", "--name-only"), "\n") {
		addChanged(f)
	}
	for _, f := range strings.Split(runGit(ctx, workspace,
		"ls-files", "--others", "--exclude-standard"), "\n") {
		s := strings.TrimSpace(f)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		if isReviewablePath(s) {
			rc.untracked = append(rc.untracked, s)
		}
	}
	for _, f := range changed {
		if isReviewablePath(f) {
			rc.reviewable = append(rc.reviewable, f)
		}
	}

	if len(rc.reviewable) > 0 || len(rc.untracked) > 0 {
		// Files = reviewable only — untracked have no diff to slice on.
		// untracked is rendered separately by assembleGroupPrompt as a
		// "new file additions" section.
		rc.ocrGroups = []reviewGroup{{
			Pattern: patternBuiltin,
			Files:   rc.reviewable,
			Rule:    BuiltinPrompt,
		}}
	}

	fillDiffs(ctx, &rc, workspace, base)
	return rc
}

// fillDiffs computes the three git diffs (committed/staged/unstaged)
// that union into "the diff a PR would have". Shared helper used by
// both precomputeReviewWithOcr and precomputeReviewWithBuiltin — the
// diff content is path-agnostic (pure git), so the same logic works
// regardless of how reviewable was obtained.
func fillDiffs(ctx context.Context, rc *reviewContext, workspace, base string) {
	switch {
	case rc.mergeBase != "":
		rc.committedDiff = truncateDiff(runGit(ctx, workspace, "diff", rc.mergeBase+"..HEAD"))
	case base != "":
		rc.committedDiff = truncateDiff(runGit(ctx, workspace, "diff", base+"...HEAD"))
	}
	rc.stagedDiff = truncateDiff(runGit(ctx, workspace, "diff", "--staged"))
	rc.unstagedDiff = truncateDiff(runGit(ctx, workspace, "diff"))
}

// resolvableDefaultBranch returns origin/<defaultBranch> (e.g.
// "origin/main"), the resolvable ref form that git merge-base /
// git diff need when the local checkout only has the remote
// tracking branch. Wraps DetectDefaultBranch which returns
// the bare name; ocr's call sites pass the result to git and
// need a resolvable ref on a feature-branch checkout. Returns
// "" when no default branch can be detected (caller drops to
// workspace-only mode).
func resolvableDefaultBranch(ctx context.Context, workspace string) string {
	base := DetectDefaultBranch(ctx, workspace)
	if base == "" || strings.HasPrefix(base, "origin/") {
		return base
	}
	return "origin/" + base
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



// collectReviewableFiles is the Tier 3 Go reprise of ocr's preview
// file selection. Returns the deduped, noise-filtered list of changed
// paths across committed (base...HEAD), staged, unstaged, AND untracked
// (`git ls-files --others --exclude-standard`). The untracked leg is
// required because `git diff --name-only` omits newly-added files that
// have never been staged — ocr's preview includes them, and so must we
// to honour the coverage mandate on the full PR diff.
// collectWorkspaceFiles enumerates the workspace's changed files for review.
// Returns two disjoint slices:
//
//   - reviewable: files with at least one diff (committed/staged/unstaged).
//     Safe to render with `git diff -- <file>` and put in
//     reviewGroup.Files.
//   - untracked:   new files never committed/staged — they have no
//     baseline to diff against, so they CANNOT appear in any diff.
//     Listed separately so the LLM prompt can call them out as
//     "new file additions" instead of pretending a diff exists.
//
// `git diff <base>...HEAD -- <untracked>` returns empty (file not in any
// commit yet). `git diff -- <untracked>` also returns empty. So merging
// untracked into reviewable would silently violate the coverage mandate
// (file listed but no content for the LLM to review). Keeping them
// separate is the only honest shape.
//
// 4 git sources, dedup'd:
//   1. git diff --name-only <base>...HEAD    (committed on branch)
//   2. git diff --staged --name-only        (staged, not committed)
//   3. git diff --name-only                 (unstaged working tree)
//   4. git ls-files --others --exclude-standard  (untracked, respects .gitignore)
//
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

// OcrAvailable reports whether the `ocr` CLI is on $PATH. Uses
// exec.LookPath (not proc.New) because this is a pure existence check
// — no spawn, no cross-platform spawn recipe needed. LookPath honours
// PATHEXT on Windows, so "ocr" resolves to ocr.cmd / ocr.exe.
//
// Exported so delegate-tier bridges can dispatch their Starter.Review
// impl to either ReviewWithOcr (when this returns true) or
// ReviewWithPrompt (when false). Keeping the detection here avoids
// duplicating the LookPath call across 5 bridge packages.
func OcrAvailable() bool {
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
// returns BOTH the concatenated markdown (single-job path) AND the
// parsed structured groups (multi-job path). A single batch failing
// (ocr error, JSON parse error) is non-fatal — other batches still
// contribute their rules, so a transient failure on one slice doesn't
// blank the whole rule section.
//
// Two parallel outputs are returned so precomputeReview can populate
// BOTH rc.ocrRules (markdown, for assembleReviewPrompt) and
// rc.ocrGroups (structured, for DelegateReview's multi-job path)
// from a single ocr invocation chain — no double-spawn.
func runOcrDelegateRulesBatched(ctx context.Context, workspace string, paths []string) (string, []reviewGroup) {
	if len(paths) == 0 {
		return "", nil
	}
	var markdown strings.Builder
	var groups []reviewGroup
	for i := 0; i < len(paths); i += ruleBatchSize {
		end := i + ruleBatchSize
		if end > len(paths) {
			end = len(paths)
		}
		if md, gs, ok := runOcrDelegateRuleBatch(ctx, workspace, paths[i:end]); ok {
			markdown.WriteString(md)
			groups = append(groups, gs...)
		}
	}
	return markdown.String(), groups
}

// runOcrDelegateRuleBatch runs one `ocr delegate rule` batch and
// returns the rule groups as BOTH markdown (for the single-job
// assembleReviewPrompt path) and structured []reviewGroup (for the
// multi-job path). ok=false on any failure for this batch. proc.New
// routes the Windows .cmd shim through cmd.exe /d /c (plain
// exec.Command would fail with ERROR_INVALID_PARAMETER (87) on a
// .cmd target).
func runOcrDelegateRuleBatch(ctx context.Context, workspace string, paths []string) (string, []reviewGroup, bool) {
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := append([]string{"delegate", "rule", "--format", "json"}, paths...)
	cmd := proc.New(c, "ocr", args...)
	cmd.Dir = workspace
	out, err := cmd.Output()
	if err != nil {
		return "", nil, false
	}
	var resp struct {
		Groups []struct {
			Pattern string   `json:"pattern"`
			Files   []string `json:"files"`
			Rule    string   `json:"rule"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return "", nil, false
	}
	if len(resp.Groups) == 0 {
		return "", nil, false
	}
	var markdown strings.Builder
	var groups []reviewGroup
	for _, g := range resp.Groups {
		fmt.Fprintf(&markdown, "### Rule (pattern %s) — applies to: %s\n%s\n\n",
			g.Pattern, strings.Join(g.Files, ", "), g.Rule)
		groups = append(groups, reviewGroup{
			Pattern: g.Pattern,
			Files:   g.Files,
			Rule:    g.Rule,
		})
	}
	return markdown.String(), groups, true
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
// back to BuiltinPrompt verbatim.
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

	// --- Untracked files (new additions, no baseline) ---
	if len(rc.untracked) > 0 {
		fmt.Fprintln(&b, "\n# New file additions (no baseline — added in this changeset)")
		fmt.Fprintln(&b, "These files are untracked (never committed/staged); no diff exists. Review by reading the file contents directly if available, or skip with reason.")
		for _, f := range rc.untracked {
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

// assembleGroupPrompt builds a per-group review prompt for the
// multi-job path. Mirrors assembleReviewPrompt's structure
// (context → files → diffs → rules → how-to → output schema)
// but:
//
//   - filters the file list to the group's Files (the rule
//     explicitly enumerates them — ocr's authoritative
//     assignment, not our heuristic)
//   - filters the diff to those files via `git diff -- <files...>`
//     (this is the smart-bundling payoff: each job's prompt
//     contains only the diff slice it can actually review)
//   - uses ONLY this group's rule (Tier 2 single-group case
//     in multi-job context)
//
// ctx is the per-job goroutine's review ctx so groupFilteredDiff's
// git subprocesses inherit /close + 30-min timeout (Finding 1).
//
// Returns "" only when workspace is empty — same fallback
// signal as assembleReviewPrompt, so the caller's "fall back to
// BuiltinPrompt" branch fires uniformly.
func assembleGroupPrompt(ctx context.Context, rc reviewContext, g *reviewGroup) string {
	if rc.workspace == "" || g == nil {
		return ""
	}
	var b strings.Builder

	fmt.Fprintln(&b, "You are a senior engineer reviewing code changes for an upcoming pull request. Find real problems, not style lectures. Be specific, concrete, actionable.")

	// --- Context ---
	fmt.Fprintln(&b, "\n# Context (precomputed by /review — do NOT re-run git)")
	fmt.Fprintf(&b, "- workspace: %s\n", rc.workspace)
	if rc.defaultBranch != "" {
		if rc.mergeBase != "" {
			fmt.Fprintf(&b, "- base branch: %s (merge-base: %s)\n", rc.defaultBranch, rc.mergeBase)
		} else {
			fmt.Fprintf(&b, "- base branch: %s\n", rc.defaultBranch)
		}
	}
	fmt.Fprintf(&b, "- rule group: pattern %s (%d files)\n", g.Pattern, len(g.Files))

	// --- Files to review (group-filtered) ---
	fmt.Fprintln(&b, "\n# Files to review")
	fmt.Fprintln(&b, "Coverage is MANDATORY: every file below must end as `reviewed` or `skipped` with a concrete reason.")
	for _, f := range g.Files {
		fmt.Fprintf(&b, "- %s\n", f)
	}

	// --- Untracked files (new additions, no baseline to diff against) ---
	// Rendered only on the first group of the fan-out to avoid repetition
	// across N goroutines. These files appear in the changeset but
	// `git diff -- <file>` returns empty for them — the LLM must
	// treat them as "new file additions" rather than expecting
	// existing diff content.
	if len(rc.untracked) > 0 && g.Pattern == patternBuiltin {
		fmt.Fprintln(&b, "\n# New file additions (no baseline — added in this changeset)")
		fmt.Fprintln(&b, "These files are untracked (never committed/staged), so no diff exists. Review by reading the file contents directly if available, or skip with reason.")
		for _, f := range rc.untracked {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}

	// --- Diffs (group-filtered via `git diff -- <files...>`) ---
	//
	// Range strategy (mirrors assembleReviewPrompt §2.4):
	//   - mergeBase known → use it (always resolvable; two-dot
	//     form so a checkout with only the remote tracking ref
	//     doesn't fail)
	//   - mergeBase empty but defaultBranch known → fall back
	//     to symbolic base...HEAD three-dot (e.g. orphan branch
	//     / no common ancestor with ocr present). Finding 2
	//     from /review: previously this branch ran `git diff`
	//     with no range (= unstaged only), then mislabeled the
	//     output as "Committed (origin/main...HEAD)".
	//   - both empty → no committed section (workspace mode)
	committedArgs := []string{"diff"}
	switch {
	case rc.mergeBase != "":
		committedArgs = append(committedArgs, rc.mergeBase+"..HEAD")
	case rc.defaultBranch != "":
		committedArgs = append(committedArgs, rc.defaultBranch+"...HEAD")
	}
	committed := groupFilteredDiff(ctx, rc, g, committedArgs...)
	staged := groupFilteredDiff(ctx, rc, g, "diff", "--staged")
	unstaged := groupFilteredDiff(ctx, rc, g, "diff")
	hasDiff := committed != "" || staged != "" || unstaged != ""
	if hasDiff {
		fmt.Fprintln(&b, "\n# Diffs (precomputed — the union below is the full diff to review for THIS group)")
		if committed != "" {
			label := rc.defaultBranch + "...HEAD"
			if rc.mergeBase != "" {
				label = rc.mergeBase + "..HEAD"
			}
			fmt.Fprintf(&b, "## Committed (%s) — group-filtered\n```\n%s\n```\n", label, committed)
		}
		if staged != "" {
			fmt.Fprintf(&b, "## Staged — group-filtered\n```\n%s\n```\n", staged)
		}
		if unstaged != "" {
			fmt.Fprintf(&b, "## Unstaged — group-filtered\n```\n%s\n```\n", unstaged)
		}
	}

	// --- Rules + how-to (header depends on group source) ---
	switch g.Pattern {
	case patternSimplify:
		fmt.Fprintln(&b, "\n# Simplify review lens (nightme-owned, complementary)")
		fmt.Fprintln(&b, g.Rule)
	case patternBuiltin:
		fmt.Fprintln(&b, "\n# Built-in review rubric (nightme-owned)")
		fmt.Fprintln(&b, g.Rule)
	default:
		// ocr-sourced rule group (Pattern = real glob like "**/*.go")
		fmt.Fprintln(&b, "\n# Review rules (matched per file by `ocr delegate` — LLM-free engineering)")
		fmt.Fprintf(&b, "### Rule (pattern %s)\n%s\n", g.Pattern, g.Rule)
	}
	fmt.Fprintln(&b, "\n# How to review")
	fmt.Fprint(&b, reviewHowTo())

	// --- Output format (per-job) ---
	fmt.Fprintln(&b, "\n# Output format")
	fmt.Fprintln(&b, "Output a coverage summary, then one block per finding. Do not pad with generic advice.")
	fmt.Fprintln(&b, "\n```")
	fmt.Fprintln(&b, "## Coverage")
	fmt.Fprintf(&b, "- total: %d\n", len(g.Files))
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

// groupFilteredDiff runs `git diff <args...> -- <g.Files...>` in the
// workspace and returns the diff trimmed, truncated per the standard
// cap. "" on any error (per runGit convention). This is the smart-
// bundling payoff: each per-group RunOnce sees ONLY the diff for its
// group's files, keeping the prompt within context.
//
// ctx must be the per-job goroutine's review ctx (parent of the
// goroutine, derived from chat session ctx + 30-min timeout) so
// /close / timeout propagation reaches the git subprocess —
// invariant #3 (REVIEW.md §1.1). Previously this derived a
// fresh context.Background() (Finding 1 from /review), which
// orphaned the subprocess when /close fired and violated the
// invariant. The 30s cap is layered ON TOP of the parent ctx
// (whichever deadline is earlier wins).
func groupFilteredDiff(ctx context.Context, rc reviewContext, g *reviewGroup, args ...string) string {
	if len(g.Files) == 0 {
		return ""
	}
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	full := append([]string{}, args...)
	full = append(full, "--")
	full = append(full, g.Files...)
	c2 := proc.New(c, "git", append([]string{"-C", rc.workspace}, full...)...)
	out, _ := c2.Output() // error swallowed: "" = "skip section", by design
	return truncateDiff(strings.TrimSpace(string(out)))
}

// mergeRunResults combines N per-job RunResults into one final
// RunResult. Per docs/REVIEW.md §2.5.4, the merge is intentionally
// trivial: pure natural-language concat with group headers. No
// structural parsing, no severity sort, no path dedup, no coverage
// aggregation. The consumer (main agent receiving this as a user
// turn via as.SendBlocks) is an LLM — prose comprehension is enough
// to find the right findings for "fix blockers"-style follow-ups.
//
// Two-step merge:
//  1. Per-group header + the group's original review text verbatim.
//  2. Failed groups get an inline `### Group: pattern X — failed:
//     <err>` marker (partial failure is non-fatal per §2.5.3).
//
// Error semantics: if ALL jobs errored, returns the first error wrapped
// with the agent name (matches single-job error wrapping). If SOME
// errored, the error is NOT returned — partial findings are still
// useful, and surfacing a hard error would mask the surviving groups'
// value.
func mergeRunResults(agentName string, groups []reviewGroup, results []RunResult, errs []error) (RunResult, error) {
	if len(results) != len(groups) || len(errs) != len(groups) {
		return RunResult{}, fmt.Errorf("agent %s: merge shape mismatch (groups=%d, results=%d, errs=%d)",
			agentName, len(groups), len(results), len(errs))
	}

	// All-failed path: surface the first error wrapped.
	successCount := 0
	var firstErr error
	for _, err := range errs {
		if err == nil {
			successCount++
			continue
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if successCount == 0 && firstErr != nil {
		return RunResult{}, fmt.Errorf("agent %s: all %d review jobs failed: %w",
			agentName, len(groups), firstErr)
	}

	// Successful merge: concatenate group-by-group. Top line is a
	// quick human-readable summary; per-group headers let the main
	// agent reference findings by pattern in fix-up follow-ups.
	var b strings.Builder
	fmt.Fprintf(&b, "Reviewed %d groups.\n\n", len(groups))
	for i, g := range groups {
		if errs[i] != nil {
			fmt.Fprintf(&b, "### Group: pattern %s — failed: %v\n\n", g.Pattern, errs[i])
			continue
		}
		fmt.Fprintf(&b, "### Group: pattern %s — files: %s\n\n%s\n\n",
			g.Pattern, strings.Join(g.Files, ", "), results[i].Text)
	}

	// Audit metadata: propagate the FIRST successful job's session /
	// model / duration / subtype so callers can tell which sessionId
	// produced the merged review. Usage stays nil by design (v12 —
	// consumer doesn't read it; per-job usage is in each RunResult).
	// Each per-job RunOnce is an independent fresh session on the
	// shared host, so the "first non-empty" is the most stable
	// primary reference (subsequent jobs may have different
	// sessionIds, and `Reviewed N groups.` already tells the caller
	// there were multiple).
	merged := RunResult{Text: strings.TrimSpace(b.String())}
	for i, err := range errs {
		if err != nil {
			continue
		}
		r := results[i]
		if merged.SessionID == "" && r.SessionID != "" {
			merged.SessionID = r.SessionID
		}
		if merged.Model == "" && r.Model != "" {
			merged.Model = r.Model
		}
		if merged.DurationMs == 0 && r.DurationMs > 0 {
			merged.DurationMs = r.DurationMs
		}
		if merged.Subtype == "" && r.Subtype != "" {
			merged.Subtype = r.Subtype
		}
		if merged.SessionID != "" && merged.Model != "" && merged.DurationMs > 0 && merged.Subtype != "" {
			break
		}
	}
	return merged, nil
}
