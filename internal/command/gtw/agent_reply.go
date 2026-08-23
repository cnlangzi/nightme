// Package gtw — agent invocation + unified reply sink.
//
// Why this file exists (F-CLAUDE-PRINT-002 follow-up):
//
// The runtime event pipeline stamps AgentName/Model/SessionID/Usage
// onto every OutboundMessage via internal/runtime/handler.go:184-195
// (per-event stamping from AgentSession state) + gateway.Translate
// (Usage from EventAgentResult.Usage). The footer renderer in
// internal/statusbar/statusbar.go reads those flat fields back off
// the OutboundMessage and renders Line 1 (agentbar: 🤖: Agent ·
// Model · SessionID) + Line 2 (usagebar: 💰:「 in/out · X% · $cost 」).
//
// GTW bypasses the runtime event pipeline (it uses agent.RunOnce
// directly in the caller's goroutine, draining the bridge's event
// stream into a private RunResult). Without this file, the
// success-path OutboundMessage built by gtw.reply carries no
// AgentName/Model/SessionID/Usage, so the footer renders only Line 3
// (gitbar — that one is stamped at the Emitter chokepoint via
// stampGitStatus) and silently drops the agentbar / usagebar.
//
// The fix: a single gtw-package sink (replyAgent) that consumes the
// agent.RunResult returned by RunOnce and stamps its non-empty
// fields onto the OutboundMessage before em.Send. All GTW
// dispatchers (commit / pr — the two that actually invoke an agent)
// route through replyAgent on their success paths.
//
// Why consume agent.RunResult directly instead of building a new
// AgentStamp wrapper struct: RunResult is the canonical "one agent
// call's full output" type defined in the agent package; the
// fields we need (Model / SessionID / Usage) are already there.
// Adding AgentStamp would just re-declare the same fields plus an
// apply method — zero net information, pure shape duplication.
//
// AgentName is the lone field NOT in RunResult (it's the "selected
// agent" caller knew before RunOnce, not part of RunResult's
// "output" semantics), so it travels as a separate parameter.
package gtw

import (
	"context"
	"fmt"
	"strings"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/gateway/outbound"
	"github.com/cnlangzi/nightme/internal/messages"
)

