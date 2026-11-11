package gtw

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
)

// The automatic mode of `/gtw fix`: the user names no issue, so
// nightme takes the next piece of work from the backlog by itself.
//
// The backlog lives on the tracker, not in nightme: an issue that
// nightme may work on carries the `nightme/assigned` label, which
// a human or an external automation attaches. The queue is the open
// issues carrying that label, minus the ones carrying
// `nightme/wip` (a fix holds them already).
//
// The pick is the oldest eligible issue: no unlanded blocker, no
// local branch of its own, no open pull request.

// autoSelector is the positional token that asks /gtw fix to pick
// the next assigned issue itself: `/gtw fix *`.
const autoSelector = "*"

// autoWakeAfter is how long the personal agent should wait before
// it comes back for another pick. It rides the outcome's Data as
// `wake_after` — the contract the agent reads to come back later
// instead of failing the run.
const autoWakeAfter = "5m"

// issueSelection records how /gtw fix chose the issue it works
// on: the user named it, or the queue picked it — and, for a
// picked issue, whether the platform exposed the dependency graph
// the queue is ordered by.
//
// The zero value is "the user named the issue".
type issueSelection struct {
	// Auto is true when the issue came from the queue.
	Auto bool
	// RelationsUnavailable is true when the platform does not
	// expose issue relations, so the queue was ordered by age
	// alone and the dependency order was not applied.
	RelationsUnavailable bool
}

// autoSkipReason names why a candidate was passed over.
type autoSkipReason string

const (
	autoSkipBlocked      autoSkipReason = "blocked"
	autoSkipBranchExists autoSkipReason = "branch_exists"
	autoSkipPROpen       autoSkipReason = "pr_open"
)

// autoCounters counts what the selection looked at.
type autoCounters struct {
	// Candidates is how many issues the queue held.
	Candidates int
	// Blocked is how many were skipped for an unlanded blocker.
	Blocked int
	// BranchExists is how many were skipped because their derived
	// branch is already in the repository.
	BranchExists int
	// PROpen is how many were skipped because their derived branch
	// already has an open pull request.
	PROpen int
}

// autoSkip records one candidate that was passed over, and why.
type autoSkip struct {
	Issue  *Issue
	Reason autoSkipReason
	// Detail names what the reason is about: the blocker numbers
	// for a blocked candidate, the branch for the other two.
	Detail string
}

// autoSelection is the result of the automatic pick.
type autoSelection struct {
	// Issue is the picked issue — nil when nothing was eligible.
	Issue *Issue
	// Counters describe the queue that was considered.
	Counters autoCounters
	// Skipped lists the candidates that were passed over.
	Skipped []autoSkip
	// RelationsUnavailable is true when the platform does not
	// expose the dependency graph. The pick then continued by age
	// alone and the reply says the dependency order was not
	// applied.
	RelationsUnavailable bool
}

