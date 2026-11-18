// overview.go — what the cards of a run carry.
//
// The card of the run is static: it says what the run is and what the
// plan of the run is. The card of a round carries the history of the
// run: the rounds that have finished, with their own facts, and the
// round that is going.
package me

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/pkg/wfe"
)

// The budgets of the cards.
//
// A card is read on a phone: a title that fills a line is a title
// nobody reads, and a plan with fifty nodes is a plan nobody scans.
// Every budget is one constant.
const (
	// maxCardTitle is how long the title of the workflow is.
	maxCardTitle = 120
	// maxStepID is how long the id of a step is.
	maxStepID = 15
	// maxStepCommand is how long the command of a step is.
	maxStepCommand = 40
	// maxPlanNodes is how many steps a job lists in the plan.
	maxPlanNodes = 24
	// maxTreeRounds is how many rounds the tree of the rounds lists.
	maxTreeRounds = 5
	// factColumn is the column the values of the facts start at.
	factColumn = 10
)

// maxStepDetail is how long the detail of a task row is.
//
// A step that reports a whole build log as its summary would
// otherwise make the row unreadable.
const maxStepDetail = 100

// The budgets of the phase line.
const (
	// maxPhaseQuestion is how long the question of the phase line is.
	maxPhaseQuestion = 100
	// maxPhaseLine is how long the phase line is.
	maxPhaseLine = 120
)

// phaseLine renders where the run stands: the first entry of the state
// card.
//
// The line is read from the run's own state, so every source that
// posts the card says the same thing and the card cannot disagree with
// the run about why it stopped.
func (r *run) phaseLine() string {
	r.mu.Lock()
	phase := r.phase
	reason := r.pauseReason
	question := r.question
	command := ""
	if r.inflight != nil {
		command = r.inflight.Command
	}
	pending := r.paused || r.pauseRequested
	r.mu.Unlock()

	switch {
	case pending:
		if reason == "" {
			return "paused"
		}
		return clipLine("paused: "+reason, maxPhaseLine)
	case phase == PhaseAsking:
		if question == "" {
			return "asking"
		}
		return clipLine("asking: "+clipLine(question, maxPhaseQuestion), maxPhaseLine)
	default:
		if command == "" {
			return "running"
		}
		return clipLine("running · step: "+command, maxPhaseLine)
	}
}

// outcomeKeyBranch is the Data key the branch of a step rides in.
//
// The card the operator read names the branch — the `/gtw` card of the
// step, and the status bar of the run card for the branch of the
// workspace — so a detail that repeats it spends its budget on a fact
// the reader already has.
const outcomeKeyBranch = "branch"

// runCard renders the card of the run: the headline, the facts of the
// run and the plan it works through.
//
// The card is published once and never changed: it answers "what is
// this run", and the live state of the work is on the card of the
// round.
func (r *run) runCard() string {
	r.mu.Lock()
	started := r.state.StartedAt
	names := append([]string(nil), r.argNames...)
	values := make(map[string]string, len(r.argVals))
	for name, value := range r.argVals {
		values[name] = value
	}
	r.mu.Unlock()

	wf := r.workflow
	lines := make([]string, 0, 16)
	lines = append(lines, "▶ [ "+wf.Name+" ] started")
	if title := cutText(wf.Title, maxCardTitle); title != "" {
		lines = append(lines, title)
	}
	lines = append(lines, "")
	lines = append(lines, runFacts(wf, started, names, values)...)
	lines = append(lines, "")
	lines = append(lines, runPlan(wf)...)
	return strings.Join(lines, "\n")
}

// runFacts renders the facts of the run: how big it is, when it
// started and what it was given.
//
// The values start at the same column: the key, its colon, and the
// padding to the column.
func runFacts(wf *wfe.Workflow, started time.Time, names []string, values map[string]string) []string {
	facts := []fact{
		{key: "jobs", value: strconv.Itoa(len(wf.JobOrder()))},
		{key: "steps", value: strconv.Itoa(len(wf.Steps()))},
		{key: "started", value: clockTime(started)},
	}
	if len(names) > 0 {
		facts = append(facts, fact{key: "args", value: argsValue(names, values)})
	}
	out := make([]string, 0, len(facts))
	for _, f := range facts {
		out = append(out, fmt.Sprintf("%-*s%s", factColumn, f.key+":", f.value))
	}
	return out
}

