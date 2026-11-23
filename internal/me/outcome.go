package me

import (
	"maps"
	"net/http"

	"github.com/cnlangzi/nightme/pkg/outcome"
)

// fallbackStatusText is the last link of the description chain: a
// failure with no words still says that something broke.
const fallbackStatusText = "the step failed"

// failedOutcome builds the outcome of a step that broke.
//
// The status says the step broke; the words say what broke. The facts
// the step produced are kept: they say what it did before it broke.
func failedOutcome(statusText string, data map[string]string) outcome.Outcome {
	if statusText == "" {
		statusText = fallbackStatusText
	}
	out := outcome.Outcome{
		Success:    false,
		Status:     http.StatusInternalServerError,
		StatusText: statusText,
	}
	if len(data) > 0 {
		out.Data = maps.Clone(data)
	}
	return out
}

// The words a skipped step carries: they name who skipped it.
const (
	// agentSkippedSummary is what a step the agent skipped carries.
	agentSkippedSummary = "the agent skipped this step"
	// operatorSkippedSummary is what a step the operator skipped carries.
	operatorSkippedSummary = "the operator skipped this step"
)

// skippedOutcome builds the outcome of a step that was not completed by
// the workflow's execution but whose plan moves past.
//
// The status is a success: the step did not break, it was passed over.
// A workflow's `${{ steps.<id>.outputs.status }}` reads it as such — a
// later step that is gated on this step having succeeded does not run.
//
// The facts the step produced are kept: they say what it did before it
// was skipped.
//
// The step's own record keeps its `Skipped` flag false: the step did
// run, it did not complete.
func skippedOutcome(out outcome.Outcome, statusText string) outcome.Outcome {
	skipped := outcome.Outcome{
		Success:    true,
		Status:     http.StatusOK,
		StatusText: statusText,
	}
	if len(out.Data) == 0 {
		return skipped
	}
	skipped.Data = maps.Clone(out.Data)
	return skipped
}

// noVerdict is the outcome of a command that reported none: the step
// is still running, or it was passed over or interrupted.
func noVerdict(statusText string) outcome.Outcome {
	return outcome.Outcome{StatusText: statusText}
}
