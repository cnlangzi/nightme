package wfe

import (
	"errors"
	"fmt"
)

// DecisionKind is what the engine decided about a run.
type DecisionKind int

const (
	// DecisionRun means the plan decides: the step runs as it is.
	DecisionRun DecisionKind = iota
	// DecisionJudgment means the plan cannot decide: the agent does.
	DecisionJudgment
)

// Decision is what the engine decided about the next step of a run.
type Decision struct {
	// Kind is what the engine decided.
	Kind DecisionKind
	// Step is the step to run. It is set when Kind is DecisionRun.
	Step Step
	// Job is the job the step belongs to.
	Job string
	// Ready is the ready set the decision was made from.
	Ready []Step
	// Reason explains a judgment point.
	Reason string
	// Exhausted is true when the plan has nothing left to run.
	Exhausted bool
}

// Decide computes what a run does next.
//
// The plan decides when the ready set holds exactly one step and the
// previous outcome was ok or noop. A run that has not executed
// anything yet counts as ok, so the first step runs on the fast path.
//
// Everything else is a judgment point: the ready set is empty, the
// ready set forks, or the previous outcome was not ok or noop. The
// agent decides what happens next.
//
// The state is the engine's: the steps whose condition did not hold
// are recorded as skipped before the decision is made.
func Decide(state *RunState, wf *Workflow) (Decision, error) {
	if state == nil {
		return Decision{}, errors.New("wfe: nil state")
	}
	if wf == nil {
		return Decision{}, errors.New("wfe: nil workflow")
	}

	ready, skipped, err := readySet(state, wf)
	if err != nil {
		return Decision{}, err
	}
	for _, step := range skipped {
		RecordStep(state, step.Job, step.Step, Outcome{
			Status:  OutcomeNoop,
			Summary: "the step's condition did not hold",
		}, true)
	}

	steps := make([]Step, 0, len(ready))
	for _, step := range ready {
		steps = append(steps, step.Step)
	}
	prev, hasPrev := lastRun(state)
	switch {
	case len(ready) == 0:
		return Decision{
			Kind:      DecisionJudgment,
			Reason:    exhaustedReason(prev, hasPrev),
			Exhausted: true,
		}, nil
	case len(ready) > 1:
		return Decision{
			Kind:   DecisionJudgment,
			Ready:  steps,
			Reason: fmt.Sprintf("%d steps are ready — the plan has to pick one", len(ready)),
		}, nil
	}
	if IsPending(state, ready[0].Step.ID) {
		return Decision{
			Kind:   DecisionJudgment,
			Ready:  steps,
			Reason: fmt.Sprintf("the step %q is still running", ready[0].Step.ID),
		}, nil
	}
	if hasPrev && prevBlocks(state, wf, prev) {
		return Decision{
			Kind:   DecisionJudgment,
			Ready:  steps,
			Reason: fmt.Sprintf("the previous step %q ended as %q", prev.ID, prev.Outcome.Status),
		}, nil
	}
	return Decision{
		Kind:  DecisionRun,
		Step:  ready[0].Step,
		Job:   ready[0].Job,
		Ready: steps,
	}, nil
}

// exhaustedReason explains why a run with nothing ready has to stop.
func exhaustedReason(prev StepState, hasPrev bool) string {
	if hasPrev && !prev.Outcome.Succeeded() {
		return fmt.Sprintf("the plan is exhausted and the last step %q ended as %q",
			prev.ID, prev.Outcome.Status)
	}
	return "the plan is exhausted: every step has run"
}

// prevBlocks reports whether the last step that ran keeps the chain
// from going on by itself.
//
// A step that did not end ok is a judgment point: the agent decides.
// A step that did not end ok but was marked continue-on-error is not:
// the chain continues, and the error is in the record.
func prevBlocks(state *RunState, wf *Workflow, prev StepState) bool {
	if prev.Outcome.Succeeded() {
		return false
	}
	step := StepByID(wf, prev.ID)
	return step == nil || !step.ContinueOnError
}

// readyStep is a step of the ready set, with the job it belongs to.
type readyStep struct {
	Step Step
	Job  string
}

// readySet returns the steps a run may run next and the steps whose
// condition did not hold.
//
// A step is ready when its job's needs are all satisfied and its own
// `if:` holds. Within a job only the first ready step counts: the
// steps of a job run in order.
func readySet(state *RunState, wf *Workflow) ([]readyStep, []readyStep, error) {
	done := completedJobs(state, wf)
	var ready, skipped []readyStep
	for _, name := range wf.JobOrder() {
		job := wf.Jobs[name]
		if done[name] {
			continue
		}
		blocked := false
		for _, need := range job.Needs {
			if !done[need] {
				blocked = true
				break
			}
		}
		if blocked {
			continue
		}
		if job.If != "" {
			ok, err := EvalCond(job.If, buildExprCtx(state), nil)
			if err != nil {
				return nil, nil, fmt.Errorf("wfe: job %q: %w", name, err)
			}
			if !ok {
				// The job does not run: every step of it is skipped.
				for _, step := range job.Steps {
					if !HasRun(state, step.ID) {
						skipped = append(skipped, readyStep{Step: step, Job: name})
					}
				}
				continue
			}
		}
		for _, step := range job.Steps {
			if HasRun(state, step.ID) {
				continue
			}
			if step.If != "" {
				ok, err := EvalCond(step.If, buildExprCtx(state), nil)
				if err != nil {
					return nil, nil, fmt.Errorf("wfe: step %q: %w", step.ID, err)
				}
				if !ok {
					skipped = append(skipped, readyStep{Step: step, Job: name})
					continue
				}
			}
			ready = append(ready, readyStep{Step: step, Job: name})
			// Only the first ready step of a job counts.
			break
		}
	}
	return ready, skipped, nil
}

