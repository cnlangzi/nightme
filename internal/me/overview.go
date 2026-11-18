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
	"unicode/utf8"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/pkg/wfe"
)

// maxStepDetail is how long the detail of a task row is.
//
// A step that reports a whole build log as its summary would
// otherwise make the row unreadable.
const maxStepDetail = 100

// outcomeKeyBranch is the Data key the branch of a step rides in.
//
// The branch is in the status bar of the card: a detail that repeats
// it spends its budget on a fact the reader already has.
const outcomeKeyBranch = "branch"

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

	rows := make([]arrowRow, 0, len(jobs)+4)
	if counts.shared {
		rows = append(rows, arrowRow{key: "rounds:", value: roundsCount(counts.count)})
	}
	rows = append(rows, arrowRow{key: "jobs:", value: jobsValue(wf, jobs)})
	rows = append(rows, arrowRow{key: "started:", value: clockTime(started)})
	if len(names) > 0 {
		rows = append(rows, arrowRow{key: "args:", value: argsValue(names, values)})
	}
	for _, name := range jobs {
		rows = append(rows, arrowRow{key: name + ":", value: jobFlow(wf, name, counts)})
	}

	lines := make([]string, 0, len(rows)+2)
	lines = append(lines, BotMark+id)
	lines = append(lines, workflowLine(wf))
	lines = append(lines, arrowRows(rows)...)
	return strings.Join(lines, "\n")
}

// workflowLine renders the workflow of the run: its name in bold and
// what it is for.
func workflowLine(wf *wfe.Workflow) string {
	line := "**" + wf.Name + "**"
	if wf.Title != "" {
		line += " — " + wf.Title
	}
	return line
}

// arrowRow is one row of a block: a key and the value of that key.
type arrowRow struct {
	key   string
	value string
}

// arrowRows renders the rows of a block as arrow rows.
//
// The key of a row is padded to the width of the longest key of the
// block, so the values of the block start at the same column.
func arrowRows(rows []arrowRow) []string {
	width := 0
	for _, row := range rows {
		if n := utf8.RuneCountInString(row.key); n > width {
			width = n
		}
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		pad := strings.Repeat(" ", width-utf8.RuneCountInString(row.key))
		out = append(out, "→ "+row.key+pad+" "+row.value)
	}
	return out
}

// jobsValue renders how many jobs the run has and how many steps
// those jobs hold.
func jobsValue(wf *wfe.Workflow, jobs []string) string {
	return fmt.Sprintf("%d · steps: %d", len(jobs), len(wf.Steps()))
}

// jobFlow renders the flow of a job: the ids of its steps, in the
// order the job runs them.
//
// A workflow whose jobs declare different round counts carries the
// count of the job on its own row.
func jobFlow(wf *wfe.Workflow, name string, counts roundCounts) string {
	steps := wf.Jobs[name].Steps
	ids := make([]string, 0, len(steps))
	for _, step := range steps {
		ids = append(ids, step.ID)
	}
	flow := "(no steps)"
	if len(ids) > 0 {
		flow = strings.Join(ids, " → ")
	}
	if !counts.shared {
		flow += " · " + roundsCount(wf.Jobs[name].Rounds())
	}
	return flow
}

// argsValue renders the arguments of the run: the name and the value
// of every argument, in the order the arguments were declared in.
func argsValue(names []string, values map[string]string) string {
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name+"="+values[name])
	}
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
	ends := roundEndTimes(history, wf)
	rows := []arrowRow{{key: "run:", value: id + " · " + clockTime(started)}}
	for _, job := range wf.JobOrder() {
		for _, round := range finishedRounds(state, wf, job) {
			value := job
			if at, ok := ends[roundKey{job: job, round: round}]; ok {
				value += " · " + clockTime(at)
			}
			rows = append(rows, arrowRow{key: fmt.Sprintf("round %d:", round), value: value})
		}
	}
	return strings.Join(arrowRows(rows), "\n")
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

// roundKey names one round of one job.
type roundKey struct {
	job   string
	round int
}

// roundEndTimes returns when each round of a job ended: the time of
// the last action the round took.
//
// The history is walked once: a run that repeats its rounds has a
// history that grows with every round, and the block is built again
// for every round the run enters.
//
// The history is persisted with the run, so the time of a round
// survives a restart of the daemon.
func roundEndTimes(history []wfe.ActionRecord, wf *wfe.Workflow) map[roundKey]time.Time {
	out := make(map[roundKey]time.Time, len(history))
	jobs := make(map[string]string, len(history))
	for _, rec := range history {
		if rec.StepID == "" {
			continue
		}
		job, known := jobs[rec.StepID]
		if !known {
			job = wfe.JobOf(wf, rec.StepID)
			jobs[rec.StepID] = job
		}
		if job == "" {
			continue
		}
		key := roundKey{job: job, round: recordRound(rec)}
		if at, seen := out[key]; !seen || rec.At.After(at) {
			out[key] = rec.At
		}
	}
	return out
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
//
// The states are the records of the round: the engine clears the
// records of a job when the job enters its next round, so the records
// that are left are the ones of the round. A record that names no
// round belongs to the round the run is in.
func (r *run) roundTasks(job string, round int) []agent.AgentTaskItem {
	r.mu.Lock()
	defer r.mu.Unlock()
	states := make(map[string]wfe.StepState, len(r.state.Steps))
	for _, st := range r.state.Steps {
		if st.Round != 0 && st.Round != round {
			continue
		}
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
		item := agent.AgentTaskItem{ID: step.ID, Subject: step.ID, Status: agent.TaskPending}
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

// stepDetail renders the detail of a step's row: what the step came
// to and what it may still do.
//
// The detail carries facts only: the marker of the row already says
// what state the step is in, so the detail does not say it a second
// time. The branch the step worked on is not in the detail either —
// the status bar of the card carries it.
//
// The detail is cut short: a step that reports a whole build log as
// its summary would otherwise make the row unreadable.
func stepDetail(st wfe.StepState, status agent.AgentTaskStatus, chances int, hasChances bool) string {
	parts := make([]string, 0, 3)
	if status != agent.TaskSkipped {
		if summary := branchlessSummary(st.Outcome); summary != "" {
			parts = append(parts, summary)
		}
		if errText := st.Outcome.Data[wfe.OutcomeKeyError]; errText != "" {
			parts = append(parts, errText)
		}
	}
	// The chances matter for the steps that may still run: a step that
	// is over is over, whatever its budget was.
	if hasChances && chances > 0 && (status == agent.TaskInProgress || status == agent.TaskFailed) {
		parts = append(parts, countText(chances, "chance")+" left")
	}
	return clipLine(strings.Join(parts, " · "), maxStepDetail)
}

// branchlessSummary renders the summary of an outcome without the
// branch it names.
//
// The summary of a step that worked on a branch repeats that branch at
// its end. The branch is in the status bar of the card, so the suffix
// is dropped — a summary that does not end with the branch is left
// alone.
func branchlessSummary(out wfe.Outcome) string {
	summary := strings.TrimSpace(out.Summary)
	branch := strings.TrimSpace(out.Data[outcomeKeyBranch])
	if branch == "" {
		return summary
	}
	suffix := " on " + branch
	if !strings.HasSuffix(summary, suffix) {
		return summary
	}
	return strings.TrimSpace(strings.TrimSuffix(summary, suffix))
}
