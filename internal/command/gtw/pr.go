package gtw

// /gtw pr — generate a Conventional Commits title + body and open
// the PR/MR on the origin platform (GitHub via `gh`, GitLab via
// `glab`). Closes the loop with /gtw push: the user has already
// committed + pushed, /gtw pr just does the remaining bit.
//
// Design refs:
//   - wip/gtw-pr.md       (design rationale, IM-friendly card, prompts)
//   - wip/gtw-pr-plan.md  (implementation phases, edge-case table)
//
// Flow (mirrors wip/gtw-pr.md §3):
//
//  1. Locate selectedCwd + .nightme/gtw.yml → Context.
//  2. Resolve default branch via DefaultBranch(repoRoot).
//  3. Reject if head has unpushed commits (hint: /gtw push first).
//  4. Reject if rev-list --count base..HEAD == 0 (nothing to PR).
//  5. Pick an agent (-a override → SelectedAgent), one-shot RunOnce
//     to generate the PR text.
//  6. parsePRReply extracts (title, body) from the fenced block.
//  7. Pick a provider (yml Repo/Provider wins; otherwise
//     RemoteOriginURL + Detect).
//  8. provider.CreatePR → URL.
//  9. Reply with ✅ PR opened + branch / base / url / worktree card.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
)

// prDiffScanCapBytes is reserved for a future pre-scan diff
// feature (currently unused; see wip/gtw-pr-plan §P1 step 2).
// Kept here so the constant has a clear home when we add the
// feature.
const prDiffScanCapBytes = 64 * 1024

// errParseAgentReply is the sentinel returned by parsePRReply
// when the agent's reply cannot be parsed into a title + body
// (no fence, empty fence, malformed fence). The dispatcher
// surfaces the agent's text verbatim in the error reply so the
// user can still paste title + body manually into gh/glab.
var errParseAgentReply = errors.New("could not parse agent reply as fenced PR block")

// prArgs bundles the parsed argv tail of `/gtw pr <...>`.
// Mirrors pushArgs (cmd.go) — Agent override + room for
// future flags. Kept in this file (rather than cmd.go) so
// dispatchPR can be tested without importing the factory
// surface.
type prArgs struct {
	// Agent, when non-empty, overrides the chat's currently
	// Selected Agent for this one-shot PR generation. Comes
	// from `-a <name>` / `--agent <name>`. Empty means:
	// use cs.SelectedAgent().
	Agent string
}