// fact is one row of the facts of a block: a key and the value of
// that key.
type fact struct {
	key   string
	value string
}

// argsValue renders the arguments of the run: the name and the value
// of every argument, in the order the run was given them.
func argsValue(names []string, values map[string]string) string {
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name+"="+values[name])
	}
	return strings.Join(parts, ", ")
}

// runPlan renders the plan of the run: one block per job, in the
// order the jobs run.
func runPlan(wf *wfe.Workflow) []string {
	jobs := wf.JobOrder()
	numbers := planNumbers(wf)
	lines := make([]string, 0, 32)
	for i, job := range jobs {
		if i > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, planJob(wf, job, numbers)...)
	}
	return lines
}

// planJob renders the block of one job: the header of the job and one
// node per step.
//
// A job with many steps lists its first steps and folds the rest into
// one node: the plan of a job is a plan, not every command the job may
// run.
func planJob(wf *wfe.Workflow, name string, numbers map[string]int) []string {
	steps := wf.Jobs[name].Steps
	shown := steps
	folded := 0
	if len(steps) > maxPlanNodes {
		shown = steps[:maxPlanNodes]
		folded = len(steps) - maxPlanNodes
	}
	width := planIDWidth(shown)
	lines := make([]string, 0, len(shown)+2)
	lines = append(lines, jobHeader(wf, name))
	for i, step := range shown {
		prefix := "├─ "
		if i == len(shown)-1 && folded == 0 {
			prefix = "└─ "
		}
		lines = append(lines, prefix+planNode(numbers[step.ID], step, width))
	}
	if folded > 0 {
		last := shown[len(shown)-1]
		lines = append(lines, fmt.Sprintf("└─ [ %d ]: … +%d more steps", numbers[last.ID]+1, folded))
	}
	return lines
}

// planNode renders one node of the plan: the number of the step, its
// id and the command it runs.
//
// The id is padded, so the commands of a job all start at the same
// column: the eye reads the commands, not the ids.
func planNode(number int, step wfe.Step, width int) string {
	return fmt.Sprintf("[ %d ]: %-*s%s", number, width, cutText(step.ID, maxStepID), stepCommand(step))
}

// planIDWidth returns the width of the id column of a job: the
// longest id of the job plus three spaces.
func planIDWidth(steps []wfe.Step) int {
	width := 0
	for _, step := range steps {
		if n := utf8.RuneCountInString(cutText(step.ID, maxStepID)); n > width {
			width = n
		}
	}
	return width + 3
}

// planNumbers returns the number of every step of the workflow.
//
// The steps are numbered across the jobs: the number of a step says
// where it stands in the whole plan.
func planNumbers(wf *wfe.Workflow) map[string]int {
	out := make(map[string]int)
	for i, step := range wf.Steps() {
		out[step.ID] = i + 1
	}
	return out
}

// jobHeader renders the header of a job's block: the job and the
// rounds it runs.
//
// A job with no round count of its own runs one round: its steps run
// once. A count of zero means the job repeats until the run ends.
func jobHeader(wf *wfe.Workflow, name string) string {
	switch rounds := wf.Jobs[name].Rounds(); {
	case rounds == 0:
		return name + " ⟳ until the run ends"
	case rounds == 1:
		return name
	default:
		return name + " ⟳ " + countText(rounds, "round")
	}
}

// stepCommand renders the command a step runs.
//
// A step is one of four things: a message to the chat, a shell
// command, a registered action or a wait. The kind is part of the
// command, so the reader knows what the step does.
func stepCommand(step wfe.Step) string {
	switch step.Kind() {
	case wfe.StepKindMessage:
		return cutText(step.Message, maxStepCommand)
	case wfe.StepKindShell:
		return cutText("(shell) "+step.Shell, maxStepCommand)
	case wfe.StepKindUse:
		return cutText("(use) "+step.Use, maxStepCommand)
	case wfe.StepKindWait:
		return cutText("(wait) "+step.Wait, maxStepCommand)
	}
	return ""
}

