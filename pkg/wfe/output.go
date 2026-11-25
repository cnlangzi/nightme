package wfe

import (
	"errors"
	"fmt"
	"strconv"
)

// The keys of a step's output that are not facts.
//
// The verdict of a step is read by the plan: the status says how the
// step went, and the success follows from it.
const (
	// outputStatus is the status code of a step.
	outputStatus = "status"
	// outputStatusText is the line of words of a step.
	outputStatusText = "statusText"
	// outputSuccess is the verdict of a step.
	//
	// It is not written: it follows from the status, and a step that
	// carries a 2xx and says it failed is a step that contradicts
	// itself.
	outputSuccess = "success"
)

// SetOutput writes into the output of a step.
//
// The engine owns the state: this is the only way a step's output
// changes. A fact is written under its own name, the status writes
// the verdict — the success follows from the status: a 2xx is a
// success, anything else is not — and the status text replaces the
// line of words.
//
// The success of a step is not written: it follows from the status.
// A step that has no record has no output to write. The function
// refuses both.
//
// The write can unblock the plan: the steps whose condition did not
// hold are looked at again, and the ones whose condition holds now
// run.
func SetOutput(state *RunState, wf *Workflow, stepID, key, value string) error {
	if state == nil {
		return errors.New("wfe: nil state")
	}
	st, ok := stepRecord(state, stepID)
	if !ok {
		return fmt.Errorf("wfe: the step %q has no record: there is no output to write", stepID)
	}
	out := st.Outcome
	switch key {
	case outputStatus:
		status, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("wfe: %q is not a status code", value)
		}
		out.Status = status
		out.Success = status >= 200 && status < 300
	case outputStatusText:
		out.StatusText = value
	case outputSuccess:
		return fmt.Errorf("wfe: the success of a step follows from its status: write the status instead")
	default:
		if out.Data == nil {
			out.Data = map[string]string{}
		}
		out.Data[key] = value
	}
	for i := range state.Steps {
		if state.Steps[i].ID != stepID {
			continue
		}
		state.Steps[i].Outcome = out
		reviveSteps(state, wf)
		return nil
	}
	return nil
}

// reviveSteps drops the records of the steps whose condition holds
// now.
//
// A step whose condition did not hold was passed over when the plan
// looked at it. An output that changes what the condition reads
// unblocks it: the step runs again.
func reviveSteps(state *RunState, wf *Workflow) {
	if wf == nil {
		return
	}
	ec := buildExprCtx(state)
	// A fresh slice: the run state is marshalled outside the run's
	// lock, so the old one must not be written to.
	kept := make([]StepState, 0, len(state.Steps))
	for _, st := range state.Steps {
		if st.Skipped && st.Outcome.StatusText == conditionDidNotHold {
			step := StepByID(wf, st.ID)
			if step != nil && step.If != "" {
				ok, err := EvalCond(step.If, ec)
				if err == nil && ok {
					// The condition holds now: the step runs.
					continue
				}
			}
		}
		kept = append(kept, st)
	}
	state.Steps = kept
}