// dispatchPR is the entry point for `/gtw pr [-a <agent>]`.
// Mirrors dispatchPush's structure: read yml → early-return
// checks → RunOnce → provider call → IM-friendly card.
//
// All non-success paths return nil error + a Result whose
// Reply has already been sent via cs.Channel() — the same
// pattern dispatchPush uses (and that the factory wrapper
// relies on for "consumed: true, drop the message").
func dispatchPR(
	ctx context.Context,
	cs *chatsession.ChatSession,
	deps HandlerDeps,
	chatID, messageID string,
	args prArgs,
) (*Result, error) {
	c, res := loadDispatchContext(ctx, cs, deps, chatID, messageID)
	if res != nil {
		return res, nil
	}

	baseBranch, err := DefaultBranch(ctx, c.RepoRoot, deps.Git)
	if err != nil {
		// Reuse the same message format /gtw sync uses — it
		// already explains the "no origin remote" case.
		return reply(ctx, cs.Channel(), chatID, messageID,
			fmt.Sprintf("❌ %v", err)), nil
	}

	// Refuse when head has unpushed commits: gh/glab will
	// report "head ref doesn't exist" with a less actionable
	// message, and the user should be nudged to /gtw push first.
	unpushed, err := countUnpushed(ctx, c.Worktree, c.Branch, deps)
	if err != nil {
		return reply(ctx, cs.Channel(), chatID, messageID,
			fmt.Sprintf("❌ check unpushed commits: %v", err)), nil
	}
	if unpushed > 0 {
		return reply(ctx, cs.Channel(), chatID, messageID,
			fmt.Sprintf(
				"⚠️ %d commit(s) made locally but not pushed to remote\n"+
					"hint: /gtw push first to publish them, then /gtw pr.",
				unpushed)), nil
	}

	// Reject when there's nothing on this branch that's not
	// already in base — opening an empty PR is a no-op.
	ahead, err := countBaseAhead(ctx, c.Worktree, baseBranch, deps)
	if err != nil {
		return reply(ctx, cs.Channel(), chatID, messageID,
			fmt.Sprintf("❌ count commits ahead of base: %v", err)), nil
	}
	if ahead == 0 {
		// "Nothing on this branch" almost always means the user
		// has local edits they haven't committed yet. Detect that
		// and point them at /gtw push (which handles the commit +
		// push step) so the next /gtw pr has something to ship.
		// Lead with the actionable state in plain language — drop
		// the technical "branch X is at Y" header so users don't
		// have to translate git jargon to figure out what to do.
		if snap, _ := CollectStatus(ctx, c.Worktree, deps.Git); snap != nil {
			switch {
			case snap.Uncommitted > 0 && snap.Untracked > 0:
				return reply(ctx, cs.Channel(), chatID, messageID,
					fmt.Sprintf(
						"⚠️ %d file(s) changed but not committed, %d new file(s) not added to git\n"+
							"hint: /gtw push first to commit + add + push, then /gtw pr.",
						snap.Uncommitted, snap.Untracked)), nil
			case snap.Uncommitted > 0:
				return reply(ctx, cs.Channel(), chatID, messageID,
					fmt.Sprintf(
						"⚠️ %d file(s) changed but not committed\n"+
							"hint: /gtw push first to commit + push, then /gtw pr.",
						snap.Uncommitted)), nil
			case snap.Untracked > 0:
				return reply(ctx, cs.Channel(), chatID, messageID,
					fmt.Sprintf(
						"⚠️ %d new file(s) not added to git\n"+
							"hint: git add them, then /gtw push, then /gtw pr.",
						snap.Untracked)), nil
			}
		}
		// Truly nothing to ship — use ✅ (not an error) and nudge
		// the user toward making a change rather than leaving them
		// staring at "nothing to PR" wondering what's wrong.
		return reply(ctx, cs.Channel(), chatID, messageID,
			fmt.Sprintf(
				"✅ branch %s is in sync with %s — nothing new to PR yet\n"+
					"hint: make some changes, then /gtw push, then /gtw pr.",
				c.Branch, baseBranch)), nil
	}

	// --- pick agent --------------------------------------------------
	agentName := args.Agent
	if agentName == "" {
		agentName = cs.SelectedAgent()
	}
	if agentName == "" {
		return reply(ctx, cs.Channel(), chatID, messageID,
			"❌ no agent selected. Send `/use <name>` first or pass `-a <name>`."), nil
	}
	a, err := agent.Builtins.Get(agentName)
	if err != nil {
		return reply(ctx, cs.Channel(), chatID, messageID,
			fmt.Sprintf("❌ unknown agent %q (check `nightme agents` or your config)", agentName)), nil
	}
	if err := a.Detect(); err != nil {
		return reply(ctx, cs.Channel(), chatID, messageID,
			fmt.Sprintf("❌ agent %s not available: %v", agentName, err)), nil
	}

	ctx, cancel := context.WithTimeout(ctx, RunOnceTimeout)
	defer cancel()

	prompt := buildPRPrompt(c, baseBranch)
	blocks := []agent.ContentBlock{{
		Type: agent.ContentText,
		Text: prompt,
	}}
	text, err := a.RunOnce(ctx,
		agent.StartConfig{Workspace: c.Worktree},
		blocks,
	)
	if err != nil {
		return reply(ctx, cs.Channel(), chatID, messageID,
			fmt.Sprintf("❌ agent %s failed: %v", agentName, err)), nil
	}

	title, body, perr := parsePRReply(text)
	if perr != nil {
		// Agent output wasn't usable. Echo the raw text so
		// the user can copy/paste into gh/glab themselves.
		return reply(ctx, cs.Channel(), chatID, messageID,
			fmt.Sprintf(
				"❌ %v — agent output was:\n%s",
				perr, indentLines(text, "  "))), nil
	}

	// --- pick provider ----------------------------------------------
	provider, owner, repo, err := resolveProvider(ctx, c, deps)
	if err != nil {
		return reply(ctx, cs.Channel(), chatID, messageID,
			fmt.Sprintf("❌ %v", err)), nil
	}

	url, err := provider.CreatePR(ctx, owner, repo, baseBranch, c.Branch, title, body)
	if err != nil {
		if errors.Is(err, ErrPRExists) {
			// Friendly reuse — the user already opened a PR
			// for this branch. We don't have the URL from
			// gh/glab stderr reliably across versions, so we
			// just point them at the repo.
			return reply(ctx, cs.Channel(), chatID, messageID,
				fmt.Sprintf(
					"❌ a PR for %s already exists — check your repo's PR list.",
					c.Branch)), nil
		}
		return reply(ctx, cs.Channel(), chatID, messageID,
			fmt.Sprintf("❌ create PR failed: %v", err)), nil
	}

	card := renderPROpenedCard(c, baseBranch, url)
	return reply(ctx, cs.Channel(), chatID, messageID, card), nil
}