// roundsTree renders the tree of the rounds of the run: the rounds
// that have finished, with their own facts, and the round that is
// going.
//
// The tree is a snapshot of the moment the round opened: a round that
// has not settled has no duration and no verdict, so its node carries
// nothing but the time it started.
func (r *run) roundsTree(current iteration) string {
	r.mu.Lock()
	records := append([]roundRecord(nil), r.roundRecords...)
	r.mu.Unlock()

	// The rounds of a job are numbered from one, so a run with several
	// jobs has rounds of the same number. The tree names the job when
	// there is more than one: with a single job the plan of the run
	// says which job the rounds belong to.
	several := len(r.workflow.JobOrder()) > 1

	bodies := make([]string, 0, len(records)+2)
	folded := 0
	if len(records) > maxTreeRounds {
		folded = len(records) - maxTreeRounds
		records = records[len(records)-maxTreeRounds:]
	}
	if folded > 0 {
		bodies = append(bodies, fmt.Sprintf("… +%d", folded))
	}
	for _, rec := range records {
		bodies = append(bodies, roundNode(rec, several))
	}
	bodies = append(bodies, currentNode(current, several, r.roundStart(current.job, current.round)))

	lines := make([]string, 0, len(bodies)+1)
	lines = append(lines, "rounds:")
	for i, body := range bodies {
		prefix := "├─ "
		if i == len(bodies)-1 {
			prefix = "└─ "
		}
		lines = append(lines, prefix+body)
	}
	return strings.Join(lines, "\n")
}

// roundNode renders the node of a finished round: its marker, when it
// started, how long it took and what it decided.
//
// The numbers are the numbers of the run so far: the tree reads as a
// ledger, and the last node carries the totals of the run.
func roundNode(rec roundRecord, several bool) string {
	return fmt.Sprintf("[ %s ] %s%d: %s · %s %s",
		outcomeIcon(rec.Outcome.Status),
		jobPrefix(rec.Job, several),
		rec.Round,
		clockTime(rec.StartedAt),
		durationText(rec.EndedAt.Sub(rec.StartedAt)),
		decisionsText(rec))
}

// jobPrefix names the job of a round when the run has more than one.
//
// The rounds of a job are numbered from one: without the job, two
// rounds of two jobs read the same.
func jobPrefix(job string, several bool) string {
	if !several {
		return ""
	}
	return job + " · "
}

// currentNode renders the node of the round that is going: its number
// and when it started.
//
// The round has not settled: it has no duration and no verdict yet, so
// its node carries nothing else.
func currentNode(it iteration, several bool, at time.Time) string {
	return fmt.Sprintf("[ > ] %s%d: %s", jobPrefix(it.job, several), it.round, clockTime(at))
}

// roundEndedLine renders the closing line of a finished round: the
// marker of its outcome, what it came to and how long it took.
func roundEndedLine(rec roundRecord) string {
	line := "ended: [ " + outcomeIcon(rec.Outcome.Status) + " ]"
	if summary := strings.TrimSpace(rec.Outcome.Summary); summary != "" {
		line += " " + summary
	}
	return line + " · " + durationText(rec.EndedAt.Sub(rec.StartedAt))
}

// decisionsText renders what decided up to the end of a round and what
// the run spent: the counts of the decisions and the tokens.
//
// A round in which nothing was decided says so: a part that is empty
// would read like a part that was cut.
func decisionsText(rec roundRecord) string {
	parts := make([]string, 0, 3)
	if n := rec.Decisions.System1; n > 0 {
		parts = append(parts, fmt.Sprintf("system-1 ×%d", n))
	}
	if n := rec.Decisions.System2; n > 0 {
		parts = append(parts, fmt.Sprintf("system-2 ×%d", n))
	}
	if n := rec.Decisions.Operator; n > 0 {
		parts = append(parts, fmt.Sprintf("operator ×%d", n))
	}
	if len(parts) == 0 {
		parts = append(parts, "none")
	}
	return "decisions:  " + strings.Join(parts, " · ") +
		" ·  tokens - " + abbrevTokens(rec.Usage.InputTokens) +
		" / " + abbrevTokens(rec.Usage.OutputTokens)
}

