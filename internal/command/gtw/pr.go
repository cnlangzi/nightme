package gtw

// /gtw pr — generate a Conventional Commits title + body and open
// the PR/MR on the origin platform (GitHub via `gh`, GitLab via
// `glab`). Closes the loop with /gtw commit + /gtw push: after
// the F-XX split, `pr` no longer runs the agent commit, and
// `push` no longer auto-commits. The user is expected to have
// run `/gtw commit` then `/gtw push` already; `pr` does the
// remaining bit (open the PR/MR).
//
// Design refs:
//   - wip/gtw-pr.md       (design rationale, IM-friendly card, prompts)
//   - wip/gtw-pr-plan.md  (implementation phases, edge-case table)
//
// Flow:
//
//  1. Locate selectedCwd + .nightme/gtw.yml → Context.
//  2. Resolve default branch via DefaultBranch(repoRoot).
//  3. Gate 1: `git ls-remote --heads origin <branch>` confirms the
//     ref really exists on origin. Refused = user has never
//     pushed (or it was deleted); hint: /gtw push first.
//  4. Pick a provider (yml Repo/Provider wins; otherwise
//     RemoteOriginURL + Detect).
//  5. Gate 2: provider.FindOpenPRForBranch returns the single open
//     PR for this head, or nil if none exists. nil = proceed;
//     non-nil = "already open", surface the URL.
//  6. Pick an agent (CLI -a > yml cfg.PR.Agent > SelectedAgent),
//     one-shot RunOnce to generate the PR text.
//  7. parsePRReply extracts (title, body) from the fenced block.
//  8. Gate 3: the format gate — the title must not be the summary
//     heading, the title must carry the Conventional Commits shape,
//     the body must open with the summary heading. A reply that
//     fails is put back to the agent once, with the reason; a reply
//     that fails again opens no PR.
//  9. provider.CreatePR → URL.
// 10. Reply with ✅ PR opened + branch / base / url / worktree card.
//
// Pre-PR state (detached HEAD / uncommitted / untracked / ahead vs
// upstream / ahead vs base / behind vs upstream / merge conflicts /
// no upstream tracking) is NOT gated here — that's the user's
// responsibility under the new design. gh/glab's own response is
// the final word on whether the platform accepts the request.
//
// See dispatchPR's doc comment for the "known error → friendly
// hint, unknown error → verbatim" error contract.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/timeouts"
)

// errParseAgentReply is the sentinel returned by parsePRReply
// when the agent's reply carries nothing that can serve as a PR
// title (the reply is empty once the surrounding noise is
// stripped). The dispatcher surfaces the agent's text verbatim in
// the error reply so the user can still paste title + body manually
// into gh/glab.
var errParseAgentReply = errors.New("the reply could not be parsed as a PR title + body")

// errMissingPRTitle is the sentinel reported when the parsed PR
// title is the NightMe summary heading — the shape a model leaves
// behind when it wrote the body and forgot the title line. The
// dispatcher refuses to open the PR instead of shipping one titled
// `## Summary by NightMe`, and points the user at a retry.
var errMissingPRTitle = errors.New("agent output is missing the PR title (the output starts with the summary heading)")

// errPRTitleFormat is the sentinel reported when the parsed PR
// title does not carry the Conventional Commits shape — the shape a
// model lands on when it echoes the branch name or answers in
// prose.
var errPRTitleFormat = errors.New("the agent's PR title is not in `<type>(<optional-scope>): <subject>` form")

// errPRMissingSummaryHeading is the sentinel reported when the
// parsed PR body does not open with the NightMe summary heading —
// the section the reviewer reads first, and the one the normalizer
// rewrites into its canonical form.
var errPRMissingSummaryHeading = errors.New("the agent's PR body does not start with `" + summaryHeading + "`")

// The codes the format gate reports in the outcome's Data["format"]:
// which check rejected the agent's reply.
const (
	prFormatParseFailed    = "parse_failed"
	prFormatTitleMissing   = "title_missing"
	prFormatTitleFormat    = "title_format"
	prFormatSummaryHeading = "summary_heading"
)

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