// buildPRPrompt renders the text block the agent receives.
// Format follows wip/gtw-pr.md §4.2 — Output Format first (the
// hard constraint), then Task, then Context. Issue ref (#N)
// appears in the body instruction when c.Issue > 0.
//
// We deliberately do NOT pre-fetch git log / git diff: the agent
// has its own workspace and can stream output of either as it
// reads them. See wip/gtw-pr.md §4.1 "agent self-inspects".
func buildPRPrompt(c Context, base string) string {
	var sb strings.Builder
	sb.WriteString("## Output Format\n")
	sb.WriteString("Reply with ONE fenced markdown code block (``` ... ```) and nothing else.\n")
	sb.WriteString("First line inside the fence is the PR title (Conventional Commits 1.0.0).\n")
	sb.WriteString("Remaining lines are the PR body (markdown).\n")
	sb.WriteString("Do NOT nest additional ``` fences — the daemon's parser stops at the first closing fence.\n")
	sb.WriteString("Indent code samples with 4 spaces instead.\n")
	sb.WriteString("Example:\n")
	sb.WriteString("```\n")
	sb.WriteString("feat(scope): short imperative subject\n")
	sb.WriteString("\n")
	sb.WriteString("- bullet describing the change\n")
	sb.WriteString("- another bullet\n")
	sb.WriteString("```\n\n")

	sb.WriteString("Conventional Commits rules (strict):\n")
	sb.WriteString("- Format: <type>(<optional-scope>): <subject>\n")
	sb.WriteString("- Types: feat, fix, chore, refactor, docs, test, build, ci, perf, style, revert\n")
	sb.WriteString("- Subject ≤72 chars, imperative mood, no trailing period\n")
	sb.WriteString("- Body explains WHY, wrapped at 72 cols\n")
	sb.WriteString("- Breaking change: `!` after type/scope + `BREAKING CHANGE:` footer\n\n")

	sb.WriteString("## Task\n")
	sb.WriteString("1. Run `git log --oneline <base>..HEAD` to inspect commits on this branch.\n")
	sb.WriteString("2. Spot-check key files with `git diff <base>...HEAD -- <path>` when you need detail.\n")
	sb.WriteString("3. Write a Conventional Commits title + structured body summarising the change.\n")
	if c.Issue > 0 {
		sb.WriteString(fmt.Sprintf("4. Reference issue with #%d in the body footer.\n", c.Issue))
	}
	sb.WriteString("\nDO NOT run `git commit`, `git push`, `gh pr create`, or `glab mr create`. ")
	sb.WriteString("Only generate the title + body.\n\n")

	sb.WriteString("## Context\n")
	if c.Repo != "" {
		fmt.Fprintf(&sb, "Repository: %s\n", c.Repo)
	} else {
		// Detect-fallback path (and the new non-worktree path
		// added in PR #105): we don't yet know the owner/repo.
		// The agent will derive it from `git remote get-url
		// origin` if needed for the body; the daemon still calls
		// provider.CreatePR with the right values.
		sb.WriteString("Repository: (resolve from `git remote get-url origin`)\n")
	}
	fmt.Fprintf(&sb, "Branch (head): %s\n", c.Branch)
	fmt.Fprintf(&sb, "Base branch: %s\n", base)
	fmt.Fprintf(&sb, "Working dir: %s\n", c.Worktree)
	return sb.String()
}

