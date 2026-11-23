// Tests for the status of a round that ended.
//
// A round that produced nothing is a round that was passed over or
// that had nothing to do. Its own card stays — the round happened —
// but it stays out of the tree of the rounds: its row would be noise.

package me

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/pkg/outcome"
	"github.com/cnlangzi/nightme/pkg/wfe"
)

// roundStatusRun builds a run of the one-step workflow.
func roundStatusRun(t *testing.T) *run {
	t.Helper()
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, noModels())
	wf, ok := svc.catalog.Lookup("one")
	if !ok {
		t.Fatal("the workflow is not in the catalogue")
	}
	bound, err := wfe.Bind(wf, map[string]string{})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	r := newRun(svc, Request{Name: "one", ChatID: "obs-1", Cwd: "/tmp/work"}, bound, nil)
	t.Cleanup(r.cancel)
	return r
}

// TestTheRoundsTreeLeavesOutTheRoundThatProducedNothing — a round that
// produced nothing is not a row of the tree.
func TestTheRoundsTreeLeavesOutTheRoundThatProducedNothing(t *testing.T) {
	r := roundStatusRun(t)

	start := time.Date(2026, 11, 18, 10, 33, 4, 0, time.UTC)
	r.roundRecords = []roundRecord{
		{
			Round:     1,
			Job:       "main",
			StartedAt: start,
			EndedAt:   start.Add(time.Minute),
			Outcome:   outcome.Outcome{Status: http.StatusOK},
			Status:    wfe.RoundStatusWorked,
		},
		{
			Round:     2,
			Job:       "main",
			StartedAt: start.Add(time.Minute),
			EndedAt:   start.Add(2 * time.Minute),
			Outcome:   outcome.Outcome{Status: http.StatusOK},
			Status:    wfe.RoundStatusSilent,
		},
	}

	tree := r.roundsTree(iteration{job: "main", round: 3})
	if !strings.Contains(tree, "[ 1 ] v ") {
		t.Errorf("the tree does not list the round that worked: %q", tree)
	}
	if strings.Contains(tree, "[ 2 ]") {
		t.Errorf("the tree lists the round that produced nothing: %q", tree)
	}
	// The round that is going is still on the tree.
	if !strings.Contains(tree, "[ 3 ] > ") {
		t.Errorf("the tree does not list the round that is going: %q", tree)
	}
}

// TestTheRecordCardLeavesOutTheRoundThatProducedNothing — the tree of
// the record of a finished run keeps the same rule.
func TestTheRecordCardLeavesOutTheRoundThatProducedNothing(t *testing.T) {
	r := roundStatusRun(t)

	start := time.Date(2026, 11, 18, 10, 33, 4, 0, time.UTC)
	r.roundRecords = []roundRecord{
		{
			Round:     1,
			Job:       "main",
			StartedAt: start,
			EndedAt:   start.Add(time.Minute),
			Outcome:   outcome.Outcome{Status: http.StatusOK},
			Status:    wfe.RoundStatusWorked,
		},
		{
			Round:     2,
			Job:       "main",
			StartedAt: start.Add(time.Minute),
			EndedAt:   start.Add(2 * time.Minute),
			Outcome:   outcome.Outcome{Status: http.StatusOK},
			Status:    wfe.RoundStatusSilent,
		},
	}

	card := r.recordCard()
	if !strings.Contains(card, "[ 1 ] v ") {
		t.Errorf("the record does not list the round that worked: %q", card)
	}
	if strings.Contains(card, "[ 2 ]") {
		t.Errorf("the record lists the round that produced nothing: %q", card)
	}
}

// TestTheFoldedRoundsCountTheRoundsThatWorked — the rounds that
// produced nothing are not counted among the folded ones.
func TestTheFoldedRoundsCountTheRoundsThatWorked(t *testing.T) {
	r := roundStatusRun(t)

	start := time.Date(2026, 11, 18, 10, 33, 4, 0, time.UTC)
	for i := 1; i <= 12; i++ {
		status := wfe.RoundStatusWorked
		if i%2 == 0 {
			status = wfe.RoundStatusSilent
		}
		r.roundRecords = append(r.roundRecords, roundRecord{
			Round:     i,
			Job:       "main",
			StartedAt: start.Add(time.Duration(i) * time.Minute),
			EndedAt:   start.Add(time.Duration(i)*time.Minute + time.Second),
			Outcome:   outcome.Outcome{Status: http.StatusOK},
			Status:    status,
		})
	}

	tree := r.roundsTree(iteration{job: "main", round: 13})
	// Six rounds worked, and the tree shows the last five of them: the
	// oldest one folds away.
	if !strings.Contains(tree, "├─ … +1") {
		t.Errorf("the tree does not fold the old rounds: %q", tree)
	}
	for _, want := range []string{"[  3 ]", "[  5 ]", "[  7 ]", "[  9 ]", "[ 11 ]"} {
		if !strings.Contains(tree, want) {
			t.Errorf("the tree does not list round %s: %q", want, tree)
		}
	}
	for _, unwanted := range []string{"[  1 ]", "[  2 ]", "[  4 ]", "[  6 ]", "[  8 ]", "[ 10 ]", "[ 12 ]"} {
		if strings.Contains(tree, unwanted) {
			t.Errorf("the tree lists round %s: %q", unwanted, tree)
		}
	}
}

// TestARoundWithoutAStatusStaysInTheTree — the status of a round is
// optional in the record that holds it: a record that carries none
// keeps its round on the tree.
func TestARoundWithoutAStatusStaysInTheTree(t *testing.T) {
	r := roundStatusRun(t)

	start := time.Date(2026, 11, 18, 10, 33, 4, 0, time.UTC)
	r.roundRecords = []roundRecord{{
		Round:     1,
		Job:       "main",
		StartedAt: start,
		EndedAt:   start.Add(time.Minute),
		Outcome:   outcome.Outcome{Status: http.StatusOK},
	}}

	tree := r.roundsTree(iteration{job: "main", round: 2})
	if !strings.Contains(tree, "[ 1 ] v ") {
		t.Errorf("the tree does not list the round without a status: %q", tree)
	}
}

// TestTheRoundThatEndedCarriesItsStatus — the record of a round says
// whether the round produced work.
func TestTheRoundThatEndedCarriesItsStatus(t *testing.T) {
	r := roundStatusRun(t)

	at := time.Date(2026, 11, 18, 10, 33, 4, 0, time.UTC)
	r.record(wfe.ActionKindRun, "/gtw commit", "commit", outcome.Outcome{Status: http.StatusOK})
	worked := r.closeRound("main", 1, at.Add(time.Minute))
	if worked.Status != wfe.RoundStatusWorked {
		t.Errorf("the round that ran a step is %q, want %q", worked.Status, wfe.RoundStatusWorked)
	}

	r.record(wfe.ActionKindRun, "/gtw commit", "commit", outcome.Outcome{Status: http.StatusOK})
	silent := r.closeRound("main", 2, at.Add(2*time.Minute))
	if silent.Status != wfe.RoundStatusSilent {
		t.Errorf("the round that had nothing to do is %q, want %q", silent.Status, wfe.RoundStatusSilent)
	}
}
