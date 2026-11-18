package me

import (
	"maps"

	"github.com/cnlangzi/nightme/pkg/wfe"
)

// fallbackErrorDescription is the last link of the description chain:
// an exception with no summary still says that something broke.
const fallbackErrorDescription = "the step failed"

// failedOutcome builds the outcome of a step that broke.
//
// The error description says what broke, and it is what tells the plan
// that the flow cannot go on by itself: an outcome without one reads
// as a normal result, and the plan goes on with the next step. The
// facts the step produced are kept: they say what it did before it
// broke.
func failedOutcome(summary string, data map[string]string) wfe.Outcome {
	if summary == "" {
		summary = fallbackErrorDescription
	}
	out := wfe.Outcome{
		Status:  wfe.OutcomeFailed,
		Summary: summary,
		Data:    make(map[string]string, len(data)+1),
	}
	maps.Copy(out.Data, data)
	out.Data[wfe.OutcomeKeyError] = summary
	return out
}

// The summaries a skipped step carries: they name who skipped it.
const (
	// agentSkippedSummary is what a step the agent skipped carries.
	agentSkippedSummary = "the agent skipped this step"
	// operatorSkippedSummary is what a step the operator skipped carries.
	operatorSkippedSummary = "the operator skipped this step"
)

// skippedOutcome builds the outcome of a step that was not completed by
// the workflow's execution but whose plan moves past.
//
// The status is `noop`: the step did not complete, and a workflow's
// `${{ steps.<id>.outputs.status }}` reads it as such — a later step
// that is gated on this step having succeeded does not run.
//
// The facts the step produced are kept: they say what it did before it
// was skipped. The error description goes: the step did not break, it
// was passed over.
//
// The step's own record keeps its `Skipped` flag false: the step did
// run, it did not complete.
func skippedOutcome(out wfe.Outcome, summary string) wfe.Outcome {
	skipped := wfe.Outcome{
		Status:  wfe.OutcomeNoop,
		Summary: summary,
	}
	if len(out.Data) == 0 {
		return skipped
	}
	skipped.Data = make(map[string]string, len(out.Data))
	maps.Copy(skipped.Data, out.Data)
	delete(skipped.Data, wfe.OutcomeKeyError)
	return skipped
}