// parsePRReply extracts (title, body) from an agent's reply.
// The reply is expected to contain ONE fenced code block
// (`` ``` ... ``` ``); the first line inside the fence is the
// title, the remainder is the body.
//
// Robust to:
//   - preamble prose ("Here you go:") before the opening fence
//   - postscript prose ("Let me know if you'd like changes")
//     after the closing fence
//   - multiple blank lines between the prose and the fence
//
// Returns errParseAgentReply when:
//   - no opening fence found
//   - no closing fence after the opening
//   - fence is empty
//
// Title is trimmed of leading/trailing whitespace; body keeps
// its interior layout (we only strip a single leading newline
// that follows the title line).
//
// Known limitation: a body that contains nested ``` fences (e.g.
// a syntax-highlighted code sample) truncates at the first
// closing fence. The buildPRPrompt tells the agent to indent
// code samples with 4 spaces instead — this is a soft constraint
// and we trust the LLM to follow it.
// prTitleRegex matches a Conventional Commits 1.0.0 title line.
//
// Allow-list of types (rather than a permissive "starts with a
// letter") so we don't accidentally treat agent prose like "Test
// passed: ..." as a PR title. Scope is optional and may contain
// any non-`)`. Breaking-change `!` is allowed after the type/scope.
//
// Pattern: <type>[(<scope>)]?!: <subject>, where subject starts
// with a non-whitespace character (rejects empty titles like
// `feat:`).
var prTitleRegex = regexp.MustCompile(
	`^(feat|fix|chore|refactor|docs|test|build|ci|perf|style|revert)(?:\([^)]+\))?!?: \S`,
)

// prTitleAnywhereRegex is the multiline variant: matches a CC
// title at the start of any line in the cleaned text. Used as
// the final fallback when per-line noise stripping can't find a
// match — e.g. JSON-wrapped single-line output like
// `{"text": "feat(...): ..."\n"body": "..."}` where the title
// is embedded inside a JSON string value, not on its own line.
var prTitleAnywhereRegex = regexp.MustCompile(
	`(?m)^(feat|fix|chore|refactor|docs|test|build|ci|perf|style|revert)(?:\([^)]+\))?!?: \S.*$`,
)

// prTitleJSONValueRegex extracts a CC title that's been embedded
// as a JSON string value: `{"text": "feat(...): ..."}`. The
// title appears after a `"<key>":` prefix and ends at the next
// unescaped `"`. Captures group 1 = the title.
//
// Used as the LAST fallback after per-line scan and multiline
// regex both fail — handles the LLM-favorite "let me wrap this
// in JSON for safety" output where the title is buried inside
// a string literal.
var prTitleJSONValueRegex = regexp.MustCompile(
	`"[^"\\]*(?:\\.[^"\\]*)*"\s*:\s*"((?:feat|fix|chore|refactor|docs|test|build|ci|perf|style|revert)(?:\([^)]+\))?!?: \S[^"\n]*)"`,
)

// prLineNoiseRegex matches the per-line noise patterns we strip
// from candidate title lines: Markdown heading marks (#, ##, …),
// bullet markers (- /*/1.), and label prefixes (Title: /
// PR Title: / Title :). See stripPRNoise.
var prLineNoiseRegex = regexp.MustCompile(
	`^(?:#+\s+|-\s+|\*\s+|\d+\.\s+|PR\s+Title\s*:|Title\s*:)\s*`,
)

