// render.go — what /review shows the chat.
//
// The review delivers its content in three moves:
//
//  1. the plan line — the runner, the rule set and the group list,
//     before any group produces output;
//  2. the findings — each group's findings, one reply per group, in
//     the thread;
//  3. the verdict — what the run found: the counts line, and — when
//     a group did not finish — the block that names the failed groups
//     under it, followed by the digest: the findings as one table per
//     severity, one row per finding, holding what kind of finding it
//     is and where it is.
//
// The merged body is NOT one of them: it is the main agent's input,
// not the channel's. The thread carries the findings; the card
// carries their index.

package review

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/messages"
)

// reviewPromptFrame is the instruction appended to the review text
// injected into the chat session's agent session.
//
// The card's digest lists where the findings are, not what they say,
// so the finding text reaches the reader in one of two places: the
// thread, which the chat's /think decides, or the agent's answer.
// With /think on the findings are already in the thread and the agent
// must not copy them; with /think off — the default — the thread
// carries nothing, and the agent's answer is the reader's only view
// of what the findings say.
func reviewPromptFrame(thinkShows bool) string {
	if thinkShows {
		return "\n\n---\n\n" +
			"Answer with a verdict and the next step: which findings block the merge, " +
			"how severe they are, and what you would do next. Do not restate the " +
			"findings — they are in this message and in the chat already."
	}
	return "\n\n---\n\n" +
		"Answer with a verdict and the next step: which findings block the merge, " +
		"how severe they are, what each of them says, and what you would do next. " +
		"The chat shows where the findings are, not what they say — state the ones " +
		"that block the merge."
}

// renderPlanLine renders the plan of a review run: the runner, the
// rule set the review ran against, the subagent count, and one entry
// per group with the number of files it covers.
//
// The plan is the first thing the review says — the user learns what
// the run is about to do before any group produces output.
//
// A continuation marks what it does with each group: a group that is
// resumed is marked "(resumed)", a group that is re-run is marked
// "(re-run)". A plain review carries no marker.
func renderPlanLine(runner string, groups []agent.ReviewPlanGroup) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🔍 review · %s · %s · %s",
		runner, reviewRuleSet(groups), plural(len(groups), "subagent"))
	for i, g := range groups {
		fmt.Fprintf(&b, "\n%d. %s — %s", i+1, agent.CodeSpan(g.Label), plural(len(g.Files), "file"))
		b.WriteString(reviewModeMarker(g.Mode))
	}
	return b.String()
}

// reviewModeMarker names what a continuation does with a group.
func reviewModeMarker(mode string) string {
	switch mode {
	case agent.ReviewModeResume:
		return " (resumed)"
	case agent.ReviewModeRerun:
		return " (re-run)"
	default:
		return ""
	}
}

// reviewRuleSet names the rule set the review ran against: nightme's
// own rubric when the plan holds the built-in group, the ocr rule set
// when the groups came from ocr.
//
// The two rule sets produce different findings, and the group names
// alone do not tell the reader which one produced them.
func reviewRuleSet(groups []agent.ReviewPlanGroup) string {
	for _, g := range groups {
		if g.Pattern == agent.PatternBuiltin {
			return "nightme code reviewer"
		}
	}
	return "open-code-review"
}

// verdict is what a review run found: the counts and the groups that
// did not finish.
//
// The card's verdict line and the thread's end hint are two renderings
// of the same numbers.
type verdict struct {
	// groups is the number of subagents the run fanned out to.
	groups int
	// findings is the number of findings the groups reported.
	findings int
	// counts holds the findings per severity.
	counts map[string]int
	// unparsed is the number of groups whose output could not be
	// parsed.
	unparsed int
	// failed holds every group that did not finish, with the state
	// the continuation could still act on.
	failed []failedGroup
}

// failedState is what a failed group can still be recovered by.
type failedState int

const (
	// failedResumable is a group that reached the agent: the
	// continuation reopens its session.
	failedResumable failedState = iota
	// failedNeverStarted is a group that never reached the agent: the
	// continuation re-runs it with its stored prompt.
	failedNeverStarted
	// failedUnrecoverable is a group that reached neither the agent
	// nor a stored prompt — the continuation cannot finish it.
	failedUnrecoverable
	// failedSpent is a group whose one recovery attempt is used up.
	failedSpent
)