// dispatchPR implements /gtw pr. The readiness contract is
// narrower than /gtw commit + /gtw push — we only gate on two
// conditions:
//
//  1. origin/<branch> ref exists (git layer; RemoteBranchExists)
//  2. no open PR for this branch (provider layer; FindOpenPRForBranch)
//
// A third gate runs on the agent's output rather than on the
// worktree: the format gate. The extracted title must carry the
// Conventional Commits shape, and the extracted body must open with
// the NightMe summary heading — so a model that answered in prose,
// or wrote the body and forgot the title line, fails loudly instead
// of opening a malformed PR. A rejected reply is put back to the
// agent once, with the reason; a reply that fails again opens no PR.
//
// Everything else (detached HEAD / uncommitted / untracked /
// ahead vs upstream / ahead vs base / behind vs upstream /
// merge conflicts / no upstream tracking) is the user's
// responsibility: /gtw pr attempts to open a PR regardless,
// and lets gh/glab's own error message be the final word on
// whether the platform accepts it.
//
// Known errors get friendly hints with a next-step suggestion;
// unknown errors propagate verbatim. The contract is implemented
// at the helper layer (RemoteBranchExists, GitHub/GitLab
// providers via wrapCreatePRError / wrapListPRError) —
// dispatchPR just dispatches on errors.Is.
//
// All non-success paths return nil error + a Result whose
// Reply has already been sent via cs.Emitter() — the same
// pattern dispatchPush uses (and that the factory wrapper
// relies on for "consumed: true, drop the message").
func dispatchPR(
	ctx context.Context,
	cs *chatsession.ChatSession,
	deps HandlerDeps,
	chatID, messageID string,
	args prArgs,
	cfg Config,
) (*Result, error) {
	c, res := loadDispatchContext(ctx, cs, deps, chatID, messageID)
	if res != nil {
		return res, nil
	}

	baseBranch, err := DefaultBranch(ctx, c.RepoRoot, deps.Git)
	if err != nil {
		// Reuse the same message format /gtw sync uses — it
		// already explains the "no origin remote" case.
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ %v", err),
			command.FailedOutcome("error", "the default branch could not be determined")), nil
	}

	// --- gate 1: origin/<branch> ref exists -------------------------
	// RemoteBranchExists wraps known stderr fragments (auth /
	// network / not-a-repo) with a friendly hint, and propagates
	// unknown stderr verbatim. We deliberately do NOT fall back
	// to "no upstream" on transient errors: telling the user
	// "push first" when the real problem is the network would
	// mislead them into running the wrong next step.
	headExists, err := RemoteBranchExists(ctx, c.Worktree, c.Branch, deps.Git)
	if err != nil {
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ %v", err),
			command.FailedOutcome("error", "the remote branch could not be checked")), nil
	}
	if !headExists {
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf(
				"❌ origin/%s does not exist — /gtw push first to publish the branch to origin",
				c.Branch),
			command.FailedOutcome("no_remote_branch",
				"the branch is not published to origin")), nil
	}

	// --- pick provider (must precede gate 2) ------------------------
	provider, owner, repo, err := resolveProvider(ctx, c, deps)
	if err != nil {
		// resolveProvider already produces friendly messages
		// (no `origin` remote / invalid URL / unsupported
		// platform). Echo verbatim.
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ %v", err),
			command.FailedOutcome("error", "the git host could not be resolved")), nil
	}

	// The label catalog is verified before the pull request is
	// opened, so the repository keeps the gtw labels the next /gtw
	// fix needs. A failed check only warns: opening a pull request
	// does not depend on the catalog holding the gtw labels. The
	// warning rides on every reply the command produces from here
	// on, so the user learns about it wherever /gtw pr ends.
	labelWarning := ""
	if _, err := ensureRepoLabels(ctx, deps, provider, owner, repo); err != nil {
		labelWarning = labelCheckWarning(owner, repo, err)
	}
	withWarning := func(body string) string {
		return prependWarning(labelWarning, body)
	}

	// --- gate 2: no open PR for this branch -------------------------
	// FindOpenPRForBranch translates ErrCLINotInstalled when the
	// gh/glab binary is missing; unknown stderr propagates
	// verbatim. Returning (nil, nil) means "no PR yet" — gate
	// passed, proceed to CreatePR.
	existing, err := provider.FindOpenPRForBranch(ctx, owner, repo, c.Branch)
	if err != nil {
		// FindOpenPRForBranch can return ErrCLINotInstalled (when
		// gh/glab is missing) — the "check existing PR" prefix
		// would mislead the user about the actual problem, so
		// surface the install hint without a gate-2 prefix.
		if errors.Is(err, ErrCLINotInstalled) {
			return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
				withWarning(fmt.Sprintf("❌ %v", err)),
				command.FailedOutcome("error", "the git host CLI is not installed")), nil
		}
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			withWarning(fmt.Sprintf("❌ check existing PR: %v", err)),
			command.FailedOutcome("error", "the existing pull requests could not be listed")), nil
	}
	if existing != nil {
		// The branch already has a pull request: a normal branch —
		// the pull request the command was asked for is there, and
		// nothing broke.
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			withWarning(fmt.Sprintf(
				"✅ branch %s already has an open PR (#%d): %s",
				c.Branch, existing.Number, existing.URL)),
			command.BranchOutcome("pr_exists", "the branch already has an open pull request",
				map[string]string{
					"branch":    c.Branch,
					"pr_number": fmt.Sprintf("%d", existing.Number),
					"pr_url":    existing.URL,
				})), nil
	}

	// --- run agent --------------------------------------------------
	// The agent part of /gtw pr is a two-attempt loop: the format
	// gate rejects a reply that does not carry a Conventional Commits
	// title and a NightMe summary heading, and a rejected reply is
	// put back to the agent once — with the reason — before the
	// command gives up.
	//
	// The language is resolved before the agent clock starts: the
	// lookup shells out to git, and its latency must not eat the
	// agent's budget.
	lang := resolveLanguage(ctx, cfg, c.Worktree, deps.Git)

	run, err := runPRAgent(ctx, cs, c, cfg, args, chatID, messageID,
		buildPRPrompt(c, baseBranch, lang))
	if err != nil {
		// The agent itself failed — no agent selected, the binary
		// is missing, the run crashed. That is not a format
		// problem, so there is no second attempt.
		return replyAgentOutcome(ctx, cs.Emitter(), chatID, messageID,
			withWarning(err.Error()), run.AgentName, run.Res,
			command.FailedOutcomeData("agent_error", "the agent did not produce a pull request",
				map[string]string{"agent": run.AgentName})), nil
	}
	if run.Failure != nil {
		// The reply did not carry the format the PR needs, twice.
		// No PR is opened: the user reads why, sees the reply the
		// agent produced, and gets the hint.
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			withWarning(fmt.Sprintf(
				"❌ %v (after %d attempts) — agent output was:\n%s\n↳ re-run `/gtw pr` to retry (or try another agent with `-a <name>`)",
				run.Failure.Reason, run.Attempts, indentLines(run.Rejected, "  "))),
			command.FailedOutcomeData("agent_error", "the agent reply did not match the pull request format",
				map[string]string{
					"agent":    run.AgentName,
					"format":   run.Failure.Code,
					"attempts": strconv.Itoa(run.Attempts),
				})), nil
	}

	title, body := run.Title, run.Body

	// Normalize the summary heading on the body in Go. The agent
	// is asked to write the plain heading, but a model that
	// paraphrases it would ship a body whose first section does
	// not carry the expected shape — the heading is a mechanical
	// property, so it is stamped in code, the same way `Closes #N`
	// is.
	body = normalizeSummaryHeading(body)

	// Stamp `Closes #N` on the body in Go. GitHub's auto-close
	// keyword only fires when the issue id is on its own line in
	// the PR description; we do this here (not via the agent
	// prompt) because the originating issue is already known
	// from c.Issue — deterministic, not worth an LLM round-trip.
	// For ModeLocal / unset Issue the helper is a no-op.
	body = appendClosesFooter(body, c.Issue)

	// Stamp the brand signature on the body in Go. The brand link
	// lives in the last line, not in the summary heading; the agent
	// writes neither, so both land here.
	body = appendPRFooter(body)

	url, err := provider.CreatePR(ctx, owner, repo, baseBranch, c.Branch, title, body)
	if err != nil {
		// Known error contract:
		//   - ErrStaleUpstream: race window — branch existed at
		//     gate 1 but was deleted before CreatePR. Same
		//     friendly hint as gate 1's "no upstream" miss.
		//   - ErrNoCommitsBetween: base and head point at the
		//     same commit. Different from ErrStaleUpstream:
		//     the branch is fine, there is just no diff to PR.
		//     "Push again" is the wrong next step — the user
		//     needs new commits on the head branch or a
		//     different base.
		//   - ErrPRExists: race window — no PR at gate 2 but
		//     one was opened before CreatePR. Point at the
		//     repo list (we don't have a reliable URL from
		//     gh/glab stderr across versions).
		//   - ErrCLINotInstalled: surface the install hint.
		//   - default: unknown — propagate verbatim, NO
		//     translation, NO masking.
		switch {
		case errors.Is(err, ErrStaleUpstream):
			return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
				withWarning(fmt.Sprintf(
					"❌ origin/%s no longer exists — /gtw push first to republish",
					c.Branch)),
				command.FailedOutcome("no_remote_branch",
					"the branch is no longer published to origin")), nil
		case errors.Is(err, ErrNoCommitsBetween):
			return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
				withWarning(fmt.Sprintf(
					"❌ no commits between %s and %s — push new commits first, or rebase onto a newer base.",
					baseBranch, c.Branch)),
				command.FailedOutcome("no_commits", "the branch has no commits to review")), nil
		case errors.Is(err, ErrPRExists):
			// The race window of gate 2: no pull request when the
			// gate ran, one when the pull request was opened. The
			// pull request is there — a normal branch.
			return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
				withWarning(fmt.Sprintf(
					"✅ a PR for %s already exists — check your repo's PR list.",
					c.Branch)),
				command.BranchOutcome("pr_exists", "the branch already has an open pull request",
					map[string]string{"branch": c.Branch})), nil
		case errors.Is(err, ErrCLINotInstalled):
			return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
				withWarning(fmt.Sprintf("❌ %v", err)),
				command.FailedOutcome("error", "the git host CLI is not installed")), nil
		default:
			return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
				withWarning(fmt.Sprintf("❌ create PR failed: %v", err)),
				command.FailedOutcome("error", "the pull request could not be created")), nil
		}
	}

	riskLevel, riskReason := extractRiskLevel(body)
	card := withWarning(renderPROpenedCard(c, baseBranch, url, riskLevel, riskReason))

	// Stamp the new PR into the workspace's cache before we
	// reply. Registry.WritePR allocates the cache on first
	// write (cwd-keyed; per-workspace), so the very next
	// outbound stamp on this cwd sees the new #N — the
	// footer doesn't have to wait for the lazy MaybeRefresh
	// (which would spawn a goroutine, hit `gh pr list`, and
	// converge on the same answer we already have). Without
	// this stamp the receipt card would render without the
	// PR link until the next TTL tick.
	if deps.PRCache != nil {
		newPR := &messages.PR{
			Number: prNumberFromURL(url),
			URL:    url,
			State:  "open",
		}
		// prcache is keyed by cwd (per-workspace), so /gtw pr
		// -a codex in a chat whose primary is claude makes the
		// link visible to claude's next StatusBar stamp.
		deps.PRCache.WritePR(c.Worktree, newPR)
	}

	// Success path: forward the run result so the footer (agentbar
	// + usagebar) renders. It is the result of the attempt whose
	// reply opened the PR — the retry's when the retry passed.
	// Failure paths above (the format gate, resolveProvider,
	// CreatePR, ErrPRExists) stay on the no-stamp reply — they're
	// not the agent-result surface.
	// The PR number comes from the URL. A URL that does not carry
	// one (a self-hosted host with a non-standard PR path) yields
	// no number at all — publishing a zero would name a PR that
	// does not exist.
	prData := map[string]string{
		"branch": c.Branch,
		"pr_url": url,
		"agent":  run.AgentName,
	}
	if number := prNumberFromURL(url); number > 0 {
		prData["pr_number"] = fmt.Sprintf("%d", number)
	}
	return replyAgentOutcome(ctx, cs.Emitter(), chatID, messageID,
		card, run.AgentName, run.Res,
		messages.Outcome{
			Status:  messages.OutcomeOK,
			Summary: "opened the pull request",
			Data:    prData,
		}), nil
}

