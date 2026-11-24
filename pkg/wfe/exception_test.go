package wfe

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/pkg/outcome"
)

// TestOutcome_Exception — the status says which branch the work
// took: a 2xx is a normal result and a 4xx or a 5xx is an exception.
func TestOutcome_Exception(t *testing.T) {
	cases := []struct {
		name string
		out  outcome.Outcome
		want bool
	}{
		{"ok", outcome.Outcome{Success: true, Status: http.StatusOK}, false},
		{"no content", outcome.Outcome{Success: true, Status: http.StatusNoContent}, false},
		{"bad request", outcome.Outcome{Success: false, Status: http.StatusBadRequest}, true},
		{"conflict", outcome.Outcome{Success: false, Status: http.StatusConflict}, true},
		{"too many requests", outcome.Outcome{Success: false, Status: http.StatusTooManyRequests}, true},
		{"internal server error", outcome.Outcome{Success: false, Status: http.StatusInternalServerError}, true},
		{"cancelled", outcome.Outcome{Success: false, Status: outcome.StatusClientClosedRequest}, true},
		{"timeout", outcome.Outcome{Success: false, Status: http.StatusRequestTimeout}, true},
		{"no verdict", outcome.Outcome{Success: false, Status: 0}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.out.Exception(); got != c.want {
				t.Errorf("Exception() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestDecide_ANormalResultKeepsTheFlowGoing — a step that ended with
// a 2xx is a normal branch: the plan goes on with the next step of
// its own and asks nobody.
func TestDecide_ANormalResultKeepsTheFlowGoing(t *testing.T) {
	wf := mustParse(t, threeStepYAML)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], outcome.Outcome{
		Success:    true,
		Status:     http.StatusOK,
		StatusText: "the worktree has nothing to commit",
		Data:       map[string]string{"commits": "0"},
	}, false)

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionRun {
		t.Fatalf("Kind = %v, want a run (the reason was %q)", decision.Kind, decision.Reason)
	}
	if decision.Step.ID != "two" {
		t.Errorf("Step.ID = %q, want two", decision.Step.ID)
	}
	if decision.Reason != "" {
		t.Errorf("Reason = %q, want none — the plan decides on its own", decision.Reason)
	}
}

// TestDecide_AnExceptionIsAJudgmentPoint — a step that broke stops
// the flow: the agent decides what happens next.
func TestDecide_AnExceptionIsAJudgmentPoint(t *testing.T) {
	wf := mustParse(t, threeStepYAML)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], outcome.Outcome{
		Success:    false,
		Status:     http.StatusInternalServerError,
		StatusText: "the worktree could not be created",
	}, false)

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionJudgment {
		t.Fatalf("Kind = %v, want a judgment point", decision.Kind)
	}
	if decision.Subject != "one" {
		t.Errorf("Subject = %q, want one", decision.Subject)
	}
	if decision.Reason == "" {
		t.Error("the judgment point has no reason")
	}
}

// TestDecide_AnUnfinishedStepIsAJudgmentPoint — a step that never
// finished is an exception, whatever the error description says.
func TestDecide_AnUnfinishedStepIsAJudgmentPoint(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, outcome.StatusClientClosedRequest, http.StatusRequestTimeout, 0} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			wf := mustParse(t, threeStepYAML)
			state := newRunState(wf)
			RecordStep(state, "main", wf.Jobs["main"].Steps[0], outcome.Outcome{Status: status}, false)

			decision, err := Decide(state, wf)
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			if decision.Kind != DecisionJudgment {
				t.Fatalf("Kind = %v, want a judgment point", decision.Kind)
			}
			if decision.Subject != "one" {
				t.Errorf("Subject = %q, want one", decision.Subject)
			}
			if decision.Reason == "" {
				t.Error("the judgment point has no reason")
			}
		})
	}
}

// TestDecide_AStepThatIsStillRunningIsAJudgmentPoint — the plan
// cannot go on while the step waits for an answer.
func TestDecide_AStepThatIsStillRunningIsAJudgmentPoint(t *testing.T) {
	wf := mustParse(t, threeStepYAML)
	state := newRunState(wf)
	RecordPending(state, "main", wf.Jobs["main"].Steps[0])

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionJudgment {
		t.Fatalf("Kind = %v, want a judgment point", decision.Kind)
	}
	if decision.Subject != "one" {
		t.Errorf("Subject = %q, want one", decision.Subject)
	}
	if !strings.Contains(decision.Reason, "still running") {
		t.Errorf("Reason = %q, want the step that is still running", decision.Reason)
	}
}

// TestDecide_TheJudgmentReasonNamesWhatBroke — the reason the agent
// reads says what broke, and it says it on one line.
func TestDecide_TheJudgmentReasonNamesWhatBroke(t *testing.T) {
	wf := mustParse(t, threeStepYAML)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], outcome.Outcome{
		Success:    false,
		Status:     http.StatusInternalServerError,
		StatusText: "the worktree could not be created\nand the branch is gone",
	}, false)

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionJudgment {
		t.Fatalf("Kind = %v, want a judgment point", decision.Kind)
	}
	if !strings.Contains(decision.Reason, "the worktree could not be created and the branch is gone") {
		t.Errorf("Reason = %q, want it to name what broke", decision.Reason)
	}
	if strings.Contains(decision.Reason, "\n") {
		t.Errorf("Reason = %q, want it on one line", decision.Reason)
	}
}

// TestDecide_AnExceptionTheAgentResolvedIsNotAJudgmentPoint — a round
// the agent ended early is not a judgment point: the agent has already
// decided what happens next.
func TestDecide_AnExceptionTheAgentResolvedIsNotAJudgmentPoint(t *testing.T) {
	wf := mustParse(t, earlyEndYAML)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], outcome.Outcome{
		Success:    false,
		Status:     http.StatusInternalServerError,
		StatusText: "the worktree could not be created",
	}, false)

	if err := EndRound(state, wf, "main"); err != nil {
		t.Fatalf("EndRound: %v", err)
	}

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionRun {
		t.Fatalf("Kind = %v, want a run (the reason was %q)", decision.Kind, decision.Reason)
	}
	if decision.Step.ID != "one" {
		t.Errorf("Step.ID = %q, want one — the job starts its next round", decision.Step.ID)
	}
}
