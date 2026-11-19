package gtw

// pr_view.go implements `/gtw pr view [number]` — the read-only
// command that reports whether a pull request can be merged.
//
// The command answers one question: is this pull request mergeable
// right now? The platforms compute the answer asynchronously, so the
// command waits for the computation instead of reporting the
// platform's "not ready yet" word as the answer.
//
// The command reads; it never writes. It runs no hook, touches no
// worktree state and changes no label. `/gtw pr` — the path that
// opens a pull request — is untouched.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
)

const (
	// prViewMaxAttempts is how many times the command reads the
	// merge state before it reports the platform as unable to
	// compute it.
	prViewMaxAttempts = 5

	// prViewRetryDelay is the wait between two reads.
	prViewRetryDelay = 3 * time.Second
)

// dispatchPRView implements `/gtw pr view`. number is the pull
// request to read; 0 means "the open pull request of the branch the
// chat works on".
//
// The platform answers in one of three ways, and the three are
// different kinds of answer:
//
//   - the pull request can be merged — the work is done, the
//     outcome is `ok`;
//   - the pull request cannot be merged yet — a conclusion, not a
//     breakdown: the outcome is `failed` without an error
//     description, so a workflow keeps going and branches on
//     Data["conflict"];
//   - the platform cannot tell — an exception: the outcome carries
//     an error description and the run hands it to the model.
func dispatchPRView(
	ctx context.Context,
	cs *chatsession.ChatSession,
	deps HandlerDeps,
	chatID, messageID string,
	number int,
) (*Result, error) {
	c, res := loadDispatchContext(ctx, cs, deps, chatID, messageID)
	if res != nil {
		return res, nil
	}

	provider, owner, repo, err := resolveProvider(ctx, c, deps)
	if err != nil {
		// resolveProvider already produces friendly messages
		// (no `origin` remote / invalid URL / unsupported
		// platform). Echo verbatim.
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ %v", err),
			command.FailedOutcome("provider_error", "the git host could not be resolved")), nil
	}

	if number <= 0 {
		existing, err := provider.FindOpenPRForBranch(ctx, owner, repo, c.Branch)
		if err != nil {
			return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
				fmt.Sprintf("❌ %v", err),
				command.FailedOutcome("provider_error", "the pull requests could not be listed")), nil
		}
		if existing == nil {
			return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
				fmt.Sprintf("✨ no open pull request for %s", c.Branch),
				command.NoopOutcome("the branch has no open pull request",
					map[string]string{"reason": "no_pr"})), nil
		}
		number = existing.Number
	}

	state, err := readPRMergeState(ctx, provider, owner, repo, number, deps)
	if err != nil {
		if errors.Is(err, ErrPRNotFound) {
			return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
				fmt.Sprintf("❌ no pull request #%d in %s/%s", number, owner, repo),
				command.FailedOutcome("pr_not_found", "the pull request does not exist")), nil
		}
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ %v", err),
			command.FailedOutcome("provider_error", "the merge state could not be read")), nil
	}

	return replyPRViewOutcome(ctx, cs.Emitter(), chatID, messageID, state), nil
}

// readPRMergeState reads the merge state and waits for the platform
// to compute it.
//
// The platforms compute the merge state asynchronously: GitHub's
// mergeable reads UNKNOWN until its background job is done,
// GitLab's detailed_merge_status reads "checking" for the same
// window. The command waits for that window to close instead of
// reporting the platform's "not ready" word as an answer.
//
// The wait is bounded: at most prViewMaxAttempts reads,
// prViewRetryDelay apart. A pull request that is no longer open is
// never computed — the platform leaves the state of a closed pull
// request alone — so it is read once and reported as closed.
func readPRMergeState(
	ctx context.Context,
	provider GitProvider,
	owner, repo string,
	number int,
	deps HandlerDeps,
) (*PRMergeState, error) {
	var state *PRMergeState
	for attempt := 0; attempt < prViewMaxAttempts; attempt++ {
		read, err := provider.GetPRMergeState(ctx, owner, repo, number)
		if err != nil {
			return nil, err
		}
		if read == nil {
			return nil, fmt.Errorf("the platform returned no merge state for PR #%d", number)
		}
		state = read
		if read.State != "open" || read.Verdict != PRMergeUnknown {
			return read, nil
		}
		if attempt == prViewMaxAttempts-1 {
			break
		}
		if err := sleepFor(ctx, deps, prViewRetryDelay); err != nil {
			return nil, err
		}
	}
	return state, nil
}

