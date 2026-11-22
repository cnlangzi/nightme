// record.go — the review a chat is in the middle of, and the
// registry of the runs the user can abort.
//
// /review fans out one agent session per review group. A group can
// fail on its own. The review is not over when its fan-out ends: a
// failed group can still be picked up by `/review --continue`, which
// reopens the sessions of the groups that reached the agent, sends
// them the text "continue", and lets the agent finish the work it
// had started. A group that never reached the agent has no
// conversation to continue, so it is re-run — a fresh session
// carries the prompt the group was asked to run.
//
// So the findings of the groups that finished wait here until every
// group is in: the record holds what each group said, the findings
// that have not been delivered yet, and the state of every group
// (pattern, files, session id, prompt, error). The review is
// delivered once, when no group can be picked up any more — one
// document for the agent, one table for the card.
//
// The record is held in the command factory's memory, keyed by chat
// id — a chat never resumes another chat's review, and nothing is
// written to disk.
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

// reviewOutcome is one review group's state: the latest word about
// the group.
//
// The state does not carry what the group said — the record keeps
// that in the contributions, and the findings in the cache.
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
}

// reviewFinding is one finding in the record's cache, with the group
// that reported it.
type reviewFinding struct {
	// groupKey is the identity of the group that reported the
	// finding.
	groupKey string
	// finding is the finding itself.
	finding agent.ReviewFinding
}

// reviewContribution is what one group produced in one run: its own
// output and the findings that were new when it arrived.
type reviewContribution struct {
	// source is the rule's source — the area the group came from.
	source string
	// pattern is the group's pattern.
	pattern string
	// files are the files the group covered.
	files []string
	// text is the group's own output.
	text string
	// findings are the findings this contribution brought into the
	// cache — the ones the cache did not hold yet.
	findings []agent.ReviewFinding
	// parsed reports whether the group's output was readable.
	parsed bool
}

// reviewRecord is the review a chat is in the middle of: the runner,
// the workspace, the state of every group, what the groups said and
// the findings that have not been delivered yet.
type reviewRecord struct {
	agent     string
	workspace string
	// outcomes holds one entry per group, keyed by the group's
	// identity: the latest word about the group.
	outcomes []reviewOutcome
	// plan is the accumulated group list — the plan of every run,
	// merged by the group's identity.
	plan []agent.ReviewPlanGroup
	// findings are the findings that have not been delivered yet, in
	// the order they arrived. A finding the cache already holds is
	// not added twice.
	findings []reviewFinding
	// contributions holds what every run of every group produced —
	// one entry per successful group per run.
	contributions []reviewContribution
	// delivered reports whether the findings of this record have
	// been delivered to the chat's agent.
	delivered bool
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
// A takeover re-runs the groups it covers with the prompt each was
// asked to run: the session belongs to the agent that ran the group,
// and a session cannot be moved to another agent.
//
// A group whose recovery attempt is spent is out of the takeover as
// well: the recovery attempt is the operator's own retry, and a
// takeover neither spends it nor reaches past it.
//
// The groups that completed are not re-asked — their findings already
// reached the chat, and asking again would duplicate them.
//
// why is the user-facing reason there is nothing to resume; empty
// when there is work to do.
func (r reviewRecord) resumable(takeover bool) ([]agent.ReviewJob, string) {
	if r.delivered {
		// The review was delivered: every group is in and the
		// findings reached the agent. There is nothing left to pick
		// up.
		return nil, "ℹ️ the last review completed — nothing to continue"
	}
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
			// The session belongs to the agent that ran the group, so
			// the takeover re-runs the group with the prompt it was
			// asked to run. A group with no stored prompt is not
			// actionable here: there is no session to move and no
			// prompt to replay.
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
	case takeover:
		// The group has a session, but the session belongs to the
		// agent that ran it: there is no prompt to replay it on
		// another agent.
		return nil, "ℹ️ the unfinished group has no prompt to re-run on another agent — nothing to resume"
	default:
		// The only remaining case: the group failed before it
		// reached the agent, and no prompt was kept for it.
		return nil, "ℹ️ the unfinished group has no session and no prompt to re-run — nothing to resume"
	}
}

// carryPrompts fills the prompt of an outcome that has none from the
// record it continues.
//
// A group that was resumed keeps its session but loses its prompt:
// the resume sent the text "continue", not the prompt the group was
// asked to run. The prompt is the only way to run the group on
// another agent, so it rides along from the record it came from —
// without it, a takeover has nothing to replay.
func carryPrompts(outcomes []reviewOutcome, prev []reviewOutcome) {
	for i := range outcomes {
		if outcomes[i].prompt != "" {
			continue
		}
		for _, p := range prev {
			// The pattern and the source together are the group's
			// identity — the same pair the record keys them by.
			if p.pattern == outcomes[i].pattern && p.source == outcomes[i].source && p.prompt != "" {
				outcomes[i].prompt = p.prompt
				break
			}
		}
	}
}

