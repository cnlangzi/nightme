package wfe

import (
	"errors"
	"fmt"
	"maps"
	"net/http"

	"github.com/cnlangzi/nightme/pkg/outcome"
)

// The reasons a target cannot be moved.
//
// The reasons are the words the operator reads: the command that
// names a target it cannot move says which of them it is.
const (
	// targetUnknown says the target is no step of the workflow.
	targetUnknown = "is not part of this workflow"
	// targetOtherJob says the target belongs to another job.
	targetOtherJob = "belongs to another job"
	// targetPassed says the target has passed: the plan of a run only
	// goes forward.
	targetPassed = "has passed"
	// targetSucceeded says the target did its job already.
	targetSucceeded = "already succeeded"
)

// SkipError says why a target cannot be moved.
type SkipError struct {
	// Step is the step the refusal is about.
	Step string
	// Reason says what is wrong with the step.
	Reason string
}

// Error implements the error interface.
func (e *SkipError) Error() string {
	return fmt.Sprintf("the step %q %s", e.Step, e.Reason)
}

// SkipRange returns the steps of a job's round that a skip through the
// target covers.
//
// The range runs from the step the run is on up to and including the
// target, within the target's job. A step that did its job keeps its
// verdict: the range covers the steps that have not run, and the
// steps that ran and did not succeed.
//
// It refuses a target that is no step of the workflow, a target that
// belongs to another job, a target that has passed and a target that
// did its job already.
func SkipRange(state *RunState, wf *Workflow, from, target string) ([]Step, error) {
	if state == nil {
		return nil, errors.New("wfe: nil state")
	}
	if wf == nil {
		return nil, errors.New("wfe: nil workflow")
	}
	if StepByID(wf, from) == nil {
		return nil, &SkipError{Step: from, Reason: targetUnknown}
	}
	if StepByID(wf, target) == nil {
		return nil, &SkipError{Step: target, Reason: targetUnknown}
	}
	job := JobOf(wf, target)
	if JobOf(wf, from) != job {
		return nil, &SkipError{Step: target, Reason: targetOtherJob}
	}
	spec, ok := wf.Jobs[job]
	if !ok {
		return nil, &SkipError{Step: target, Reason: targetOtherJob}
	}
	fromIdx := stepIndexOf(spec.Steps, from)
	targetIdx := stepIndexOf(spec.Steps, target)
	if fromIdx < 0 || targetIdx < 0 {
		return nil, &SkipError{Step: target, Reason: targetUnknown}
	}
	if st, ok := stepRecord(state, target); ok && st.Outcome.Succeeded() {
		// The step did its job: the plan moved past it, and there is
		// nothing to skip.
		return nil, &SkipError{Step: target, Reason: targetSucceeded}
	}
	if targetIdx < fromIdx {
		// The plan of a run only goes forward.
		return nil, &SkipError{Step: target, Reason: targetPassed}
	}
	out := make([]Step, 0, targetIdx-fromIdx+1)
	for _, step := range spec.Steps[fromIdx : targetIdx+1] {
		if st, ok := stepRecord(state, step.ID); ok && st.Outcome.Succeeded() {
			// The step did its job: its verdict stays.
			continue
		}
		out = append(out, step)
	}
	return out, nil
}

// SkipThrough records the steps of the range as skipped.
//
// The facts the steps produced are kept: a step that was passed over
// keeps what it did before it was passed over. The words of the
// skipped steps say who passed them over.
//
// It returns the records it wrote.
func SkipThrough(state *RunState, wf *Workflow, from, target, summary string) ([]StepState, error) {
	steps, err := SkipRange(state, wf, from, target)
	if err != nil {
		return nil, err
	}
	return SkipSteps(state, wf, steps, summary), nil
}

// SkipSteps records the steps of a range as skipped.
//
// The range is the one SkipRange returns: the caller that needs to
// look at the range before it is passed over does not walk it twice.
func SkipSteps(state *RunState, wf *Workflow, steps []Step, summary string) []StepState {
	out := make([]StepState, 0, len(steps))
	for _, step := range steps {
		RecordStep(state, JobOf(wf, step.ID), step, passedOver(state, step, summary), false)
		st, _ := stepRecord(state, step.ID)
		out = append(out, st)
	}
	return out
}

// passedOver builds the outcome of a step that was passed over.
//
// The status is a success: the step did not break, it was passed
// over. The facts the step produced are kept.
func passedOver(state *RunState, step Step, summary string) outcome.Outcome {
	out := outcome.Outcome{
		Success:    true,
		Status:     http.StatusOK,
		StatusText: summary,
	}
	if st, ok := stepRecord(state, step.ID); ok && len(st.Outcome.Data) > 0 {
		out.Data = maps.Clone(st.Outcome.Data)
	}
	return out
}

// stepIndexOf returns the index of the step with the id, or -1.
func stepIndexOf(steps []Step, id string) int {
	for i, step := range steps {
		if step.ID == id {
			return i
		}
	}
	return -1
}

// stepRecord returns the record of a step.
func stepRecord(state *RunState, stepID string) (StepState, bool) {
	for _, st := range state.Steps {
		if st.ID == stepID {
			return st, true
		}
	}
	return StepState{}, false
}