// parsePRReply extracts title and body from an LLM-generated PR
// description.
//
// The original implementation required a strict ``` … ``` fence
// around the reply. LLMs are unreliable at exact format compliance:
// they may forget the fence, wrap the output in JSON, prefix it
// with a Markdown heading, or scatter the title inside a bullet
// list. Any of those made the daemon hard-error with
// "could not parse agent reply", forcing the user to re-invoke
// /gtw pr manually — a bad experience for a feature whose whole
// point is automation.
//
// This version is permissive:
//
//  1. Normalize the input (stripPRNoise): trim surrounding
//     whitespace, drop leading/trailing unmatched ``` fences, peel
//     off a JSON wrapper if present. Body content is preserved
//     verbatim by this step.
//  2. Scan lines from the top; for each non-empty line, strip
//     per-line noise (heading marks, bullet markers, label
//     prefixes) and check prTitleRegex. The first match is the
//     title. Per-line noise stripping is intentionally limited
//     to candidate title lines so bullets / headings that
//     legitimately appear in the body are preserved.
//  3. Body = lines after the title, truncated at the first
//     trailing ``` if one is present. If no closing fence
//     exists (the agent forgot it), body = everything after
//     the title — that's exactly the production regression
//     case.
//  4. If no line matches the CC regex, fall back to the first
//     non-empty line as title. Better to open a PR with a
//     non-conforming title than to fail outright — the user
//     can edit before merging.
//
// The classic fenced-block path is still preferred (cleaner
// inputs produce the same result either way); this version
// degrades gracefully when the agent's output is messy.
func parsePRReply(text string) (title, body string, err error) {
	cleaned := stripPRNoise(text)
	if cleaned == "" {
		return "", "", errParseAgentReply
	}

	lines := strings.Split(cleaned, "\n")

	// Phase 1: find the first line that, after stripping per-line
	// noise, matches prTitleRegex. The noise strip is per-line and
	// ONLY applied to candidate title lines — body bullets and
	// headings are left alone so they survive into the body.
	titleIdx := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		candidate := prLineNoiseRegex.ReplaceAllString(trimmed, "")
		// Compound prefixes (`- Title: feat: ...`, `## Title:
		// feat: ...`, `**Title:** feat: ...`) need multiple passes —
		// a single ReplaceAll only peels the outermost layer.
		// Loop until the line is stable.
		for {
			next := prLineNoiseRegex.ReplaceAllString(candidate, "")
			if next == candidate {
				break
			}
			candidate = next
		}
		if prTitleRegex.MatchString(candidate) {
			title = candidate
			titleIdx = i
			break
		}
	}

	// Phase 1b: multiline regex over the whole cleaned text. Catches
	// cases where the per-line approach fails because the title is
	// embedded inside a JSON value or otherwise not on its own
	// line — e.g. `{"text": "feat(...): ..."\n...}`. We only fall
	// through to this if Phase 1 found nothing.
	if titleIdx == -1 {
		if loc := prTitleAnywhereRegex.FindStringIndex(cleaned); loc != nil {
			title = strings.TrimSpace(cleaned[loc[0]:loc[1]])
			// Compute titleIdx from the byte offset: count newlines
			// before loc[0] to find the line index.
			titleIdx = strings.Count(cleaned[:loc[0]], "\n")
		}
	}

	// Phase 1c: extract a title that's been embedded as a JSON
	// string value ({"text": "feat(...): ..."}). Handles the
	// common LLM habit of wrapping replies in a JSON object for
	// safety. Only reached if Phases 1 and 1b both fail.
	if titleIdx == -1 {
		if m := prTitleJSONValueRegex.FindStringSubmatch(cleaned); m != nil {
			title = m[1]
			// titleIdx: count newlines before the title's start
			// position in the cleaned text.
			idx := strings.Index(cleaned, m[1])
			if idx >= 0 {
				titleIdx = strings.Count(cleaned[:idx], "\n")
			}
		}
	}

	// Phase 2: fallback — first non-empty line is the title.
	// Reaching here means the agent emitted something that doesn't
	// even pretend to follow Conventional Commits. Take what we
	// can get; the user can fix the title before merging.
	if titleIdx == -1 {
		for i, line := range lines {
			if trimmed := strings.TrimSpace(line); trimmed != "" {
				title = trimmed
				titleIdx = i
				break
			}
		}
		if titleIdx == -1 {
			return "", "", errParseAgentReply
		}
	}

	// Body: everything after the title, truncated at the first
	// trailing ``` (so a fenced block parses cleanly), then
	// TrimSpace'd. If no trailing fence exists, body = all of
	// it — the agent's "forgot the fence" regression case.
	bodyLines := lines[titleIdx+1:]
	endIdx := len(bodyLines)
	for i, line := range bodyLines {
		if strings.TrimSpace(line) == "```" {
			endIdx = i
			break
		}
	}
	if endIdx > 0 {
		body = strings.TrimSpace(strings.Join(bodyLines[:endIdx], "\n"))
	}

	// Normalize: title must be a single line. Phases 1/1b/2
	// already produce single-line titles, but Phase 1c
	// (JSON-wrapped capture) can pull a multi-line value when
	// the JSON value contains literal `\n` escape sequences.
	// Substitute any literal backslash-n with a real newline
	// first, then split at the first one. First line → title,
	// rest → merged into body.
	title = strings.ReplaceAll(title, "\\n", "\n")
	if i := strings.Index(title, "\n"); i >= 0 {
		extraBody := strings.TrimSpace(title[i+1:])
		title = strings.TrimSpace(title[:i])
		if extraBody != "" {
			if body == "" {
				body = extraBody
			} else {
				body = extraBody + "\n" + body
			}
		}
	}
	return title, body, nil
}

