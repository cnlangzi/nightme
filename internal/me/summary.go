package me

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/wfe"
)

// The caps of the summary.
//
// A repeating workflow can produce hundreds of wake-ups and dozens of
// failures. The card has to stay readable, and the totals stay in the
// facts lines.
const (
	maxSummaryErrors    = 5
	maxSummaryDecisions = 10
	// maxSummaryValue is how long the value of a tool argument is.
	maxSummaryValue = 40
	// maxSummaryCommand is how long the command of a row is.
	maxSummaryCommand = 100
	// maxSummaryText is how long the summary of an action or the reason
	// of a decision is.
	maxSummaryText = 120
	// maxSummaryLine is how long a line of the card is.
	//
	// The card is a message of the chat: a row that carries a whole
	// transcript would make the card unreadable, and a card that is
	// too long is refused.
	maxSummaryLine = 300
)

// postSummary posts the summary of a run the operator stopped.
//
// The summary is the record of the session: it says what the run did,
// what went wrong and what the models decided. It is a card of its
// own — the run is over, and its live card is no longer the record
// of it.
//
// A chat that refuses the card changes nothing else: the run still
// ends, and no error reaches the operator.
func (s *Service) postSummary(r *run) {
	if s.cfg.Operator == nil {
		return
	}
	// The run's own scope is cancelled when the run ends, and the
	// summary is posted after that. The context outlives the run here
	// on purpose.
	ctx := context.WithoutCancel(r.ctx)
	sess := s.openSession(ctx, r, sessionRun, r.id, "", 0, "")
	if sess.cardID == "" {
		// The chat refused the card: the run still ends, and no error
		// reaches the operator.
		return
	}
	s.publishInit(ctx, r, sess)
	s.publishTasks(ctx, r, sess, r.runTasks())
	for _, line := range r.summaryLines() {
		s.publishReply(ctx, r, sess, line)
	}
	s.publishResult(ctx, r, sess, string(r.currentPhase()))
}

// summaryLines renders the summary of a stopped run.
//
// The facts come first: the run, how long it went, the jobs it
// entered, what it did, how often it woke a model and what each
// capability decided and spent. The errors and the decisions follow,
// one per line, and the totals stay in the facts.
func (r *run) summaryLines() []string {
	r.mu.Lock()
	id := r.id
	started := r.state.StartedAt
	rounds := make(map[string]int, len(r.state.Rounds))
	for job, round := range r.state.Rounds {
		rounds[job] = round
	}
	history := append([]wfe.ActionRecord(nil), r.policy.History...)
	s1d, s2d := r.system1Decisions, r.system2Decisions
	s1u, s2u := r.system1Usage.Total(), r.system2Usage.Total()
	inflight := r.inflight
	stepIDs := workflowStepIDs(r.workflow)
	jobs := r.workflow.JobOrder()
	r.mu.Unlock()
	wakes := r.wakeRecords()

	lines := make([]string, 0, 16)
	lines = append(lines, "run: "+id)
	lines = append(lines, "duration: "+compactDuration(r.svc.now().Sub(started)))
	if line := jobsLine(jobs, rounds); line != "" {
		lines = append(lines, clipLine(line, maxSummaryLine))
	}
	lines = append(lines, actionsLine(history))
	lines = append(lines, wakeLine(wakes))
	lines = append(lines, stepsLine(stepIDs, history))
	lines = append(lines, fmt.Sprintf("system-1: %d decisions, %d tokens", s1d, s1u))
	lines = append(lines, fmt.Sprintf("system-2: %d decisions, %d tokens", s2d, s2u))

	if errs := errorLines(history); len(errs) > 0 {
		lines = append(lines, errs...)
	}
	if decs := decisionLines(wakes); len(decs) > 0 {
		lines = append(lines, decs...)
	}
	if inflight != nil && inflight.Command != "" {
		lines = append(lines, clipLine("stopped while: "+inflight.Command, maxSummaryLine))
	}
	return lines
}

// workflowStepIDs returns the ids of the steps of the workflow.
func workflowStepIDs(wf *wfe.Workflow) []string {
	steps := wf.Steps()
	out := make([]string, 0, len(steps))
	for _, step := range steps {
		out = append(out, step.ID)
	}
	return out
}

