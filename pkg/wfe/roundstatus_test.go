package wfe

import (
	"net/http"
	"testing"

	"github.com/cnlangzi/nightme/pkg/outcome"
)

// roundStatusYAML is a workflow with a step that waits and a second
// job: a round belongs to one job, and a wait is not work.
const roundStatusYAML = `
title: rounds
jobs:
  main:
    steps:
      - id: fix
        message: "/fix"
      - id: rest
        wait: 5m
  other:
    steps:
      - id: ship
        message: "/ship"
`

// runOf builds the action of a step that ran.
func runOf(stepID string, out outcome.Outcome) ActionRecord {
	return ActionRecord{Kind: ActionKindRun, StepID: stepID, Round: 1, Outcome: out}
}

func TestPolicy_RoundStatus(t *testing.T) {
	wf := mustParse(t, roundStatusYAML)

	tests := []struct {
		name    string
		history []ActionRecord
		want    RoundStatus
	}{
		{
			name: "a round without an action",
			want: RoundStatusSilent,
		},
		{
			name:    "a step that ran and came to something",
			history: []ActionRecord{runOf("fix", outcome.Outcome{Success: true, Status: http.StatusOK})},
			want:    RoundStatusWorked,
		},
		{
			name:    "a step that was passed over",
			history: []ActionRecord{{Kind: ActionKindRun, StepID: "fix", Round: 1, Skipped: true}},
			want:    RoundStatusSilent,
		},
		{
			name:    "a step that found nothing to do",
			history: []ActionRecord{runOf("fix", outcome.Outcome{Success: true, Status: http.StatusOK, StatusText: "nothing to commit"})},
			want:    RoundStatusWorked,
		},
		{
			name: "a step that broke",
			history: []ActionRecord{runOf("fix", outcome.Outcome{
				Success:    false,
				Status:     http.StatusInternalServerError,
				StatusText: "the remote refused",
			})},
			want: RoundStatusWorked,
		},
		{
			name:    "a step that did not finish",
			history: []ActionRecord{runOf("fix", outcome.Outcome{Success: false, Status: http.StatusInternalServerError})},
			want:    RoundStatusWorked,
		},
		{
			name:    "a step that was stopped",
			history: []ActionRecord{runOf("fix", outcome.Outcome{Success: false, Status: outcome.StatusClientClosedRequest})},
			want:    RoundStatusWorked,
		},
		{
			name:    "a step that ran out of time",
			history: []ActionRecord{runOf("fix", outcome.Outcome{Success: false, Status: http.StatusRequestTimeout})},
			want:    RoundStatusWorked,
		},
		{
			name:    "a step that reported no verdict",
			history: []ActionRecord{runOf("fix", outcome.Outcome{Success: false, Status: 0})},
			want:    RoundStatusWorked,
		},
		{
			name:    "a step that waited",
			history: []ActionRecord{runOf("rest", outcome.Outcome{Success: true, Status: http.StatusOK})},
			want:    RoundStatusSilent,
		},
		{
			name:    "a command the run issued",
			history: []ActionRecord{{Kind: ActionKindCommand, StepID: "fix", Round: 1}},
			want:    RoundStatusWorked,
		},
		{
			name:    "an instruction the run gave",
			history: []ActionRecord{{Kind: ActionKindInstruct, StepID: "fix", Round: 1}},
			want:    RoundStatusWorked,
		},
		{
			name:    "a turn without a tool call",
			history: []ActionRecord{{Kind: ActionKindStall, StepID: "fix", Round: 1}},
			want:    RoundStatusSilent,
		},
		{
			name:    "a budget that was reset",
			history: []ActionRecord{{Kind: ActionKindReset, StepID: "fix", Round: 1}},
			want:    RoundStatusSilent,
		},
		{
			name: "an action of another round",
			history: []ActionRecord{{
				Kind:    ActionKindRun,
				StepID:  "fix",
				Round:   2,
				Outcome: outcome.Outcome{Success: true, Status: http.StatusOK},
			}},
			want: RoundStatusSilent,
		},
		{
			name: "an action of another job",
			history: []ActionRecord{{
				Kind:    ActionKindRun,
				StepID:  "ship",
				Round:   1,
				Outcome: outcome.Outcome{Success: true, Status: http.StatusOK},
			}},
			want: RoundStatusSilent,
		},
		{
			name: "an action that belongs to no step",
			history: []ActionRecord{{
				Kind:    ActionKindRun,
				Round:   1,
				Outcome: outcome.Outcome{Success: true, Status: http.StatusOK},
			}},
			want: RoundStatusSilent,
		},
		{
			name: "a wait beside a step that worked",
			history: []ActionRecord{
				runOf("rest", outcome.Outcome{Success: true, Status: http.StatusOK}),
				runOf("fix", outcome.Outcome{Success: true, Status: http.StatusOK}),
			},
			want: RoundStatusWorked,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := NewPolicy(wf, tc.history)
			if got := p.RoundStatus("main", 1); got != tc.want {
				t.Errorf("RoundStatus = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPolicy_RoundStatusReadsTheRoundOfTheAction — the actions of the
// other rounds do not speak for a round.
func TestPolicy_RoundStatusReadsTheRoundOfTheAction(t *testing.T) {
	wf := mustParse(t, roundStatusYAML)

	p := NewPolicy(wf, []ActionRecord{
		{Kind: ActionKindRun, StepID: "fix", Round: 1, Outcome: outcome.Outcome{Success: true, Status: http.StatusOK}},
		{Kind: ActionKindRun, StepID: "fix", Round: 2, Outcome: outcome.Outcome{Success: true, Status: http.StatusOK}, Skipped: true},
	})

	if got := p.RoundStatus("main", 1); got != RoundStatusWorked {
		t.Errorf("the first round is %q, want %q", got, RoundStatusWorked)
	}
	if got := p.RoundStatus("main", 2); got != RoundStatusSilent {
		t.Errorf("the second round is %q, want %q", got, RoundStatusSilent)
	}
	if got := p.RoundStatus("main", 3); got != RoundStatusSilent {
		t.Errorf("the third round is %q, want %q", got, RoundStatusSilent)
	}
}

// TestPolicy_RoundStatusReadsTheJobOfTheAction — the actions of another
// job do not speak for a round.
func TestPolicy_RoundStatusReadsTheJobOfTheAction(t *testing.T) {
	wf := mustParse(t, roundStatusYAML)

	p := NewPolicy(wf, []ActionRecord{
		{Kind: ActionKindRun, StepID: "ship", Round: 1, Outcome: outcome.Outcome{Success: true, Status: http.StatusOK}},
	})

	if got := p.RoundStatus("main", 1); got != RoundStatusSilent {
		t.Errorf("the round of main is %q, want %q", got, RoundStatusSilent)
	}
	if got := p.RoundStatus("other", 1); got != RoundStatusWorked {
		t.Errorf("the round of other is %q, want %q", got, RoundStatusWorked)
	}
}