// stripPRNoise removes whole-input noise that surrounds the
// title: leading/trailing whitespace, unmatched fences, and an
// outermost JSON wrapper. Body content (bullet lists, headings
// inside the body, …) is preserved verbatim — only noise
// OUTSIDE the body is removed here. Per-line noise near the
// title is handled separately by parsePRReply's candidate scan.
func stripPRNoise(text string) string {
	text = strings.TrimSpace(text)

	// Unmatched leading fence (agent opened ``` but never closed).
	// Strip one or more in case the agent opened several nested
	// by accident.
	for strings.HasPrefix(text, "```") {
		nl := strings.IndexByte(text, '\n')
		if nl < 0 {
			return "" // bare fence at EOF, nothing useful
		}
		text = strings.TrimLeft(text[nl+1:], " \t")
	}

	// Unmatched trailing fence.
	text = strings.TrimSpace(strings.TrimSuffix(text, "```"))

	// JSON wrapper: outermost `{...}`. Don't try to parse JSON —
	// LLM JSON wrappers are usually malformed or have extra prose
	// keys. Just peel the braces; parsePRReply's regex scan will
	// find the real title inside.
	if strings.HasPrefix(text, "{") && strings.HasSuffix(text, "}") {
		text = strings.TrimSpace(text[1 : len(text)-1])
	}

	return text
}

