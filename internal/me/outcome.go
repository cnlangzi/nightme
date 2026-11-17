package me

import (
	"maps"

	"github.com/cnlangzi/nightme/internal/wfe"
)

// outcomeKeyError is the key the error description rides in.
//
// It is the key the outcome contract names (see docs/me.md §1): the
// plan reads it to tell a normal branch from a breakage.
const outcomeKeyError = "error"

// failedOutcome builds the outcome of a step that broke.
//
// The error description says what broke, and it is what tells the plan
// that the flow cannot go on by itself: an outcome without one reads
// as a normal result, and the plan goes on with the next step. The
// facts the step produced are kept: they say what it did before it
// broke.
func failedOutcome(summary string, data map[string]string) wfe.Outcome {
	out := wfe.Outcome{
		Status:  wfe.OutcomeFailed,
		Summary: summary,
		Data:    make(map[string]string, len(data)+1),
	}
	maps.Copy(out.Data, data)
	out.Data[outcomeKeyError] = summary
	return out
}