// jobsLine renders the jobs the run entered and the round of each.
//
// A run that entered no job yet has no line: the map of the rounds is
// empty then.
func jobsLine(jobs []string, rounds map[string]int) string {
	parts := make([]string, 0, len(rounds))
	for _, job := range jobs {
		if round, ok := rounds[job]; ok {
			parts = append(parts, fmt.Sprintf("%s × %d", job, round))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "jobs: " + strings.Join(parts, " · ")
}

// outcomeStatusOrder is the order the counts of the actions follow.
var outcomeStatusOrder = []wfe.OutcomeStatus{
	wfe.OutcomeOK,
	wfe.OutcomeNoop,
	wfe.OutcomeIncomplete,
	wfe.OutcomeFailed,
	wfe.OutcomeCancelled,
	wfe.OutcomeTimeout,
	wfe.OutcomeUnknown,
}

// actionsLine renders what the run did and how the actions ended.
func actionsLine(history []wfe.ActionRecord) string {
	counts := map[wfe.OutcomeStatus]int{}
	for _, rec := range history {
		status := rec.Outcome.Status
		if status == "" {
			status = wfe.OutcomeUnknown
		}
		counts[status]++
	}
	parts := make([]string, 0, len(counts))
	for _, status := range outcomeStatusOrder {
		if counts[status] == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%d %s", counts[status], status))
	}
	line := fmt.Sprintf("actions: %d", len(history))
	if len(parts) > 0 {
		line += " (" + strings.Join(parts, " · ") + ")"
	}
	return line
}

// wakeLine renders how often the run woke a model and who woke.
func wakeLine(wakes []wakeRecord) string {
	counts := map[string]int{}
	for _, rec := range wakes {
		counts[rec.Who]++
	}
	parts := make([]string, 0, len(counts))
	for _, who := range []string{decisionDecider, decisionChat} {
		if counts[who] == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%d by %s", counts[who], who))
	}
	line := fmt.Sprintf("wake-ups: %d", len(wakes))
	if len(parts) > 0 {
		line += " (" + strings.Join(parts, " · ") + ")"
	}
	return line
}

// stepsLine renders how many steps of the workflow ran.
//
// A step whose condition did not hold is recorded in the run state but
// not in the history, so it counts as a step that did not run.
func stepsLine(stepIDs []string, history []wfe.ActionRecord) string {
	ran := map[string]bool{}
	for _, rec := range history {
		if rec.Kind == wfe.ActionKindRun && rec.StepID != "" {
			ran[rec.StepID] = true
		}
	}
	count := 0
	for _, id := range stepIDs {
		if ran[id] {
			count++
		}
	}
	return fmt.Sprintf("steps: %d ran · %d not run", count, len(stepIDs)-count)
}

// isErrorStatus reports whether an action went wrong.
func isErrorStatus(status wfe.OutcomeStatus) bool {
	switch status {
	case wfe.OutcomeFailed, wfe.OutcomeIncomplete, wfe.OutcomeCancelled, wfe.OutcomeTimeout:
		return true
	}
	return false
}

// errorLines renders the actions that went wrong.
//
// The actions that went well are counted, not listed: a row per step
// of a long workflow is noise, and what went wrong is what the
// operator needs.
func errorLines(history []wfe.ActionRecord) []string {
	failed := make([]wfe.ActionRecord, 0, len(history))
	for _, rec := range history {
		if !isErrorStatus(rec.Outcome.Status) {
			continue
		}
		failed = append(failed, rec)
	}
	if len(failed) == 0 {
		return nil
	}
	heading := "errors"
	if len(failed) > maxSummaryErrors {
		heading = fmt.Sprintf("errors (last %d of %d)", maxSummaryErrors, len(failed))
		failed = failed[len(failed)-maxSummaryErrors:]
	}
	lines := make([]string, 0, len(failed)+1)
	lines = append(lines, heading)
	for _, rec := range failed {
		lines = append(lines, "· "+errorLine(rec))
	}
	return lines
}

// errorLine renders one action that went wrong.
//
// The command and the summary are cut: a shell step reports a whole
// build log as its summary, and a card that carries it is unreadable.
func errorLine(rec wfe.ActionRecord) string {
	out := rec.Outcome
	line := fmt.Sprintf("%s — %s", clipLine(rec.Command, maxSummaryCommand), out.Status)
	if out.Summary != "" {
		line += ": " + clipLine(out.Summary, maxSummaryText)
	}
	if reason := out.Data["reason"]; reason != "" {
		line += " (reason: " + reason + ")"
	}
	return clipLine(line, maxSummaryLine)
}

// decisionLines renders the decisions the models made at the wake-ups.
func decisionLines(wakes []wakeRecord) []string {
	if len(wakes) == 0 {
		return nil
	}
	heading := "decisions"
	if len(wakes) > maxSummaryDecisions {
		heading = fmt.Sprintf("decisions (last %d of %d)", maxSummaryDecisions, len(wakes))
		wakes = wakes[len(wakes)-maxSummaryDecisions:]
	}
	lines := make([]string, 0, len(wakes)+1)
	lines = append(lines, heading)
	for _, rec := range wakes {
		line := "· " + rec.Who + " — " + clipLine(rec.What, maxSummaryCommand)
		if rec.Why != "" {
			line += " · because: " + clipLine(rec.Why, maxSummaryText)
		}
		lines = append(lines, clipLine(line, maxSummaryLine))
	}
	return lines
}

// toolCallsLine renders the tool calls of a turn of the chat model.
//
// A turn that carried no tool call decided nothing: the wake log says
// so.
func toolCallsLine(calls []llm.Part) string {
	if len(calls) == 0 {
		return "no tool call"
	}
	parts := make([]string, 0, len(calls))
	for _, call := range calls {
		parts = append(parts, toolCallLine(call))
	}
	return strings.Join(parts, ", ")
}

// toolCallLine renders one tool call: its name and its arguments.
func toolCallLine(call llm.Part) string {
	args := renderArgs(call.Args)
	if args == "" {
		return call.ToolName
	}
	return call.ToolName + "(" + args + ")"
}

// renderArgs renders the arguments of a tool call.
//
// The arguments are read in the order the JSON object holds them, and
// every value is cut short: the card is for a person.
func renderArgs(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return clipLine(strings.TrimSpace(string(raw)), maxSummaryValue)
	}
	parts := make([]string, 0, 4)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			break
		}
		key, ok := keyTok.(string)
		if !ok {
			break
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			break
		}
		parts = append(parts, key+"="+clipLine(argValue(value), maxSummaryValue))
	}
	return strings.Join(parts, ", ")
}

// argValue renders the value of an argument.
func argValue(raw json.RawMessage) string {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	return strings.TrimSpace(string(raw))
}

// compactDuration renders a duration in the short form: the seconds,
// the minutes, or the hours and the minutes.
func compactDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
