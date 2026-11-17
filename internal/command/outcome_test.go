package command

// The outcome constructors every command shares.
//
// An outcome says two things about a failure: the status says the
// work did not get done, the error description says whether something
// broke. The second is what separates an exception from a normal
// branch — a request that was valid and had nothing to do.

import (
	"testing"

	"github.com/cnlangzi/nightme/internal/messages"
)

// TestFailedOutcome_CarriesTheErrorDescription — an exception says
// what broke.
func TestFailedOutcome_CarriesTheErrorDescription(t *testing.T) {
	got := FailedOutcome("provider_error", "the git host could not be reached")
	if got.Status != messages.OutcomeFailed {
		t.Errorf("status = %s, want %s", got.Status, messages.OutcomeFailed)
	}
	if got.Data["error"] == "" {
		t.Errorf("the error description is empty; an exception must say what broke")
	}
	if got.Data["error"] != "the git host could not be reached" {
		t.Errorf("the error description = %q, want the summary", got.Data["error"])
	}
}

// TestFailedOutcome_EmptySummaryFallsBackToTheReason — a failure
// without a summary still says what broke: the reason code.
func TestFailedOutcome_EmptySummaryFallsBackToTheReason(t *testing.T) {
	got := FailedOutcome("bad_state", "")
	if got.Data["error"] != "bad_state" {
		t.Errorf("the error description = %q, want the reason code", got.Data["error"])
	}
}

// TestFailedOutcome_CallerDescriptionWins — a caller that knows more
// than the summary says so, and the summary keeps its own meaning.
func TestFailedOutcome_CallerDescriptionWins(t *testing.T) {
	got := FailedOutcome("rejected", "the push was rejected",
		"the remote refused the non-fast-forward")
	if got.Data["error"] != "the remote refused the non-fast-forward" {
		t.Errorf("the error description = %q, want the caller's own", got.Data["error"])
	}
	if got.Summary != "the push was rejected" {
		t.Errorf("the summary = %q, want it unchanged", got.Summary)
	}
}

// TestFailedOutcomeData_KeepsTheExtraFacts — the facts a command
// reports beside the failure reach the program together with the
// description.
func TestFailedOutcomeData_KeepsTheExtraFacts(t *testing.T) {
	got := FailedOutcomeData("agent_error", "the agent did not commit the work",
		map[string]string{"branch": "feat/x", "worktree": "/tmp/wt"})
	if got.Data["branch"] != "feat/x" {
		t.Errorf("the branch = %q, want it kept", got.Data["branch"])
	}
	if got.Data["worktree"] != "/tmp/wt" {
		t.Errorf("the worktree = %q, want it kept", got.Data["worktree"])
	}
	if got.Data["error"] != "the agent did not commit the work" {
		t.Errorf("the error description = %q, want the summary", got.Data["error"])
	}
}

// TestBranchOutcome_IsANormalResult — a normal branch reports the
// reason code and no error description: nothing broke, there was
// nothing to do.
func TestBranchOutcome_IsANormalResult(t *testing.T) {
	got := BranchOutcome("nothing_to_commit", "the worktree has nothing to commit",
		map[string]string{"branch": "feat/x"})
	if got.Status != messages.OutcomeFailed {
		t.Errorf("status = %s, want %s — the work asked for did not happen",
			got.Status, messages.OutcomeFailed)
	}
	if got.Data["reason"] != "nothing_to_commit" {
		t.Errorf("the reason = %q, want it set", got.Data["reason"])
	}
	if _, ok := got.Data["error"]; ok {
		t.Errorf("a normal result carries no error description; the data is %v", got.Data)
	}
	if got.Data["branch"] != "feat/x" {
		t.Errorf("the branch = %q, want it kept", got.Data["branch"])
	}
}

// TestBranchOutcome_DropsAnErrorFromTheFacts — the constructor owns
// the error key: a normal branch cannot turn into an exception through
// the facts it carries.
func TestBranchOutcome_DropsAnErrorFromTheFacts(t *testing.T) {
	got := BranchOutcome("pr_exists", "the branch already has an open pull request",
		map[string]string{"error": "the pull request is already open"})
	if _, ok := got.Data["error"]; ok {
		t.Errorf("a normal result carries no error description; the data is %v", got.Data)
	}
}

// TestMerge_AnExceptionSurvivesACleanTurn — the clean turn reports no
// error description, so the command's reaches the program.
func TestMerge_AnExceptionSurvivesACleanTurn(t *testing.T) {
	got := messages.Merge(
		FailedOutcome("provider_error", "the git host could not be reached"),
		messages.Outcome{Status: messages.OutcomeOK, Data: map[string]string{"end_reason": "clean"}},
	)
	if got.Data["error"] != "the git host could not be reached" {
		t.Errorf("the error description = %q, want the command's", got.Data["error"])
	}
}

// TestMerge_TheTurnsExceptionSurvivesANormalBranch — a normal branch
// reports no error description, so the turn's reaches the program.
func TestMerge_TheTurnsExceptionSurvivesANormalBranch(t *testing.T) {
	got := messages.Merge(
		BranchOutcome("nothing_to_commit", "the worktree has nothing to commit", nil),
		FailedOutcome("agent_error", "the agent did not commit the work"),
	)
	if got.Data["error"] != "the agent did not commit the work" {
		t.Errorf("the error description = %q, want the turn's", got.Data["error"])
	}
}
