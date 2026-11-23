package review

// deliver.go — what a finished review run hands to the chat.
//
// A review is not over when its fan-out ends: a group that failed can
// still be picked up by /review --continue. So the findings of the
// groups that finished wait in the chat's record until every group is
// in. Then the review is delivered — once: one document for the
// agent, one table for the card.
//
// A run that leaves the review pending delivers nothing: the card says
// what is still missing and how to finish it, and the findings wait
// for the round that completes the review.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentsession"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/pkg/outcome"
)

// reviewDelivery is what a finished review run hands to the delivery.
type reviewDelivery struct {
	// ctx is the run's context.
	ctx context.Context
	// cs is the chat session the review ran in.
	cs *chatsession.ChatSession
	// work is the message's work — the outcome rides on it.
	work *chatsession.Work
	// emitter is the chat's emitter: the card.
	emitter messages.Emitter
	// sink is the run's event sink: the thread.
	sink func(agent.AgentEvent)
	// drain empties the sink, so the thread's messages go out before
	// the review's own lines.
	drain func()
	// chatID, replyTo and runner name the chat, the message the
	// review answers and the agent that ran it.
	chatID  string
	replyTo string
	runner  string
	// sendBlocks injects the merged document into the chat's agent
	// session.
	sendBlocks func(context.Context, []agent.ContentBlock, ...agentsession.SendBlocksOption) error
	// rec is the chat's review record, with this run's results merged
	// into it.
	rec reviewRecord
	// result is the fan-out's result — the run metadata the verdict
	// message is stamped with.
	result agent.RunResult
	// data is the outcome data of the run.
	data map[string]string
	// notice is the notice the run sends on its own message — the
	// groups that went silent, for instance. It goes out before the
	// verdict line, which summarizes the run.
	notice string
}

// deliver hands the run's result to the chat.
//
// The review is delivered when no group can be picked up any more: the
// card carries the verdict and the findings table, and the agent gets
// the merged document — once. A review that is not over yet delivers
// nothing: the card says what is missing and how to finish it.
func (f *Factory) deliver(d reviewDelivery) {
	pending := d.rec.pending()
	verdict := newVerdict(d.rec, pending)

	// The end hint goes into the sink BEFORE the drain, so it lands
	// after the groups' findings — the same contract as a plain
	// review.
	sendVerdictHint(d.sink, reviewVerdictHint(d.rec, pending))

	// The sink is drained now: the groups' findings are still queued
	// behind its drain goroutine, and the review's own lines must
	// land after them.
	d.drain()

	// The notice names the groups that went silent — the one thing
	// the user must act on. It goes out as a reply of its own, so it
	// is visible whatever the chat's /think setting says, and before
	// the verdict line that summarizes the run.
	if d.notice != "" {
		sendReviewLine(d.ctx, d.emitter, d.chatID, d.replyTo, d.runner, d.notice)
	}

	// The verdict line — the review's own report of the run. It goes
	// out BEFORE the injected turn: the main agent starts answering
	// as soon as the turn is injected, and the verdict must not land
	// after the answer it summarizes.
	//
	// A failed injection sends one more line after it: the verdict
	// says the review found its findings, the delivery line says the
	// agent never received them.
	//
	// A review that is not over yet carries no table: the table is
	// the review's report, and a partial report reads as a final
	// one.
	digest := ""
	if !pending {
		digest = reviewDigest(d.rec)
		// The review is over: the record knows it before the card
		// goes out. The user reads the card and may ask for a
		// continuation right away — there is nothing left to pick up.
		f.markDelivered(d.chatID, d.rec.findings, d.rec.deliveredContributions())
	}
	if d.ctx.Err() == nil {
		sendVerdict(d.ctx, d.emitter, d.chatID, d.replyTo, d.runner, d.result,
			verdict, digest)
	}

	d.report(verdict, pending)

	if pending {
		// The review is not over: nothing is injected, and the
		// findings wait in the record for the round that completes
		// the review.
		return
	}

	// The merged document — rebuilt from the record, so it carries
	// every group's findings, not only this run's. The notices of
	// every round of the review ride along: the agent reads them with
	// the findings.
	formatted := agent.FormatReviewMessage(d.rec.workspace, d.runner,
		d.rec.mergedDocument(), d.rec.notes...)
	blocks := []agent.ContentBlock{{
		Type: agent.ContentText,
		Text: formatted + reviewPromptFrame(threadShowsFindings(d.cs)),
	}}

	// The baseline is read HERE — right before the findings go out —
	// so only the injected turn's effect is measured. A change the
	// user made while the review ran is not credited to the review.
	//
	// The self-verdict waits for that turn: it is where the fixes the
	// findings asked for land.
	before := readBaseline(d.ctx, d.rec.workspace)
	selfVerdict := watchSelfVerdict(d.ctx, d.cs, d.replyTo, d.rec.workspace, d.runner,
		before, len(d.rec.findings))
	injectWork := d.cs.HoldUntilPromptEnd(d.replyTo)
	if err := d.sendBlocks(d.ctx, blocks, agentsession.WithReplyTo(d.replyTo)); err != nil {
		slog.Default().Warn("/review: AS inject failed",
			"agent", d.runner,
			"err", err,
		)
		// The refusal reaches the user through the review's own chat
		// line: the structured outcome is not a candidate, because
		// messages.OutMessageSettled is never rendered to humans.
		if line := injectionRefusalLine(err); line != "" {
			sendReviewLine(d.ctx, d.emitter, d.chatID, d.replyTo, d.runner, line)
		}
		// The findings were produced but never reached the agent —
		// the review is not complete. The findings stay in the
		// record: the next review command delivers them.
		//
		// The report says the findings were not delivered: the
		// delivery's own report said the opposite, and the two merge.
		d.work.Report(outcome.Outcome{
			Success:    false,
			Status:     http.StatusInternalServerError,
			StatusText: "review findings could not be delivered to the agent",
			Data: map[string]string{
				KeyAgent: d.runner, BoolKeyDelivered: "false",
			},
		})
		injectWork.Release()
		// No turn will run, so nothing was fixed: give the share of
		// the message's work back.
		selfVerdict.release()
		// The findings never reached the agent: they go back into the
		// cache, and the next review command delivers them.
		f.restoreFindings(d.chatID, d.rec.findings, d.rec.deliveredContributions())
		return
	}
}

