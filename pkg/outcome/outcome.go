// Package outcome is the contract for the structured result of one
// unit of work: one slash command, one agent turn, one workflow step.
//
// The contract is one type and one set of facts. A program that
// drives nightme branches on the status and reads the facts; it
// never parses the text a channel rendered.
package outcome

import "maps"

// StatusClientClosedRequest is the status of work whose client went
// away before it finished.
//
// It is nginx's private code — net/http has no constant for it — and
// it is the status a turn that the operator stopped carries.
const StatusClientClosedRequest = 499

// Outcome is the structured result of one unit of work.
//
// Success says the work got done — or that there was nothing to do,
// which counts as done too. Status carries the detail: a successful
// outcome is a 2xx, a failed one is a 4xx or a 5xx. StatusText is the
// one line of words that says what happened, and Data carries the
// facts the producing command knows.
//
// Data is deliberately map[string]string rather than a struct: the
// set of facts differs per command, and the consumer reads the keys
// it cares about ("issue", "branch", "pr_number", "unfinished").
type Outcome struct {
	// Success says the outcome was a success: the work got done, or
	// there was nothing to do.
	Success bool `json:"success"`
	// Status is the HTTP status of the outcome. Zero when the work
	// carries no HTTP status.
	Status int `json:"status"`
	// StatusText says what happened, in one line of words.
	StatusText string `json:"statusText,omitempty"`
	// Data carries the facts the producing command knows.
	Data map[string]string `json:"data,omitempty"`
}

// Succeeded reports whether the work got done.
func (o Outcome) Succeeded() bool {
	return o.Success
}

// Exception reports whether the outcome is an exception: the flow
// cannot go on by itself and whoever drives it has to decide what
// happens next.
//
// A 2xx is a normal branch; a 4xx or a 5xx is an exception.
func (o Outcome) Exception() bool {
	return !o.Success
}

// Reported reports whether the outcome carries a verdict at all.
//
// The zero outcome says nothing — no status, no words, no facts. It
// is what a message nobody reported on settles with.
func (o Outcome) Reported() bool {
	return o.Status != 0 || o.StatusText != "" || len(o.Data) > 0
}

// Merge combines two outcomes into one.
//
// The failing side wins: a command that reports "there was nothing
// to do" does not hide a turn that died on an upstream 429. Two
// failures keep the earlier one, so a command's own conclusion
// survives a later, more generic one.
//
// A side with no HTTP status carries no verdict of its own: it says
// what it knows — the facts — and nothing about how the work went.
// It never wins over a side that has a status, so the facts of a
// work that was stopped before it could be judged ride along without
// deciding how the work went.
//
// An outcome that carries no verdict at all loses to one that does:
// saying nothing is not a failure.
//
// The facts are merged key by key, the later value winning. The
// words come from the winner; the loser's words fill in when the
// winner has none.
func Merge(a, b Outcome) Outcome {
	switch {
	case !a.Reported():
		return clone(b)
	case !b.Reported():
		return clone(a)
	}
	winner, loser := a, b
	switch {
	case a.Status == 0 && b.Status != 0:
		winner, loser = b, a
	case b.Status == 0 && a.Status != 0:
		winner, loser = a, b
	case a.Success && !b.Success:
		winner, loser = b, a
	}
	out := Outcome{
		Success:    winner.Success,
		Status:     winner.Status,
		StatusText: winner.StatusText,
	}
	if out.StatusText == "" {
		out.StatusText = loser.StatusText
	}
	if len(a.Data) > 0 || len(b.Data) > 0 {
		out.Data = make(map[string]string, len(a.Data)+len(b.Data))
		maps.Copy(out.Data, a.Data)
		maps.Copy(out.Data, b.Data)
	}
	return out
}

// clone copies an outcome so that the merged one never shares the
// facts map of the outcome it came from.
func clone(o Outcome) Outcome {
	out := Outcome{
		Success:    o.Success,
		Status:     o.Status,
		StatusText: o.StatusText,
	}
	if len(o.Data) > 0 {
		out.Data = maps.Clone(o.Data)
	}
	return out
}
