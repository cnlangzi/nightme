// overview.go — what the cards of a run carry.
//
// The card of the run is static: it says what the run is, how big it
// is and which jobs it has. The card of a round is live: it names the
// run the round belongs to, lists the rounds that have finished and
// shows the state of every step of the round.
package me

import (
	"fmt"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/wfe"
)

// maxStepDetail is how long the detail of a task row is.
//
// A step that reports a whole build log as its summary would
// otherwise make the row unreadable.
const maxStepDetail = 100

// runOverview renders the overview of the run: the run itself, the
// workflow it works through, the arguments it was given and the jobs
// it has.
//
// The overview is published once and never changed: the card answers
// "what is this run", and the live state of the work is on the card
// of the round.
func (r *run) runOverview() string {
	r.mu.Lock()
	id := r.id
	started := r.state.StartedAt
	names := append([]string(nil), r.argNames...)
	values := make(map[string]string, len(r.argVals))
	for name, value := range r.argVals {
		values[name] = value
	}
	r.mu.Unlock()

	wf := r.workflow
	jobs := wf.JobOrder()
	counts := sharedRounds(wf, jobs)
	lines := make([]string, 0, 8)
	lines = append(lines, BotMark+id)
	lines = append(lines, workflowLine(wf))
	lines = append(lines, factsLine(wf, jobs, counts, started))
	lines = append(lines, argLines(names, values)...)
	lines = append(lines, "jobs:")
	for _, name := range jobs {
		lines = append(lines, "  - "+jobLine(wf, name, counts))
		for _, step := range wf.Jobs[name].Steps {
			lines = append(lines, "     - "+step.ID)
		}
	}
	return strings.Join(lines, "\n")
}

// workflowLine renders the workflow of the run: its name in bold and
// what it is for.
func workflowLine(wf *wfe.Workflow) string {
	if wf == nil {
		return ""
	}
	line := "**" + wf.Name + "**"
	if wf.Title != "" {
		line += " — " + wf.Title
	}
	return line
}

// factsLine renders how big the run is: how many rounds it runs, how
// many jobs it has, how many steps those jobs hold and when the run
// started.
//
// The round count is the count every job of the workflow shares. A
// workflow whose jobs declare different counts carries no count here:
// each job carries its own count on its own line.
func factsLine(wf *wfe.Workflow, jobs []string, counts roundCounts, started time.Time) string {
	parts := make([]string, 0, 4)
	if counts.shared {
		parts = append(parts, roundsCount(counts.count))
	}
	parts = append(parts, countText(len(jobs), "job"))
	parts = append(parts, countText(len(wf.Steps()), "step"))
	parts = append(parts, "started "+clockTime(started))
	return strings.Join(parts, " · ")
}

// roundCounts holds the round count of the jobs of a workflow.
type roundCounts struct {
	// count is the count the jobs share.
	count int
	// shared reports whether every job declares the same count.
	shared bool
}

// sharedRounds returns the round count the jobs of the workflow
// share. The count is not shared when the jobs declare different
// counts.
func sharedRounds(wf *wfe.Workflow, jobs []string) roundCounts {
	if len(jobs) == 0 {
		return roundCounts{}
	}
	count := wf.Jobs[jobs[0]].Rounds()
	for _, name := range jobs[1:] {
		if wf.Jobs[name].Rounds() != count {
			return roundCounts{}
		}
	}
	return roundCounts{count: count, shared: true}
}

// jobLine renders the line of a job: its name and, when the jobs of
// the workflow declare different round counts, its own count.
func jobLine(wf *wfe.Workflow, name string, counts roundCounts) string {
	if counts.shared {
		return name
	}
	return name + " · " + roundsCount(wf.Jobs[name].Rounds())
}

// roundsCount renders how many rounds a job runs. Zero rounds means
// the job repeats until the run ends.
func roundsCount(rounds int) string {
	if rounds == 0 {
		return "until the run ends"
	}
	return countText(rounds, "round")
}

