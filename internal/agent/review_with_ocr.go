// review_with_ocr.go — ReviewWithOcr runner + multi-job fan-out
// machinery for the ocr delegation flow (docs/REVIEW.md §2.2).
//
// Two review runners exist in package agent; every bridge dispatches
// uniformly via agent.ReviewDispatch:
//   - ReviewWithOcr: this file; ocr delegation flow with multi-job
//     fan-out via delegateReviewMultiJob.
//   - ReviewWithPrompt: in review.go; pure-prompt path used when ocr
//     isn't installed or workspace precompute fails.
//
// simplify runs as a parallel reviewGroup inside both runners
// (see patternSimplify / simplifyGroup below) — one session turn per
// group, concurrently with the main dimension(s).
//
// ocr is an external CLI (like git), not a bridge/agent. Its delegation
// mode is LLM-free: it emits only deterministic engineering (file list
// + rules) as JSON; the host agent (this bridge's agent) supplies the
// LLM via the session turn. No second LLM config.
//
// Every step degrades gracefully: ocr missing/failed → ReviewWithPrompt
// fallback; precompute totally failed (no workspace) → BuiltinPrompt
// verbatim. /review never hard-fails on precompute.

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/gitbase"
	"github.com/cnlangzi/nightme/internal/proc"
)

// groupDiffBudgetBytes caps the diff payload one rule group sends to
// the host agent: the committed / staged / unstaged sections together
// may occupy at most this many bytes.
//
// 1 MiB is roughly 300K tokens of diff — about 30% of a 1M-token
// context window, leaving room for the system prompt, tool schemas,
// the file list, the agent's output and the file reads the agent
// performs when a section was cut.
const groupDiffBudgetBytes = 1 << 20

// ReviewWithOcr runs review using the ocr delegation flow (docs/REVIEW.md
// §2.2 / §2.5). ocr provides deterministic engineering (file selection +
// rule matching, LLM-free); the host agent does the actual review
// reasoning with its own LLM.
//
// Pre-conditions:
//   - agent.ReviewDispatch has already checked OcrAvailable and routed
//     here. ReviewWithOcr does NOT re-check — single responsibility:
//     "given ocr, do the ocr flow".
//
// Flow:
//  1. precomputeReview fills reviewable / ocrGroups.
//  2. Fan out via delegateReviewMultiJob with [ocrGroups..., simplifyGroup].
//     simplify always runs as a parallel dimension (its own session turn) alongside
//     the ocr-sourced groups.
//
// If precomputeReview returns empty (no workspace, no ocr groups), the
// fan-out degrades gracefully to [simplifyGroup(nil)] — one goroutine,
// one session turn. No internal fallback to ReviewWithPrompt; if ocr isn't
// actually available, that's a caller bug, not this function's problem.
func ReviewWithOcr(ctx context.Context, s Starter, cfg StartConfig, opts ...RunOnceOption) (RunResult, error) {
	pre := precomputeReviewWithOcr(ctx, cfg.Workspace)
	// The base branch could not be determined and could not be
	// repaired: the committed diff was not reviewed. Saying "no diff
	// to review" would be a lie, so this fails distinctly — and before
	// any agent is spawned.
	if err := pre.baseError(); err != nil {
		return RunResult{}, err
	}
	// Short-circuit on empty diff — see ReviewWithPrompt for the
	// rationale. Both runners must agree on the contract: zero
	// reviewable + zero untracked → ErrNoDiff, no agent spawn.
	if pre.isEmptyDiff() {
		return RunResult{}, ErrNoDiff
	}
	groups := append(pre.ocrGroups, simplifyGroup(pre.reviewable))
	res, err := delegateReviewMultiJob(ctx, s, cfg, pre, groups, opts...)
	if err != nil {
		return res, err
	}
	res.Notes = append(res.Notes, pre.notes()...)
	return res, nil
}

