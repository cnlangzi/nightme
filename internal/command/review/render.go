// render.go — what /review shows the chat.
//
// The review delivers its content in three moves:
//
//  1. the plan line — the runner and the group list, before any group
//     produces output;
//  2. the coalesced stream — the groups' findings, batch by batch,
//     each batch labeled with its group;
//  3. the verdict line — what the run found, in one line.
//
// The merged body is NOT one of them: it is the main agent's input,
// not the channel's. The channel sees the findings once, in the
// stream.

package review

import (
	"context"
	"fmt"
	"strings"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/messages"
)

// reviewPromptFrame is the instruction appended to the review text
// injected into the chat session's agent session.
//
// The findings already reached the chat through the coalesced stream.
// Without this instruction the agent's answer becomes a third copy of
// them; with it, the agent answers with a verdict and the next step.
const reviewPromptFrame = "\n\n---\n\n" +
	"Answer with a verdict and the next step: which findings block the merge, " +
	"how severe they are, and what you would do next. Do not restate the " +
	"findings — they are in this message and in the chat already."

// renderPlanLine renders the plan of a review run: the runner, the
// group count, and one entry per group with the number of files it
// covers.
//
// The plan is the first thing the review says — the user learns what
// the run is about to do before any group produces output.
func renderPlanLine(runner string, groups []agent.ReviewPlanGroup) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🔍 review · %s · %s", runner, plural(len(groups), "group"))
	for i, g := range groups {
		fmt.Fprintf(&b, "\n%d. %s — %s", i+1, g.Label, plural(len(g.Files), "file"))
	}
	return b.String()
}

// renderCountLine renders the verdict of a review run: the group
// count, the finding count, the severity distribution, the number of
// groups whose output could not be parsed, and the groups that failed.
//
// reports holds one entry per group that completed; failed holds the
// label of every group that did not. A group that failed is counted as
// failed — never as unparsed.
//
// The verdict is a claim about what the run found, so it is marked
// with ⚠️ whenever it is incomplete: a group failed, or a group's
// output could not be parsed (the count is then a lower bound).
func renderCountLine(groups int, reports []agent.ReviewReport, failed []string) string {
	counts := make(map[string]int)
	findings := 0
	unparsed := 0
	for _, r := range reports {
		if !r.Parsed {
			unparsed++
		}
		for _, f := range r.Findings {
			findings++
			counts[f.Severity]++
		}
	}

	parts := []string{plural(groups, "group"), plural(findings, "finding")}
	for _, sev := range agent.SeverityOrder() {
		if counts[sev] == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%d %s", counts[sev], sev))
	}
	if unparsed > 0 {
		parts = append(parts, plural(unparsed, "unparsed group"))
	}

	icon := "✅"
	if len(failed) > 0 || unparsed > 0 {
		icon = "⚠️"
	}
	line := icon + " review · " + strings.Join(parts, " · ")
	if len(failed) > 0 {
		line += " · failed: " + strings.Join(failed, ", ") + " → /review --continue"
	}
	return line
}

// reviewVerdict builds the verdict line of a review run from its group
// outcomes and the plan that named the groups.
//
// A failed group is counted as failed, never as unparsed. Its label
// comes from the plan; a group the plan did not name falls back to its
// pattern.
func reviewVerdict(outcomes []reviewOutcome, plan []agent.ReviewPlanGroup) string {
	if len(outcomes) == 0 {
		return ""
	}
	labels := make(map[string]string, len(plan))
	for _, g := range plan {
		labels[g.Pattern] = g.Label
	}
	var reports []agent.ReviewReport
	var failed []string
	for _, o := range outcomes {
		if o.err != nil {
			label := labels[o.pattern]
			if label == "" {
				label = o.pattern
			}
			failed = append(failed, label)
			continue
		}
		reports = append(reports, o.report)
	}
	return renderCountLine(len(outcomes), reports, failed)
}

// sendReviewLine sends one of the review's own lines to the channel,
// anchored to the /review message so it folds into the same receipt as
// the streamed groups.
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
// itself sends. The main agent's answer follows it.
func sendVerdict(ctx context.Context, em messages.Emitter, chatID, replyTo, runner string,
	result agent.RunResult, outcomes []reviewOutcome, plan []agent.ReviewPlanGroup,
) {
	text := reviewVerdict(outcomes, plan)
	if text == "" {
		return
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

// plural renders a count with its noun: "1 group", "4 groups".
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