// replyPRViewOutcome turns a merge state into the card and the
// structured outcome.
//
// The card and the outcome are two faces of the same decision: `ok`
// says the pull request can be merged and the card says the same
// thing. A pull request that cannot be merged is a conclusion, not
// a breakdown — the outcome carries no error description, so a
// workflow keeps going and branches on Data["conflict"] instead.
//
// The rule that ties the two together: the status is `ok` if and
// only if Data["mergeable"] is "1".
func replyPRViewOutcome(
	ctx context.Context,
	em messages.Emitter,
	chatID, messageID string,
	state *PRMergeState,
) *Result {
	data := map[string]string{
		"pr_number":   strconv.Itoa(state.Number),
		"pr_url":      state.URL,
		"pr_state":    state.State,
		"base_branch": state.Base,
		"mergeable":   "0",
		"conflict":    "0",
		"merge_state": string(state.Verdict),
	}

	// A pull request that is no longer open is never going to
	// merge — the platform stopped computing its merge state when
	// it was closed or merged. Nothing is broken and nothing is
	// waiting: the request has nothing to do.
	if state.State != "" && state.State != "open" {
		data["reason"] = "pr_closed"
		return replyOutcome(ctx, em, chatID, messageID,
			renderPRViewCard(prViewTitle(state), state, ""),
			command.NoopOutcome(prViewSummary(state), data))
	}

	switch state.Verdict {
	case PRMergeMergeable:
		data["mergeable"] = "1"
		return replyOutcome(ctx, em, chatID, messageID,
			renderPRViewCard(prViewTitle(state), state, ""),
			messages.Outcome{
				Status:  messages.OutcomeOK,
				Summary: prViewSummary(state),
				Data:    data,
			})
	case PRMergeConflicting:
		data["conflict"] = "1"
		return replyOutcome(ctx, em, chatID, messageID,
			renderPRViewCard(prViewTitle(state), state, prViewHint(state)),
			command.BranchOutcome("merge_conflict", prViewSummary(state), data))
	case PRMergeBehind:
		return replyOutcome(ctx, em, chatID, messageID,
			renderPRViewCard(prViewTitle(state), state, prViewHint(state)),
			command.BranchOutcome("pr_behind", prViewSummary(state), data))
	case PRMergeBlocked:
		return replyOutcome(ctx, em, chatID, messageID,
			renderPRViewCard(prViewTitle(state), state, ""),
			command.BranchOutcome("pr_blocked", prViewSummary(state), data))
	}

	// The platform never computed the state. That is a breakdown,
	// not a conclusion: the command cannot say whether the pull
	// request merges, so the run hands the question to the model.
	return replyOutcome(ctx, em, chatID, messageID,
		renderPRViewCard(prViewTitle(state), state, ""),
		command.FailedOutcomeData("state_unknown",
			prViewSummary(state), data,
			fmt.Sprintf("the platform did not compute the merge state of PR #%d", state.Number)))
}

// prViewTitle is the card's title line.
//
// The emoji comes from the gtw emoji dictionary (README §1.1) and
// sits on the same side as the outcome status (§1.2): ✅ for ok, ❌
// for failed, ✨ for noop.
func prViewTitle(state *PRMergeState) string {
	switch state.State {
	case "merged":
		return fmt.Sprintf("✨ PR #%d is already merged", state.Number)
	case "closed":
		return fmt.Sprintf("✨ PR #%d is closed", state.Number)
	}
	switch state.Verdict {
	case PRMergeMergeable:
		return fmt.Sprintf("✅ PR #%d is mergeable", state.Number)
	case PRMergeConflicting:
		return fmt.Sprintf("❌ PR #%d has merge conflicts", state.Number)
	case PRMergeBehind:
		if state.Base != "" {
			return fmt.Sprintf("❌ PR #%d is behind %s", state.Number, state.Base)
		}
		return fmt.Sprintf("❌ PR #%d is behind its base", state.Number)
	case PRMergeBlocked:
		return fmt.Sprintf("❌ PR #%d is blocked", state.Number)
	default:
		return fmt.Sprintf("❌ PR #%d state is unknown", state.Number)
	}
}

