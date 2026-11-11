package gtw

// Unit tests for the automatic pick of `/gtw fix *`: the queue
// filter, the age ordering, the blocker / branch / pull-request
// gates and the counters the no-candidate report carries.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// autoGitRunner answers only the `show-ref --verify --quiet
// refs/heads/<branch>` probe BranchExists makes. Every other argv
// is a no-op success — the selection asks git nothing else.
type autoGitRunner struct {
	mu sync.Mutex
	// existingBranches are the branches the repository has.
	existingBranches map[string]bool
}

func (g *autoGitRunner) Run(_ context.Context, _ string, args ...string) (string, string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(args) >= 4 && args[0] == "show-ref" {
		if g.existingBranches[strings.TrimPrefix(args[3], "refs/heads/")] {
			return "", "", nil
		}
		// A miss: git exits non-zero with empty stderr, which
		// BranchExists reads as "no such branch".
		return "", "", errors.New("exit status 1")
	}
	return "", "", nil
}

// autoAt builds a timestamp `days` before the reference point.
func autoAt(days int) time.Time {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -days)
}

// autoIssue builds an issue carrying the given labels.
func autoIssue(id int, title string, days int, labels ...string) *Issue {
	return &Issue{
		ID:        id,
		Title:     title,
		State:     "open",
		Labels:    labels,
		CreatedAt: autoAt(days),
	}
}

// autoSelect runs the selection against a fake provider.
func autoSelect(t *testing.T, prov *fakeGitProvider, branches ...string) (*autoSelection, error) {
	t.Helper()
	existing := map[string]bool{}
	for _, b := range branches {
		existing[b] = true
	}
	deps := HandlerDeps{Git: &autoGitRunner{existingBranches: existing}}
	return selectNextAssignedIssue(context.Background(), prov, "cnlangzi", "nightme", "/repo", deps)
}

// TestSelectNextAssignedIssue_PicksOldestEligible pins the pick:
// the oldest queue issue that passes every gate wins, and the
// issue that carries no queue label is never considered.
func TestSelectNextAssignedIssue_PicksOldestEligible(t *testing.T) {
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	prov.SetListIssues([]*Issue{
		autoIssue(7, "feat: newer", 1, LabelAssigned),
		autoIssue(3, "feat: older", 5, LabelAssigned),
		autoIssue(9, "feat: unassigned", 0),
	})

	sel, err := autoSelect(t, prov)
	if err != nil {
		t.Fatalf("selectNextAssignedIssue: %v", err)
	}
	if sel.Issue == nil {
		t.Fatalf("no issue picked; counters=%+v", sel.Counters)
	}
	if sel.Issue.ID != 3 {
		t.Errorf("picked issue #%d, want #3 (the oldest assigned one)", sel.Issue.ID)
	}
	if sel.Counters.Candidates != 2 {
		t.Errorf("candidates = %d, want 2 (the unassigned issue is not in the queue)", sel.Counters.Candidates)
	}
	if sel.Branch != DeriveBranchFromTitle("feat: older", 3) {
		t.Errorf("branch = %q, want the derived branch", sel.Branch)
	}
}

// TestSelectNextAssignedIssue_OrdersByAgeThenNumber pins the
// tie-break: two issues created at the same instant are ordered
// by ascending issue number.
func TestSelectNextAssignedIssue_OrdersByAgeThenNumber(t *testing.T) {
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	same := autoAt(2)
	prov.SetListIssues([]*Issue{
		{ID: 11, Title: "feat: b", State: "open", Labels: []string{LabelAssigned}, CreatedAt: same},
		{ID: 4, Title: "feat: a", State: "open", Labels: []string{LabelAssigned}, CreatedAt: same},
	})

	sel, err := autoSelect(t, prov)
	if err != nil {
		t.Fatalf("selectNextAssignedIssue: %v", err)
	}
	if sel.Issue == nil || sel.Issue.ID != 4 {
		t.Fatalf("picked %+v, want issue #4 (the lower number at the same age)", sel.Issue)
	}
}

// TestSelectNextAssignedIssue_SkipsClaimedIssues pins the queue
// filter: an issue that carries `nightme/wip` as well is held by
// a fix already and is never picked.
func TestSelectNextAssignedIssue_SkipsClaimedIssues(t *testing.T) {
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	prov.SetListIssues([]*Issue{
		autoIssue(1, "feat: claimed", 1, LabelAssigned, LabelWIP),
		autoIssue(2, "feat: free", 2, LabelAssigned),
	})

	sel, err := autoSelect(t, prov)
	if err != nil {
		t.Fatalf("selectNextAssignedIssue: %v", err)
	}
	if sel.Issue == nil || sel.Issue.ID != 2 {
		t.Fatalf("picked %+v, want issue #2 (the claimed one is out of the queue)", sel.Issue)
	}
	if sel.Counters.Candidates != 1 {
		t.Errorf("candidates = %d, want 1 (the claimed issue is not a candidate)", sel.Counters.Candidates)
	}
}