// merge folds a run's results into the record.
//
// The record accumulates. A group's state is replaced by the newer
// word about it — a group that ran again and succeeded is a group
// that succeeded. The plan grows by the groups it did not name yet.
// What the groups said is kept: every successful group of every run
// adds a contribution, and the findings that are new add themselves
// to the cache.
func (r *reviewRecord) merge(run reviewRun) {
	if len(run.outcomes) > 0 {
		// The review is in a new round: whatever was delivered before
		// belongs to the round before it.
		r.delivered = false
	}
	r.plan = mergeReviewPlan(r.plan, run.plan)

	// The record travels by value: whoever holds a copy must not see
	// the merge write into the slices it shares with the record it
	// came from. The merge works on copies of its own.
	outcomes := slices.Clone(r.outcomes)
	findings := slices.Clone(r.findings)
	contributions := slices.Clone(r.contributions)

	for _, o := range run.outcomes {
		if i := groupIndex(outcomes, o.source, o.pattern); i >= 0 {
			outcomes[i] = o
		} else {
			outcomes = append(outcomes, o)
		}
	}
	for _, c := range run.contributions {
		var added []agent.ReviewFinding
		findings, added = addFindings(findings, groupKey(c.source, c.pattern), c.findings)
		c.findings = added
		contributions = append(contributions, c)
	}
	r.outcomes = outcomes
	r.findings = findings
	r.contributions = contributions
}

// indexOfGroup returns the position of the group's state in the
// record, or -1 when the record does not know the group yet.
func (r reviewRecord) indexOfGroup(source, pattern string) int {
	return groupIndex(r.outcomes, source, pattern)
}

// groupIndex returns the position of the group's state in the list,
// or -1 when the list does not know the group yet.
func groupIndex(outcomes []reviewOutcome, source, pattern string) int {
	for i, o := range outcomes {
		if o.source == source && o.pattern == pattern {
			return i
		}
	}
	return -1
}

// addFindings adds the findings the cache does not hold yet. It
// returns the new cache and the findings it took.
//
// A finding's identity is the group that reported it, the path and
// the content. The line and the severity are not part of it: the
// same problem reported again with a different line is the same
// problem. Two groups reporting the same path and content are two
// findings — each group's word is its own.
func addFindings(findings []reviewFinding, key string, add []agent.ReviewFinding) ([]reviewFinding, []agent.ReviewFinding) {
	var added []agent.ReviewFinding
	for _, f := range add {
		if holdsFinding(findings, key, f) {
			continue
		}
		findings = append(findings, reviewFinding{groupKey: key, finding: f})
		added = append(added, f)
	}
	return findings, added
}

// holdsFinding reports whether the cache already holds the finding.
func holdsFinding(findings []reviewFinding, key string, f agent.ReviewFinding) bool {
	for _, cached := range findings {
		if cached.groupKey != key {
			continue
		}
		if cached.finding.Path == f.Path && cached.finding.Content == f.Content {
			return true
		}
	}
	return false
}

// pending reports whether a group of this review can still be
// picked up by /review --continue.
//
// A failed group holds the review back while it can be recovered: it
// reached the agent (the continuation reopens its session) or it
// kept its prompt (the continuation re-runs it). A group whose
// recovery attempt is spent, and a group that reached neither the
// agent nor a stored prompt, are the end of the line — they no
// longer hold the review back.
func (r reviewRecord) pending() bool {
	for _, o := range r.outcomes {
		if o.err == nil || o.spent {
			continue
		}
		if o.sessionID != "" || o.prompt != "" {
			return true
		}
	}
	return false
}

// finishedGroups returns the number of groups that finished.
func (r reviewRecord) finishedGroups() int {
	n := 0
	for _, o := range r.outcomes {
		if o.err == nil {
			n++
		}
	}
	return n
}

// failedGroups returns the groups that did not finish, with the state
// the continuation could still act on.
//
// A failed group is counted as failed, never as unparsed.
func (r reviewRecord) failedGroups() []failedGroup {
	var failed []failedGroup
	for _, o := range r.outcomes {
		if o.err == nil {
			continue
		}
		failed = append(failed, failedGroup{
			label:    r.labelOf(o.source, o.pattern),
			state:    failedStateOf(o),
			timedOut: isGroupTimeout(o.err),
		})
	}
	return failed
}

// labelOf names a group: the label of the plan that named it, the
// name its identity gives otherwise.
func (r reviewRecord) labelOf(source, pattern string) string {
	for _, g := range r.plan {
		if g.Source != source || g.Pattern != pattern {
			continue
		}
		if g.Label != "" {
			return g.Label
		}
	}
	return agent.ReviewGroupName(source, pattern)
}