// resolveProvider picks a GitProvider for the current chat and
// returns the (owner, repo) split CreatePR needs.
//
// Two-stage resolution (matches the design doc's "Provider
// 二段式解析" §4.5):
//
//  1. If c.Repo AND c.Provider are populated (set by /gtw fix
//     when the worktree was created), use them directly via
//     NewProvider — no network. owner/repo come from the yml
//     (already in "owner/repo" form), split with the cheap
//     first-slash helper from action.go.
//  2. Otherwise fall back to RemoteOriginURL + Detect, which
//     costs one or two HTTP probes (see Detect's Stage A/B).
//     For the Detect path owner/repo come from ParseRepoOwner,
//     which is URL-aware and handles self-hosted nested-group
//     GitLab URLs (group/subgroup/owner/repo).
//
// Errors are reformatted with redactForDisplay so we don't
// echo credentials; the bucket split mirrors runFixRemote.
func resolveProvider(ctx context.Context, c Context, deps HandlerDeps) (GitProvider, string, string, error) {
	if c.Repo != "" && c.Provider != "" {
		prov, err := NewProvider(ProviderKind(c.Provider), "", c.Worktree)
		if err != nil {
			return nil, "", "", fmt.Errorf("provider %q from gtw.yml unsupported: %w", c.Provider, err)
		}
		owner, repo, err := splitOwnerRepo(c.Repo)
		if err != nil {
			return nil, "", "", err
		}
		return prov, owner, repo, nil
	}
	remoteURL, err := RemoteOriginURL(ctx, c.RepoRoot, deps.Git)
	if err != nil || remoteURL == "" {
		return nil, "", "", fmt.Errorf("no `origin` remote — add one with `git remote add origin <url>`")
	}
	detect := deps.Detect
	if detect == nil {
		detect = Detect
	}
	prober := deps.Prober
	if prober == nil {
		prober = &ExecHTTPProber{Timeout: 3 * time.Second}
	}
	prov, err := detect(ctx, remoteURL, prober, c.Worktree)
	if err != nil {
		redacted := redactForDisplay(remoteURL)
		switch {
		case errors.Is(err, ErrInvalidRemoteURL):
			return nil, "", "", fmt.Errorf("invalid remote URL: %s", redacted)
		default:
			return nil, "", "", fmt.Errorf("unsupported git platform (host: %s)", redacted)
		}
	}
	if prov == nil {
		return nil, "", "", fmt.Errorf("provider detection returned no result")
	}
	owner, repo, err := ParseRepoOwner(remoteURL)
	if err != nil {
		return nil, "", "", fmt.Errorf("cannot parse owner/repo from remote URL: %v", err)
	}
	return prov, owner, repo, nil
}

// countBaseAhead returns the number of commits on HEAD that are
// not in `base` (`git rev-list --count base..HEAD`). Used to
// reject "/gtw pr" when the branch is at base (nothing to PR).
//
// `<base>` might not exist as a local ref (e.g. when the user
// invoked /gtw pr from a manually-created worktree that never
// ran /gtw fix's `git checkout <base>` step). In that case the
// local ref is `origin/<base>`; we retry with the remote form
// before giving up. This matches what /gtw fix gets for free
// because RefreshDefaultBranch checks the local branch out
// first; /gtw pr has no such precondition so we handle it here.
func countBaseAhead(ctx context.Context, worktree, base string, deps HandlerDeps) (int, error) {
	ranges := []string{base + "..HEAD", "origin/" + base + "..HEAD"}
	var lastStderr string
	for _, rng := range ranges {
		out, stderr, err := deps.Git.Run(ctx, worktree, "rev-list", "--count", rng)
		if err == nil {
			n, perr := atoi(strings.TrimSpace(out))
			if perr != nil {
				return 0, perr
			}
			return n, nil
		}
		lastStderr = strings.TrimSpace(stderr)
	}
	return 0, fmt.Errorf("rev-list --count %s..HEAD (also tried origin/%s..HEAD): %s",
		base, base, lastStderr)
}

// renderPROpenedCard renders the IM-friendly success card.
// Mirrors the /gtw push and /gtw close card style (✅ + branch /
// worktree footer; see wip/gtw-pr.md §5).
// renderPROpenedCard renders the IM-friendly success card.
// Format 1 (gtw/README.md §2.1): ✅ title + `→ field: value`
// rows. The previous `━━━━━━━━━━━━━━ \n 🌿/🔗/📁` form was a
// legacy mix that didn't fit any rule; the section divider is
// gone, and `🌿/🔗/📁` merge into the `→` family alongside the
// existing `→ base:` row.
func renderPROpenedCard(c Context, base, url string) string {
	return fmt.Sprintf(
		"✅ PR opened\n"+
			"→ branch:   %s\n"+
			"→ base:     %s\n"+
			"→ url:      %s\n"+
			"→ worktree: %s\n",
		c.Branch, base, url, c.Worktree,
	)
}