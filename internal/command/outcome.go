package command

import (
	"net/http"

	"github.com/cnlangzi/nightme/pkg/outcome"
)

// The builders below are the shared vocabulary of the structured
// outcome a command reports (see docs/me.md §1 for the full
// contract). A command that decides a result reports it next to the
// reply that shows the same decision to the user, so a program that
// drives nightme never has to read the card.
//
// The status is an HTTP status: 2xx when the work got done or there
// was nothing to do, 4xx when the request or the world refused it,
// 5xx when something broke. The words are the one line a person
// reads, and the facts are the ones the command knows.

// SuccessOutcome builds the outcome of work that got done.
//
// There was nothing to do counts as done too: a request that was
// valid and that the world already satisfied is a 2xx, not a failure.
func SuccessOutcome(statusText string, data map[string]string) outcome.Outcome {
	return outcome.Outcome{
		Success:    true,
		Status:     http.StatusOK,
		StatusText: statusText,
		Data:       data,
	}
}

// FailedOutcome builds the outcome of work that broke.
//
// The status says which kind of breakage it was and the words say
// what broke.
func FailedOutcome(status int, statusText string) outcome.Outcome {
	return outcome.Outcome{
		Success:    false,
		Status:     status,
		StatusText: statusText,
	}
}

// FailedOutcomeData is FailedOutcome with the facts the command knows
// (the agent that was asked, the branch that was pushed).
func FailedOutcomeData(status int, statusText string, data map[string]string) outcome.Outcome {
	return outcome.Outcome{
		Success:    false,
		Status:     status,
		StatusText: statusText,
		Data:       data,
	}
}