// TestSelectNextAssignedIssue_SkipsUnlandedBlocker pins the
// dependency gate: a candidate whose blocker is still open is
// passed over, and the blocker is named in the report.
func TestSelectNextAssignedIssue_SkipsUnlandedBlocker(t *testing.T) {
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	prov.SetListIssues([]*Issue{
		autoIssue(1, "feat: dependent", 5, LabelAssigned),
		autoIssue(2, "feat: independent", 1, LabelAssigned),
	})
	prov.SetBlockedBy(1, []*Issue{{ID: 30, State: "open"}})

	sel, err := autoSelect(t, prov)
	if err != nil {
		t.Fatalf("selectNextAssignedIssue: %v", err)
	}
	if sel.Issue == nil || sel.Issue.ID != 2 {
		t.Fatalf("picked %+v, want issue #2 (the blocked one is passed over)", sel.Issue)
	}
	if sel.Counters.Blocked != 1 {
		t.Errorf("blocked counter = %d, want 1", sel.Counters.Blocked)
	}
	if len(sel.Skipped) != 1 || sel.Skipped[0].Reason != autoSkipBlocked {
		t.Fatalf("skipped = %+v, want one blocked entry", sel.Skipped)
	}
	if !strings.Contains(sel.Skipped[0].Detail, "#30") {
		t.Errorf("blocked detail = %q, want it to name the blocker #30", sel.Skipped[0].Detail)
	}
}

// TestSelectNextAssignedIssue_ClosedBlockerIsLanded pins that a
// closed blocker does not hold the candidate back: the
// prerequisite's change is in the base branch.
func TestSelectNextAssignedIssue_ClosedBlockerIsLanded(t *testing.T) {
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	prov.SetListIssues([]*Issue{autoIssue(1, "feat: dependent", 5, LabelAssigned)})
	prov.SetBlockedBy(1, []*Issue{{ID: 30, State: "closed"}})

	sel, err := autoSelect(t, prov)
	if err != nil {
		t.Fatalf("selectNextAssignedIssue: %v", err)
	}
	if sel.Issue == nil {
		t.Fatalf("no issue picked; the closed blocker must not block it")
	}
}

// TestSelectNextAssignedIssue_SkipsExistingBranch pins the local
// branch gate: a candidate whose derived branch is already in the
// repository was worked on before.
func TestSelectNextAssignedIssue_SkipsExistingBranch(t *testing.T) {
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	prov.SetListIssues([]*Issue{
		autoIssue(1, "feat: first", 5, LabelAssigned),
		autoIssue(2, "feat: second", 1, LabelAssigned),
	})
	branch := DeriveBranchFromTitle("feat: first", 1)

	sel, err := autoSelect(t, prov, branch)
	if err != nil {
		t.Fatalf("selectNextAssignedIssue: %v", err)
	}
	if sel.Issue == nil || sel.Issue.ID != 2 {
		t.Fatalf("picked %+v, want issue #2 (the one with an existing branch is passed over)", sel.Issue)
	}
	if sel.Counters.BranchExists != 1 {
		t.Errorf("branch_exists counter = %d, want 1", sel.Counters.BranchExists)
	}
}

// TestSelectNextAssignedIssue_SkipsOpenPR pins the pull-request
// gate: a candidate whose derived branch already has an open
// pull request is passed over.
func TestSelectNextAssignedIssue_SkipsOpenPR(t *testing.T) {
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	prov.SetListIssues([]*Issue{
		autoIssue(1, "feat: first", 5, LabelAssigned),
		autoIssue(2, "feat: second", 1, LabelAssigned),
	})
	prov.SetFindOpenPRFunc(func(head string) (*PR, error) {
		if head == DeriveBranchFromTitle("feat: first", 1) {
			return &PR{Number: 99}, nil
		}
		return nil, nil
	})

	sel, err := autoSelect(t, prov)
	if err != nil {
		t.Fatalf("selectNextAssignedIssue: %v", err)
	}
	if sel.Issue == nil || sel.Issue.ID != 2 {
		t.Fatalf("picked %+v, want issue #2 (the one with an open PR is passed over)", sel.Issue)
	}
	if sel.Counters.PROpen != 1 {
		t.Errorf("pr_open counter = %d, want 1", sel.Counters.PROpen)
	}
}

// TestSelectNextAssignedIssue_BlockerCheckIsLazy pins that the
// relation graph is consulted once per candidate until one is
// eligible: a queue whose first candidate is eligible costs
// exactly one ListBlockedBy call.
func TestSelectNextAssignedIssue_BlockerCheckIsLazy(t *testing.T) {
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	prov.SetListIssues([]*Issue{
		autoIssue(1, "feat: first", 5, LabelAssigned),
		autoIssue(2, "feat: second", 1, LabelAssigned),
	})

	if _, err := autoSelect(t, prov); err != nil {
		t.Fatalf("selectNextAssignedIssue: %v", err)
	}
	if calls := prov.CallsByMethod("ListBlockedBy"); len(calls) != 1 {
		t.Errorf("ListBlockedBy calls = %d, want 1 (the first candidate is eligible)", len(calls))
	}
}