// countText renders a count with its unit: "1 job", "3 rounds".
func countText(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// argLines renders the arguments of the run: one list item per
// argument. A run without arguments has no list at all.
func argLines(names []string, values map[string]string) []string {
	if len(names) == 0 {
		return nil
	}
	lines := make([]string, 0, len(names)+1)
	lines = append(lines, "args:")
	for _, name := range names {
		lines = append(lines, " - "+name+": "+values[name])
	}
	return lines
}

// clockTime renders a moment as the time of day.
func clockTime(at time.Time) string {
	return at.Format("15:04:05")
}

// runBlock renders the block that names the run a round belongs to:
// the run's id and start time, and the rounds that have finished.
//
// The block is published when the round opens: the card of a round
// has to say which run it is part of, and the operator has to see the
// run's history at a glance.
func (r *run) runBlock() string {
	r.mu.Lock()
	id := r.id
	started := r.state.StartedAt
	state := r.state
	history := append([]wfe.ActionRecord(nil), r.policy.History...)
	r.mu.Unlock()

	wf := r.workflow
	lines := []string{"Run: " + id + " · " + clockTime(started)}
	for _, job := range wf.JobOrder() {
		for _, round := range finishedRounds(state, wf, job) {
			line := fmt.Sprintf("|- round %d: %s", round, job)
			if at, ok := roundEndTime(history, wf, job, round); ok {
				line += " · " + clockTime(at)
			}
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// finishedRounds returns the rounds of a job that have finished: the
// rounds before the round the job is in, plus the round it is in when
// every step of the job has run.
func finishedRounds(state *wfe.RunState, wf *wfe.Workflow, job string) []int {
	current := state.RoundOf(job)
	last := current - 1
	if wfe.JobComplete(state, wf, job) {
		last = current
	}
	if last < 1 {
		return nil
	}
	out := make([]int, 0, last)
	for round := 1; round <= last; round++ {
		out = append(out, round)
	}
	return out
}

// roundEndTime returns when a round of a job ended: the time of the
// last action the round took.
//
// The history is persisted with the run, so the time of a round
// survives a restart of the daemon.
func roundEndTime(history []wfe.ActionRecord, wf *wfe.Workflow, job string, round int) (time.Time, bool) {
	var at time.Time
	found := false
	for _, rec := range history {
		if rec.StepID == "" {
			continue
		}
		if wfe.JobOf(wf, rec.StepID) != job {
			continue
		}
		if recordRound(rec) != round {
			continue
		}
		if !found || rec.At.After(at) {
			at = rec.At
			found = true
		}
	}
	return at, found
}

// recordRound returns the round an action was taken in. An action
// without a round belongs to the first one.
func recordRound(rec wfe.ActionRecord) int {
	if rec.Round <= 0 {
		return 1
	}
	return rec.Round
}

// roundTasks renders the steps of a round as the checklist of the
// round's card.
//
// One row per step of the job: the round is one pass through the
// job's step list. Every row carries the state of its step and the
// detail of that state, so a step that failed never reads like a step
// that has not started.
func (r *run) roundTasks(job string, round int) []agent.AgentTaskItem {
	r.mu.Lock()
	defer r.mu.Unlock()
	states := make(map[string]wfe.StepState, len(r.state.Steps))
	for _, st := range r.state.Steps {
		states[st.ID] = st
	}
	remaining := r.policy.Remaining()
	var steps []wfe.Step
	for _, step := range r.workflow.Steps() {
		if wfe.JobOf(r.workflow, step.ID) == job {
			steps = append(steps, step)
		}
	}
	items := make([]agent.AgentTaskItem, 0, len(steps))
	for _, step := range steps {
		item := agent.AgentTaskItem{ID: step.ID, Subject: stepLabel(step), Status: agent.TaskPending}
		if st, ok := states[step.ID]; ok {
			item.Status = taskStatusOf(st)
			chances, hasChances := r.stepChances(step.ID, remaining)
			item.ActiveForm = stepDetail(st, item.Status, chances, hasChances)
		}
		items = append(items, item)
	}
	return items
}

// taskStatusOf maps the state of a step onto the state of its row.
//
// A step that has not started is pending, a step that is still running
// is in progress, a step that came to nothing is failed, and a step
// whose condition did not hold is skipped.
func taskStatusOf(st wfe.StepState) agent.AgentTaskStatus {
	switch {
	case st.Skipped:
		return agent.TaskSkipped
	case st.Pending:
		return agent.TaskInProgress
	case st.Outcome.Succeeded():
		return agent.TaskCompleted
	case st.Outcome.Status == wfe.OutcomeCancelled:
		return agent.TaskCancelled
	}
	return agent.TaskFailed
}

// stepChances returns how many chances the step has left in the
// current round. It reports false when the policy has no budget: the
// step may then run as often as the run needs.
func (r *run) stepChances(stepID string, remaining wfe.Remaining) (int, bool) {
	idx := r.stepIndex(stepID)
	if idx < 0 || idx >= len(remaining.Steps) {
		return 0, false
	}
	n := remaining.Steps[idx]
	if n < 0 {
		return 0, false
	}
	return n, true
}

// stepDetail renders the detail of a step's row: what the step is
// doing, what it came to and what it may still do.
//
// The detail is cut short: a step that reports a whole build log as
// its summary would otherwise make the row unreadable.
func stepDetail(st wfe.StepState, status agent.AgentTaskStatus, chances int, hasChances bool) string {
	parts := make([]string, 0, 3)
	switch status {
	case agent.TaskSkipped:
		parts = append(parts, "skipped")
	case agent.TaskInProgress:
		parts = append(parts, "running")
	default:
		word := string(st.Outcome.Status)
		if word == "" {
			word = string(wfe.OutcomeUnknown)
		}
		if st.Outcome.Summary != "" {
			word += ": " + st.Outcome.Summary
		}
		parts = append(parts, word)
		if errText := st.Outcome.Data[wfe.OutcomeKeyError]; errText != "" {
			parts = append(parts, errText)
		}
	}
	// The chances matter for the steps that may still run: a step that
	// is over is over, whatever its budget was.
	if hasChances && (status == agent.TaskInProgress || status == agent.TaskFailed) {
		parts = append(parts, countText(chances, "chance")+" left")
	}
	return clipLine(strings.Join(parts, " · "), maxStepDetail)
}
