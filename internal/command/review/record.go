// record.go — the resume state of the last review in a chat, and
// the registry of the runs the user can abort.
//
// /review fans out one agent session per review group. A group can
// fail on its own, and the user is left with a merged text that
// marks the dead group inline. `/review --continue` picks that run
// back up: it reopens the sessions of the groups that reached the
// agent, sends them the text "continue", and lets the agent finish
// the work it had started. A group that never reached the agent has
// no conversation to continue, so it is re-run — a fresh session
// carries the prompt the group was asked to run.
//
// The state a continuation needs lives here: which agent ran the
// review, which workspace it ran in, and one entry per group
// (pattern, files, session id, prompt, error). It is held in the
// command factory's memory, keyed by chat id — a chat never resumes
// another chat's review, and nothing is written to disk.
//
// The registry below holds the runs that are in flight, so
// `/review --abort` can stop one of them (see abort.go).

package review

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// runHandle is one running review run the user can abort.
//
// The command layer holds no handle on the fan-out's agent sessions —
// they belong to agent.RunSession / agent.ResumeSession — so the run's
// context is the only thing an abort can reach. Cancelling it closes
// the underlying agent sessions and unwinds the fan-out.
type runHandle struct {
	// cancel cancels the run's context with the abort sentinel.
	cancel context.CancelCauseFunc
	// done is closed when the run's goroutine has finished — after
	// the record was written and the chat's running marker released.
	done chan struct{}
	// agent is the runner of this run.
	agent string
	// kind is what the run is: "review" or "continue".
	kind string
	// started is when the run began.
	started time.Time
	// abortable marks a run whose fan-out can still be stopped. It
	// goes false when the fan-out ends (the findings are in hand —
	// there is nothing left to stop) and when the run is aborted (a
	// second abort must not report the same run again).
	//
	// Guarded by the factory's mutex.
	abortable bool
}

// newRun creates a review run's context and registers the run.
//
// The run is registered BEFORE its goroutine starts, so an abort that
// arrives right after the command returned cannot race the
// registration.
//
// The context carries no deadline: the run ends when the chat
// session's context is cancelled or when the user aborts it.
func (f *Factory) newRun(chatID string, parent context.Context, runner, kind string) (context.Context, *runHandle) {
	ctx, cancel := context.WithCancelCause(parent)
	h := &runHandle{
		cancel:  cancel,
		done:    make(chan struct{}),
		agent:   runner,
		kind:    kind,
		started: time.Now(),
	}
	f.beginRun(chatID, h)
	return ctx, h
}

// beginRun registers a running review run of the chat.
//
// The run is registered BEFORE its goroutine starts, so an abort that
// arrives right after the command returned cannot race the
// registration.
func (f *Factory) beginRun(chatID string, h *runHandle) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.runs == nil {
		f.runs = make(map[string][]*runHandle)
	}
	h.abortable = true
	f.runs[chatID] = append(f.runs[chatID], h)
}

// finishFanout marks a run's fan-out as over.
//
// The groups' sessions are closed and the findings are in hand: there
// is nothing left for an abort to stop, and the delivery that follows
// must not be cancelled by one. The run stays registered until its
// goroutine ends — the reply's "still running" count is about the
// runs in flight, not about the ones that can be stopped.
func (f *Factory) finishFanout(chatID string, h *runHandle) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cur, ok := f.findRun(chatID, h); ok {
		cur.abortable = false
	}
}

// findRun returns the chat's registered run h, if it is still there.
//
// The caller holds the factory's mutex.
func (f *Factory) findRun(chatID string, h *runHandle) (*runHandle, bool) {
	for _, r := range f.runs[chatID] {
		if r == h {
			return r, true
		}
	}
	return nil, false
}

// endRun removes a finished run from the chat's registry.
func (f *Factory) endRun(chatID string, h *runHandle) {
	f.mu.Lock()
	defer f.mu.Unlock()
	runs := f.runs[chatID]
	for i, r := range runs {
		if r != h {
			continue
		}
		runs = slices.Delete(runs, i, i+1)
		if len(runs) == 0 {
			delete(f.runs, chatID)
			return
		}
		f.runs[chatID] = runs
		return
	}
}

// abortRun cancels the newest abortable run of the chat with the
// abort sentinel and reports it, together with the number of the
// chat's other runs that are still in flight.
//
// The newest run is the one the user means — the chat's resume state
// is per chat, and the newest run is the one whose groups a
// `--continue` would pick up. ok is false when the chat has no run
// whose fan-out can still be stopped.
//
// The run is marked before it is cancelled, so a second abort does not
// report the same run again.
func (f *Factory) abortRun(chatID string) (h *runHandle, others int, ok bool) {
	f.mu.Lock()
	runs := f.runs[chatID]
	idx := -1
	for i := len(runs) - 1; i >= 0; i-- {
		if runs[i].abortable {
			idx = i
			break
		}
	}
	if idx < 0 {
		f.mu.Unlock()
		return nil, 0, false
	}
	h = runs[idx]
	h.abortable = false
	others = len(runs) - 1
	f.mu.Unlock()
	h.cancel(errReviewAborted)
	return h, others, true
}

