// record.go — the resume state of the last review in a chat.
//
// /review fans out one agent session per review group. A group can
// fail on its own, and the user is left with a merged text that
// marks the dead group inline. `/review --continue` picks that run
// back up: it reopens the sessions of the groups that did not
// complete, sends them the text "continue", and lets the agent
// finish the work it had started.
//
// The state a continuation needs lives here: which agent ran the
// review, which workspace it ran in, and one entry per group
// (pattern, files, session id, error). It is held in the command
// factory's memory, keyed by chat id — a chat never resumes another
// chat's review, and nothing is written to disk.

package review

import (
	"sync"

	"github.com/cnlangzi/nightme/internal/agent"
)

// reviewOutcome is one review group's entry in a record.
type reviewOutcome struct {
	// pattern is the group's pattern — the glob of an ocr rule group,
	// or the builtin / simplify sentinel.
	pattern string
	// files are the files the group covered.
	files []string
	// sessionID is the agent session id the group's session ran
	// under — the id a continuation resumes. Empty when the group
	// never reached the agent.
	sessionID string
	// err is the error that ended the group, nil on success.
	err error
	// spent marks a group whose one resume attempt is used up: a
	// continuation already retried it and it failed again. The group
	// is not resumed a second time.
	spent bool
}

// reviewRecord is the resume state of the last review run in one
// chat: the runner, the workspace, and one entry per group.
type reviewRecord struct {
	// id identifies this record. A run that resumes a record only
	// replaces it when it is still the current one — a newer run
	// that finished in the meantime owns the chat's resume state.
	id        uint64
	agent     string
	workspace string
	outcomes  []reviewOutcome
}

// resumable returns the groups a continuation can resume: the ones
// that failed AND produced a session id AND still have their resume
// attempt.
//
// The groups that completed are not re-asked — their findings already
// reached the chat, and asking again would duplicate them.
//
// why is the user-facing reason there is nothing to resume; empty
// when there is work to do.
func (r reviewRecord) resumable() ([]agent.ReviewJob, string) {
	var jobs []agent.ReviewJob
	failed := false
	neverReached := false
	for _, o := range r.outcomes {
		if o.err == nil {
			continue
		}
		failed = true
		switch {
		case o.spent:
			// The group's one resume attempt is used up — it is not
			// retried, and "never reached the agent" does not
			// describe it either.
		case o.sessionID == "":
			neverReached = true
		default:
			jobs = append(jobs, agent.ReviewJob{
				Pattern:   o.pattern,
				Files:     o.files,
				SessionID: o.sessionID,
			})
		}
	}
	switch {
	case len(jobs) > 0:
		return jobs, ""
	case !failed:
		return nil, "ℹ️ the last review completed — nothing to continue"
	case neverReached:
		return nil, "ℹ️ the unfinished group never reached the agent — nothing to resume"
	default:
		// The only remaining case: the group failed after reaching
		// the agent, and its one resume attempt is spent.
		return nil, "ℹ️ the unfinished group was already retried — nothing to resume"
	}
}

// record replaces the resume state of a chat.
func (f *Factory) record(chatID, agentName, workspace string, outcomes []reviewOutcome) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.records == nil {
		f.records = make(map[string]reviewRecord)
	}
	f.nextID++
	f.records[chatID] = reviewRecord{
		id:        f.nextID,
		agent:     agentName,
		workspace: workspace,
		outcomes:  outcomes,
	}
}

// recordIfCurrent replaces the resume state of a chat only when the
// chat's record is still the one the caller resumed from.
//
// A run that resumes a record must not overwrite a newer run's
// record: the newer run owns the chat's resume state, even when the
// older run finishes later.
func (f *Factory) recordIfCurrent(chatID string, id uint64, agentName, workspace string, outcomes []reviewOutcome) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cur, ok := f.records[chatID]; ok && cur.id != id {
		return
	}
	if f.records == nil {
		f.records = make(map[string]reviewRecord)
	}
	f.nextID++
	f.records[chatID] = reviewRecord{
		id:        f.nextID,
		agent:     agentName,
		workspace: workspace,
		outcomes:  outcomes,
	}
}

// lookup returns the resume state of a chat.
func (f *Factory) lookup(chatID string) (reviewRecord, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.records[chatID]
	return rec, ok
}

// startReview marks a review as running for the chat. Overlapping
// reviews are counted, so the marker clears only when the last one
// finishes.
func (f *Factory) startReview(chatID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running == nil {
		f.running = make(map[string]int)
	}
	f.running[chatID]++
}

// finishReview marks one running review of the chat as finished.
func (f *Factory) finishReview(chatID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.running[chatID]
	if n <= 1 {
		delete(f.running, chatID)
		return
	}
	f.running[chatID] = n - 1
}

// reviewRunning reports whether a review is running for the chat.
func (f *Factory) reviewRunning(chatID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running[chatID] > 0
}

// startContinuation marks the chat as running a continuation and
// reports whether the caller may proceed.
//
// The check and the mark happen under one lock: two concurrent
// --continue commands must not both pass the busy check and resume
// the same group sessions.
func (f *Factory) startContinuation(chatID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running[chatID] > 0 {
		return false
	}
	if f.running == nil {
		f.running = make(map[string]int)
	}
	f.running[chatID]++
	return true
}

// outcomeCollector collects the per-group outcomes a review fan-out
// reports through agent.WithReviewJobObserver.
//
// The observer runs on each group's own goroutine, so the collector
// guards its slice with a mutex.
type outcomeCollector struct {
	mu       sync.Mutex
	outcomes []reviewOutcome
}

// observe records one group's outcome. It is the
// agent.WithReviewJobObserver callback.
func (c *outcomeCollector) observe(job agent.ReviewJob, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.outcomes = append(c.outcomes, reviewOutcome{
		pattern:   job.Pattern,
		files:     job.Files,
		sessionID: job.SessionID,
		err:       err,
	})
}

// results returns the collected outcomes in the order they were
// reported.
func (c *outcomeCollector) results() []reviewOutcome {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]reviewOutcome(nil), c.outcomes...)
}

// spentResults returns the collected outcomes with the failed groups'
// resume attempt marked as spent: they are not resumed a second time.
//
// A continuation that fails again is terminal for that group — the
// merged text already carries the inline "failed:" marker, and the
// group's one resume attempt is used up. The session id stays in the
// record: the failure is still the group's, only the retry is gone.
func (c *outcomeCollector) spentResults() []reviewOutcome {
	out := c.results()
	for i := range out {
		if out[i].err != nil {
			out[i].spent = true
		}
	}
	return out
}
