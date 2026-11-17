// render.go — what /review shows the chat.
//
// The review delivers its content in three moves:
//
//  1. the plan line — the runner, the rule set and the group list,
//     before any group produces output;
//  2. the findings — each group's findings, one reply per group, in
//     the thread;
//  3. the verdict line — what the run found, in one line, followed by
//     the digest: the findings as one table per severity, so the card
//     answers "what was found" on its own.
//
// The merged body is NOT one of them: it is the main agent's input,
// not the channel's. The thread carries the findings; the card
// carries their digest.

package review

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/messages"
)

// reviewPromptFrame is the instruction appended to the review text
// injected into the chat session's agent session.
//
// The findings already reached the chat through the thread. Without
// this instruction the agent's answer becomes a third copy of them;
// with it, the agent answers with a verdict and the next step.
const reviewPromptFrame = "\n\n---\n\n" +
	"Answer with a verdict and the next step: which findings block the merge, " +
	"how severe they are, and what you would do next. Do not restate the " +
	"findings — they are in this message and in the chat already."

// renderPlanLine renders the plan of a review run: the runner, the
// rule set the review ran against, the subagent count, and one entry
// per group with the number of files it covers.
//
// The plan is the first thing the review says — the user learns what
// the run is about to do before any group produces output.
func renderPlanLine(runner string, groups []agent.ReviewPlanGroup) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🔍 review · %s · %s · %s",
		runner, reviewRuleSet(groups), plural(len(groups), "subagent"))
	for i, g := range groups {
		fmt.Fprintf(&b, "\n%d. %s — %s", i+1, g.Label, plural(len(g.Files), "file"))
	}
	return b.String()
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
	// failed holds the label of every group that did not finish.
	failed []string
}