// reviewOutcome is one review group's entry in a record.
type reviewOutcome struct {
	// pattern is the group's pattern — the glob of an ocr rule group,
	// or the builtin / simplify sentinel.
	pattern string
	// source is the rule's source — the area the group came from.
	// Together with the pattern it is the group's identity, so a
	// continuation names the resumed group the way the review did.
	source string
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
	// prompt is the group's assembled prompt — what a re-run sends.
	prompt string
	// report is the parsed form of the group's own review output.
	// The verdict line counts its findings; a group whose output
	// matched no known shape is reported as unparsed.
	report agent.ReviewReport
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

// resumable returns the groups a continuation can finish: the ones
// that failed AND still have their recovery attempt.
//
// There are two ways to finish a group. A group that reached the
// agent is resumed — its session id rides along on the job. A group
// that never reached the agent has no conversation to continue, so it
// is re-run: its stored prompt rides along on the job instead. A
// group with neither is not actionable — there is nothing the
// continuation could do with it.
//
// A takeover re-runs every group with the prompt it was asked to run:
// the session belongs to the agent that ran the group, and a session
// cannot be moved to another agent.
//
// The groups that completed are not re-asked — their findings already
// reached the chat, and asking again would duplicate them.
//
// why is the user-facing reason there is nothing to resume; empty
// when there is work to do.
func (r reviewRecord) resumable(takeover bool) ([]agent.ReviewJob, string) {
	var jobs []agent.ReviewJob
	failed := false
	spent := false
	for _, o := range r.outcomes {
		if o.err == nil {
			continue
		}
		failed = true
		switch {
		case o.spent:
			// The group's one recovery attempt is used up — it is
			// not retried, and neither "never reached the agent" nor
			// "already retried" describes it alone.
			spent = true
		case takeover:
			// The session belongs to the agent that ran the group:
			// the takeover re-runs it with the prompt it was asked
			// to run.
			if o.prompt != "" {
				jobs = append(jobs, agent.ReviewJob{
					Pattern: o.pattern,
					Source:  o.source,
					Files:   o.files,
					Prompt:  o.prompt,
				})
			}
		case o.sessionID != "":
			jobs = append(jobs, agent.ReviewJob{
				Pattern:   o.pattern,
				Source:    o.source,
				Files:     o.files,
				SessionID: o.sessionID,
			})
		case o.prompt != "":
			jobs = append(jobs, agent.ReviewJob{
				Pattern: o.pattern,
				Source:  o.source,
				Files:   o.files,
				Prompt:  o.prompt,
			})
		}
	}
	switch {
	case len(jobs) > 0:
		return jobs, ""
	case !failed:
		return nil, "ℹ️ the last review completed — nothing to continue"
	case spent:
		return nil, "ℹ️ the unfinished group was already retried — nothing to resume"
	default:
		// The only remaining case: the group failed before it
		// reached the agent, and no prompt was kept for it.
		return nil, "ℹ️ the unfinished group has no session and no prompt to re-run — nothing to resume"
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
// reports through agent.WithReviewJobObserver, plus the plan the
// fan-out reported through agent.WithReviewPlanObserver.
//
// The observer runs on each group's own goroutine, so the collector
// guards its slice with a mutex.
type outcomeCollector struct {
	mu         sync.Mutex
	outcomes   []reviewOutcome
	planGroups []agent.ReviewPlanGroup
}

// observe records one group's outcome. It is the
// agent.WithReviewJobObserver callback.
func (c *outcomeCollector) observe(job agent.ReviewJob, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.outcomes = append(c.outcomes, reviewOutcome{
		pattern:   job.Pattern,
		source:    job.Source,
		files:     job.Files,
		sessionID: job.SessionID,
		err:       err,
		prompt:    job.Prompt,
		report:    job.Report,
	})
}

// logFailedGroups wraps the outcome observer so every failed group is
// logged as it happens: the group's name, the agent that ran it, the
// error that ended it and whether the group reached the agent.
//
// The failure reason goes to the log, not to the chat: it is a long
// string that would break the one-line verdict.
func logFailedGroups(scope, runner string, next func(agent.ReviewJob, error)) func(agent.ReviewJob, error) {
	return func(job agent.ReviewJob, err error) {
		if err != nil {
			slog.Default().Warn(scope+": group failed",
				"agent", runner,
				"group", agent.ReviewGroupName(job.Source, job.Pattern),
				"reached_agent", job.SessionID != "",
				"err", err,
			)
		}
		next(job, err)
	}
}

// setPlan records the plan of the fan-out. It is the
// agent.WithReviewPlanObserver callback.
func (c *outcomeCollector) setPlan(groups []agent.ReviewPlanGroup) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.planGroups = append([]agent.ReviewPlanGroup(nil), groups...)
}

// plan returns the recorded plan — the group list the fan-out
// reported before it started.
func (c *outcomeCollector) plan() []agent.ReviewPlanGroup {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]agent.ReviewPlanGroup(nil), c.planGroups...)
}

// results returns the collected outcomes in the order they were
// reported.
func (c *outcomeCollector) results() []reviewOutcome {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]reviewOutcome(nil), c.outcomes...)
}

// spentResults returns the collected outcomes with the failed groups'
// recovery attempt marked as spent: they are not resumed or re-run a
// second time.
//
// A continuation that fails again is terminal for that group — the
// merged text already carries the inline "failed:" marker, and the
// group's one recovery attempt is used up. The session id stays in the
// record: the failure is still the group's, only the retry is gone.
//
// A timeout is the exception: the environment stalled, not the group's
// content, so the group keeps its recovery attempt and can be
// continued again.
func (c *outcomeCollector) spentResults() []reviewOutcome {
	out := c.results()
	for i := range out {
		if out[i].err != nil && !isGroupTimeout(out[i].err) {
			out[i].spent = true
		}
	}
	return out
}
