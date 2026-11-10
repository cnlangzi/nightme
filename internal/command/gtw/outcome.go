// Package gtw — the structured outcome every /gtw exit path reports.
//
// A program that drives nightme (the `me` personal agent) judges the
// result of a command from the Outcome that rides the settled
// signal — never from the text of the card the channel renders. So
// every place where a /gtw subcommand decides what happened reports
// that decision here, next to the reply that shows the same decision
// to the user.
//
// The Work rides on the ctx: the inbound router takes it before the
// dispatch and puts it there with chatsession.WithWork (see
// internal/gateway/inbound/command.go). chatsession.ReportOutcome is
// a no-op when the ctx carries no Work, so the exported Run* functions
// keep working from tests and from the runtime without one.
//
// The status vocabulary and the Data keys are documented in
// docs/me.md §1 — that table is the contract with the programs that
// read these outcomes.
package gtw

import (
	"context"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/messages"
)

// reportOutcome merges a structured outcome into the inbound
// message's work.
//
// A run cancelled by /gtw abort reports `cancelled` — the factory
// does that once, when it notices the cancellation.
//
// A `failed` verdict from a cancelled run is dropped: the failure is
// a consequence of the cancellation (the subprocess was killed
// mid-flight), and `failed` outranks `cancelled` in the merge, so
// keeping it would report the cancellation as a failure. The other
// verdicts are kept: `ok`, `noop` and `incomplete` all rank below
// `cancelled`, so the merged status stays `cancelled` while their
// Data — the worktree that was removed, the branch that was
// deleted — still reaches the program.
func reportOutcome(ctx context.Context, o messages.Outcome) {
	if aborted(ctx) && o.Status == messages.OutcomeFailed {
		return
	}
	chatsession.ReportOutcome(ctx, o)
}

// replyOutcome reports the outcome and posts the card.
//
// Every exit path that decides a result uses it, so the card the user
// reads and the outcome a program reads cannot disagree. The plain
// reply stays for the cards that are NOT a decision point — load
// notes, hook output, the mid-flow cards of a multi-card command.
func replyOutcome(
	ctx context.Context,
	em messages.Emitter,
	chatID, messageID, text string,
	o messages.Outcome,
) *Result {
	reportOutcome(ctx, o)
	return reply(ctx, em, chatID, messageID, text)
}

// replyAgentOutcome is replyOutcome for the cards that carry the
// agent footer (/gtw commit, /gtw pr): the agent metadata is stamped
// the same way replyAgent stamps it.
func replyAgentOutcome(
	ctx context.Context,
	em messages.Emitter,
	chatID, messageID, text, agentName string,
	res agent.RunResult,
	o messages.Outcome,
) *Result {
	reportOutcome(ctx, o)
	return replyAgent(ctx, em, chatID, messageID, text, agentName, res)
}
