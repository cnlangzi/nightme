package command

import (
	"github.com/cnlangzi/nightme/internal/messages"
)

// The builders below are the shared vocabulary of the structured
// outcome a command reports (see docs/me.md §1 for the full
// contract). A command that decides a result reports it next to the
// reply that shows the same decision to the user, so a program that
// drives nightme never has to read the card.
//
// The reason code is a stable string: a program reacts to it without
// parsing anything.

// FailedOutcome builds the outcome of a path that could not do what
// was asked.
func FailedOutcome(reason, summary string) messages.Outcome {
	return messages.Outcome{
		Status:  messages.OutcomeFailed,
		Summary: summary,
		Data:    map[string]string{"reason": reason},
	}
}

// FailedOutcomeData is FailedOutcome with extra machine-readable
// facts (the agent that was asked, the branch that was pushed).
func FailedOutcomeData(reason, summary string, data map[string]string) messages.Outcome {
	o := FailedOutcome(reason, summary)
	for k, v := range data {
		o.Data[k] = v
	}
	return o
}

// NoopOutcome builds the outcome of a path that had nothing to do —
// the request was valid but the world already satisfied it.
func NoopOutcome(summary string, data map[string]string) messages.Outcome {
	return messages.Outcome{
		Status:  messages.OutcomeNoop,
		Summary: summary,
		Data:    data,
	}
}