// completedJobs returns the jobs whose steps have all run.
func completedJobs(state *RunState, wf *Workflow) map[string]bool {
	out := map[string]bool{}
	for name, job := range wf.Jobs {
		complete := true
		for _, step := range job.Steps {
			if !HasRun(state, step.ID) {
				complete = false
				break
			}
		}
		if complete {
			out[name] = true
		}
	}
	return out
}

// HasRun reports whether the step already ran.
//
// A step that started and has not settled yet has not run: it is
// still the ready one.
func HasRun(state *RunState, stepID string) bool {
	if state == nil {
		return false
	}
	for _, st := range state.Steps {
		if st.ID == stepID && !st.Pending {
			return true
		}
	}
	return false
}

// lastRun returns the last step that ran. A step that was skipped
// does not count: the chain continues past it.
func lastRun(state *RunState) (StepState, bool) {
	if state == nil {
		return StepState{}, false
	}
	for i := len(state.Steps) - 1; i >= 0; i-- {
		if state.Steps[i].Skipped || state.Steps[i].Pending {
			continue
		}
		return state.Steps[i], true
	}
	return StepState{}, false
}

// RecordStep records the outcome of a step.
//
// The engine owns the state: this is the only way a step's result
// enters it. The agent proposes a directive; the engine writes down
// what came of it.
//
// A step that ran already keeps its place in the run: the new outcome
// is its latest one. A step that runs again — a repair the agent made
// inside the step — therefore replaces the verdict the step had.
func RecordStep(state *RunState, job string, step Step, out Outcome, skipped bool) {
	if state == nil {
		return
	}
	for i := range state.Steps {
		if state.Steps[i].ID != step.ID {
			continue
		}
		state.Steps[i].Outcome = out
		state.Steps[i].Skipped = skipped
		state.Steps[i].Pending = false
		return
	}
	state.Steps = append(state.Steps, StepState{
		ID:      step.ID,
		Job:     job,
		Outcome: out,
		Skipped: skipped,
	})
}

// RecordPending records that a step started and has not settled.
//
// The step is still the ready one — it has not run — but the plan
// cannot go on without the agent: the command is waiting for an
// answer.
func RecordPending(state *RunState, job string, step Step) {
	if state == nil {
		return
	}
	for i := range state.Steps {
		if state.Steps[i].ID != step.ID {
			continue
		}
		state.Steps[i].Pending = true
		return
	}
	state.Steps = append(state.Steps, StepState{
		ID:      step.ID,
		Job:     job,
		Outcome: Outcome{Status: OutcomeUnknown},
		Pending: true,
	})
}

// IsPending reports whether the step started and has not settled.
func IsPending(state *RunState, stepID string) bool {
	if state == nil {
		return false
	}
	for _, st := range state.Steps {
		if st.ID == stepID {
			return st.Pending
		}
	}
	return false
}

// JobOf returns the job a step belongs to.
func JobOf(wf *Workflow, stepID string) string {
	if wf == nil {
		return ""
	}
	for name, job := range wf.Jobs {
		for _, step := range job.Steps {
			if step.ID == stepID {
				return name
			}
		}
	}
	return ""
}

// StepByID returns the step with the id, or nil.
func StepByID(wf *Workflow, stepID string) *Step {
	if wf == nil {
		return nil
	}
	for _, job := range wf.Jobs {
		for i := range job.Steps {
			if job.Steps[i].ID == stepID {
				return &job.Steps[i]
			}
		}
	}
	return nil
}

// LastOutcome returns the outcome of the last step that ran.
//
// A step that was skipped does not count: the chain continues past it.
func LastOutcome(state *RunState) (Outcome, bool) {
	prev, ok := lastRun(state)
	if !ok {
		return Outcome{}, false
	}
	return prev.Outcome, true
}

// ExprContext renders the run state as the context an expression
// reads.
func ExprContext(state *RunState) ExprCtx {
	return buildExprCtx(state)
}

// buildExprCtx renders the state as the context an expression reads.
func buildExprCtx(state *RunState) ExprCtx {
	ec := ExprCtx{
		Steps: map[string]map[string]string{},
		Needs: map[string]map[string]string{},
		Env:   state.Env,
		Now:   state.UpdatedAt,
	}
	for _, st := range state.Steps {
		ec.Steps[st.ID] = outcomeOutputs(st.Outcome)
	}
	// The steps are in the order they ran, so the last write per job
	// is the job's last step.
	for _, st := range state.Steps {
		ec.Needs[st.Job] = outcomeOutputs(st.Outcome)
	}
	return ec
}

// outcomeOutputs renders an outcome as the outputs an expression
// reads: outputs.status, outputs.summary and outputs.data.<key>.
func outcomeOutputs(o Outcome) map[string]string {
	out := map[string]string{
		"outputs.status":  string(o.Status),
		"outputs.summary": o.Summary,
	}
	for k, v := range o.Data {
		out["outputs.data."+k] = v
	}
	return out
}
