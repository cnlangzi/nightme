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
	// Subject is the step the judgment is about.
	//
	// It is set when the judgment names one step: the step that
	// failed, the step that is still running. A judgment about no step
	// — the plan forks, a job starts a new round — leaves it empty.
	Subject string
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
// A job whose round is over starts its next round instead of ending
// the run: the round's outputs are cleared and the step budgets start
// over.
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
	recordSkipped(state, skipped)

	steps := make([]Step, 0, len(ready))
	for _, step := range ready {
		steps = append(steps, step.Step)
	}
	prev, hasPrev := lastRun(state)
	// A step that did not end ok is a judgment point: the plan cannot
	// go on by itself. A round the agent ended early is not a
	// judgment point: the agent has already decided.
	blocked := hasPrev && prevBlocks(state, wf, prev) && !roundEnded(state, prev)

	if len(ready) == 0 {
		repeating := repeatingJobs(state, wf)
		switch {
		case len(repeating) == 0:
			return Decision{
				Kind:      DecisionJudgment,
				Reason:    exhaustedReason(prev, hasPrev),
				Exhausted: true,
				Subject:   subjectOf(prev, hasPrev),
			}, nil
		case blocked:
			// The round does not advance on its own: the agent decides
			// what to do about the step that did not end ok.
			return Decision{
				Kind:    DecisionJudgment,
				Reason:  fmt.Sprintf("the previous step %q ended as %q", prev.ID, prev.Outcome.Status),
				Subject: prev.ID,
			}, nil
		case len(repeating) > 1:
			return Decision{
				Kind:   DecisionJudgment,
				Reason: fmt.Sprintf("%d jobs are ready to start another round", len(repeating)),
			}, nil
		}
		// The round is over: the job starts the next one.
		startRound(state, wf, repeating[0])
		ready, skipped, err = readySet(state, wf)
		if err != nil {
			return Decision{}, err
		}
		recordSkipped(state, skipped)
		steps = steps[:0]
		for _, step := range ready {
			steps = append(steps, step.Step)
		}
		if len(ready) == 0 {
			return Decision{
				Kind:      DecisionJudgment,
				Reason:    exhaustedReason(prev, hasPrev),
				Exhausted: true,
				Subject:   subjectOf(prev, hasPrev),
			}, nil
		}
	}

	if len(ready) > 1 {
		return Decision{
			Kind:   DecisionJudgment,
			Ready:  steps,
			Reason: fmt.Sprintf("%d steps are ready — the plan has to pick one", len(ready)),
		}, nil
	}
	if IsPending(state, ready[0].Step.ID) {
		return Decision{
			Kind:    DecisionJudgment,
			Ready:   steps,
			Reason:  fmt.Sprintf("the step %q is still running", ready[0].Step.ID),
			Subject: ready[0].Step.ID,
		}, nil
	}
	if blocked {
		return Decision{
			Kind:    DecisionJudgment,
			Ready:   steps,
			Reason:  fmt.Sprintf("the previous step %q ended as %q", prev.ID, prev.Outcome.Status),
			Subject: prev.ID,
		}, nil
	}
	return Decision{
		Kind:  DecisionRun,
		Step:  ready[0].Step,
		Job:   ready[0].Job,
		Ready: steps,
	}, nil
}

// ReadySet returns the steps a run may run next: the unrun steps
// whose job's needs are all satisfied and whose own `if:` holds.
//
// Within a job only the first such step counts: the steps of a job
// run in order. The set is the plan's, computed from the state as it
// stands — it does not advance a round and it does not record the
// steps whose condition did not hold.
func ReadySet(state *RunState, wf *Workflow) ([]Step, error) {
	if state == nil {
		return nil, errors.New("wfe: nil state")
	}
	// The engine counts the rounds as it looks at the jobs: a copy
	// keeps the caller's state where it was.
	copied := *state
	copied.Rounds = make(map[string]int, len(state.Rounds))
	for job, round := range state.Rounds {
		copied.Rounds[job] = round
	}
	ready, _, err := readySet(&copied, wf)
	if err != nil {
		return nil, err
	}
	out := make([]Step, 0, len(ready))
	for _, rs := range ready {
		out = append(out, rs.Step)
	}
	return out, nil
}