// TestSelectNextAssignedIssue_RelationsUnsupportedContinuesByAge
// pins the degraded mode: a platform without a relation graph is
// reported, the pick continues by age alone, and the reply says
// the dependency order was not applied.
func TestSelectNextAssignedIssue_RelationsUnsupportedContinuesByAge(t *testing.T) {
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	prov.SetListIssues([]*Issue{
		autoIssue(1, "feat: first", 5, LabelAssigned),
		autoIssue(2, "feat: second", 1, LabelAssigned),
	})
	prov.SetBlockedByErr(1, fmt.Errorf("%w: no dependencies api", ErrRelationsUnsupported))

	sel, err := autoSelect(t, prov)
	if err != nil {
		t.Fatalf("selectNextAssignedIssue: %v", err)
	}
	if !sel.RelationsUnavailable {
		t.Errorf("RelationsUnavailable = false, want true")
	}
	if sel.Issue == nil || sel.Issue.ID != 1 {
		t.Fatalf("picked %+v, want issue #1 (the oldest one)", sel.Issue)
	}
	// The relation check ends for the rest of the selection.
	if calls := prov.CallsByMethod("ListBlockedBy"); len(calls) != 1 {
		t.Errorf("ListBlockedBy calls = %d, want 1 (the checks stop after the first failure)", len(calls))
	}
	if !strings.Contains(renderAutoNoCandidate(sel), "not ordered") {
		t.Errorf("the report must say the dependency order was not applied")
	}
}

// TestSelectNextAssignedIssue_RelationErrorIsNotDowngraded pins
// that a relation call failing for another reason (network, auth)
// fails the command instead of being read as "no blockers".
func TestSelectNextAssignedIssue_RelationErrorIsNotDowngraded(t *testing.T) {
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	prov.SetListIssues([]*Issue{autoIssue(1, "feat: first", 5, LabelAssigned)})
	prov.SetBlockedByErr(1, errors.New("403 Forbidden: token expired"))

	if _, err := autoSelect(t, prov); err == nil {
		t.Fatalf("selectNextAssignedIssue returned no error; the relation failure must surface")
	}
}

// TestSelectNextAssignedIssue_NoCandidates pins the empty queue:
// nothing is picked and the counters report zero.
func TestSelectNextAssignedIssue_NoCandidates(t *testing.T) {
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	prov.SetListIssues([]*Issue{autoIssue(1, "feat: unassigned", 1)})

	sel, err := autoSelect(t, prov)
	if err != nil {
		t.Fatalf("selectNextAssignedIssue: %v", err)
	}
	if sel.Issue != nil {
		t.Fatalf("picked %+v, want nothing", sel.Issue)
	}
	if sel.Counters.Candidates != 0 {
		t.Errorf("candidates = %d, want 0", sel.Counters.Candidates)
	}
}

// TestAutoNoCandidateOutcome pins the outcome the personal agent
// reads: a noop carrying the wake-up hint and the counters.
func TestAutoNoCandidateOutcome(t *testing.T) {
	sel := &autoSelection{Counters: autoCounters{
		Candidates:   4,
		Blocked:      1,
		BranchExists: 2,
		PROpen:       1,
	}}
	o := autoNoCandidateOutcome(sel)
	if o.Status != "noop" {
		t.Errorf("status = %q, want noop", o.Status)
	}
	want := map[string]string{
		"reason":        "no_candidate",
		"wake_after":    "5m",
		"candidates":    "4",
		"blocked":       "1",
		"branch_exists": "2",
		"pr_open":       "1",
	}
	for k, v := range want {
		if o.Data[k] != v {
			t.Errorf("Data[%q] = %q, want %q", k, o.Data[k], v)
		}
	}
}

// TestRenderFixSuccessCard_AutoNotes pins the two notes the
// automatic mode adds to the success card: the issue was picked
// automatically, and — on a platform without issue relations —
// the dependency order was not applied.
func TestRenderFixSuccessCard_AutoNotes(t *testing.T) {
	issue := &Issue{ID: 42, Title: "x"}
	card := renderFixSuccessCard(issue, "br", "/wt", "o/r", "abc1234abcd", DispatchAssess,
		issueSelection{Auto: true, RelationsUnavailable: true})
	for _, want := range []string{
		"✅ Fix #42 ready",
		"→ issue:    o/r#42 [nightme/wip] · picked automatically",
		"not ordered",
	} {
		if !strings.Contains(card, want) {
			t.Errorf("auto success card missing %q; got:\n%s", want, card)
		}
	}
}

// TestRenderFixSuccessCard_NamedIssueHasNoNotes pins that a
// named issue keeps the plain card.
func TestRenderFixSuccessCard_NamedIssueHasNoNotes(t *testing.T) {
	issue := &Issue{ID: 42, Title: "x"}
	card := renderFixSuccessCard(issue, "br", "/wt", "o/r", "abc1234abcd", DispatchAssess,
		issueSelection{})
	if strings.Contains(card, "picked automatically") || strings.Contains(card, "not ordered") {
		t.Errorf("a named issue must not carry the automatic-mode notes; got:\n%s", card)
	}
}