// runAgentFor is the GTW-internal one-shot agent invoker shared by
// /gtw commit and /gtw pr. Replaces the previous runAgentToCommit
// (commit.go) and the inlined agent.Builtins.Get + Detect + RunOnce
// block in pr.go's dispatchPR.
//
// The agent name resolution order (CLI -a > yml <commit/pr>.agent
// > cs.SelectedAgent()) is the caller's responsibility — pass the
// already-resolved cliAgent / ymlAgent in. The workspace + prompt
// are also caller's responsibility: commit builds its prompt via
// buildAgentPrompt(c) on the worktree; pr builds via
// buildPRPrompt(c, baseBranch). runAgentFor doesn't know about
// prompts — it just spawns and drains.
//
// Returns:
//
//   - res      — full RunResult (Text / Model / SessionID / Usage).
//     Empty RunResult when the spawn itself fails before producing
//     any bridge events (very rare; preserves a usable value type).
//   - agentName — the resolved name (also returned on the failure
//     path so the caller can surface "agent X failed: …").
//   - err      — non-nil when the spawn failed, the agent binary
//     was missing (Detect failed), or RunOnce returned an error.
//     The error message is already user-presentable; the caller
//     pastes it into the IM reply verbatim.
//
// All failure modes that should short-circuit the dispatcher (no
// agent selected, unknown agent, binary missing, RunOnce errored)
// are surfaced through (res, agentName, err); the caller decides
// whether to convert into an IM reply.
//
// Contract: caller is responsible for the per-invocation context
// timeout if it wants one — runAgentFor does NOT wrap ctx with
// timeouts.Agent itself. dispatchCommit / dispatchPR both wrap
// ctx with timeouts.Agent at their call sites (the timeout is
// per-command-policy, not per-agent-call).
func runAgentFor(
	ctx context.Context,
	cs *chatsession.ChatSession,
	workspace, prompt, chatID, messageID string,
	cliAgent, ymlAgent string,
) (agent.RunResult, string, error) {
	agentName, agentNotes := ResolveAgent(cliAgent, ymlAgent, cs)
	if agentName == "" {
		var msg strings.Builder
		msg.WriteString("❌ no agent selected. Send `/use <name>` first or pass `-a <name>`.")
		for _, n := range agentNotes {
			msg.WriteByte('\n')
			msg.WriteString(n)
		}
		return agent.RunResult{}, "", fmt.Errorf("%s", msg.String())
	}

	a, err := agent.Builtins.Get(agentName)
	if err != nil {
		return agent.RunResult{}, agentName, fmt.Errorf("❌ unknown agent %q (check `nightme agents` or your config)", agentName)
	}
	if err := a.Detect(); err != nil {
		return agent.RunResult{}, agentName, fmt.Errorf("❌ agent %s not available: %v", agentName, err)
	}

	blocks := []agent.ContentBlock{{
		Type: agent.ContentText,
		Text: prompt,
	}}

	// Wire up the sink so the user sees the agent's intermediate
	// events (thinking, tool calls, tool results, …) streaming
	// into the chat while /gtw commit / /gtw pr runs. The drain
	// goroutine lives on ctx; cancellation tears it down.
	sink := outbound.StreamRunOnceToEmitter(ctx, cs.Emitter(), chatID, messageID, agentName)

	res, err := a.RunOnce(ctx,
		agent.StartConfig{Workspace: workspace},
		blocks,
		agent.WithEventSink(sink),
	)
	if err != nil {
		return res, agentName, fmt.Errorf("❌ agent %s failed: %v", agentName, err)
	}
	return res, agentName, nil
}

// replyAgent is the GTW-package unified message sink. Every
// success-path OutboundMessage built by a GTW dispatcher that
// invoked an agent flows through this function so the footer
// (agentbar / usagebar) can be rendered.
//
// Stamp application (mirrors outbound.emitImpl.stampGitStatus's
// posture: "caller didn't fill, fill it; caller already filled,
// respect it"):
//
//   - AgentName: always set from the agentName arg (caller chose
//     the agent; this is the caller's authoritative name).
//   - Model: filled from res.Model only when out.Model is empty.
//   - SessionID: filled from res.SessionID only when out.SessionID
//     is empty.
//   - Usage: filled from res.Usage when non-nil (the channel
//     footer's Line 2 is suppressed when Usage is nil — same
//     behavior as the runtime path).
//
// res is agent.RunResult{} (zero value) for callers that did not
// invoke an agent — all "if != '' / if != nil" guards below
// short-circuit naturally, leaving the OutboundMessage identical
// to the pre-fix reply(...) output. Same nil-safety as the old
// reply: em == nil drops the message and returns Consumed=true.
//
// Returns &Result{Consumed: true} so dispatchCommit / dispatchPR
// can keep their `return replyAgent(...), nil` shape.
func replyAgent(
	ctx context.Context,
	em outbound.Emitter,
	chatID, messageID, text, agentName string,
	res agent.RunResult,
) *Result {
	if em == nil {
		return &Result{Consumed: true}
	}
	out := messages.OutboundMessage{
		ChatID:    chatID,
		Kind:      messages.OutReply,
		ReplyTo:   messageID,
		Text:      text,
		AgentName: agentName,
	}
	if out.Model == "" {
		out.Model = res.Model
	}
	if out.SessionID == "" {
		out.SessionID = res.SessionID
	}
	if res.Usage != nil {
		out.Usage = (*messages.UsageInfo)(res.Usage)
	}
	_ = em.Send(ctx, out)
	return &Result{Consumed: true}
}