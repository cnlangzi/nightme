package gtw

// /gtw assign — the queue marker, attached or taken off by hand.
//
// The queue lives on the tracker: an issue NightMe may work on
// carries `nightme/assigned`. The automatic flows never put that
// label on an issue and never take it off — /gtw fix only claims an
// issue (`nightme/wip`), /gtw close finalises the state labels, and
// /gtw pr --ready moves the claim to `nightme/ready`. Filling the
// queue was therefore the user's move, and a move that had to happen
// on the tracker's web UI.
//
// This command is that move: it attaches the queue marker to a
// named issue, or takes it off again. It touches nothing else — no
// worktree, no branch, no agent, no state file — so it takes no run
// lock and fires no hooks.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
)

// assignCmdSpec is the argv grammar for /gtw assign.
//
// The command names exactly one issue and takes one boolean flag:
// `--remove` (or `-r`) takes the queue marker off instead of putting
// it on. The issue id goes through parseIssueID, so the `#42` form
// the trackers use is accepted.
var assignCmdSpec = command.CmdSpec{
	Name:  "/gtw assign",
	Usage: "/gtw assign <issue-id> [--remove|-r]",
	Flags: map[string]command.FlagSpec{
		"--remove": {Name: "remove"},
		"-r":       {Name: "remove"},
	},
	MinArgs: 1,
	MaxArgs: 1,
}

// assignArgs is the parsed argv tail of `/gtw assign <...>`.
type assignArgs struct {
	// IssueID is the issue the user named.
	IssueID int
	// Remove is true when the user asked for the queue marker to be
	// taken off rather than put on.
	Remove bool
}

// parseAssignArgs lexes the argv tail of `/gtw assign <...>` via the
// shared command.ParseCmdArgs and resolves the issue id.
//
// The shared lexer owns the arity (exactly one positional) and
// rejects unknown flags; parseIssueID owns the id's shape — digits
// only, an optional leading `#`, and never 0.
func parseAssignArgs(argv []string) (assignArgs, error) {
	parsed, err := command.ParseCmdArgs(argv, assignCmdSpec)
	if err != nil {
		return assignArgs{}, err
	}
	issueID, err := parseIssueID(parsed.Arg(0))
	if err != nil {
		return assignArgs{}, fmt.Errorf("%s: %v%s",
			assignCmdSpec.Name, err, assignCmdSpec.UsageTail())
	}
	return assignArgs{IssueID: issueID, Remove: parsed.Bool("remove")}, nil
}

// RunAssign attaches the queue marker `nightme/assigned` to an
// issue, or takes it off.
//
// Steps:
//
//  1. Resolve the repository of the chat's current working
//     directory and the provider behind its origin remote.
//  2. Verify the repository's label catalog. On the add path a
//     failed check is fatal — the marker cannot be attached to a
//     catalog that does not hold it. On the remove path it only
//     warns: a missing catalog entry does not stop a removal.
//  3. Fetch the issue. It is fetched so the reply can name it and
//     so the command can tell a real change from a no-op.
//  4. Attach or remove the label. Both provider calls are
//     idempotent, so they are made unconditionally.
//  5. Reply with one card and report the outcome.
//
// The command has no issue-state guard: the queue marker is
// bookkeeping, and which issues the automatic pick may offer is the
// pick's own filter (see selectNextAssignedIssue). A refusal would
// add a second, drift-prone definition of eligibility.
//
// It takes no per-chat run lock. The platform's label API is a
// read-modify-write of the issue's whole label set, so two label
// writers on one issue can lose one another's change — that window
// already exists between two chats, and the run lock is per chat
// anyway. What the lock would buy here is a narrower one: /gtw fix
// and /gtw close cannot run at the same time as this command in the
// same chat.
func RunAssign(
	ctx context.Context,
	cs *chatsession.ChatSession,
	deps HandlerDeps,
	chatID, messageID string,
	issueID int,
	remove bool,
) (*Result, error) {
	if cs == nil || cs.SelectedCwd() == "" {
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			"❌ "+command.NoActiveCwdReply,
			command.FailedOutcome("no_cwd", "the command needs an active workspace")), nil
	}

	target, res := resolveRemoteTarget(ctx, cs, deps, chatID, messageID)
	if res != nil {
		return res, nil
	}
	provider := target.Provider
	owner, repo := target.Owner, target.Repo

	// The catalog check runs before the mutation. It also repairs the
	// catalog — it creates the labels the repository is missing — so
	// it runs on the remove path too; a repository whose catalog
	// cannot be read is still a repository whose issue can lose a
	// label, and the removal goes ahead with a warning.
	var warning string
	if _, err := ensureRepoLabels(ctx, deps, provider, owner, repo); err != nil {
		if !remove {
			return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
				fmt.Sprintf("❌ Could not ensure gtw labels on %s/%s: %v\n"+
					"fix the cause and re-run /gtw assign", owner, repo, err),
				command.FailedOutcome("label_error", "the gtw labels could not be created")), nil
		}
		warning = labelCheckWarning(owner, repo, err)
	}

	issue, err := provider.GetIssue(ctx, owner, repo, issueID)
	if err != nil {
		if errors.Is(err, ErrIssueNotFound) {
			return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
				fmt.Sprintf("❌ Issue #%d not found in %s/%s.", issueID, owner, repo),
				command.FailedOutcome("issue_not_found", "the issue does not exist")), nil
		}
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ Failed to fetch issue: %v", err),
			command.FailedOutcome("provider_error", "the issue could not be fetched")), nil
	}

	// What the issue carried before the call. The reply says whether
	// anything changed, so an idempotent call does not claim a change
	// it did not make.
	had := issueHasLabel(issue, LabelAssigned)
	if remove {
		if err := provider.RemoveIssueLabel(ctx, owner, repo, issueID, LabelAssigned); err != nil {
			return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
				fmt.Sprintf("❌ Failed to remove %s from issue #%d: %v", LabelAssigned, issueID, err),
				command.FailedOutcome("label_error", "the label could not be removed")), nil
		}
	} else {
		if err := provider.AddIssueLabel(ctx, owner, repo, issueID, LabelAssigned); err != nil {
			return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
				fmt.Sprintf("❌ Failed to add %s to issue #%d: %v", LabelAssigned, issueID, err),
				command.FailedOutcome("label_error", "the label could not be added")), nil
		}
	}

	result := assignResultFor(issueID, remove, had)
	out := messages.Outcome{
		Status:  messages.OutcomeOK,
		Summary: result.summary,
		Data: map[string]string{
			"issue":  strconv.Itoa(issueID),
			"url":    issue.URL,
			"action": result.action,
		},
	}
	body := renderAssignCard(issue, owner+"/"+repo, result)
	return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
		prependWarning(body, warning), out), nil
}

