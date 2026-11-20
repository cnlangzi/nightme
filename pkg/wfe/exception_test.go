package wfe

import (
	"strings"
	"testing"
)

// TestOutcome_Exception — the predicate reads the error description
// and the status: an outcome that carries a description is an
// exception whatever its status, and a status that is not a completed
// piece of work is an exception whatever it carries.
func TestOutcome_Exception(t *testing.T) {
	cases := []struct {
		name string
		out  Outcome
		want bool
	}{
		{"ok", Outcome{Status: OutcomeOK}, false},
		{"ok with an error description", Outcome{Status: OutcomeOK, Data: map[string]string{"error": "it broke"}}, true},
		{"noop", Outcome{Status: OutcomeNoop}, false},
		{"noop with an error description", Outcome{Status: OutcomeNoop, Data: map[string]string{"error": "it broke"}}, true},
		{"failed without an error description", Outcome{Status: OutcomeFailed}, false},
		{"failed with an error description", Outcome{Status: OutcomeFailed, Data: map[string]string{"error": "it broke"}}, true},
		{"failed with another fact", Outcome{Status: OutcomeFailed, Data: map[string]string{"reason": "nothing_to_commit"}}, false},
		{"failed with an empty error description", Outcome{Status: OutcomeFailed, Data: map[string]string{"error": ""}}, false},
		{"incomplete", Outcome{Status: OutcomeIncomplete}, true},
		{"incomplete with an error description", Outcome{Status: OutcomeIncomplete, Data: map[string]string{"error": "it broke"}}, true},
		{"cancelled", Outcome{Status: OutcomeCancelled}, true},
		{"cancelled with an error description", Outcome{Status: OutcomeCancelled, Data: map[string]string{"error": "it broke"}}, true},
		{"timeout", Outcome{Status: OutcomeTimeout}, true},
		{"timeout with an error description", Outcome{Status: OutcomeTimeout, Data: map[string]string{"error": "it broke"}}, true},
		{"unknown", Outcome{Status: OutcomeUnknown}, true},
		{"unknown with an error description", Outcome{Status: OutcomeUnknown, Data: map[string]string{"error": "it broke"}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.out.Exception(); got != c.want {
				t.Errorf("Exception() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestOutcomeKeyUnfinished pins the name of the key the unfinished
// count rides in: the command that reports the count and the program
// that reads it agree on the literal, and a rename would break the
// handover silently.
func TestOutcomeKeyUnfinished(t *testing.T) {
	if OutcomeKeyUnfinished != "unfinished" {
		t.Errorf("OutcomeKeyUnfinished = %q, want %q", OutcomeKeyUnfinished, "unfinished")
	}
}

// TestDecide_ANormalResultKeepsTheFlowGoing — a step that ended
// failed without an error description is a normal branch: the plan
// goes on with the next step of its own and asks nobody.
func TestDecide_ANormalResultKeepsTheFlowGoing(t *testing.T) {
	wf := mustParse(t, threeStepYAML)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], Outcome{
		Status:  OutcomeFailed,
		Summary: "the worktree has nothing to commit",
		Data:    map[string]string{"reason": "nothing_to_commit"},
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
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], Outcome{
		Status:  OutcomeFailed,
		Summary: "the worktree could not be created",
		Data:    map[string]string{"error": "the worktree could not be created"},
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
	for _, status := range []OutcomeStatus{OutcomeIncomplete, OutcomeCancelled, OutcomeTimeout, OutcomeUnknown} {
		t.Run(string(status), func(t *testing.T) {
			wf := mustParse(t, threeStepYAML)
			state := newRunState(wf)
			RecordStep(state, "main", wf.Jobs["main"].Steps[0], Outcome{Status: status}, false)

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
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], Outcome{
		Status:  OutcomeFailed,
		Summary: "the worktree could not be created",
		Data:    map[string]string{OutcomeKeyError: "the worktree could not be created\nand the branch is gone"},
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
	wf := mustParse(t, finalStepYAML)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], Outcome{
		Status:  OutcomeFailed,
		Summary: "the worktree could not be created",
		Data:    map[string]string{"error": "the worktree could not be created"},
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
	if decision.Step.ID != "cleanup" {
		t.Errorf("Step.ID = %q, want cleanup", decision.Step.ID)
	}
}