// selectNextAssignedIssue picks the next issue `/gtw fix *` should
// work on.
//
// The candidates are the queue's issues, oldest first, and the
// first eligible one wins: no unlanded blocker, no branch of its
// own in the repository, no open pull request.
//
// The blocker check is lazy — the relation graph is consulted once
// per candidate until one is eligible. A platform without
// relations reports ErrRelationsUnsupported once; from then on the
// pick continues by age alone.
//
// A relation call that fails for another reason (network, auth)
// is returned to the caller: it is not downgraded to "no
// blockers".
func selectNextAssignedIssue(
	ctx context.Context,
	provider GitProvider,
	owner, repo, repoRoot string,
	deps HandlerDeps,
) (*autoSelection, error) {
	issues, err := provider.ListIssues(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	candidates := assignedCandidates(issues)
	sel := &autoSelection{Counters: autoCounters{Candidates: len(candidates)}}

	for _, c := range candidates {
		if !sel.RelationsUnavailable {
			blockers, err := provider.ListBlockedBy(ctx, owner, repo, c.ID)
			if err != nil {
				if !errors.Is(err, ErrRelationsUnsupported) {
					return nil, err
				}
				sel.RelationsUnavailable = true
			} else if unlanded := unlandedBlockers(blockers); len(unlanded) > 0 {
				sel.Counters.Blocked++
				sel.Skipped = append(sel.Skipped, autoSkip{
					Issue:  c,
					Reason: autoSkipBlocked,
					Detail: blockerNumbers(unlanded),
				})
				continue
			}
		}

		branch := DeriveBranchFromTitle(c.Title, c.ID)
		exists, err := BranchExists(ctx, repoRoot, branch, deps.Git)
		if err != nil {
			return nil, err
		}
		if exists {
			sel.Counters.BranchExists++
			sel.Skipped = append(sel.Skipped, autoSkip{
				Issue:  c,
				Reason: autoSkipBranchExists,
				Detail: branch,
			})
			continue
		}

		pr, err := provider.FindOpenPRForBranch(ctx, owner, repo, branch)
		if err != nil {
			return nil, err
		}
		if pr != nil {
			sel.Counters.PROpen++
			sel.Skipped = append(sel.Skipped, autoSkip{
				Issue:  c,
				Reason: autoSkipPROpen,
				Detail: branch,
			})
			continue
		}

		sel.Issue = c
		return sel, nil
	}
	return sel, nil
}

// assignedCandidates filters the issue list down to the queue —
// open issues carrying `nightme/assigned`, minus those carrying
// `nightme/wip` — and orders them oldest first. Issues created at
// the same instant are ordered by ascending issue number, so the
// order is total and stable.
func assignedCandidates(issues []*Issue) []*Issue {
	out := make([]*Issue, 0, len(issues))
	for _, iss := range issues {
		if iss == nil {
			continue
		}
		if !issueHasLabel(iss, LabelAssigned) || issueHasLabel(iss, LabelWIP) {
			continue
		}
		out = append(out, iss)
	}
	slices.SortStableFunc(out, func(a, b *Issue) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return out
}

// unlandedBlockers returns the blockers that have not landed yet.
// A blocker counts as landed only when it is closed — its pull
// request has to be merged before the dependent is built on top
// of it.
func unlandedBlockers(blockers []*Issue) []*Issue {
	var out []*Issue
	for _, b := range blockers {
		if b == nil {
			continue
		}
		if !strings.EqualFold(b.State, "closed") {
			out = append(out, b)
		}
	}
	return out
}

// blockerNumbers names the blockers as "#12, #13".
func blockerNumbers(blockers []*Issue) string {
	names := make([]string, 0, len(blockers))
	for _, b := range blockers {
		names = append(names, fmt.Sprintf("#%d", b.ID))
	}
	return strings.Join(names, ", ")
}

// renderAutoNoCandidate builds the reply for a queue with nothing
// eligible. It says what was considered and why every candidate
// was passed over, so the user can act on it.
func renderAutoNoCandidate(sel *autoSelection) string {
	var b strings.Builder
	b.WriteString("✨ No eligible assigned issue\n")

	rows := make([][2]string, 0, len(sel.Skipped)+2)
	if sel.Counters.Candidates == 0 {
		rows = append(rows, [2]string{"queue", fmt.Sprintf(
			"empty — none of the %d oldest open issues carries the `%s` label",
			listIssuesPerPage, LabelAssigned)})
	} else {
		rows = append(rows, [2]string{"queue", fmt.Sprintf(
			"%d candidate(s) — %d blocked, %d with an existing branch, %d with an open pull request",
			sel.Counters.Candidates, sel.Counters.Blocked, sel.Counters.BranchExists, sel.Counters.PROpen)})
	}
	for _, s := range sel.Skipped {
		switch s.Reason {
		case autoSkipBlocked:
			rows = append(rows, [2]string{"blocked", fmt.Sprintf("#%d is blocked by %s", s.Issue.ID, s.Detail)})
		case autoSkipBranchExists:
			rows = append(rows, [2]string{"branch", fmt.Sprintf("#%d already has the branch `%s` in this repository", s.Issue.ID, s.Detail)})
		case autoSkipPROpen:
			rows = append(rows, [2]string{"pr", fmt.Sprintf("#%d already has an open pull request (`%s`)", s.Issue.ID, s.Detail)})
		}
	}
	if sel.RelationsUnavailable {
		rows = append(rows, [2]string{"deps", "not ordered — this platform does not expose issue dependencies"})
	}

	width := 0
	for _, r := range rows {
		if n := len(r[0]) + 1; n > width {
			width = n
		}
	}
	for _, r := range rows {
		fmt.Fprintf(&b, "→ %-*s %s\n", width, r[0]+":", r[1])
	}
	fmt.Fprintf(&b, "↳ come back in %s for another pick.\n", autoWakeAfter)
	return b.String()
}

// autoNoCandidateOutcome is the outcome of a queue with nothing
// eligible: a noop (the request was valid, the world had nothing
// to offer) carrying the counters and the wake-up hint.
func autoNoCandidateOutcome(sel *autoSelection) messages.Outcome {
	return command.NoopOutcome("no assigned issue was eligible", map[string]string{
		"reason":        "no_candidate",
		"wake_after":    autoWakeAfter,
		"candidates":    strconv.Itoa(sel.Counters.Candidates),
		"blocked":       strconv.Itoa(sel.Counters.Blocked),
		"branch_exists": strconv.Itoa(sel.Counters.BranchExists),
		"pr_open":       strconv.Itoa(sel.Counters.PROpen),
	})
}