// assignResult is what the command did to the queue marker: the
// card's header line, the outcome summary, and the operation the
// outcome reports.
//
// One value carries all three so the card and the outcome cannot
// disagree about whether anything changed.
type assignResult struct {
	// header is the card's title line, without the ✅.
	header string
	// summary is the outcome's one-line restatement.
	summary string
	// action is the operation the outcome reports: "added" or
	// "removed" — the operation the user asked for, which on a
	// no-op is the operation the label was already in.
	action string
}

// assignResultFor names what the command did, given whether the
// marker was on the issue before the call.
//
// The marker is idempotent to attach and to detach, so the provider
// call happens either way; what differs is what the user is told.
// The card says "nothing changed" when the marker was already where
// the user wanted it, so an idempotent call never claims a change it
// did not make.
func assignResultFor(issueID int, remove, had bool) assignResult {
	switch {
	case remove && had:
		return assignResult{
			header:  fmt.Sprintf("`%s` removed from #%d", LabelAssigned, issueID),
			summary: fmt.Sprintf("the queue marker was removed from issue #%d", issueID),
			action:  "removed",
		}
	case remove:
		return assignResult{
			header:  fmt.Sprintf("`%s` was not on #%d — nothing changed", LabelAssigned, issueID),
			summary: fmt.Sprintf("issue #%d did not carry the queue marker", issueID),
			action:  "removed",
		}
	case had:
		return assignResult{
			header:  fmt.Sprintf("`%s` was already on #%d — nothing changed", LabelAssigned, issueID),
			summary: fmt.Sprintf("issue #%d already carried the queue marker", issueID),
			action:  "added",
		}
	default:
		return assignResult{
			header:  fmt.Sprintf("`%s` added to #%d", LabelAssigned, issueID),
			summary: fmt.Sprintf("the queue marker was added to issue #%d", issueID),
			action:  "added",
		}
	}
}

// renderAssignCard builds the /gtw assign card (Format 1: a header
// line plus `→ field: value` rows).
func renderAssignCard(issue *Issue, repo string, result assignResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "✅ %s\n", result.header)
	fmt.Fprintf(&b, "→ issue: %s#%d", repo, issue.ID)
	if issue.Title != "" {
		fmt.Fprintf(&b, " — %s", issue.Title)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "→ label: `%s`\n", LabelAssigned)
	if issue.URL != "" {
		fmt.Fprintf(&b, "→ url:   %s\n", issue.URL)
	}
	b.WriteString(assignHint(issue, result))
	return b.String()
}

// assignHint is the card's closing hint: where the issue stands in
// the `/gtw fix *` queue after the command.
//
// The hint never promises a pick that will not come. The queue holds
// open issues that carry the queue marker alone: a closed issue is
// not in it, and an issue another agent holds is not offered either.
func assignHint(issue *Issue, result assignResult) string {
	held := heldLabel(issue)
	switch {
	case result.action == "removed":
		return "↳ the queue marker is off the issue — `/gtw fix *` will not offer it\n"
	case issue.State == "closed":
		return "↳ the `/gtw fix *` queue holds open issues — this one is closed\n"
	case held != "":
		return fmt.Sprintf("↳ `%s` on this issue keeps it out of the `/gtw fix *` queue\n", held)
	default:
		return "↳ `/gtw fix *` picks the oldest eligible issue — this one is queued\n"
	}
}