// prAgentRun is what the agent phase of /gtw pr produced.
type prAgentRun struct {
	// Title / Body are the extracted PR text. They are valid only
	// when Failure is nil.
	Title string
	Body  string
	// Res is the RunResult of the attempt that produced the text —
	// or of the attempt that failed, on the agent-error path. The
	// success reply stamps it onto the OutboundMessage, so the
	// agent footer names the attempt that opened the PR.
	Res agent.RunResult
	// AgentName is the agent the attempts ran on.
	AgentName string
	// Failure is the format failure that stopped the run. nil when
	// the reply passed the gate.
	Failure *prFormatFailure
	// Rejected is the reply the gate rejected — the last one, when
	// the run made two attempts.
	Rejected string
	// Attempts counts the agent calls that were made.
	Attempts int
}

// runPRAgent runs the PR agent until it produces a reply that
// passes the format gate, or the attempt budget runs out.
//
// At most two attempts: the first one, and one correction round
// that names what was wrong with the first reply.
//
// Both attempts share the command's wall clock (timeouts.Command,
// applied by Factory.Handle) and each is additionally capped at
// timeouts.Agent. The retry never inherits the first attempt's
// expiry — it runs on the command's context — but it is not
// guaranteed an agent budget of its own: a first attempt that used
// the command's clock up leaves the retry nothing to run on.
//
// A failing agent call is not a format problem. When the FIRST
// attempt fails that way there is no reply to judge, and the caller
// gets the agent error. When the RETRY fails that way, the format
// failure of the first attempt is the diagnosis the user can act
// on — it is what the refusal is about — so that is what the
// refusal names.
func runPRAgent(
	ctx context.Context,
	cs *chatsession.ChatSession,
	c Context,
	cfg Config,
	args prArgs,
	chatID, messageID string,
	basePrompt string,
) (prAgentRun, error) {
	out := prAgentRun{}
	prompt := basePrompt
	for attempt := 1; attempt <= 2; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, timeouts.Agent)
		res, name, err := runAgentFor(attemptCtx, cs, c.Worktree, prompt, chatID, messageID,
			args.Agent, cfg.PR.Agent,
			// Drop OutResult; the dispatcher's card is the only
			// result surface.
			messages.OutResult)
		cancel()
		out.Res = res
		out.AgentName = name
		out.Attempts++
		if err != nil {
			if out.Failure != nil {
				return out, nil
			}
			return out, err
		}
		title, body, failure := checkPRReply(res.Text)
		if failure == nil {
			out.Title = title
			out.Body = body
			// The retry passed: the failure of the first attempt is
			// no longer the outcome.
			out.Failure = nil
			out.Rejected = ""
			return out, nil
		}
		out.Failure = failure
		out.Rejected = res.Text
		if attempt == 1 {
			prompt = buildPRCorrectionPrompt(basePrompt, failure.Reason.Error(), res.Text)
		}
	}
	return out, nil
}