// abbrevTokens renders a token count the way the status bar renders it:
// "4.2k", "1.1M".
//
// The card keeps the rule to itself: the status bar's helper is not
// exported, and a card that counted its tokens differently from the bar
// that reports them would say two things about one number.
func abbrevTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return abbrevOneDecimal(float64(n)/1_000_000, "M")
	case n >= 1_000:
		return abbrevOneDecimal(float64(n)/1_000, "k")
	default:
		return strconv.Itoa(n)
	}
}

// abbrevOneDecimal renders a number with one decimal and a unit.
//
// A whole number drops its decimal: "1M", not "1.0M". The unit stays:
// the decimal goes, the unit does not.
func abbrevOneDecimal(v float64, unit string) string {
	s := fmt.Sprintf("%.1f%s", v, unit)
	zeroSuffix := ".0" + unit
	if strings.HasSuffix(s, zeroSuffix) {
		s = s[:len(s)-len(zeroSuffix)] + unit
	}
	return s
}

// outcomeIcon returns the icon of an outcome.
//
// The icons are the ones the checklist of a step uses, so a card reads
// the same way everywhere it is read. The marker of a card is wider
// than the marker of a checklist: a space on each side of the icon
// keeps the glyph from crowding it.
func outcomeIcon(status wfe.OutcomeStatus) string {
	switch status {
	case wfe.OutcomeOK, wfe.OutcomeNoop:
		return "√"
	case wfe.OutcomeFailed, wfe.OutcomeIncomplete, wfe.OutcomeTimeout:
		return "!"
	case wfe.OutcomeCancelled:
		return "-"
	}
	return " "
}

// durationText renders a duration compactly: "13s", "5m14s", "1h05m".
func durationText(d time.Duration) string {
	seconds := int(d.Seconds())
	if seconds < 0 {
		seconds = 0
	}
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%dm%02ds", seconds/60, seconds%60)
	default:
		return fmt.Sprintf("%dh%02dm", seconds/3600, (seconds%3600)/60)
	}
}

// cutText cuts a text to a number of runes and marks the cut.
//
// The ellipsis sits on top of the budget: a text that was cut says so.
func cutText(text string, n int) string {
	runes := []rune(text)
	if len(runes) <= n {
		return text
	}
	return string(runes[:n]) + "…"
}

// countText renders a count with its unit: "1 round", "3 chances".
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
// that was skipped is skipped — whether its condition did not hold or
// the operator passed it over.
func taskStatusOf(st wfe.StepState) agent.AgentTaskStatus {
	switch {
	case st.Skipped:
		return agent.TaskSkipped
	case st.Pending:
		return agent.TaskInProgress
	case st.Outcome.Status == wfe.OutcomeNoop:
		return agent.TaskSkipped
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
// time. The branch the step worked on is not in the detail either:
// the card the operator read names it.
//
// A step that was skipped says why in its summary: the engine's own
// skip repeats what the row says, and the summary is left out — the
// skip of the operator or of the agent names who skipped it, and the
// row carries it.
//
// The error of a step that broke is on every row: a step that carries
// one is not a step that was passed over.
//
// The detail is cut short: a step that reports a whole build log as
// its summary would otherwise make the row unreadable.
func stepDetail(st wfe.StepState, status agent.AgentTaskStatus, chances int, hasChances bool) string {
	parts := make([]string, 0, 3)
	engineSkipped := status == agent.TaskSkipped && st.Skipped
	if !engineSkipped {
		if summary := branchlessSummary(st.Outcome); summary != "" {
			parts = append(parts, summary)
		}
	}
	if errText := st.Outcome.Data[wfe.OutcomeKeyError]; errText != "" {
		parts = append(parts, errText)
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
// its end. The card the operator read names the branch, so the suffix
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
