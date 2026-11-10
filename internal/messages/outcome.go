package messages

// OutcomeStatus is the machine-readable verdict of one unit of
// work — one slash command, one agent turn, one playbook step.
//
// A program that drives nightme (see internal/me) judges from
// these values instead of parsing the human-readable cards the
// channels render. The card text stays exactly as it is; this is
// the parallel machine channel.
type OutcomeStatus string

const (
	// OutcomeOK means the work completed as asked.
	OutcomeOK OutcomeStatus = "ok"
	// OutcomeNoop means there was nothing to do — the request was
	// valid but the world already satisfied it.
	OutcomeNoop OutcomeStatus = "noop"
	// OutcomeIncomplete means the work started and part of it
	// finished; the remainder did not.
	OutcomeIncomplete OutcomeStatus = "incomplete"
	// OutcomeFailed means the work did not complete.
	OutcomeFailed OutcomeStatus = "failed"
	// OutcomeCancelled means a user or an operator stopped it.
	OutcomeCancelled OutcomeStatus = "cancelled"
	// OutcomeTimeout means it ran out of time.
	OutcomeTimeout OutcomeStatus = "timeout"
	// OutcomeUnknown means nothing reported a verdict. It is the
	// zero value: a command that never reported leaves its message
	// in this state, and a merge only produces it when no side
	// reported anything either.
	OutcomeUnknown OutcomeStatus = "unknown"
)

// outcomeSeverity ranks a status for merge purposes. Higher wins.
//
//	failed > cancelled > timeout > incomplete > ok > noop > unknown
func outcomeSeverity(s OutcomeStatus) int {
	switch s {
	case OutcomeFailed:
		return 6
	case OutcomeCancelled:
		return 5
	case OutcomeTimeout:
		return 4
	case OutcomeIncomplete:
		return 3
	case OutcomeOK:
		return 2
	case OutcomeNoop:
		return 1
	default:
		return 0
	}
}

// Outcome is the structured result of one unit of work.
//
// Status is the primary evidence — a consumer branches on it.
// Summary is a one-line human-readable restatement. Data carries
// the machine-readable facts the producing command knows: a branch
// name, a commit range, a PR URL, the unfinished review groups.
//
// Data is deliberately map[string]string rather than a struct:
// the set of facts differs per command, and the consumer reads the
// keys it cares about ("pr_url", "sha_range", "unfinished_groups").
type Outcome struct {
	Status  OutcomeStatus
	Summary string
	Data    map[string]string
}

// Merge combines two outcomes into one.
//
// The result carries the more severe status. "unknown" only
// survives when neither side reported anything. Data is merged with
// later values winning.
//
// The summary comes from the outcome that supplied the winning
// status; on a tie the earlier one wins, so a command's specific
// summary is not replaced by a later, more generic one. When the
// winner carries no summary the other side's is used.
func Merge(a, b Outcome) Outcome {
	if a.Status == "" {
		a.Status = OutcomeUnknown
	}
	if b.Status == "" {
		b.Status = OutcomeUnknown
	}

	winner, loser := a, b
	if outcomeSeverity(b.Status) > outcomeSeverity(a.Status) {
		winner, loser = b, a
	}

	out := Outcome{Status: winner.Status, Summary: winner.Summary}
	if out.Summary == "" {
		out.Summary = loser.Summary
	}

	if len(a.Data) > 0 || len(b.Data) > 0 {
		out.Data = make(map[string]string, len(a.Data)+len(b.Data))
		for k, v := range a.Data {
			out.Data[k] = v
		}
		for k, v := range b.Data {
			out.Data[k] = v
		}
	}
	return out
}

// IsReported reports whether the outcome carries an actual verdict
// (anything other than unknown).
func (o Outcome) IsReported() bool {
	return o.Status != "" && o.Status != OutcomeUnknown
}