// unparsedGroups returns the number of groups whose output could not
// be read: a group is unparsed when none of its contributions could
// be read.
func (r reviewRecord) unparsedGroups() int {
	n := 0
	for _, o := range r.outcomes {
		if o.err != nil {
			continue
		}
		contributions := 0
		readable := false
		for _, c := range r.contributions {
			if c.source != o.source || c.pattern != o.pattern {
				continue
			}
			contributions++
			if c.parsed {
				readable = true
			}
		}
		if contributions > 0 && !readable {
			n++
		}
	}
	return n
}

// groupOrder returns the identities of the record's groups in the
// order the merged document lists them: the plan's order first, the
// groups the plan did not name last.
func (r reviewRecord) groupOrder() []string {
	var keys []string
	seen := make(map[string]bool, len(r.outcomes))
	for _, g := range r.plan {
		key := groupKey(g.Source, g.Pattern)
		if seen[key] || r.indexOfGroup(g.Source, g.Pattern) < 0 {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
	}
	for _, o := range r.outcomes {
		key := groupKey(o.source, o.pattern)
		if seen[key] {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
	}
	return keys
}

// contributingTexts returns the outputs of a group that belong in
// the merged document: the group's first output, and every output
// after it that brought a finding the cache did not hold yet.
//
// An output that only repeats what the agent already heard would
// double the agent's input without adding anything.
func (r reviewRecord) contributingTexts(source, pattern string) []string {
	var texts []string
	first := true
	for _, c := range r.contributions {
		if c.source != source || c.pattern != pattern {
			continue
		}
		if !first && len(c.findings) == 0 {
			continue
		}
		first = false
		texts = append(texts, c.text)
	}
	return texts
}

// mergedDocument renders the review's merged document from the
// record: one section per group, the group's own output under its
// header.
func (r reviewRecord) mergedDocument() string {
	sections := make([]agent.ReviewGroupSection, 0, len(r.outcomes))
	for _, key := range r.groupOrder() {
		i := r.indexOfGroupByKey(key)
		if i < 0 {
			continue
		}
		o := r.outcomes[i]
		section := agent.ReviewGroupSection{
			Pattern: o.pattern,
			Files:   o.files,
			Err:     o.err,
		}
		if o.err == nil {
			section.Texts = r.contributingTexts(o.source, o.pattern)
		}
		sections = append(sections, section)
	}
	return agent.MergeReviewDocument(sections)
}

// indexOfGroupByKey returns the position of the group with the given
// identity in the record, or -1 when the record does not know it.
func (r reviewRecord) indexOfGroupByKey(key string) int {
	for i, o := range r.outcomes {
		if groupKey(o.source, o.pattern) == key {
			return i
		}
	}
	return -1
}

// mergeReviewPlan merges a run's plan into the accumulated plan.
//
// The groups are keyed by their identity: the first plan that named
// a group owns its label and its position.
func mergeReviewPlan(base, add []agent.ReviewPlanGroup) []agent.ReviewPlanGroup {
	if len(add) == 0 {
		return base
	}
	merged := append([]agent.ReviewPlanGroup(nil), base...)
	seen := make(map[string]bool, len(merged))
	for _, g := range merged {
		seen[groupKey(g.Source, g.Pattern)] = true
	}
	for _, g := range add {
		key := groupKey(g.Source, g.Pattern)
		if seen[key] {
			continue
		}
		seen[key] = true
		merged = append(merged, g)
	}
	return merged
}

// reviewRun is what a finished run hands to the record: the state of
// every group it ran, what the groups that finished said, and the
// plan that named them.
type reviewRun struct {
	// outcomes holds one entry per group the run ran.
	outcomes []reviewOutcome
	// contributions holds one entry per group that finished.
	contributions []reviewContribution
	// plan is the group list the run reported.
	plan []agent.ReviewPlanGroup
}

// record folds a run's results into the chat's review record and
// returns the merged record.
func (f *Factory) record(chatID, agentName, workspace string, run reviewRun) reviewRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.records == nil {
		f.records = make(map[string]reviewRecord)
	}
	rec := f.records[chatID]
	rec.agent = agentName
	rec.workspace = workspace
	rec.merge(run)
	f.records[chatID] = rec
	return rec
}

// lookup returns the chat's review record.
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
	mu            sync.Mutex
	outcomes      []reviewOutcome
	contributions []reviewContribution
	planGroups    []agent.ReviewPlanGroup
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
	})
	if err != nil {
		// A failed group produced no contribution: it has no findings
		// and nothing to say.
		return
	}
	c.contributions = append(c.contributions, reviewContribution{
		source:   job.Source,
		pattern:  job.Pattern,
		files:    job.Files,
		text:     job.Text,
		findings: job.Report.Findings,
		parsed:   job.Report.Parsed,
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

// said returns what the groups that finished said.
func (c *outcomeCollector) said() []reviewContribution {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]reviewContribution(nil), c.contributions...)
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
