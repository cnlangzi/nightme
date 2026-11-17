package command

import (
	"maps"

	"github.com/cnlangzi/nightme/internal/messages"
)

// The builders below are the shared vocabulary of the structured
// outcome a command reports (see docs/me.md §1 for the full
// contract). A command that decides a result reports it next to the
// reply that shows the same decision to the user, so a program that
// drives nightme never has to read the card.
//
// The reason code is a stable string: a program reacts to it without
// parsing anything. The error description is the prose beside it —
// what broke, in words. Its presence is the axis the two kinds of
// failure sit on: an outcome that carries one is an exception,
// an outcome without one is a normal branch.

// outcomeKeyError is the Data key the error description rides in.
//
// The key belongs to the constructors: the exception ones fill it, the
// others never leave one behind.
const outcomeKeyError = "error"

// fallbackErrorDescription is the last link of the description chain:
// an exception with no summary and no reason code still says that
// something broke.
const fallbackErrorDescription = "the command failed"

// FailedOutcome builds the outcome of an exception — a path that
// broke.
//
// The error description says what broke: the caller's own description
// when it gives one (the first non-empty one), else the summary, else
// the reason code. It is never empty, so a consumer tells an
// exception from a normal branch by the key alone.
func FailedOutcome(reason, summary string, desc ...string) messages.Outcome {
	return failedOutcome(reason, summary, nil, desc)
}

// FailedOutcomeData is FailedOutcome with extra machine-readable
// facts (the agent that was asked, the branch that was pushed).
func FailedOutcomeData(reason, summary string, data map[string]string, desc ...string) messages.Outcome {
	return failedOutcome(reason, summary, data, desc)
}

// BranchOutcome builds the outcome of a normal branch — the request
// was valid and there was nothing to do.
//
// The status stays `failed`: the work that was asked for did not
// happen. What separates it from an exception is the missing error
// description — the answer is "there was nothing to do", not "it
// broke".
func BranchOutcome(reason, summary string, data map[string]string) messages.Outcome {
	o := baseFailedOutcome(reason, summary, data)
	delete(o.Data, outcomeKeyError)
	return o
}

// NoopOutcome builds the outcome of a path that had nothing to do —
// the request was valid but the world already satisfied it.
func NoopOutcome(summary string, data map[string]string) messages.Outcome {
	o := messages.Outcome{
		Status:  messages.OutcomeNoop,
		Summary: summary,
		Data:    data,
	}
	if _, ok := data[outcomeKeyError]; ok {
		o.Data = maps.Clone(data)
		delete(o.Data, outcomeKeyError)
	}
	return o
}

// failedOutcome builds the outcome of a failure and fills the error
// description.
//
// The constructor owns the error key: the facts never carry one, so
// the description is always the one the caller asked for.
func failedOutcome(reason, summary string, data map[string]string, desc []string) messages.Outcome {
	o := baseFailedOutcome(reason, summary, data)
	o.Data[outcomeKeyError] = errorDescription(reason, summary, desc)
	return o
}

// baseFailedOutcome builds the outcome of a failure: the status, the
// summary, the reason code and the facts.
func baseFailedOutcome(reason, summary string, data map[string]string) messages.Outcome {
	o := messages.Outcome{
		Status:  messages.OutcomeFailed,
		Summary: summary,
		Data:    map[string]string{"reason": reason},
	}
	maps.Copy(o.Data, data)
	return o
}

// errorDescription picks the prose that says what broke.
//
// The caller's own description wins; then the summary — every failure
// site already writes the explanation; then the reason code, so the
// description is never empty.
func errorDescription(reason, summary string, desc []string) string {
	for _, d := range desc {
		if d != "" {
			return d
		}
	}
	if summary != "" {
		return summary
	}
	if reason != "" {
		return reason
	}
	return fallbackErrorDescription
}