// recordSkipped records the steps whose condition did not hold.
func recordSkipped(state *RunState, skipped []readyStep) {
	for _, step := range skipped {
		RecordStep(state, step.Job, step.Step, Outcome{
			Status:  OutcomeNoop,
			Summary: "the step's condition did not hold",
		}, true)
	}
}

// repeatingJobs returns the jobs whose round is over and that have
// rounds left.
//
// A round in which no step ran did nothing: the job does not start
// another one. Without that a job whose steps are all skipped would
// go round after round and never come back.
func repeatingJobs(state *RunState, wf *Workflow) []string {
	var out []string
	for _, name := range wf.JobOrder() {
		if !jobComplete(state, wf, name) {
			continue
		}
		if !jobDidSomething(state, wf, name) {
			continue
		}
		rounds := wf.Jobs[name].Rounds()
		if rounds == 0 || state.Rounds[name] < rounds {
			out = append(out, name)
		}
	}
	return out
}

// jobDidSomething reports whether the job's last round ran at least
// one step. A step that was skipped did not run.
func jobDidSomething(state *RunState, wf *Workflow, name string) bool {
	for _, st := range state.Steps {
		if st.Job == name && !st.Skipped {
			return true
		}
	}
	return false
}

// startRound begins the next round of a job.
//
// The round's outputs are cleared, so an expression reads the next
// round's facts, and the step budgets start over.
func startRound(state *RunState, wf *Workflow, name string) {
	if state.Rounds == nil {
		state.Rounds = map[string]int{}
	}
	state.Rounds[name]++
	delete(state.EndedRounds, name)
	clearJobSteps(state, wf, name)
}

// roundEnded reports whether the step is the one whose failure ended
// the round of its job.
//
// The agent ended the round: the plan goes on with the final step of
// the job or with the next round, not with a judgment point.
func roundEnded(state *RunState, prev StepState) bool {
	if prev.Job == "" || prev.ID == "" {
		return false
	}
	ended, ok := state.EndedRounds[prev.Job]
	if !ok {
		return false
	}
	return ended.Round == state.RoundOf(prev.Job) && ended.Step == prev.ID
}

// EndRound ends the current round of a job early.
//
// It is what the agent does when there is nothing left to do in this
// round: the steps of the round that did not run are recorded as
// skipped — the final step of the job excepted, for it runs in their
// place — and the round is marked as ended. The plan goes on with the
// final step of the job, or with its next round when it has none.
//
// The engine owns the state: this is the only way a round ends early.
func EndRound(state *RunState, wf *Workflow, job string) error {
	if state == nil {
		return errors.New("wfe: nil state")
	}
	if wf == nil {
		return errors.New("wfe: nil workflow")
	}
	spec, ok := wf.Jobs[job]
	if !ok {
		return fmt.Errorf("wfe: no job %q", job)
	}
	for _, step := range spec.Steps {
		if IsPending(state, step.ID) {
			// A step of the round is still running: the round cannot
			// end under a command that has not settled.
			return fmt.Errorf("wfe: the step %q is still running", step.ID)
		}
	}
	round := state.RoundOf(job)
	ended := lastRunStep(state, job)
	if prev, ok := state.EndedRounds[job]; ok && prev.Round == round && prev.Step == ended {
		// The round is already over at this very point: ending it
		// again would change nothing, and the plan would ask the
		// agent the same question forever.
		return fmt.Errorf("wfe: the round %d of %q is already over", round, job)
	}
	for _, step := range spec.Steps {
		if HasRun(state, step.ID) {
			continue
		}
		if step.Final {
			// The final step runs in place of the steps the round
			// cut short.
			continue
		}
		RecordStep(state, job, step, Outcome{
			Status:  OutcomeNoop,
			Summary: "the round was ended before this step ran",
		}, true)
	}
	if state.EndedRounds == nil {
		state.EndedRounds = map[string]EndedRound{}
	}
	state.EndedRounds[job] = EndedRound{Round: round, Step: ended}
	return nil
}

