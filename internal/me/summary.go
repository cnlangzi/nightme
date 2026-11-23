// summary.go — the record a finished run leaves behind.
//
// The record is the account of the session: it says what the run did,
// what went wrong and what the models decided. It rides on the card of
// the run — the run is over, and the card it worked on is the record of
// it. A run the operator stopped rides on the card of the stop
// instead: the stop and the record are one card.
package me

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/pkg/wfe"
)

// The caps of the record.
//
// A repeating workflow can produce hundreds of wake-ups and dozens of
// failures. The card has to stay readable, and the totals stay in the
// tree of the rounds.
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

// postRecord posts the record a finished run leaves behind.
//
// The record is the account of the session: it says what the run did,
// what went wrong and what the models decided.
//
// The record rides on the card of the run: the run is over, and the
// card it worked on is the record of it. A run the operator stopped
// rides on the card of the stop instead: the stop and the record are
// one card.
//
// The record is posted whenever the run ends, whatever ended it: a
// run that finishes on its own has a session worth recording too.
//
// A run that has no card of its own posts the record on its own: the
// chat shows it the way it shows every other message of the run.
func (s *Service) postRecord(r *run) {
	if s.cfg.Operator == nil {
		return
	}
	// The run's own scope is cancelled when the run ends, and the
	// record is posted after that. The context outlives the run here
	// on purpose.
	ctx := context.WithoutCancel(r.ctx)
	sess := &session{kind: sessionRecord, id: r.id, anchor: r.recordAnchor()}
	s.publishInit(ctx, r, sess)
	s.publishRecord(ctx, r, sess, r.recordCard())
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

// isErrorStatus reports whether an action went wrong.
//
// A 4xx or a 5xx went wrong. A status of zero says nobody reported a
// verdict, which is not the same thing.
func isErrorStatus(status int) bool {
	return status >= 400
}

// reportFilter says what stays in the record of a finished run.
//
// A round that ended quietly stays out of it: no action of it, no
// decision of it and no row of it. The round happened and it cost what
// it cost — the tokens are counted either way — but a row of an empty
// round is noise on the panel.
type reportFilter struct {
	wf      *wfe.Workflow
	skipped wfe.SkippedRounds
}

// newReportFilter returns the filter of the rounds the run left out.
func newReportFilter(wf *wfe.Workflow, skipped []wfe.SkippedRound) reportFilter {
	return reportFilter{wf: wf, skipped: wfe.NewSkippedRounds(skipped)}
}

// skippedRound reports whether the round of the job ended quietly.
func (f reportFilter) skippedRound(job string, round int) bool {
	return f.skipped.Has(job, round)
}

// action reports whether the action stays in the record.
//
// The job of an action is the job of the step it belongs to. An action
// that belongs to no step stays: it is the run's own bookkeeping.
func (f reportFilter) action(rec wfe.ActionRecord) bool {
	if rec.StepID == "" || f.wf == nil {
		return true
	}
	round := rec.Round
	if round <= 0 {
		// An action that names no round belongs to the first one.
		round = 1
	}
	return !f.skippedRound(wfe.JobOf(f.wf, rec.StepID), round)
}

// decision reports whether the decision stays in the record.
func (f reportFilter) decision(rec wakeRecord) bool {
	return !f.skippedRound(rec.Job, rec.Round)
}

// errorLines renders the actions that went wrong.
//
// The actions that went well are counted, not listed: a row per step
// of a long workflow is noise, and what went wrong is what the
// operator needs.
func errorLines(history []wfe.ActionRecord, f reportFilter) []string {
	failed := make([]wfe.ActionRecord, 0, len(history))
	for _, rec := range history {
		if !isErrorStatus(rec.Outcome.Status) {
			continue
		}
		if !f.action(rec) {
			continue
		}
		failed = append(failed, rec)
	}
	if len(failed) == 0 {
		return nil
	}
	heading := "errors:"
	if len(failed) > maxSummaryErrors {
		heading = fmt.Sprintf("errors (last %d of %d):", maxSummaryErrors, len(failed))
		failed = failed[len(failed)-maxSummaryErrors:]
	}
	lines := make([]string, 0, len(failed)+1)
	lines = append(lines, heading)
	for i, rec := range failed {
		lines = append(lines, treeRow(i, len(failed), errorLine(rec)))
	}
	return lines
}

// errorLine renders one action that went wrong.
//
// The command and the summary are cut: a shell step reports a whole
// build log as its summary, and a card that carries it is unreadable.
func errorLine(rec wfe.ActionRecord) string {
	out := rec.Outcome
	line := fmt.Sprintf("[ %s ] %s — %d",
		outcomeIcon(out.Status), clipLine(rec.Command, maxSummaryCommand), out.Status)
	if out.StatusText != "" {
		line += ": " + clipLine(out.StatusText, maxSummaryText)
	}
	return clipLine(line, maxSummaryLine)
}

// decisionLines renders the decisions the models made at the wake-ups.
func decisionLines(wakes []wakeRecord, f reportFilter) []string {
	kept := make([]wakeRecord, 0, len(wakes))
	for _, rec := range wakes {
		if !f.decision(rec) {
			continue
		}
		kept = append(kept, rec)
	}
	if len(kept) == 0 {
		return nil
	}
	heading := "decisions:"
	if len(kept) > maxSummaryDecisions {
		heading = fmt.Sprintf("decisions (last %d of %d):", maxSummaryDecisions, len(kept))
		kept = kept[len(kept)-maxSummaryDecisions:]
	}
	lines := make([]string, 0, len(kept)+1)
	lines = append(lines, heading)
	for i, rec := range kept {
		line := rec.Who + " — " + clipLine(rec.What, maxSummaryCommand)
		if rec.Why != "" {
			line += " · because: " + clipLine(rec.Why, maxSummaryText)
		}
		lines = append(lines, treeRow(i, len(kept), clipLine(line, maxSummaryLine)))
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