// failedGroup is one group that did not finish, with the state the
// verdict tells the user about.
type failedGroup struct {
	// label is the group's human-facing name.
	label string
	// state is what the continuation can still do with the group.
	state failedState
	// timedOut marks a group whose subagent went silent. The group is
	// still resumable — a timeout does not spend its recovery
	// attempt — but the user needs to know why it ended.
	timedOut bool
}

// text renders the group's name with its state.
//
// The state is prose, so it stays outside the code span around the
// name.
func (g failedGroup) text() string {
	name := agent.CodeSpan(g.label)
	if g.timedOut {
		return name + " (timed out)"
	}
	switch g.state {
	case failedNeverStarted:
		return name + " (never started)"
	case failedUnrecoverable:
		return name + " (not recoverable)"
	case failedSpent:
		return name + " (already retried)"
	default:
		return name
	}
}

// actionable reports whether the continuation can still finish the
// group. A spent group cannot, and neither can a group that reached
// neither the agent nor a stored prompt.
func (g failedGroup) actionable() bool {
	return g.state == failedResumable || g.state == failedNeverStarted
}

// newVerdict builds the verdict of a review run.
//
// reports holds one entry per group that completed; failed holds
// every group that did not. A group that failed is counted as failed
// — never as unparsed.
func newVerdict(groups int, reports []agent.ReviewReport, failed []failedGroup) verdict {
	v := verdict{
		groups: groups,
		counts: make(map[string]int),
		failed: failed,
	}
	for _, r := range reports {
		if !r.Parsed {
			v.unparsed++
		}
		for _, f := range r.Findings {
			v.findings++
			v.counts[f.Severity]++
		}
	}
	return v
}

// parts renders the verdict's counts: the subagent count, the finding
// count, the severity distribution and the groups that could not be
// parsed.
func (v verdict) parts() []string {
	parts := []string{plural(v.groups, "subagent"), plural(v.findings, "finding")}
	for _, sev := range agent.SeverityOrder() {
		if v.counts[sev] == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%d %s", v.counts[sev], sev))
	}
	if v.unparsed > 0 {
		parts = append(parts, plural(v.unparsed, "unparsed subagent"))
	}
	return parts
}

// icon is the verdict's marker: a clean run reads as a success, an
// incomplete one is marked.
func (v verdict) icon() string {
	if len(v.failed) > 0 || v.unparsed > 0 {
		return "⚠️"
	}
	return "✅"
}

// body renders the verdict's body: the counts, and — when a group
// failed — the block that names the failed groups.
//
// The counts come first and the block follows them under a blank line.
// The block is the run's failure, not another count: a failure that
// hides in the tail of a dense counts line is a failure the reader
// misses, and the digest below grows with the number of findings.
//
// The card's verdict and the thread's end hint share this body, so the
// two can never disagree.
func (v verdict) body() string {
	counts := strings.Join(v.parts(), " · ")
	block := v.failureBlock()
	if block == "" {
		return counts
	}
	return counts + "\n\n" + block
}

// failureBlock renders the groups that did not finish: the header
// that counts them, one line per group, and — when a failed group is
// still actionable — the command that recovers them.
//
// The continuation is offered only when a failed group is still
// actionable: the offer must match what the continuation can do.
//
// The hint stands on its own: a line that follows a list item without
// a blank line is the item's continuation, so the hint would end up
// inside the last group's bullet.
func (v verdict) failureBlock() string {
	if len(v.failed) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "❌ **%d of %s failed**", len(v.failed), plural(v.groups, "subagent"))
	actionable := false
	for _, g := range v.failed {
		b.WriteString("\n- " + g.text())
		if g.actionable() {
			actionable = true
		}
	}
	if actionable {
		b.WriteString("\n\n💡 hint: run `/review --continue` to continue the failed groups")
	}
	return b.String()
}