// lastRunStep returns the id of the last step that ran in the job,
// or an empty string when the job ran nothing.
func lastRunStep(state *RunState, job string) string {
	for i := len(state.Steps) - 1; i >= 0; i-- {
		st := state.Steps[i]
		if st.Job != job || st.Skipped || st.Pending {
			continue
		}
		return st.ID
	}
	return ""
}

// clearJobSteps drops the steps of a job from the run state.
func clearJobSteps(state *RunState, wf *Workflow, name string) {
	job, ok := wf.Jobs[name]
	if !ok {
		return
	}
	ids := make(map[string]bool, len(job.Steps))
	for _, step := range job.Steps {
		ids[step.ID] = true
	}
	// A fresh slice: the run state is marshalled outside the run's
	// lock, so the old one must not be written to.
	kept := make([]StepState, 0, len(state.Steps))
	for _, st := range state.Steps {
		if ids[st.ID] {
			continue
		}
		kept = append(kept, st)
	}
	state.Steps = kept
}

// exhaustedReason explains why a run with nothing ready has to stop.
func exhaustedReason(prev StepState, hasPrev bool) string {
	if hasPrev && !prev.Outcome.Succeeded() {
		return fmt.Sprintf("the plan is exhausted and the last step %q ended as %q",
			prev.ID, prev.Outcome.Status)
	}
	return "the plan is exhausted: every step has run"
}

// subjectOf returns the step a judgment is about: the step that ran
// last. Empty when nothing ran yet.
func subjectOf(prev StepState, hasPrev bool) string {
	if !hasPrev {
		return ""
	}
	return prev.ID
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
	// The context is the same for every condition of the round.
	ec := buildExprCtx(state)
	var ready, skipped []readyStep
	for _, name := range wf.JobOrder() {
		job := wf.Jobs[name]
		// The job's first round begins as soon as the plan looks at it.
		if state.Rounds == nil {
			state.Rounds = map[string]int{}
		}
		if state.Rounds[name] == 0 {
			state.Rounds[name] = 1
		}
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
			ok, err := EvalCond(job.If, ec)
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
				ok, err := EvalCond(step.If, ec)
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
	for name := range wf.Jobs {
		if jobComplete(state, wf, name) {
			out[name] = true
		}
	}
	return out
}

// jobComplete reports whether every step of the job has run.
func jobComplete(state *RunState, wf *Workflow, name string) bool {
	job, ok := wf.Jobs[name]
	if !ok {
		return false
	}
	for _, step := range job.Steps {
		if !HasRun(state, step.ID) {
			return false
		}
	}
	return true
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
// is its latest one. A step that runs again therefore replaces the
// verdict the step had. A repair — a command the agent issued for the
// step — does not: it never reaches this function.
func RecordStep(state *RunState, job string, step Step, out Outcome, skipped bool) {
	if state == nil {
		return
	}
	round := state.RoundOf(job)
	for i := range state.Steps {
		if state.Steps[i].ID != step.ID {
			continue
		}
		state.Steps[i].Outcome = out
		state.Steps[i].Skipped = skipped
		state.Steps[i].Pending = false
		state.Steps[i].Round = round
		return
	}
	state.Steps = append(state.Steps, StepState{
		ID:      step.ID,
		Job:     job,
		Round:   round,
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
	round := state.RoundOf(job)
	for i := range state.Steps {
		if state.Steps[i].ID != step.ID {
			continue
		}
		state.Steps[i].Pending = true
		state.Steps[i].Round = round
		return
	}
	state.Steps = append(state.Steps, StepState{
		ID:      step.ID,
		Job:     job,
		Round:   round,
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
// reads: status, summary and one data.<key> per fact.
func outcomeOutputs(o Outcome) map[string]string {
	out := map[string]string{
		"status":  string(o.Status),
		"summary": o.Summary,
	}
	for k, v := range o.Data {
		out["data."+k] = v
	}
	return out
}