// buildPRPrompt renders the text block the agent receives.
//
// Format (v5, NightMe-branded + Sourcery-style summary):
//
//	## Summary by NightMe
//	<1-2 sentences, imperative, WHAT not WHY>
//
//	New Features:        ← derived from this PR's commit types
//	Bug Fixes:
//	Enhancements:
//	Tests:
//	Documentation:
//	Chore / Build / CI:
//
//	Risk: <low|medium|high> — <concise reason>  ← MANDATORY on every PR body
//
// The `Closes #N` line is NOT part of the agent's output. dispatchPR
// appends it in Go via appendClosesFooter(c.Issue) — keeping it out
// of the prompt avoids a soft LLM guarantee that breaks on every
// model regression (lowercase / wrong issue / dropped / mid-
// paragraph). For ModeLocal worktrees (c.Issue == -1) the helper
// is a no-op.
//
// The brand signature is NOT part of the agent's output either:
// dispatchPR appends it in Go via appendPRFooter, so the body
// ends with the NightMe link to nightme.dev.
//
// Differences from v2:
//
//   - Body shape is category-prefixed bullets, not four rigid
//     dimensions. Reviewers scan by category, not by Why/What/
//     Diff/Test sections — the v2 four-dimension prompt made
//     reviewers read hundreds of words to extract the same
//     ~150 words of decision-relevant info (verified against
//     PR #303: gtw-generated body was 4× longer than the
//     equivalent Sourcery summary with identical coverage).
//   - Diff overview dropped entirely. GitHub's PR review UI
//     shows file changes; body duplication is noise.
//   - "Lead with Why" dropped. Sourcery leads with WHAT —
//     reviewers decide merge-worthiness from the category
//     prefix (Bug Fixes = behavior risk; New Features = new
//     contract; Enhancements = non-breaking).
//   - Risk line required on every PR body. Feeds the IM-card `→ risk:`
//     row in renderPROpenedCard.
//   - ## Context (repo / branch / base / worktree) dropped.
//     GitHub PR header shows all of this; body duplication
//     is noise.
//
// Hard constraint: only ONE `## ` markdown heading in the body
// (`## Summary by NightMe`). Categories are
// inline labels followed by a colon (`New Features:`), NOT
// `## New Features`.
// GitHub renders each `## ` heading with a horizontal rule, and a
// body with several such headings looks fragmented instead of
// scannable. See the `Do NOT` section for the explicit rules.
//
// Tool floor unchanged from v2 — mandatory git log + diff
// inspection before writing. The v2 self-check ("body shorter
// than git log = too little") is dropped: category-prefix mode
// is self-bounding on bullet count and doesn't need it.
//
// We deliberately do NOT pre-fetch git log / git diff here:
// the agent has its own workspace and Bash tool, and we want the
// body to be grounded in real diff output the LLM itself
// inspected.
//
// The PR title subject and the body prose are written in the
// configured output language (lang). The structural markers — the
// `## Summary by NightMe` heading, the
// category labels, the `Risk:` line, the Conventional Commits type
// keyword — stay in English: they are a machine-readable contract
// (extractRiskLevel parses the Risk line, the body shape is asserted
// by reviewers).
func buildPRPrompt(c Context, base string, lang string) string {
	// v5 prompt. The semantic flow is:
	//   commit history + final diff → PR body → Summary by NightMe → PR title
	//
	// The title is a semantic compression of the Summary, NOT an
	// inheritance of the latest commit subject. parsePRReply is
	// unchanged: the fenced block still has the title as its first
	// line and the body as the remainder. The `Closes #N` footer
	// continues to be appended in Go by dispatchPR via
	// appendClosesFooter, not by the agent.
	current := c.Branch
	var sb strings.Builder

	// --- Heading --------------------------------------------------
	sb.WriteString("You are preparing the Pull Request for the current Git branch.\n\n")

	// --- Context --------------------------------------------------
	sb.WriteString("## Context\n\n")
	sb.WriteString("The current branch is:\n\n")
	sb.WriteString("    " + current + "\n\n")
	sb.WriteString("The Pull Request will compare it against:\n\n")
	sb.WriteString("    " + base + "\n\n")
	sb.WriteString("This is ONE Pull Request.\n\n")
	sb.WriteString("The current branch may contain multiple commits since `" + base + "`.\n")
	sb.WriteString("Those commits are the development history of this ONE Pull Request.\n\n")
	sb.WriteString("The Pull Request represents the complete accumulated change:\n\n")
	sb.WriteString("    " + base + " → " + current + "\n\n")
	sb.WriteString("Your task is to describe that complete change for a Pull Request reviewer.\n\n")
	sb.WriteString("A commit is a development step.\n\n")
	sb.WriteString("A Pull Request is the reviewable unit of work.\n\n")
	sb.WriteString("Therefore, the Pull Request title and body should describe the resulting work of the Pull Request, rather than simply describing one of its commits.\n\n")

	// --- Goal -----------------------------------------------------
	sb.WriteString("## Goal\n\n")
	sb.WriteString("The goal is to produce a clear and accurate Pull Request description that allows a reviewer to understand:\n\n")
	sb.WriteString("- what this Pull Request is mainly about\n")
	sb.WriteString("- what meaningful changes it introduces\n")
	sb.WriteString("- what behavior or capability is affected\n")
	sb.WriteString("- what the overall result of the accumulated changes is\n\n")
	sb.WriteString("The commit history is useful evidence for understanding the work.\n\n")
	sb.WriteString("The final diff is the authoritative evidence of what the Pull Request actually changes.\n\n")
	sb.WriteString("The Pull Request body explains the complete change.\n\n")
	sb.WriteString("The `" + summaryHeading + "` section provides the concise semantic summary of that complete change.\n\n")
	sb.WriteString("The Pull Request title is the shortest useful expression of that Summary.\n\n")
	sb.WriteString("Therefore the semantic flow is:\n\n")
	sb.WriteString("    commit history + final diff\n")
	sb.WriteString("              ↓\n")
	sb.WriteString("       understand the PR\n")
	sb.WriteString("              ↓\n")
	sb.WriteString("          PR body\n")
	sb.WriteString("              ↓\n")
	sb.WriteString("      Summary by NightMe\n")
	sb.WriteString("              ↓\n")
	sb.WriteString("          PR title\n\n")
	sb.WriteString("Do not reverse this order.\n\n")
	sb.WriteString("Although the title appears first in the final output, determine its meaning only after the overall PR Summary has been established.\n\n")
	sb.WriteString("Do not decide the title first and then write the Summary around it.\n\n")

	// --- Understand the Complete Change ---------------------------
	sb.WriteString("## Understand the Complete Change\n\n")
	sb.WriteString("Before writing the PR description, inspect the complete change between the two branches.\n\n")
	sb.WriteString("Use:\n\n")
	sb.WriteString("    git log --oneline " + base + ".." + current + "\n\n")
	sb.WriteString("    git diff " + base + "..." + current + " --stat\n\n")
	sb.WriteString("    git diff " + base + "..." + current + "\n\n")
	sb.WriteString("When necessary, inspect important files individually:\n\n")
	sb.WriteString("    git diff " + base + "..." + current + " -- <path>\n\n")
	sb.WriteString("Use the commit history to understand:\n\n")
	sb.WriteString("- how the work evolved\n")
	sb.WriteString("- what problems were encountered\n")
	sb.WriteString("- why particular changes were made\n\n")
	sb.WriteString("Use the final diff to determine:\n\n")
	sb.WriteString("- what the branch actually changes\n")
	sb.WriteString("- what behavior exists after all commits are applied\n")
	sb.WriteString("- which changes are meaningful to the reviewer\n\n")
	sb.WriteString("Do not treat individual commits as separate units of PR output.\n\n")
	sb.WriteString("Do not assume that the latest commit represents the purpose of the Pull Request.\n\n")

	// --- Information Hierarchy ------------------------------------
	sb.WriteString("## Information Hierarchy\n\n")
	sb.WriteString("Different information has different roles.\n\n")
	sb.WriteString("### Commit history\n\n")
	sb.WriteString("Commit messages describe individual development steps.\n\n")
	sb.WriteString("They may be:\n\n")
	sb.WriteString("- narrow\n")
	sb.WriteString("- implementation-specific\n")
	sb.WriteString("- incremental\n")
	sb.WriteString("- corrective\n")
	sb.WriteString("- temporary\n")
	sb.WriteString("- focused on one part of the work\n\n")
	sb.WriteString("Therefore:\n\n")
	sb.WriteString("    commit subject ≠ PR title\n\n")
	sb.WriteString("Commit messages are evidence for understanding the Pull Request, not the final source for its title.\n\n")
	sb.WriteString("### Final diff\n\n")
	sb.WriteString("The final diff represents the accumulated result of all commits.\n\n")
	sb.WriteString("It is the strongest evidence for what the Pull Request finally changes.\n\n")
	sb.WriteString("### PR body\n\n")
	sb.WriteString("The PR body explains the meaningful changes introduced by the complete Pull Request.\n\n")
	sb.WriteString("It must follow the existing NightMe PR body format defined below.\n\n")
	sb.WriteString("### Summary by NightMe\n\n")
	sb.WriteString("`" + summaryHeading + "` is the canonical concise summary of the complete Pull Request.\n\n")
	sb.WriteString("It is the semantic bridge between the detailed PR body and the concise PR title.\n\n")
	sb.WriteString("### PR title\n\n")
	sb.WriteString("The title is a concise representation of the overall Pull Request.\n\n")
	sb.WriteString("It should be derived from the meaning of `" + summaryHeading + "`.\n\n")
	sb.WriteString("It is NOT:\n\n")
	sb.WriteString("- the latest commit title\n")
	sb.WriteString("- the most important-looking individual commit title\n")
	sb.WriteString("- a concatenation of commit titles\n")
	sb.WriteString("- an independent interpretation of one commit\n\n")

	// --- Output language ------------------------------------------
	sb.WriteString("## Output language\n\n")
	sb.WriteString("Write the PR title subject and the body prose in " + placeholderLanguage + ".\n\n")
	sb.WriteString("Keep these exactly in English, whatever the output language: the `" + summaryHeading + "` heading, the category labels (`New Features:`, `Bug Fixes:`, `Enhancements:`, `Tests:`, `Documentation:`, `Chore / Build / CI:`), the `Risk: <low|medium|high> — <reason>` line, and the Conventional Commits type keyword.\n\n")

	// --- PR Body Format -------------------------------------------
	sb.WriteString("## PR Body Format\n\n")
	sb.WriteString("The PR body MUST use the following existing format.\n\n")
	sb.WriteString("Do not invent a different structure.\n\n")
	sb.WriteString("The body MUST begin immediately with:\n\n")
	sb.WriteString(summaryHeading + "\n\n")
	sb.WriteString("<summary>\n\n")
	sb.WriteString("This Summary must be the first section of the PR body, immediately after the PR title.\n\n")
	sb.WriteString("Do not place any other section before it.\n\n")
	sb.WriteString("This Summary must describe the overall purpose and result of the complete Pull Request.\n\n")
	sb.WriteString("It should synthesize the meaningful changes into a concise description of what this Pull Request accomplishes as a whole.\n\n")
	sb.WriteString("Do not turn the Summary into a list of commit messages.\n\n")
	sb.WriteString("Do not make the Summary depend on the latest commit.\n\n")
	sb.WriteString("After the Summary section, use these category sections when applicable, in this order:\n\n")
	sb.WriteString("New Features:\n")
	sb.WriteString("Bug Fixes:\n")
	sb.WriteString("Enhancements:\n")
	sb.WriteString("Tests:\n")
	sb.WriteString("Documentation:\n")
	sb.WriteString("Chore / Build / CI:\n\n")
	sb.WriteString("Each section should contain concise bullet points describing meaningful changes in the complete Pull Request.\n\n")
	sb.WriteString("Do not merely copy commit subjects into these sections.\n\n")
	sb.WriteString("Do not create additional top-level sections.\n\n")
	sb.WriteString("If a section has no meaningful content, omit it.\n\n")
	sb.WriteString("After the category sections, the body MUST also contain exactly one Risk line:\n\n")
	sb.WriteString("Risk: <low|medium|high> — <concise reason>\n\n")
	sb.WriteString("Risk assessment is required for every PR.\n\n")
	sb.WriteString("Assess the risk based on the actual changes in the PR, including compatibility, breaking behavior, API or wire changes, migration requirements, dependency/version coupling, lifecycle or state changes, operational impact, and regression risk when applicable.\n\n")
	sb.WriteString("Even when the change is low risk, do not omit the Risk line.\n\n")

	// --- PR Title -------------------------------------------------
	sb.WriteString("## PR Title\n\n")
	sb.WriteString("After the PR body and `" + summaryHeading + "` have been established, derive the PR title from that Summary.\n\n")
	sb.WriteString("The title should be a semantic compression of the Summary.\n\n")
	sb.WriteString("It should NOT simply truncate or copy the Summary.\n\n")
	sb.WriteString("Rewrite the Summary into a concise, natural Conventional Commit title.\n\n")
	sb.WriteString("The title must describe the overall Pull Request, not an individual development step.\n\n")
	sb.WriteString("For example, if the branch contains several commits such as:\n\n")
	sb.WriteString("    fix(heartbeat): distinguish error vs clean terminal verdict\n")
	sb.WriteString("    fix(bridge-dsh): wire format and per-session permissions\n")
	sb.WriteString("    fix(outbound): finalize drain before dispatcher cancels ctx\n\n")
	sb.WriteString("do not automatically use:\n\n")
	sb.WriteString("    fix(outbound): finalize drain before dispatcher cancels ctx\n\n")
	sb.WriteString("merely because it is the latest commit.\n\n")
	sb.WriteString("Instead, first establish what the complete Pull Request accomplishes, express that in `" + summaryHeading + "`, and then derive the title from that Summary.\n\n")
	sb.WriteString("An individual commit title may happen to be a good PR title, but only when it accurately represents the overall change described by the Summary.\n\n")

	// --- Conventional Commit Requirements -------------------------
	sb.WriteString("## Conventional Commit Requirements\n\n")
	sb.WriteString("The PR title MUST use:\n\n")
	sb.WriteString("    <type>(<optional-scope>): <subject>\n\n")
	sb.WriteString("Allowed types:\n\n")
	sb.WriteString("    feat\n")
	sb.WriteString("    fix\n")
	sb.WriteString("    chore\n")
	sb.WriteString("    refactor\n")
	sb.WriteString("    docs\n")
	sb.WriteString("    test\n")
	sb.WriteString("    build\n")
	sb.WriteString("    ci\n")
	sb.WriteString("    perf\n")
	sb.WriteString("    style\n")
	sb.WriteString("    revert\n\n")
	sb.WriteString("Choose the type according to the overall purpose of the Pull Request, as expressed by `" + summaryHeading + "`.\n\n")
	sb.WriteString("Do not simply inherit the type from the latest commit.\n\n")
	sb.WriteString("Choose the scope according to the subsystem or area represented by the overall Pull Request.\n\n")
	sb.WriteString("Do not simply inherit the scope from the latest commit.\n\n")
	sb.WriteString("If no single scope accurately represents the overall Pull Request, omit the scope.\n\n")
	sb.WriteString("Keep the subject concise, specific, and focused on the resulting behavior or capability.\n\n")
	sb.WriteString("Keep the title within 72 characters when reasonably possible.\n\n")

	// --- Semantic Consistency -------------------------------------
	sb.WriteString("## Semantic Consistency\n\n")
	sb.WriteString("Before producing the final output, verify internally:\n\n")
	sb.WriteString("- The body describes the complete `" + base + "` → `" + current + "` change.\n")
	sb.WriteString("- `" + summaryHeading + "` accurately summarizes the complete PR.\n")
	sb.WriteString("- The title expresses the same overall meaning as the Summary.\n")
	sb.WriteString("- The title does not introduce a different interpretation.\n")
	sb.WriteString("- The title is not merely copied from the latest commit.\n")
	sb.WriteString("- The title represents the Pull Request rather than one development step.\n\n")
	sb.WriteString("Do not output this verification.\n\n")

	// --- Output Format --------------------------------------------
	sb.WriteString("## Output Format\n\n")
	sb.WriteString("Reply with ONE fenced markdown code block.\n\n")
	sb.WriteString("The FIRST line inside the code block MUST be the PR title.\n\n")
	sb.WriteString("The SECOND line MUST be empty.\n\n")
	sb.WriteString("The THIRD line MUST be `" + summaryHeading + "`.\n\n")
	sb.WriteString("The remaining lines MUST be the PR body.\n\n")
	sb.WriteString("The PR body MUST follow the existing format described above.\n\n")
	sb.WriteString("Do not output:\n\n")
	sb.WriteString("- reasoning\n")
	sb.WriteString("- analysis\n")
	sb.WriteString("- alternative titles\n")
	sb.WriteString("- commit-by-commit title candidates\n")
	sb.WriteString("- explanations outside the fenced block\n")
	sb.WriteString("- additional output before or after the fenced block\n")
	sb.WriteString("- nested fenced code blocks\n\n")
	sb.WriteString("Do NOT execute:\n\n")
	sb.WriteString("    git commit\n")
	sb.WriteString("    git push\n")
	sb.WriteString("    gh pr create\n")
	sb.WriteString("    glab mr create\n\n")
	sb.WriteString("The caller is responsible for creating the Pull Request.\n")

	return applyLanguage(sb.String(), lang)
}

