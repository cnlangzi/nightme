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
				"❌ branch %s has %d unpushed commits\n"+
					"hint: /gtw push first, then /gtw pr.",
				c.Branch, unpushed)), nil
	}

	// Reject when there's nothing on this branch that's not
	// already in base — opening an empty PR is a no-op.
	ahead, err := countBaseAhead(ctx, c.Worktree, baseBranch, deps)
	if err != nil {
		return reply(ctx, cs.Channel(), chatID, messageID,
			fmt.Sprintf("❌ count commits ahead of base: %v", err)), nil
	}
	if ahead == 0 {
		return reply(ctx, cs.Channel(), chatID, messageID,
			fmt.Sprintf(
				"❌ branch %s is at %s — nothing to PR",
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
func parsePRReply(text string) (title, body string, err error) {
	const fence = "```"
	start := strings.Index(text, fence)
	if start < 0 {
		return "", "", errParseAgentReply
	}
	// Move past the opening fence (and any info-string like
	// ```markdown — we accept whatever sits on the fence line).
	afterOpen := start + len(fence)
	// Skip rest of fence line (info-string) and the trailing \n.
	if nl := strings.IndexByte(text[afterOpen:], '\n'); nl >= 0 {
		afterOpen += nl + 1
	} else {
		// Fence was on its own at EOF with no body — empty.
		return "", "", errParseAgentReply
	}

	// Find the closing fence after afterOpen.
	end := strings.Index(text[afterOpen:], fence)
	if end < 0 {
		return "", "", errParseAgentReply
	}
	inner := text[afterOpen : afterOpen+end]
	inner = strings.TrimRight(inner, "\n")
	if inner == "" {
		return "", "", errParseAgentReply
	}

	// Split title (first line) from body (rest). We allow \n or
	// \r\n as the line separator; use SplitN so the body keeps
	// its newlines intact.
	lines := strings.SplitN(inner, "\n", 2)
	title = strings.TrimSpace(lines[0])
	if title == "" {
		return "", "", errParseAgentReply
	}
	if len(lines) == 2 {
		body = strings.TrimLeft(lines[1], "\n")
	}
	return title, body, nil
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