// prViewSummary is the outcome's one-line restatement of the same
// decision the card shows.
func prViewSummary(state *PRMergeState) string {
	switch state.State {
	case "merged":
		return fmt.Sprintf("PR #%d is already merged", state.Number)
	case "closed":
		return fmt.Sprintf("PR #%d is closed", state.Number)
	}
	switch state.Verdict {
	case PRMergeMergeable:
		return fmt.Sprintf("PR #%d is mergeable", state.Number)
	case PRMergeConflicting:
		return fmt.Sprintf("PR #%d has merge conflicts", state.Number)
	case PRMergeBehind:
		if state.Base != "" {
			return fmt.Sprintf("PR #%d is behind %s", state.Number, state.Base)
		}
		return fmt.Sprintf("PR #%d is behind its base", state.Number)
	case PRMergeBlocked:
		return fmt.Sprintf("PR #%d is blocked", state.Number)
	default:
		return fmt.Sprintf("the merge state of PR #%d could not be computed", state.Number)
	}
}

// prViewHint is the card's closing "↳" line. Only the two
// verdicts the user can act on carry one — the rest say what the
// state is, and no action follows from that.
func prViewHint(state *PRMergeState) string {
	switch state.Verdict {
	case PRMergeConflicting, PRMergeBehind:
		if state.Base == "" {
			return "merge the base into the branch, then push"
		}
		return fmt.Sprintf("merge %s into the branch, then push", state.Base)
	}
	return ""
}

// renderPRViewCard builds the status card.
//
// Format 1 (gtw/README.md §2.1): a title line plus `→ field: value`
// rows, with the field names aligned, and an optional "↳" hint.
func renderPRViewCard(title string, state *PRMergeState, hint string) string {
	var b strings.Builder
	b.WriteString(title)
	b.WriteString("\n")
	if state.Head != "" {
		fmt.Fprintf(&b, "→ branch:   %s\n", state.Head)
	}
	if state.Base != "" {
		fmt.Fprintf(&b, "→ base:     %s\n", state.Base)
	}
	if word := prViewStateWord(state); word != "" {
		fmt.Fprintf(&b, "→ state:    %s\n", word)
	}
	if state.URL != "" {
		fmt.Fprintf(&b, "→ url:      %s\n", state.URL)
	}
	if hint != "" {
		fmt.Fprintf(&b, "↳ %s\n", hint)
	}
	return b.String()
}

// prViewStateWord is the card's "state" row: the platform's own
// merge word for an open pull request (GitHub's "DIRTY", GitLab's
// "need_rebase"), and the pull request's own state once it is
// closed or merged — the platform stops computing the merge state
// then, so there is no merge word left to report.
func prViewStateWord(state *PRMergeState) string {
	if state.State != "" && state.State != "open" {
		return state.State
	}
	return state.Raw
}

// prViewCmdSpec is the argv grammar for `/gtw pr view`.
//
// The command takes no flag and one optional positional: the pull
// request number. The shared lexer rejects a flag-shaped token and
// a second positional, so a typo never reads a different pull
// request.
var prViewCmdSpec = command.CmdSpec{
	Name:    "/gtw pr view",
	Usage:   "/gtw pr view [number]",
	MinArgs: 0,
	MaxArgs: 1,
}

// parsePRViewArgs reads the optional pull request number.
//
// The number is optional: without it the command reads the open
// pull request of the branch the chat works on. A number that is
// not a positive integer is a usage error — the command never
// guesses which pull request the user meant.
func parsePRViewArgs(argv []string) (int, error) {
	parsed, err := command.ParseCmdArgs(argv, prViewCmdSpec)
	if err != nil {
		return 0, err
	}
	raw := strings.TrimSpace(parsed.Arg(0))
	if raw == "" {
		return 0, nil
	}
	number, err := strconv.Atoi(raw)
	if err != nil || number <= 0 {
		return 0, fmt.Errorf("%q is not a pull request number — expected a positive integer. Usage: %s",
			raw, prViewCmdSpec.Usage)
	}
	return number, nil
}

// sleepFor waits for d. The wait goes through deps.Sleep so a test
// does not spend real seconds; nil means the real clock.
func sleepFor(ctx context.Context, deps HandlerDeps, d time.Duration) error {
	if deps.Sleep != nil {
		return deps.Sleep(ctx, d)
	}
	return sleepContext(ctx, d)
}

// sleepContext waits for d unless the context ends first.
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