// newVerdict builds the verdict of a review run.
//
// reports holds one entry per group that completed; failed holds the
// label of every group that did not. A group that failed is counted as
// failed — never as unparsed.
func newVerdict(groups int, reports []agent.ReviewReport, failed []string) verdict {
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

// body renders the verdict's counts and the groups that failed.
func (v verdict) body() string {
	parts := v.parts()
	if len(v.failed) > 0 {
		parts = append(parts, "failed: "+strings.Join(v.failed, ", ")+" → /review --continue")
	}
	return strings.Join(parts, " · ")
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
// numbers, in Markdown, so the thread closes on the outcome.
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
// of the groups that completed and the labels of the groups that did
// not.
//
// A failed group's label comes from the plan; a group the plan did not
// name falls back to the name its identity gives.
func verdictInputs(outcomes []reviewOutcome, plan []agent.ReviewPlanGroup) (reports []agent.ReviewReport, failed []string) {
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
		failed = append(failed, label)
	}
	return reports, failed
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

// digestContentRunes is the number of runes a digest's content cell
// holds before it is cut.
const digestContentRunes = 80

// verdictMessageBudgetRunes is the rune budget of the verdict
// message — the verdict line and the digest together.
//
// Discord caps a message at 2000 characters and drops a longer one
// whole, so an unbounded digest would take the verdict's counts down
// with it. The budget is the tightest limit nightme ships against,
// with room for the channel's own framing.
const verdictMessageBudgetRunes = 1900

// digestTailReserve is the room a digest leaves for its tail line —
// the line that names the findings that did not fit.
const digestTailReserve = 64

// reviewDigest renders a review run's findings as severity-grouped
// markdown tables: one table per severity bucket, one row per
// finding.
//
// The digest is the card's account of what the run found — the
// verdict line carries the counts, the tables carry the rows. It is
// built from the same parsed reports the verdict line counts, so the
// two can never disagree.
//
// The digest fits into the given rune budget. A digest that does not
// fit keeps the rows that do and names the findings it left out —
// the reader learns that the digest is partial instead of reading it
// as the whole run.
//
// A run with no findings produces no digest: the verdict message is
// then the verdict line alone.
func reviewDigest(outcomes []reviewOutcome, plan []agent.ReviewPlanGroup, budget int) string {
	findings := digestFindings(outcomes, plan)
	if len(findings) == 0 || budget <= 0 {
		return ""
	}
	tables := digestTables(findings)

	var full strings.Builder
	for i, t := range tables {
		if i > 0 {
			full.WriteString("\n")
		}
		full.WriteString(t.heading(len(t.rows)))
		full.WriteString(t.header)
		for _, row := range t.rows {
			full.WriteString(row)
		}
	}
	if utf8.RuneCountInString(full.String()) <= budget {
		return strings.TrimRight(full.String(), "\n")
	}

	var b strings.Builder
	used := 0
	dropped := 0
	emitted := 0
	for _, t := range tables {
		// The heading is sized by the rows that fit, so the room it
		// takes is reserved with the bucket's full count.
		avail := budget - digestTailReserve - used -
			utf8.RuneCountInString(t.heading(len(t.rows))) -
			utf8.RuneCountInString(t.header)
		shown := 0
		rowsUsed := 0
		for _, row := range t.rows {
			n := utf8.RuneCountInString(row)
			if n > avail {
				break
			}
			avail -= n
			rowsUsed += n
			shown++
		}
		if shown == 0 {
			dropped += len(t.rows)
			continue
		}
		if emitted > 0 {
			b.WriteString("\n")
		}
		head := t.heading(shown)
		b.WriteString(head)
		b.WriteString(t.header)
		for i := 0; i < shown; i++ {
			b.WriteString(t.rows[i])
		}
		used += utf8.RuneCountInString(head) + utf8.RuneCountInString(t.header) + rowsUsed
		dropped += len(t.rows) - shown
		emitted++
	}
	b.WriteString("\n")
	b.WriteString(digestTail(dropped))
	return strings.TrimRight(b.String(), "\n")
}

// digestTail names the findings a digest left out. The thread holds
// them — /think off hides them, so the tail says how to see them.
func digestTail(dropped int) string {
	return fmt.Sprintf("… +%d more findings — see the thread (/think on).", dropped)
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
func (t digestTable) heading(shown int) string {
	return fmt.Sprintf("**%s** · %d\n", t.severity, shown)
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
	header := "| path | content |\n| --- | --- |\n"
	if hasCategory {
		header = "| path | category | content |\n| --- | --- | --- |\n"
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

// digestRow renders one finding as a table row.
func digestRow(f agent.ReviewFinding, hasCategory bool) string {
	var b strings.Builder
	b.WriteString("| " + digestPath(f) + " | ")
	if hasCategory {
		b.WriteString(digestCategory(f) + " | ")
	}
	b.WriteString(digestContent(f.Content) + " |\n")
	return b.String()
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

// digestContent renders a finding's text: sanitized, then cut to
// the cell's budget.
func digestContent(content string) string {
	return truncateRunes(sanitizeCell(content), digestContentRunes)
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

// truncateRunes cuts s to at most n runes and marks the cut with an
// ellipsis.
//
// The cut is rune-safe: findings and paths carry CJK text, and
// splitting a multi-byte rune would leave a broken character in the
// cell.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// sendReviewLine sends one of the review's own lines to the channel,
// anchored to the /review message so it folds into the same receipt as
// the plan and the verdict.
func sendReviewLine(ctx context.Context, em messages.Emitter, chatID, replyTo, runner, text string) {
	if em == nil || text == "" {
		return
	}
	_ = em.Send(ctx, messages.OutboundMessage{
		ChatID:    chatID,
		Kind:      messages.OutReply,
		ReplyTo:   replyTo,
		AgentName: runner,
		Text:      text,
	})
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
// The message is budgeted: a channel that caps a message (Discord
// drops one over 2000 characters) would lose the counts along with
// the rows, so the digest gets what is left of the budget.
func sendVerdict(ctx context.Context, em messages.Emitter, chatID, replyTo, runner string,
	result agent.RunResult, outcomes []reviewOutcome, plan []agent.ReviewPlanGroup,
) {
	text := reviewVerdict(outcomes, plan)
	if text == "" {
		return
	}
	// The blank line between the verdict line and the digest is part
	// of the message's budget.
	budget := verdictMessageBudgetRunes - utf8.RuneCountInString(text) - 2
	if digest := reviewDigest(outcomes, plan, budget); digest != "" {
		text += "\n\n" + digest
	}
	out := messages.OutboundMessage{
		ChatID:  chatID,
		Kind:    messages.OutReply,
		ReplyTo: replyTo,
		Text:    text,
	}
	messages.StampRunResult(&out, runner, result)
	if em != nil {
		_ = em.Send(ctx, out)
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