// delegateReviewMultiJob runs one session turn per ocr rule group, in
// parallel (sem-capped at maxConcurrentReviewJobs), each with a
// per-group prompt + aggregator-wrapped sink. The aggregator collapses
// N per-job Ready/terminal streams into one outer lifecycle so the
// chat channel sees a single review run, not N interleaved ones.
//
// Each group runs on the agent's session path (RunSession): its own
// process, its own session, one turn. The prompt travels over the
// bridge's protocol pipe, so a group prompt larger than the platform's
// argv limit still reaches the agent.
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
	run := func(ctx context.Context, groupIdx int, cfg StartConfig, runOpts []RunOnceOption) (RunResult, error) {
		prompt := assembleGroupPrompt(ctx, pre, &groups[groupIdx])
		if prompt == "" {
			prompt = BuiltinPrompt
		}
		return RunSession(ctx, s, cfg, []ContentBlock{{
			Type: ContentText,
			Text: prompt,
		}}, runOpts...)
	}
	return runReviewGroups(ctx, s, cfg, groups, opts, run)
}

// reviewGroupRunner runs one group of a review fan-out.
//
// The core owns the goroutines, the concurrency cap, the event
// aggregation and the merge; the runner owns what the group's turn
// actually is — a fresh session carrying the group's prompt (a
// review) or a reopened session carrying a resume text (a
// continuation).
type reviewGroupRunner func(ctx context.Context, groupIdx int, cfg StartConfig, opts []RunOnceOption) (RunResult, error)

