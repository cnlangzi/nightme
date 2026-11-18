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

// doneSummary is what a step the agent declared done carries.
const doneSummary = "the agent decided the step was done"

// doneOutcome builds the outcome of a step the agent declared done.
//
// The facts the step produced are kept: they say what it did before it
// broke. The error description goes: the step is done, so the flow can
// go on by itself.
func doneOutcome(out wfe.Outcome) wfe.Outcome {
	done := wfe.Outcome{
		Status:  wfe.OutcomeOK,
		Summary: doneSummary,
	}
	if len(out.Data) == 0 {
		return done
	}
	done.Data = make(map[string]string, len(out.Data))
	maps.Copy(done.Data, out.Data)
	delete(done.Data, wfe.OutcomeKeyError)
	return done
}
