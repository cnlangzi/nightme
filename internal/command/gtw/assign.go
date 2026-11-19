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

	repoRoot, err := RepoRoot(ctx, cs.SelectedCwd(), deps.Git)
	if err != nil {
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			"❌ Not in a git repository. Run /cwd <inside a repo> first.",
			command.FailedOutcome("not_a_repo", "the workspace is not inside a git repository")), nil
	}
	remoteURL, err := RemoteOriginURL(ctx, repoRoot, deps.Git)
	if err != nil || remoteURL == "" {
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			"❌ No `origin` remote. Add one with `git remote add origin <url>`.",
			command.FailedOutcome("no_remote", "the repository has no origin remote")), nil
	}
	provider, err := detectProvider(ctx, deps, remoteURL, repoRoot)
	if err != nil {
		card, summary := providerErrorCard(remoteURL, err)
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID, card,
			command.FailedOutcome("provider_error", summary)), nil
	}
	if provider == nil {
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			"❌ Provider detection returned no result (deps.Detect override bug).",
			command.FailedOutcome("provider_error", "the git platform could not be detected")), nil
	}
	owner, repo, err := ParseRepoOwner(remoteURL)
	if err != nil {
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ Cannot parse owner/repo from remote URL %s.", redactForDisplay(remoteURL)),
			command.FailedOutcome("provider_error", "the remote URL does not name a repository")), nil
	}

	// The catalog check runs before the mutation. Adding the marker
	// needs it in the catalog; removing it does not — a repository
	// whose catalog cannot be read is still a repository whose issue
	// can lose a label.
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
				command.FailedOutcome("provider_error", "the label could not be removed")), nil
		}
	} else {
		if err := provider.AddIssueLabel(ctx, owner, repo, issueID, LabelAssigned); err != nil {
			return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
				fmt.Sprintf("❌ Failed to add %s to issue #%d: %v", LabelAssigned, issueID, err),
				command.FailedOutcome("provider_error", "the label could not be added")), nil
		}
	}

	action := "added"
	summary := fmt.Sprintf("the queue marker was added to issue #%d", issueID)
	if remove {
		action = "removed"
		summary = fmt.Sprintf("the queue marker was removed from issue #%d", issueID)
	}
	if had != remove {
		// Nothing changed: the marker was already where the user
		// wanted it.
		if remove {
			summary = fmt.Sprintf("issue #%d did not carry the queue marker", issueID)
		} else {
			summary = fmt.Sprintf("issue #%d already carried the queue marker", issueID)
		}
	}
	out := messages.Outcome{
		Status:  messages.OutcomeOK,
		Summary: summary,
		Data: map[string]string{
			"issue":  strconv.Itoa(issueID),
			"url":    issue.URL,
			"action": action,
		},
	}
	body := renderAssignCard(issue, owner+"/"+repo, remove, had)
	return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
		prependWarning(body, warning), out), nil
}

// renderAssignCard builds the /gtw assign card (Format 1: a header
// line plus `→ field: value` rows).
//
// `had` is what the issue carried before the call — the card says
// whether anything changed, so an idempotent call does not claim a
// change it did not make.
func renderAssignCard(issue *Issue, repo string, remove, had bool) string {
	var b strings.Builder
	switch {
	case remove && had:
		fmt.Fprintf(&b, "✅ `%s` removed from #%d\n", LabelAssigned, issue.ID)
	case remove:
		fmt.Fprintf(&b, "✅ `%s` was not on #%d — nothing changed\n", LabelAssigned, issue.ID)
	case had:
		fmt.Fprintf(&b, "✅ `%s` was already on #%d — nothing changed\n", LabelAssigned, issue.ID)
	default:
		fmt.Fprintf(&b, "✅ `%s` added to #%d\n", LabelAssigned, issue.ID)
	}
	fmt.Fprintf(&b, "→ issue: %s#%d", repo, issue.ID)
	if issue.Title != "" {
		fmt.Fprintf(&b, " — %s", issue.Title)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "→ label: `%s`\n", LabelAssigned)
	if issue.URL != "" {
		fmt.Fprintf(&b, "→ url:   %s\n", issue.URL)
	}
	b.WriteString(assignHint(issue, remove, had))
	return b.String()
}

// assignHint is the card's closing hint: what the change means for
// the queue. An issue held by another agent stays out of the queue
// whatever the marker says, so the hint says so rather than
// promising a pick that will not come.
func assignHint(issue *Issue, remove, had bool) string {
	switch {
	case remove:
		if had {
			return "↳ the issue is out of the `/gtw fix *` queue\n"
		}
		return "↳ nothing changed\n"
	default:
		if held := heldLabel(issue); held != "" {
			return fmt.Sprintf("↳ `%s` on this issue keeps it out of the `/gtw fix *` queue\n", held)
		}
		return "↳ `/gtw fix *` picks it up on the next run\n"
	}
}
