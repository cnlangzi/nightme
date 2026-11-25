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

// noVerdict is the outcome of a command that reported none: the step
// is still running, or it was passed over or interrupted.
func noVerdict(statusText string) outcome.Outcome {
	return outcome.Outcome{StatusText: statusText}
}