// injectionRefusalLine renders the reason the injection was refused,
// for the chat.
//
// Every refusal earns a line: the findings were produced and the
// agent never received them, and the structured outcome is not
// rendered to humans — messages.OutMessageSettled never reaches the
// chat. A silent failure would leave the user believing the agent
// is already fixing what the review found.
//
// A stalled turn names the commands that end it; a busy one says the
// turn is worth waiting out.
//
// The line rides the review's own chat path (messages.OutReply).
func injectionRefusalLine(err error) string {
	var refused *agentsession.InjectionRefused
	if !errors.As(err, &refused) {
		return ""
	}
	if !refused.Stalled {
		return "The findings were not delivered to the agent: " + refused.Reason +
			". They stay in the review record — run /review again to hand them over."
	}
	return "The findings were not delivered to the agent: " + refused.Reason + "."
}

// report tells the message what the run did.
//
// A review that is delivered is a success — unless a group died for
// good, which is not a clean result. A review that is not over yet is
// incomplete: the findings are in, but the review is not.
func (d reviewDelivery) report(verdict verdict, pending bool) {
	success := true
	status := http.StatusOK
	summary := "review delivered"
	if pending || len(verdict.failed) > 0 {
		success = false
		status = http.StatusInternalServerError
		summary = fmt.Sprintf("%d of %d groups unfinished", len(verdict.failed), verdict.groups)
	}
	data := d.data
	if data == nil {
		data = map[string]string{}
	}
	if verdict.groups > 0 {
		data[KeyUnfinished] = strconv.Itoa(len(verdict.failed))
	}
	if verdict.findings > 0 {
		data[KeyFindings] = strconv.Itoa(verdict.findings)
	}
	data[BoolKeyDelivered] = strconv.FormatBool(!pending)
	d.work.Report(outcome.Outcome{
		Success:    success,
		Status:     status,
		StatusText: summary,
		Data:       data,
	})
}

// markDelivered empties the chat's cache of the findings that were
// delivered, marks the outputs that reached the agent and marks the
// record as delivered.
//
// The groups' states stay: they are what a later `--continue` reads.
func (f *Factory) markDelivered(chatID string, delivered []reviewFinding, outputs []reviewContribution) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.records[chatID]
	if !ok {
		return
	}
	rec.findings = withoutFindings(rec.findings, delivered)
	rec.contributions = markContributions(rec.contributions, outputs, true)
	rec.delivered = len(rec.findings) == 0
	f.records[chatID] = rec
}

// restoreFindings puts the findings that never reached the agent back
// into the chat's cache: the review is not over, and the next review
// command delivers them.
func (f *Factory) restoreFindings(chatID string, findings []reviewFinding, outputs []reviewContribution) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.records[chatID]
	if !ok {
		return
	}
	for _, f := range findings {
		if holdsFinding(rec.findings, f.groupKey, f.finding) {
			continue
		}
		rec.findings = append(rec.findings, f)
	}
	rec.contributions = markContributions(rec.contributions, outputs, false)
	rec.delivered = false
	f.records[chatID] = rec
}

// markContributions marks the given outputs as delivered (or not) in
// the record's contributions.
func markContributions(contributions, outputs []reviewContribution, delivered bool) []reviewContribution {
	if len(outputs) == 0 {
		return contributions
	}
	marked := make([]reviewContribution, 0, len(contributions))
	for _, c := range contributions {
		for _, o := range outputs {
			if c.source == o.source && c.pattern == o.pattern && c.text == o.text {
				c.delivered = delivered
				break
			}
		}
		marked = append(marked, c)
	}
	return marked
}

// withoutFindings returns the findings that are not in the delivered
// set.
func withoutFindings(findings, delivered []reviewFinding) []reviewFinding {
	if len(delivered) == 0 {
		return findings
	}
	gone := make(map[reviewFinding]bool, len(delivered))
	for _, f := range delivered {
		gone[f] = true
	}
	var kept []reviewFinding
	for _, f := range findings {
		if gone[f] {
			continue
		}
		kept = append(kept, f)
	}
	return kept
}