// line renders the verdict as the card's line.
//
// The verdict is a claim about what the run found, so it is marked
// with ⚠️ whenever it is incomplete: a group failed, or a group's
// output could not be parsed (the count is then a lower bound).
func (v verdict) line() string {
	return v.icon() + " review · " + v.body()
}

// hint renders the verdict as the thread's end hint — the same
// counts and the same failure block as the card's verdict, in
// Markdown, so the thread closes on the outcome.
func (v verdict) hint() string {
	return v.icon() + " **review done** · " + v.body()
}

// reviewVerdict builds the verdict of a review run from its group
// outcomes and the plan that named the groups.
//
// A failed group is counted as failed, never as unparsed. Its label
// comes from the plan; a group the plan did not name falls back to the
// name its identity gives.
func reviewVerdict(outcomes []reviewOutcome, plan []agent.ReviewPlanGroup) string {
	if len(outcomes) == 0 {
		return ""
	}
	reports, failed := verdictInputs(outcomes, plan)
	return newVerdict(len(outcomes), reports, failed).line()
}

// groupKey is a group's identity: the rule's source and the pattern
// together. Two rules over the same glob are two groups, so the
// pattern alone does not identify one.
func groupKey(source, pattern string) string {
	return source + "\x00" + pattern
}

// reviewVerdictHint builds the thread's end hint of a review run: the
// verdict in Markdown.
func reviewVerdictHint(outcomes []reviewOutcome, plan []agent.ReviewPlanGroup) string {
	if len(outcomes) == 0 {
		return ""
	}
	reports, failed := verdictInputs(outcomes, plan)
	return newVerdict(len(outcomes), reports, failed).hint()
}

// verdictInputs splits a review run's group outcomes into the reports
// of the groups that completed and the groups that did not.
//
// A failed group's label comes from the plan; a group the plan did not
// name falls back to the name its identity gives.
func verdictInputs(outcomes []reviewOutcome, plan []agent.ReviewPlanGroup) (reports []agent.ReviewReport, failed []failedGroup) {
	labels := make(map[string]string, len(plan))
	for _, g := range plan {
		labels[groupKey(g.Source, g.Pattern)] = g.Label
	}
	for _, o := range finishedOutcomes(outcomes) {
		reports = append(reports, o.report)
	}
	for _, o := range failedOutcomes(outcomes) {
		label := labels[groupKey(o.source, o.pattern)]
		if label == "" {
			label = agent.ReviewGroupName(o.source, o.pattern)
		}
		failed = append(failed, failedGroup{
			label:    label,
			state:    failedStateOf(o),
			timedOut: isGroupTimeout(o.err),
		})
	}
	return reports, failed
}

// failedStateOf names the state a failed group is in: whether it
// reached the agent, whether it can be re-run, and whether its one
// recovery attempt is used up.
func failedStateOf(o reviewOutcome) failedState {
	switch {
	case o.spent:
		return failedSpent
	case o.sessionID != "":
		return failedResumable
	case o.prompt != "":
		return failedNeverStarted
	default:
		return failedUnrecoverable
	}
}

// finishedOutcomes returns the outcomes of the groups that completed,
// in the order they were reported.
//
// A group that failed produced no report: it has no findings, and it
// is not a group the verdict counts. The verdict line and the digest
// both take their findings from here, so the two cannot disagree.
func finishedOutcomes(outcomes []reviewOutcome) []reviewOutcome {
	var out []reviewOutcome
	for _, o := range outcomes {
		if o.err == nil {
			out = append(out, o)
		}
	}
	return out
}

// failedOutcomes returns the outcomes of the groups that did not
// finish, in the order they were reported.
func failedOutcomes(outcomes []reviewOutcome) []reviewOutcome {
	var out []reviewOutcome
	for _, o := range outcomes {
		if o.err != nil {
			out = append(out, o)
		}
	}
	return out
}