// runReviewGroups is the fan-out core shared by a fresh-session
// review and a resume run: one goroutine per group, concurrency
// capped at maxConcurrentReviewJobs, every group's events folded
// into one outer lifecycle by the eventAggregator, and the per-group
// results merged by mergeRunResults.
//
// opts carries the caller's outer sink (WithEventSink) and the
// per-group observer (WithReviewJobObserver). The outer sink is
// replaced per group by the aggregator's wrapped sink; the observer
// is invoked once per group with that group's identity and the error
// that ended it.
//
// A failed group reports the session id its session ran under, so the
// caller can reopen that session later (see ResumeSession).
func runReviewGroups(
	ctx context.Context,
	s Starter,
	cfg StartConfig,
	groups []reviewGroup,
	opts []RunOnceOption,
	run reviewGroupRunner,
) (RunResult, error) {
	// Extract the outer sink (if any) and rebuild per-job opts with
	// the aggregator's per-job sink replacing it. ParseRunOnceOptions
	// is the single source of truth for option resolution; the
	// per-group observer is consumed here, not forwarded to the
	// per-group turn.
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
			// markJobDone, regardless of the turn's outcome or
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

			var runOpts []RunOnceOption
			if agg != nil {
				runOpts = []RunOnceOption{WithEventSink(agg.wrapJob(fmt.Sprintf("group-%d", i+1)))}
			}

			res, err := run(ctx, i, cfg, runOpts)
			results[i] = res
			errs[i] = err
			if cfgOpts.OnReviewJob != nil {
				cfgOpts.OnReviewJob(ReviewJob{
					Pattern:   groups[i].Pattern,
					Files:     groups[i].Files,
					SessionID: res.SessionID,
				}, err)
			}
		}(i)
	}
	wg.Wait()

	// Finding 3 from /review: safety net for the case where
	// some per-job turn never emitted a terminal event
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
// "could not compute". The diff itself is NOT precomputed — each rule
// group slices its own diff at prompt-assembly time, where the shared
// byte budget is known.
type reviewContext struct {
	workspace     string
	defaultBranch string // resolvable ref "origin/main"; "" = could not detect
	mergeBase     string // commit hash; "" = could not compute
	reviewable    []string
	untracked     []string // new files never committed/staged; can't be diffed (no baseline)
	excluded      []string // "path (reason)" entries; Tier 2 only
	// base is the resolution of the workspace's base branch: how it
	// was determined, and whether the missing origin/HEAD was
	// repaired.
	base gitbase.Resolution
	// Tier 2 only (populated when ocr is available + produced rules):
	ocrGroups []reviewGroup // parsed structured groups, one per ocr pattern
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

// baseError reports the base-unknown failure: the repository has an
// origin remote, but the base branch could neither be read nor
// repaired. The committed diff was not reviewed — announcing "no diff
// to review" would claim there was nothing to review, while the truth
// is that nothing was examined.
//
// A repository without an origin remote is not this case: there the
// working tree is the scope.
func (rc *reviewContext) baseError() error {
	if rc == nil || rc.base.Branch != "" || !rc.base.HasRemote {
		return nil
	}
	return &BaseUnknownError{Workspace: rc.workspace, Reason: rc.base.Reason}
}

// notes returns the run-level notices the precompute produced — today
// only the base-branch repair, which the user must know about because
// the review touched their repository.
func (rc *reviewContext) notes() []string {
	if rc == nil || !rc.base.WroteHead {
		return nil
	}
	return []string{repairNotice(rc.base.Branch)}
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
// caller (agent.ReviewDispatch → ReviewWithOcr) is responsible for
// verifying OcrAvailable() before reaching this — this function
// assumes ocr is on $PATH.
//
// The committed leg needs the base branch; the staged / unstaged /
// untracked legs do not, so they are collected even when the base is
// unknown. ocr's file list wins when the preview answered; the Go-side
// enumeration is the fallback, so an ocr failure no longer reads as
// an empty diff.
func precomputeReviewWithOcr(ctx context.Context, workspace string) reviewContext {
	rc := reviewContext{workspace: workspace}
	if workspace == "" {
		return rc
	}

	res, base := resolveBase(ctx, workspace)
	rc.base = res
	rc.defaultBranch = base
	if base != "" {
		rc.mergeBase = runGit(ctx, workspace, "merge-base", base, "HEAD")
	}

	goReviewable, goUntracked := collectWorkspaceChanges(ctx, workspace, base)
	rc.untracked = goUntracked

	if base != "" {
		if preview, ok := runOcrDelegatePreview(ctx, workspace, base, "HEAD"); ok {
			rc.reviewable = preview.reviewable
			rc.excluded = preview.excluded
			if preview.mergeBase != "" {
				rc.mergeBase = preview.mergeBase
			}
		}
	}
	if len(rc.reviewable) == 0 {
		rc.reviewable = goReviewable
	}

	rc.ocrGroups = runOcrDelegateRulesBatched(ctx, workspace, rc.reviewable)
	if len(rc.ocrGroups) == 0 && !rc.isEmptyDiff() {
		// ocr produced no rules for these files — the Go enumeration
		// supplies the file list and the builtin rubric the review.
		rc.ocrGroups = []reviewGroup{{
			Pattern: patternBuiltin,
			Files:   rc.reviewable,
			Rule:    BuiltinPrompt,
		}}
	}

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
func precomputeReviewWithBuiltin(ctx context.Context, workspace string) reviewContext {
	rc := reviewContext{workspace: workspace}
	if workspace == "" {
		return rc
	}

	res, base := resolveBase(ctx, workspace)
	rc.base = res
	rc.defaultBranch = base
	if base != "" {
		rc.mergeBase = runGit(ctx, workspace, "merge-base", base, "HEAD")
	}

	rc.reviewable, rc.untracked = collectWorkspaceChanges(ctx, workspace, base)

	if !rc.isEmptyDiff() {
		// Files = reviewable only — untracked have no diff to slice on.
		// untracked is rendered separately by assembleGroupPrompt as a
		// "new file additions" section.
		rc.ocrGroups = []reviewGroup{{
			Pattern: patternBuiltin,
			Files:   rc.reviewable,
			Rule:    BuiltinPrompt,
		}}
	}

	return rc
}

// collectWorkspaceChanges enumerates the workspace's changed files.
// Returns two disjoint slices:
//
//   - reviewable: files with at least one diff (committed/staged/
//     unstaged). Safe to render with `git diff -- <file>` and put in
//     reviewGroup.Files.
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
// Only the committed leg needs the base branch; the staged / unstaged
// / untracked legs run unconditionally, so the uncommitted changes of a
// workspace with an unknown base are still collected. They are not
// silently reviewed, though: a repository that HAS a remote fails the
// run with ErrBaseUnknown rather than passing off a partial diff as
// the whole change set.
//
// 4 git sources, dedup'd:
//  1. git diff --name-only <base>...HEAD    (committed on branch)
//  2. git diff --staged --name-only        (staged, not committed)
//  3. git diff --name-only                 (unstaged working tree)
//  4. git ls-files --others --exclude-standard  (untracked, .gitignore)
func collectWorkspaceChanges(ctx context.Context, workspace, baseRef string) (reviewable, untracked []string) {
	seen := map[string]bool{}
	var changed []string
	addChanged := func(s string) {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			changed = append(changed, s)
		}
	}
	if baseRef != "" {
		for _, f := range strings.Split(runGit(ctx, workspace,
			"diff", "--name-only", baseRef+"...HEAD"), "\n") {
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
			untracked = append(untracked, s)
		}
	}
	for _, f := range changed {
		if isReviewablePath(f) {
			reviewable = append(reviewable, f)
		}
	}
	return reviewable, untracked
}

// resolveBase determines the workspace's base branch and returns it
// with the resolvable ref form the git commands need ("origin/main").
//
// The resolver repairs a missing refs/remotes/origin/HEAD, so later
// resolutions stay offline. An empty ref means the repository has no
// origin remote — the working tree is then the whole scope, which is
// not an error.
func resolveBase(ctx context.Context, workspace string) (gitbase.Resolution, string) {
	res := gitbase.Resolve(ctx, workspace, gitbase.Options{Repair: true})
	if res.Branch == "" {
		return res, ""
	}
	return res, "origin/" + res.Branch
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
// Exported for agent.ReviewDispatch (the single in-package caller
// that routes to ReviewWithOcr or ReviewWithPrompt). Keeping the
// detection here avoids duplicating the LookPath call.
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
// returns the parsed structured groups. A single batch failing
// (ocr error, JSON parse error) is non-fatal — other batches still
// contribute their rules, so a transient failure on one slice doesn't
// blank the whole rule set.
func runOcrDelegateRulesBatched(ctx context.Context, workspace string, paths []string) []reviewGroup {
	if len(paths) == 0 {
		return nil
	}
	var groups []reviewGroup
	for i := 0; i < len(paths); i += ruleBatchSize {
		end := i + ruleBatchSize
		if end > len(paths) {
			end = len(paths)
		}
		if gs, ok := runOcrDelegateRuleBatch(ctx, workspace, paths[i:end]); ok {
			groups = append(groups, gs...)
		}
	}
	return groups
}

// runOcrDelegateRuleBatch runs one `ocr delegate rule` batch and
// returns the rule groups as structured []reviewGroup. ok=false on any
// failure for this batch. proc.New routes the Windows .cmd shim
// through cmd.exe /d /c (plain exec.Command would fail with
// ERROR_INVALID_PARAMETER (87) on a .cmd target).
func runOcrDelegateRuleBatch(ctx context.Context, workspace string, paths []string) ([]reviewGroup, bool) {
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := append([]string{"delegate", "rule", "--format", "json"}, paths...)
	cmd := proc.New(c, "ocr", args...)
	cmd.Dir = workspace
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	var resp struct {
		Groups []struct {
			Pattern string   `json:"pattern"`
			Files   []string `json:"files"`
			Rule    string   `json:"rule"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, false
	}
	if len(resp.Groups) == 0 {
		return nil, false
	}
	groups := make([]reviewGroup, 0, len(resp.Groups))
	for _, g := range resp.Groups {
		groups = append(groups, reviewGroup{
			Pattern: g.Pattern,
			Files:   g.Files,
			Rule:    g.Rule,
		})
	}
	return groups, true
}

// diffTruncationMarker is appended to a diff section the byte budget
// cut. It names the number of dropped bytes and points the agent at
// the affected files for the omitted context — the agent recovers the
// rest by reading those files itself.
func diffTruncationMarker(dropped int) string {
	return "\n…(diff truncated: " + strconv.Itoa(dropped) +
		" bytes dropped; read the affected files directly for full context)…\n"
}

// truncateDiffToBytes keeps whole lines of s that fit within budget
// and returns the kept text plus the number of bytes dropped. A
// dropped-byte count of 0 means the section was not cut.
//
// No line is ever cut in half: the kept text ends on a line boundary,
// so the agent never sees half a diff hunk.
func truncateDiffToBytes(s string, budget int) (string, int) {
	if s == "" {
		return "", 0
	}
	if len(s) <= budget {
		return s, 0
	}
	if budget <= 0 {
		return "", len(s)
	}
	// Keep the longest prefix of whole lines that fits. IndexByte scans
	// for \n and advances past each line; a trailing line without \n
	// is never kept (it would be a partial line).
	idx := 0
	for idx < len(s) {
		next := strings.IndexByte(s[idx:], '\n')
		if next < 0 {
			break
		}
		if idx+next+1 > budget {
			break
		}
		idx += next + 1
	}
	return s[:idx], len(s) - idx
}

// allocateDiffQuotas splits the byte budget across the diff sections
// of one rule group.
//
// The split is fair-share: the budget is a level that rises until it is
// spent, and every section takes min(its own size, level). A section
// smaller than the level keeps its full content and releases the rest
// to the sections that are still short, so one oversized section never
// starves the small ones. A quota never exceeds its own section's size
// and the quotas never sum to more than the budget.
func allocateDiffQuotas(sizes []int, budget int) []int {
	quotas := make([]int, len(sizes))
	pending := make([]int, 0, len(sizes))
	for i, s := range sizes {
		if s > 0 {
			pending = append(pending, i)
		}
	}
	remaining := budget
	for len(pending) > 0 {
		total := 0
		for _, i := range pending {
			total += sizes[i]
		}
		if total <= remaining {
			for _, i := range pending {
				quotas[i] = sizes[i]
			}
			return quotas
		}
		share := remaining / len(pending)
		if share <= 0 {
			return quotas
		}
		// Sections that fit in the share take their full content and
		// release the remainder to the sections still short.
		var short []int
		used := 0
		for _, i := range pending {
			if sizes[i] <= share {
				quotas[i] = sizes[i]
				used += sizes[i]
				continue
			}
			short = append(short, i)
		}
		if len(short) == len(pending) {
			// Nobody fits the share — the level is capped by the budget.
			for _, i := range short {
				quotas[i] = share
			}
			return quotas
		}
		remaining -= used
		pending = short
	}
	return quotas
}

// diffSection is one of the three git diffs that union into "the diff
// a PR would have" for a single rule group.
type diffSection struct {
	title   string
	content string
}

// scaffold returns the section's framing strings: the "## <title>"
// header with its opening fence, and the closing fence. Both the
// budget sizing and the rendering derive from these, so the shape has
// one source of truth.
func (s diffSection) scaffold() (string, string) {
	return "## " + s.title + "\n```\n", "\n```\n"
}

// overhead returns the byte cost of the section's scaffolding — its
// "## <title>" header and the code fences around the body. The
// scaffolding counts against the section's quota, so the rendered
// blocks together stay inside the group budget.
func (s diffSection) overhead() int {
	head, tail := s.scaffold()
	return len(head) + len(tail)
}

// diffSectionSizes returns the untruncated size of each section — its
// scaffolding plus its content. This is the basis the group budget is
// split over; empty sections get no share because they are not
// rendered at all.
func diffSectionSizes(sections []diffSection) []int {
	sizes := make([]int, len(sections))
	for i, s := range sections {
		if s.content == "" {
			continue
		}
		sizes[i] = s.overhead() + len(s.content)
	}
	return sizes
}

// render fits the section into its quota. The returned block is the
// rendered section (its "## <title>" header plus the fenced body); the
// returned count is the number of content bytes that did not fit. A
// quota too small even for the truncation marker omits the section.
func (s diffSection) render(quota int) (string, int) {
	if s.content == "" {
		return "", 0
	}
	head, tail := s.scaffold()
	allowance := quota - len(head) - len(tail)
	if allowance <= 0 {
		return "", len(s.content)
	}
	body, dropped := fitDiffBody(s.content, allowance)
	if body == "" {
		return "", dropped
	}
	return head + body + tail, dropped
}

// fitDiffBody fits content into allowance bytes. Under the allowance the
// content is returned verbatim; over it, the longest prefix of whole
// lines that fits alongside the truncation marker is kept and the
// marker — naming the dropped byte count — is appended.
//
// The marker wins when content and marker cannot both fit: a body that
// is the marker alone still tells the agent the diff was cut and which
// files to read, which beats dropping the section silently. An empty
// body means the allowance is too small even for the marker.
func fitDiffBody(content string, allowance int) (string, int) {
	if len(content) <= allowance {
		return content, 0
	}
	// Reserve the widest marker the section can carry: the dropped byte
	// count never exceeds len(content), so its digit count is bounded.
	reserve := len(diffTruncationMarker(len(content)))
	if reserve > allowance {
		return "", len(content)
	}
	kept, dropped := truncateDiffToBytes(content, allowance-reserve)
	if kept == "" {
		return diffTruncationMarker(len(content)), len(content)
	}
	return kept + diffTruncationMarker(dropped), dropped
}

// reviewHowTo is the language-agnostic review methodology, shared by
// every rule group. It is the "process" part of the rubric (read
// first, distinguish blockers, cite locations, skip linter territory,
// false-positive filter, simplification) — NOT the language-specific
// "what to look for" list, which lives in the group's own rule
// (ocr's per-file rules, or the builtin rubric).
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

// assembleGroupPrompt builds a per-group review prompt for the
// multi-job path. Structure: context → files → diffs → rules → how-to
// → output schema.
//
//   - filters the file list to the group's Files (the rule
//     explicitly enumerates them — ocr's authoritative
//     assignment, not our heuristic)
//   - filters the diff to those files via `git diff -- <files...>`
//     (this is the smart-bundling payoff: each job's prompt
//     contains only the diff slice it can actually review)
//   - uses ONLY this group's rule (Tier 2 single-group case
//     in multi-job context)
//   - bounds the three diff sections with the shared group byte
//     budget, cutting at line boundaries and marking what was dropped
//
// ctx is the per-job goroutine's review ctx so groupFilteredDiff's
// git subprocesses inherit /close + 30-min timeout (Finding 1).
//
// Returns "" only when workspace is empty — the caller's "fall back to
// BuiltinPrompt" branch fires on it.
func assembleGroupPrompt(ctx context.Context, rc reviewContext, g *reviewGroup) string {
	if g == nil {
		return ""
	}
	if rc.workspace == "" {
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
	// These files appear in the changeset but `git diff -- <file>`
	// returns empty for them — the LLM must treat them as "new file
	// additions" rather than expecting existing diff content.
	//
	// Rendered for every group: in the ocr path the untracked files
	// belong to no rule group, so no group's file list would name
	// them otherwise.
	if len(rc.untracked) > 0 {
		fmt.Fprintln(&b, "\n# New file additions (no baseline — added in this changeset)")
		fmt.Fprintln(&b, "These files are untracked (never committed/staged), so no diff exists. Review by reading the file contents directly if available, or skip with reason.")
		for _, f := range rc.untracked {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}

	// --- Diffs (group-filtered via `git diff -- <files...>`) ---
	//
	// Range strategy:
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
	//
	// The three sections share one byte budget (groupDiffBudgetBytes):
	// each gets a quota, a section over its quota is cut at line
	// boundaries and marked with the number of dropped bytes, and a
	// cut group is logged.
	committedArgs := []string{"diff"}
	switch {
	case rc.mergeBase != "":
		committedArgs = append(committedArgs, rc.mergeBase+"..HEAD")
	case rc.defaultBranch != "":
		committedArgs = append(committedArgs, rc.defaultBranch+"...HEAD")
	}
	label := rc.defaultBranch + "...HEAD"
	if rc.mergeBase != "" {
		label = rc.mergeBase + "..HEAD"
	}
	sections := []diffSection{
		{title: "Committed (" + label + ") — group-filtered", content: groupFilteredDiff(ctx, rc, g, committedArgs...)},
		{title: "Staged — group-filtered", content: groupFilteredDiff(ctx, rc, g, "diff", "--staged")},
		{title: "Unstaged — group-filtered", content: groupFilteredDiff(ctx, rc, g, "diff")},
	}
	sizes := diffSectionSizes(sections)
	quotas := allocateDiffQuotas(sizes, groupDiffBudgetBytes)

	var payload, dropped int
	rendered := make([]string, 0, len(sections))
	for i, s := range sections {
		block, cut := s.render(quotas[i])
		dropped += cut
		if block == "" {
			continue
		}
		payload += len(block)
		rendered = append(rendered, block)
	}
	if len(rendered) > 0 {
		fmt.Fprintln(&b, "\n# Diffs (precomputed — the union below is the full diff to review for THIS group)")
		for _, block := range rendered {
			fmt.Fprint(&b, block)
		}
	}
	if dropped > 0 {
		slog.Info("agent: review group diff cut by the byte budget",
			"pattern", g.Pattern,
			"budget_bytes", groupDiffBudgetBytes,
			"payload_bytes", payload,
			"dropped_bytes", dropped)
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
// workspace and returns the diff trimmed but UNTRUNCATED. "" on any
// error (per runGit convention). This is the smart-bundling payoff:
// each per-group RunOnce sees ONLY the diff for its group's files.
//
// The diff is cut later — in assembleGroupPrompt, where the group's
// shared byte budget is known and can be split across the committed /
// staged / unstaged sections.
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
	return strings.TrimSpace(string(out))
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