// parsePRReply extracts (title, body) from an agent's reply.
// The reply is expected to contain ONE fenced code block
// (“ ``` ... ``` “); the first line inside the fence is the
// title, the remainder is the body.
//
// Robust to:
//   - preamble prose ("Here you go:") before the opening fence
//   - postscript prose ("Let me know if you'd like changes")
//     after the closing fence
//   - multiple blank lines between the prose and the fence
//
// Returns errParseAgentReply when the reply carries nothing that
// can serve as a title — the reply is empty once the surrounding
// noise is stripped.
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

// prTitleFormatRegex is the shape the format gate requires of the
// extracted PR title: <type>(<optional-scope>): <subject>.
//
// The type token stops at whitespace, `(` or `:`, so the scope and
// the colon can only appear where the shape puts them; the scope
// must be non-empty; the subject only has to start with a
// non-whitespace character.
//
// This is NOT the same rule as prTitleRegex: that one LOCATES the
// title in the agent's reply and carries the fixed Conventional
// Commits type allow-list; this one checks the shape of whatever was
// located, and deliberately accepts any type word.
var prTitleFormatRegex = regexp.MustCompile(`^[^\s(:]+(\([^)\n]+\))?!?: \S`)

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

// riskLineRegex extracts the `Risk: <low|medium|high> — <reason>`
// line that buildPRPrompt requires on every PR body. The separator
// is intentionally tolerant —
// em-dash, hyphen, or colon all work, since LLMs are
// inconsistent. The level match is case-insensitive so
// `Risk: HIGH — ...` and `Risk: high — ...` both match — the
// caller lowercases the captured value for canonical output.
// Multiline (?m) so a Risk line anywhere in the body is
// recognised, not just at the very end.
//
// Examples that match:
//
//	Risk: low — typo fix
//	Risk: medium - touches version fallback
//	Risk: HIGH: auth change
var riskLineRegex = regexp.MustCompile(
	`(?im)^Risk:\s*(low|medium|high)\s*[—\-:]\s*(.+?)\s*$`,
)