// reviewDigest renders a review run's findings as severity-grouped
// markdown tables: one table per severity bucket, one row per
// finding.
//
// The digest is the card's account of what the run found — the
// verdict line carries the counts, the tables carry the rows. It is
// built from the same parsed reports the verdict line counts, so the
// two can never disagree.
//
// The digest is an index of the findings, not their prose: a row
// names what kind of finding it is and where it is. The finding's text
// stays in the main agent's input, and reaches the reader through the
// thread (/think on) or the main agent's answer (/think off), so the
// card stays proportional to the number of findings.
//
// A run with no findings produces no digest: the verdict message is
// then the verdict line alone.
func reviewDigest(outcomes []reviewOutcome, plan []agent.ReviewPlanGroup) string {
	findings := digestFindings(outcomes, plan)
	if len(findings) == 0 {
		return ""
	}
	var b strings.Builder
	for i, t := range digestTables(findings) {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(t.heading())
		b.WriteString(t.header)
		for _, row := range t.rows {
			b.WriteString(row)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// digestTable is one severity bucket's table: its heading, its
// header row and its rows.
type digestTable struct {
	// severity is the bucket's name — the heading's subject.
	severity string
	// header is the table's header row and its separator row.
	header string
	// rows are the table's rows, one per finding.
	rows []string
}

// heading renders the table's heading: the bucket and the number of
// rows the table shows.
func (t digestTable) heading() string {
	return fmt.Sprintf("**%s** · %d\n", t.severity, len(t.rows))
}

// digestTables splits the run's findings into one table per severity
// bucket, in the order the buckets come.
//
// The category column is a property of the run: it exists when some
// finding of the run named a category, and every table carries it.
func digestTables(findings []agent.ReviewFinding) []digestTable {
	hasCategory := false
	for _, f := range findings {
		if f.Category != "" {
			hasCategory = true
			break
		}
	}
	header := "| path |\n| --- |\n"
	if hasCategory {
		header = "| category | path |\n| --- | --- |\n"
	}
	buckets := make(map[string][]agent.ReviewFinding, len(agent.SeverityOrder()))
	for _, f := range findings {
		buckets[f.Severity] = append(buckets[f.Severity], f)
	}
	var tables []digestTable
	for _, sev := range agent.SeverityOrder() {
		rows := buckets[sev]
		if len(rows) == 0 {
			continue
		}
		t := digestTable{severity: sev, header: header}
		for _, f := range rows {
			t.rows = append(t.rows, digestRow(f, hasCategory))
		}
		tables = append(tables, t)
	}
	return tables
}

// digestRow renders one finding as a table row: its category, when the
// run carries a category column, and its location.
func digestRow(f agent.ReviewFinding, hasCategory bool) string {
	if hasCategory {
		return "| " + digestCategory(f) + " | " + digestPath(f) + " |\n"
	}
	return "| " + digestPath(f) + " |\n"
}

// digestFindings returns the run's findings in the order the digest
// lists them: by the producing group's position in the plan, and
// within a group by the finding's order inside that group's report.
//
// A group the plan did not name sorts after the planned groups,
// keeping its outcome order. A group that failed contributes nothing
// — it produced no report.
func digestFindings(outcomes []reviewOutcome, plan []agent.ReviewPlanGroup) []agent.ReviewFinding {
	positions := make(map[string]int, len(plan))
	for i, g := range plan {
		positions[groupKey(g.Source, g.Pattern)] = i
	}
	type group struct {
		order   int
		outcome reviewOutcome
	}
	groups := make([]group, 0, len(outcomes))
	for _, o := range finishedOutcomes(outcomes) {
		order, planned := positions[groupKey(o.source, o.pattern)]
		if !planned {
			// A group the plan did not name sorts after the planned
			// ones. The sort is stable, so the groups that share
			// this key keep their outcome order.
			order = len(plan)
		}
		groups = append(groups, group{order: order, outcome: o})
	}
	slices.SortStableFunc(groups, func(a, b group) int {
		return a.order - b.order
	})
	var findings []agent.ReviewFinding
	for _, g := range groups {
		findings = append(findings, g.outcome.report.Findings...)
	}
	return findings
}

// digestPath renders a finding's location: the path with its line
// when the finding has one, the path alone when it does not.
func digestPath(f agent.ReviewFinding) string {
	path := sanitizeCell(f.Path)
	line := sanitizeCell(f.Line)
	switch {
	case path == "":
		return line
	case line == "":
		return path
	default:
		return path + ":" + line
	}
}

// digestCategory renders a finding's category, or the em dash that
// stands for a finding that named none.
func digestCategory(f agent.ReviewFinding) string {
	category := sanitizeCell(f.Category)
	if category == "" {
		return "—"
	}
	return category
}

// sanitizeCell makes a cell safe to render inside a markdown table
// row: a line break or a tab becomes a space, a pipe becomes a
// slash, and the cell is trimmed.
//
// The pipe matters — a pipe inside a cell ends the cell, so the rest
// of the finding would land in a column of its own.
func sanitizeCell(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return ' '
		case '|':
			return '/'
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

// sendReviewLine sends one of the review's own lines to the channel,
// anchored to the /review message so it folds into the same receipt as
// the plan and the verdict.
//
// A channel that rejects the line (a body over the channel's own
// limit, an outage) is logged: the line is the user's only record of
// what the review did, so a lost one must not vanish silently.
func sendReviewLine(ctx context.Context, em messages.Emitter, chatID, replyTo, runner, text string) {
	if em == nil || text == "" {
		return
	}
	out := messages.OutboundMessage{
		ChatID:    chatID,
		Kind:      messages.OutReply,
		ReplyTo:   replyTo,
		AgentName: runner,
		Text:      text,
	}
	if err := em.Send(ctx, out); err != nil {
		slog.Default().Warn("/review: line send failed",
			"chat_id", chatID,
			"kind", out.Kind.String(),
			"err", err.Error())
	}
}

// planObserver returns the agent.WithReviewPlanObserver callback of a
// review run: it records the plan the verdict names the groups with,
// and sends the plan line — the first thing the user sees.
//
// The callback runs on the goroutine that drives the fan-out, before
// the first group starts, so the plan line reaches the channel before
// any group's output.
func planObserver(ctx context.Context, em messages.Emitter, collector *outcomeCollector,
	chatID, replyTo, runner string,
) func([]agent.ReviewPlanGroup) {
	return func(groups []agent.ReviewPlanGroup) {
		collector.setPlan(groups)
		sendReviewLine(ctx, em, chatID, replyTo, runner, renderPlanLine(runner, groups))
	}
}

// sendVerdict sends the run's verdict line — the last thing the review
// itself sends to the card. The main agent's answer follows it.
//
// The digest rides inside the same message: one message, one card
// entry, and the counts and the rows read as one report. It is sent
// as a reply, so it reaches the card whatever the chat's /think
// setting says.
//
// The message carries an index of the run's findings, not their
// prose: the card stays proportional to the number of findings, and
// the finding text reaches the reader through the thread (/think on)
// or the main agent's answer (/think off).
func sendVerdict(ctx context.Context, em messages.Emitter, chatID, replyTo, runner string,
	result agent.RunResult, outcomes []reviewOutcome, plan []agent.ReviewPlanGroup,
) {
	text := reviewVerdict(outcomes, plan)
	if text == "" {
		return
	}
	if digest := reviewDigest(outcomes, plan); digest != "" {
		text += "\n\n" + digest
	}
	out := messages.OutboundMessage{
		ChatID:  chatID,
		Kind:    messages.OutReply,
		ReplyTo: replyTo,
		Text:    text,
	}
	messages.StampRunResult(&out, runner, result)
	if em == nil {
		return
	}
	if err := em.Send(ctx, out); err != nil {
		slog.Default().Warn("/review: verdict send failed",
			"chat_id", chatID,
			"kind", out.Kind.String(),
			"err", err.Error())
	}
}

// sendVerdictHint sends the review's end hint to the thread: the
// verdict in Markdown, so the thread closes on the outcome.
//
// The hint goes through the review's event sink, so it takes the same
// path as the findings: translated to the thinking kind, gated by the
// chat's /think setting, and anchored to the /review message. The
// visibility of the hints is therefore the visibility of the findings.
func sendVerdictHint(sink func(agent.AgentEvent), text string) {
	if text == "" {
		return
	}
	sink(agent.AgentEvent{Kind: agent.EventAgentThinking, Text: text})
}

// plural renders a count with its noun: "1 subagent", "4 subagents".
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
