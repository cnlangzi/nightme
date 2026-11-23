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
	"github.com/cnlangzi/nightme/pkg/outcome"
)

// reportOutcome merges a structured outcome into the inbound
// message's work.
//
// A run cancelled by /gtw abort reports `cancelled` — the factory
// does that once, when it notices the cancellation.
//
// A failure from a cancelled run is reported as the cancellation: the
// failure is a consequence of the cancellation (the subprocess was
// killed mid-flight), and a failure outranks a cancellation in the
// merge, so keeping it would report the cancellation as a breakage.
//
// The facts the command produced still reach the program: a /gtw
// close that removed the worktree and was aborted during its sync did
// remove the worktree.
func reportOutcome(ctx context.Context, o outcome.Outcome) {
	if aborted(ctx) && !o.Success && o.Status != outcome.StatusClientClosedRequest {
		o.Status = outcome.StatusClientClosedRequest
		o.StatusText = "the command was cancelled by /gtw abort"
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
	o outcome.Outcome,
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
	o outcome.Outcome,
) *Result {
	reportOutcome(ctx, o)
	return replyAgent(ctx, em, chatID, messageID, text, agentName, res)
}
