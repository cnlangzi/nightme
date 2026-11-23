package command

// The outcome constructors every command shares.
//
// An outcome says two things about a failure: the status says which
// kind of breakage it was, and the words say what broke. The status
// alone tells a consumer whether the flow can go on by itself.

import (
	"net/http"
	"testing"

	"github.com/cnlangzi/nightme/pkg/outcome"
)

// TestFailedOutcome_SaysWhatBroke — a failure carries its status and
// the one line of words that says what broke.
func TestFailedOutcome_SaysWhatBroke(t *testing.T) {
	got := FailedOutcome(http.StatusServiceUnavailable, "the git host could not be reached")
	if got.Success {
		t.Error("a failure is not a success")
	}
	if got.Status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", got.Status, http.StatusServiceUnavailable)
	}
	if got.StatusText != "the git host could not be reached" {
		t.Errorf("the words = %q, want the summary", got.StatusText)
	}
	if got.Exception() != true {
		t.Error("a failure is an exception")
	}
}

// TestFailedOutcome_CarriesNoCategoryCode — the situation is the
// status and the facts; there is no code beside them.
func TestFailedOutcome_CarriesNoCategoryCode(t *testing.T) {
	got := FailedOutcome(http.StatusBadRequest, "the command line is not valid")
	if len(got.Data) != 0 {
		t.Errorf("a failure carries no facts of its own; the data is %v", got.Data)
	}
}

// TestFailedOutcomeData_KeepsTheExtraFacts — the facts a command
// reports beside the failure reach the program together with the
// words.
func TestFailedOutcomeData_KeepsTheExtraFacts(t *testing.T) {
	got := FailedOutcomeData(http.StatusInternalServerError, "the agent did not commit the work",
		map[string]string{"branch": "feat/x", "worktree": "/tmp/wt"})
	if got.Data["branch"] != "feat/x" {
		t.Errorf("the branch = %q, want it kept", got.Data["branch"])
	}
	if got.Data["worktree"] != "/tmp/wt" {
		t.Errorf("the worktree = %q, want it kept", got.Data["worktree"])
	}
	if got.StatusText != "the agent did not commit the work" {
		t.Errorf("the words = %q, want the summary", got.StatusText)
	}
}

// TestSuccessOutcome_IsATwoHundred — work that got done is a 2xx, and
// there was nothing to do counts as done too.
func TestSuccessOutcome_IsATwoHundred(t *testing.T) {
	got := SuccessOutcome("the branch was already up to date",
		map[string]string{"branch": "main"})
	if !got.Success {
		t.Error("a success is a success")
	}
	if got.Status != http.StatusOK {
		t.Errorf("status = %d, want %d", got.Status, http.StatusOK)
	}
	if got.Exception() {
		t.Error("a success is not an exception")
	}
	if got.Data["branch"] != "main" {
		t.Errorf("the branch = %q, want it kept", got.Data["branch"])
	}
}

// TestMerge_AFailureSurvivesACleanTurn — the clean turn reports no
// failure, so the command's reaches the program.
func TestMerge_AFailureSurvivesACleanTurn(t *testing.T) {
	got := outcome.Merge(
		FailedOutcome(http.StatusServiceUnavailable, "the git host could not be reached"),
		outcome.Outcome{Success: true, Status: http.StatusOK},
	)
	if got.Status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want the command's", got.Status)
	}
	if got.StatusText != "the git host could not be reached" {
		t.Errorf("the words = %q, want the command's", got.StatusText)
	}
}

// TestMerge_TheTurnsFailureSurvivesANormalBranch — a normal branch
// reports no failure, so the turn's reaches the program.
func TestMerge_TheTurnsFailureSurvivesANormalBranch(t *testing.T) {
	got := outcome.Merge(
		SuccessOutcome("the worktree has nothing to commit", nil),
		FailedOutcome(http.StatusInternalServerError, "the agent did not commit the work"),
	)
	if got.Status != http.StatusInternalServerError {
		t.Errorf("status = %d, want the turn's", got.Status)
	}
	if got.StatusText != "the agent did not commit the work" {
		t.Errorf("the words = %q, want the turn's", got.StatusText)
	}
}