// prLineNoiseRegex matches the per-line noise patterns we strip
// from candidate title lines: Markdown heading marks (#, ##, …),
// bullet markers (- /*/1.), and label prefixes (Title: /
// PR Title: / Title :). See stripPRNoise.
var prLineNoiseRegex = regexp.MustCompile(
	`^(?:#+\s+|-\s+|\*\s+|\d+\.\s+|PR\s+Title\s*:|Title\s*:)\s*`,
)

// prNumberFromURL extracts the PR/MR number from a gh/glab
// URL like `https://github.com/o/r/pull/123` or
// `https://gitlab.com/o/r/-/merge_requests/123`. Returns 0
// when the URL doesn't match a known pattern — the caller
// still has the URL for rendering, the number just won't be
// available for the link label. We don't bother with
// gh/glab enterprise URL variants; production has used
// the default URL shape since v1 and the regex tolerates
// any host prefix.
var prNumberFromURLRegex = regexp.MustCompile(`/(?:pull|merge_requests)/(\d+)(?:[?#]|$)`)

func prNumberFromURL(rawURL string) int {
	m := prNumberFromURLRegex.FindStringSubmatch(rawURL)
	if len(m) < 2 {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return n
}

// appendClosesFooter stamps the GitHub auto-close keyword on the
// last line of the PR body. Returns body unchanged when issue <= 0
// (ModeLocal worktrees, which never had a remote issue).
//
// Why this lives in Go instead of the agent prompt: the originating
// issue id is already known from c.Issue, and GitHub's auto-close
// rule is mechanical — a `Closes #N` line anywhere in the body
// closes the issue on merge. Letting the LLM produce it is a soft
// guarantee that breaks on every model regression (lowercase, wrong
// issue, dropped, mid-paragraph). Doing it in code is deterministic
// and zero-cost.
//
// Idempotency: if the agent's body already contains the exact
// `Closes #N` line (e.g. an older prompt still in flight during a
// rollout), we leave the body alone — appending a duplicate
// wouldn't hurt GitHub but would clutter the body. Case-sensitive
// match is intentional: lowercase `closes #N` still satisfies
// GitHub, so the append stays harmless.
func appendClosesFooter(body string, issue int) string {
	if issue <= 0 {
		return body
	}
	footer := fmt.Sprintf("Closes #%d", issue)
	if strings.Contains(body, footer) {
		return body
	}
	// Trim trailing whitespace, then append with a blank-line
	// separator so the body reads naturally. We always end with
	// a single trailing newline so downstream tooling that
	// concatenates PR descriptions doesn't glue the footer to
	// the last bullet.
	body = strings.TrimRight(body, " \t\r\n")
	return body + "\n\n" + footer + "\n"
}

// appendPRFooter stamps the brand signature as the last line of a
// PR body, separated from the content above it by a horizontal
// rule.
//
// Why this lives in Go instead of the agent prompt: the brand link
// is a mechanical property of the body. A model that paraphrases
// the signature — dropping the link, or pointing it at a stale
// target — would otherwise ship an unbranded body, and the
// regression would only show up on some PRs. Doing it in code is
// deterministic and zero-cost, the same reasoning as
// appendClosesFooter.
//
// The signature is appended, never substituted: whatever the agent
// wrote stays in the body, so a quoted example of the footer line
// survives.
func appendPRFooter(body string) string {
	return strings.TrimRight(body, " \t\r\n") + brandFooter
}

// isSummaryHeading reports whether line is the NightMe summary
// heading in any form a model can write it: the plain heading, the
// linked heading with any URL, and any casing of the words.
//
// The brand link is stripped before the comparison, so
// `## Summary by [NightMe](https://nightme.dev)` reduces to the
// plain heading.
//
// This is the one rule the format gate and the body normalizer
// share: every heading the gate accepts is rewritten into the
// canonical plain form, and a line that merely starts like the
// heading (e.g. `## Summary by NightMe Notes`) is neither accepted
// nor rewritten.
func isSummaryHeading(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.EqualFold(brandLinkRegex.ReplaceAllString(trimmed, "NightMe"), summaryHeading)
}

// hasSummaryHeading reports whether the body's first line is the
// NightMe summary heading.
func hasSummaryHeading(body string) bool {
	line, _, _ := strings.Cut(body, "\n")
	return isSummaryHeading(line)
}

// normalizeSummaryHeading rewrites the body's leading summary
// heading into the canonical plain form.
//
// Why this lives in Go instead of the agent prompt: the heading is
// a mechanical property of the body. A model that paraphrases it —
// adding a link, keeping an older link target, or trailing the line
// with spaces — would otherwise ship a body whose first section
// does not carry the expected shape, and the regression would only
// show up on some PRs. Doing it in code is deterministic and
// zero-cost, the same reasoning as appendClosesFooter.
//
// The leading line is rewritten when isSummaryHeading accepts it —
// the plain heading, any linked variant of it, any casing; trailing
// spaces and a CR from CRLF input are dropped with it. A heading
// that is not the body's first line keeps its text.
func normalizeSummaryHeading(body string) string {
	line, rest, found := strings.Cut(body, "\n")
	if !isSummaryHeading(line) {
		return body
	}
	if !found {
		return summaryHeading
	}
	return summaryHeading + "\n" + rest
}

// prFormatFailure is the verdict of the format gate over one agent
// reply: which check rejected it, and the reason the user reads.
type prFormatFailure struct {
	// Code names the failed check; it rides on the outcome's
	// Data["format"].
	Code string
	// Reason is the sentence the user reads, and the sentence the
	// correction prompt names.
	Reason error
}

// validatePRText is the format gate over an extracted PR title and
// body: nil means the pair may open a PR.
//
// The checks run in order — the title is present, the title carries
// the Conventional Commits shape, the body opens with the summary
// heading — so the first failure names the reason.
func validatePRText(title, body string) *prFormatFailure {
	switch {
	case isSummaryHeading(title):
		return &prFormatFailure{Code: prFormatTitleMissing, Reason: errMissingPRTitle}
	case !prTitleFormatRegex.MatchString(title):
		return &prFormatFailure{Code: prFormatTitleFormat, Reason: errPRTitleFormat}
	case !hasSummaryHeading(body):
		return &prFormatFailure{Code: prFormatSummaryHeading, Reason: errPRMissingSummaryHeading}
	}
	return nil
}

// checkPRReply extracts the PR text from one agent reply and puts it
// through the format gate.
//
// The failure names the check that rejected the reply: the parse
// step, or one of the three format checks.
func checkPRReply(text string) (title, body string, failure *prFormatFailure) {
	title, body, err := parsePRReply(text)
	if err != nil {
		return "", "", &prFormatFailure{Code: prFormatParseFailed, Reason: err}
	}
	if failure := validatePRText(title, body); failure != nil {
		return "", "", failure
	}
	return title, body, nil
}

// prCorrectionReplyLimit caps how much of the rejected reply the
// correction prompt quotes back.
const prCorrectionReplyLimit = 4000

// buildPRCorrectionPrompt appends the correction round to the PR
// prompt: what was rejected, why, and the format the reply has to
// carry.
//
// The rejected reply is quoted back — truncated to its last
// prCorrectionReplyLimit characters, behind an elision marker —
// because the agent has to see what it wrote to know what to fix.
func buildPRCorrectionPrompt(prompt, reason, reply string) string {
	excerpt := reply
	elided := false
	if runes := []rune(reply); len(runes) > prCorrectionReplyLimit {
		excerpt = string(runes[len(runes)-prCorrectionReplyLimit:])
		elided = true
	}
	var sb strings.Builder
	sb.WriteString(prompt)
	sb.WriteString("\n\n## Correction\n\n")
	sb.WriteString("Your previous reply was rejected: " + reason + ".\n\n")
	sb.WriteString("Your previous reply was:\n\n")
	if elided {
		sb.WriteString("… (truncated)\n")
	}
	sb.WriteString(excerpt)
	sb.WriteString("\n\nProduce the corrected reply now. Keep the same format:\n\n")
	sb.WriteString("- the FIRST line is the PR title: `<type>(<optional-scope>): <subject>`\n")
	sb.WriteString("- the SECOND line is empty\n")
	sb.WriteString("- the THIRD line is `" + summaryHeading + "`\n")
	sb.WriteString("- the remaining lines are the PR body\n")
	return sb.String()
}

// extractRiskLevel pulls the `Risk: <level> — <reason>` line out
// of a parsed PR body. The buildPRPrompt requires Risk on every
// PR body, so a present line is the expected case; absence is
// tolerated as a defensive fallback (returns ("", "")) so a
// malformed LLM reply doesn't crash dispatchPR. The caller
// treats absence as "no risk field in the IM card". The level
// is lowercased before returning so `Risk: HIGH — ...` and
// `Risk: high — ...` produce the same result.
//
// Body is passed in raw (already trimmed of leading/trailing
// whitespace by parsePRReply); the regex runs multi-line so a
// Risk line anywhere in the body is recognised.
func extractRiskLevel(body string) (level, reason string) {
	m := riskLineRegex.FindStringSubmatch(body)
	if len(m) < 3 {
		return "", ""
	}
	return strings.ToLower(m[1]), strings.TrimSpace(m[2])
}

// parsePRReply extracts title and body from an LLM-generated PR
// description.
//
// The original implementation required a strict ``` … ``` fence
// around the reply. LLMs are unreliable at exact format compliance:
// they may forget the fence, wrap the output in JSON, prefix it
// with a Markdown heading, or scatter the title inside a bullet
// list. Any of those made the daemon hard-error on the parse,
// forcing the user to re-invoke /gtw pr manually — a bad
// experience for a feature whose whole point is automation.
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
//     non-empty line as title. The fallback only decides WHICH
//     line is the title — the format gate then judges it, and a
//     title that does not carry the Conventional Commits shape is
//     refused rather than opened.
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
	// can get: the format gate judges it next, and a title it
	// refuses is put back to the agent rather than opened.
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
		// Split the yml's "owner/repo" form. The gtw yml stores
		// Repo in the canonical single-slash form (see runFixRemote
		// §5.2.③); ParseRepoOwner is URL-aware and not appropriate
		// here. Validation must reject empty halves so a malformed
		// entry doesn't silently produce an empty owner / repo.
		idx := strings.Index(c.Repo, "/")
		if idx <= 0 || idx == len(c.Repo)-1 {
			return nil, "", "", fmt.Errorf("gtw: invalid owner/repo %q", c.Repo)
		}
		return prov, c.Repo[:idx], c.Repo[idx+1:], nil
	}
	remoteURL, err := RemoteOriginURL(ctx, c.RepoRoot, deps.Git)
	if err != nil || remoteURL == "" {
		return nil, "", "", fmt.Errorf("no `origin` remote — add one with `git remote add origin <url>`")
	}
	prov, err := detectProvider(ctx, deps, remoteURL, c.Worktree)
	if err != nil {
		redacted := redactForDisplay(remoteURL)
		switch {
		case errors.Is(err, ErrInvalidRemoteURL):
			return nil, "", "", fmt.Errorf("invalid remote URL: %s", redacted)
		case errors.Is(err, ErrProviderUnreachable):
			return nil, "", "", fmt.Errorf("git host unreachable (%s): %w", redacted, err)
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

// renderPROpenedCard renders the IM-friendly success card.
// Format 1 (gtw/README.md §2.1): ✅ title + `→ field: value`
// rows. The previous `━━━━━━━━━━━━━━ \n 🌿/🔗/📁` form was a
// legacy mix that didn't fit any rule; the section divider is
// gone, and `🌿/🔗/📁` merge into the `→` family alongside the
// existing `→ base:` row.
//
// Renders the `→ risk:` row sourced via extractRiskLevel(body).
// The buildPRPrompt requires Risk on every PR body, so the row
// is normally always present; the `riskLevel == ""` branch stays
// as a defensive fallback for malformed LLM replies.
func renderPROpenedCard(c Context, base, url, riskLevel, riskReason string) string {
	var sb strings.Builder
	sb.WriteString("✅ PR opened\n")
	fmt.Fprintf(&sb, "→ branch:   %s\n", c.Branch)
	fmt.Fprintf(&sb, "→ base:     %s\n", base)
	fmt.Fprintf(&sb, "→ url:      %s\n", url)
	fmt.Fprintf(&sb, "→ worktree: %s\n", c.Worktree)
	if riskLevel != "" {
		fmt.Fprintf(&sb, "→ risk:     %s — %s\n", riskLevel, riskReason)
	}
	return sb.String()
}
