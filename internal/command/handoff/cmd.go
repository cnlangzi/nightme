// Package handoff implements the `/handoff` slash command.
//
// /handoff generates `./handoff.md` so the next AI coding agent
// (or a fresh session of the same one) can pick up the current
// task with zero prior context. The command:
//
//  1. Submits the embedded handoff prompt as a discrete queue
//     barrier (MessageKindQueue).
//  2. Subscribes to the chat's AgentEventBus to capture the
//     agent's text reply.
//  3. Waits for the matching PromptEndBus signal (5-minute
//     ceiling).
//  4. Writes the captured text verbatim to <cwd>/handoff.md.
//
// Pattern mirrors /wiki init (internal/command/wiki/init.go): the
// slash handler returns an immediate ack, a goroutine does the
// long-running work and posts the final reply through the chat's
// Emitter. The collector helpers (startCollector / stopCollector /
// waitForPromptEnd) are intentionally duplicated rather than
// shared with /wiki — the only consumer of either pattern is one
// command each, and pulling the helpers into a shared package
// would create a third import edge for a 30-line helper.
//
// Factory holds *chatsession.Manager indirectly (cs comes from
// the dispatcher parameter at Handle time).
package handoff

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentsession"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
)

// handoffPrompt is the Agent's task for /handoff. The Agent has
// the chat's full context; it produces a Markdown document
// conforming to the structure described below. Its reply text
// (EventAgentText + EventAgentResult) is captured verbatim and
// written to <cwd>/handoff.md.
//
// The prompt is wrapped verbatim from the design spec — the Agent
// owns all the section logic and validation.
const handoffPrompt = `You are generating a handoff document for the CURRENT task.
Your reader is the next AI coding agent (or a fresh session of yourself). It has ZERO prior context and must be able to continue the work productively within 2 minutes.
The purpose of this document is not to summarize the conversation. It is to serialize the current task state so another agent can safely continue from where the previous agent stopped.
Use only information available in the current conversation/session. Preserve concrete facts, decisions, verified results, important constraints, and actionable next steps.
You may infer implicit state only when it is directly supported by the sequence of actions, decisions, or outcomes in the conversation. Never invent facts, causes, files, commands, test results, implementation details, or decisions.
Prefer actionable state over narrative. Omit discussion that does not affect future execution.

# Handoff

## Task
[Describe what is being built, fixed, or investigated, and what success looks like. 2-4 sentences.]

## Completed
- [specific item]: [what was actually completed, changed, or verified]

## In Progress
- [specific item]: [what has already been done] → [what remains to finish]

## Blocked
- [blocker]: [what currently prevents or materially delays progress] → [what is needed to unblock it]

## Planned
1. [next action — start with a verb and be specific]
2. [next action — start with a verb and be specific]

## Execution Notes

### Confirmed Working
- [approach/configuration/pattern]: [what was actually verified or demonstrated to work]

### Unknowns / Risks
- [unknown or risk]: [known evidence, current uncertainty, and potential impact]

## Constraints
- Output ONLY the handoff document. The response must begin with # Handoff and contain nothing before or after it.
- Write the handoff in the language predominantly used by the user. Preserve code, identifiers, commands, file paths, API names, and error messages verbatim.
- Replace all bracketed instructions with actual content. Never output the bracketed instructions themselves.
- Completed means actually done or verified, not merely discussed, proposed, or planned.
- In Progress means work has started but is not complete.
- Blocked means something currently prevents or materially delays progress. Include the concrete blocker and, when known, the condition or action required to unblock it.
- Do not list optional improvements, resolved issues, or inconveniences as blockers.
- Planned means not yet started and identified as a useful next action. Order items by execution priority or dependency when possible.
- Confirmed Working requires evidence from execution, testing, verification, or an explicit successful outcome in the conversation.
- Unknowns / Risks should contain unresolved questions or credible risks that may affect future work. Do not turn guesses into facts.
- Do not claim an approach failed merely because it was replaced, discussed, or not chosen.
- Do not invent reasons for failures, decisions, blockers, or design choices.
- Preserve important negative knowledge when it materially affects future execution, but express it as current state or constraint rather than inventing a failure history.
- Include file paths, symbols, commands, configuration names, and relevant error messages when they help the next agent locate or continue the work.
- Prefer concrete references over vague descriptions. For example, prefer internal/command/handoff over “the command package”.
- Do not repeat the full history of the task. Compress the past into the smallest state representation that preserves correct continuation.
- Every item should answer one of these questions: What is done? What is happening now? What is blocking progress? What should happen next? What must the next agent know?
- If a section has no entries, write None.`

// handoffTimeout caps how long /handoff waits for the Agent to
// produce the document. Generating a handoff doc is lighter than
// /wiki init (which also writes files), but the Agent still needs
// to reason about the chat history; 5 minutes is the budget.
const handoffTimeout = 5 * time.Minute

// handoffFile is the on-disk filename written under the chat's
// active CWD. Fixed name so /resume can locate it without args.
const handoffFile = "handoff.md"

// Factory is the command.SlashCommandFactory for /handoff.
type Factory struct{}

// NewFactory constructs a Factory. command/* factories do not
// receive a *chatsession.Manager — cs comes from the dispatcher
// parameter at Handle time.
func NewFactory() *Factory { return &Factory{} }

// init self-registers the /handoff builder. SetDeps (called once
// at startup by internal/runtime) builds the factory against
// currentDeps.
func init() {
	command.RegisterBuilder(func(d command.Deps) command.SlashCommandFactory {
		return NewFactory()
	})
}

// Spec implements command.SlashCommandFactory.
func (f *Factory) Spec() command.Spec {
	return command.Spec{
		Name:     "handoff",
		Summary:  "Serialize the current task into ./handoff.md so the next agent can continue.",
		Usage:    "/handoff",
		Category: "session",
	}
}

// handoffSpec declares /handoff's argv grammar for the shared
// lexer (issue #291): no flags, no positional args. /handoff is
// single-action; any arg is a usage error rather than silently
// dropped, mirroring /stop's contract.
var handoffSpec = command.CmdSpec{
	Name:    "/handoff",
	Usage:   "/handoff",
	MinArgs: 0,
	MaxArgs: 0,
}

// Handle implements command.SlashCommandFactory.
//
// Flow:
//
//  1. ChatSession + active CWD preflight (RequireActiveCwd).
//  2. Active-agent preflight (SelectedAgent + LookupSelectedAgentSession)
//     — same shape as /wiki init: QueueUserMessage → TryFlush
//     only rewind on a missing selectedAS, never look it up.
//  3. Reject trailing args / any flag via ParseCmdArgs.
//  4. Build a MessageKindQueue Message whose Body is the
//     embedded handoff prompt; cs.QueueUserMessage enqueues it
//     as its own discrete Prompt batch.
//  5. Spawn a goroutine on a detached context.Background (the
//     slash lifetime is short — ctx cancellation would tear down
//     the collector mid-write). The goroutine waits for the
//     matching PromptEndBus event, writes <cwd>/handoff.md,
//     and posts the final reply via the chat's Emitter.
//  6. Return an immediate ack ("⏳ /handoff queued…") so the
//     channel can render the placeholder card.
//
// Reply kind: command.Reply (→ OutCommandReply) for the ack and
// OutReply (via Emitter) for the final reply. Same shape as
// /wiki init.
func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices,
	mgr *chatsession.Manager, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {

	if cs == nil {
		return command.Reply(ctx, rt, "No active chat session."), nil
	}
	if _, failOut := command.RequireActiveCwd(cs); failOut != nil {
		return failOut, nil
	}

	if cs.SelectedAgent() == "" {
		return command.Reply(ctx, rt, "❌ no active agent; run /use <agent> first"), nil
	}
	if _, err := cs.LookupSelectedAgentSession(); err != nil {
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}

	if _, err := command.ParseCmdArgs(input.Args[1:], handoffSpec); err != nil {
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}

	msg := chatsession.Message{
		ID:     input.MessageID,
		ChatID: input.ChatID,
		Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: handoffPrompt}},
		Kind:   chatsession.MessageKindQueue,
	}
	if err := cs.QueueUserMessage(msg); err != nil {
		return command.Reply(ctx, rt, fmt.Sprintf("Queue failed: %v", err)), nil
	}

	workCtx, cancel := context.WithCancel(context.Background())
	go func() {
		defer cancel()
		final := runHandoff(workCtx, cs, input.MessageID)
		em := cs.Emitter()
		if em == nil {
			return
		}
		if err := em.Send(context.Background(), messages.OutboundMessage{
			ChatID:  input.ChatID,
			ReplyTo: input.MessageID,
			Kind:    messages.OutReply,
			Text:    final,
		}); err != nil {
			slog.Warn("handoff: final reply send failed",
				"chat_id", input.ChatID, "message_id", input.MessageID,
				"err", err)
		}
	}()

	return command.Reply(ctx, rt, "⏳ /handoff queued; agent is generating handoff.md."), nil
}

// runHandoff drives the long-running part of /handoff: subscribe
// to the AgentEventBus to capture the Agent's text reply, wait
// for the matching PromptEndBus signal, then write the captured
// text verbatim to <cwd>/handoff.md.
//
// Returns the user-facing reply text — either a success summary
// naming the written file, or a ❌ error line. The caller posts
// this through the chat's Emitter as OutReply.
func runHandoff(ctx context.Context, cs *chatsession.ChatSession, msgID string) string {
	cwd := cs.SelectedCwd()
	collector := startCollector(cs, msgID)
	defer stopCollector(collector)

	if err := waitForPromptEnd(ctx, cs, msgID, handoffTimeout); err != nil {
		return "❌ /handoff: " + err.Error()
	}

	text := strings.TrimRight(collector.text(), "\n")
	if text == "" {
		return "❌ /handoff: agent produced no output; handoff.md not written"
	}

	path := filepath.Join(cwd, handoffFile)
	if err := os.WriteFile(path, []byte(text+"\n"), 0o644); err != nil {
		return fmt.Sprintf("❌ /handoff: write %s failed: %v", path, err)
	}
	return formatSuccessReply(path, text)
}

// formatSuccessReply composes the /handoff success line.
// Echoes the written path and the document's byte / line counts
// so the user can sanity-check that the Agent produced a
// non-trivial document (vs. a one-line stub).
func formatSuccessReply(path, text string) string {
	lines := strings.Count(text, "\n") + 1
	return fmt.Sprintf("✅ /handoff\n\nWrote %s (%d lines, %d bytes).",
		path, lines, len(text))
}

// eventCollector accumulates text events from a single Agent
// prompt so the runtime can write the captured reply to disk.
// Mirrors the collector in internal/command/wiki/init.go; kept
// package-local because there are exactly two consumers
// (/handoff, /wiki init) and they don't share enough context to
// justify a shared helper package yet.
type eventCollector struct {
	mu     sync.Mutex
	buf    strings.Builder
	cancel func()
}

func (c *eventCollector) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func (c *eventCollector) append(ev *agent.AgentEvent) {
	if ev == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch ev.Kind {
	case agent.EventAgentText:
		c.buf.WriteString(ev.Text)
		c.buf.WriteString("\n")
	case agent.EventAgentResult:
		if ev.Result != nil && ev.Result.Text != "" {
			c.buf.WriteString(ev.Result.Text)
			c.buf.WriteString("\n")
		}
	}
}

// startCollector subscribes to AgentEventBus for the duration of
// the prompt. The returned handle's text() reflects the Agent's
// accumulated reply as the prompt progresses.
func startCollector(cs *chatsession.ChatSession, userMsgID string) *eventCollector {
	c := &eventCollector{}
	if cs == nil || cs.AgentEventBus == nil {
		return c
	}
	unsub := cs.AgentEventBus.Subscribe(func(env chatsession.AgentEventEnvelope) bool {
		if env.UserMsgID != userMsgID {
			return false
		}
		c.append(env.Event)
		return false
	})
	c.cancel = unsub
	return c
}

func stopCollector(c *eventCollector) {
	if c == nil || c.cancel == nil {
		return
	}
	c.cancel()
}

// waitForPromptEnd blocks until PromptEndBus delivers an event
// for userMsgID, or ctx is cancelled, or the timeout elapses.
// Returns nil on clean termination; non-nil otherwise.
func waitForPromptEnd(ctx context.Context, cs *chatsession.ChatSession, userMsgID string, timeout time.Duration) error {
	if cs == nil || cs.PromptEndBus == nil {
		return fmt.Errorf("PromptEndBus unavailable")
	}
	evCh := make(chan agentsession.PromptEndedEvent, 1)
	unsub := cs.PromptEndBus.Subscribe(func(e agentsession.PromptEndedEvent) bool {
		if e.UserMsgID != userMsgID {
			return false
		}
		select {
		case evCh <- e:
		default:
		}
		return true
	})
	defer unsub()

	if ctx == nil {
		ctx = context.Background()
	}
	timeoutCh := time.NewTimer(timeout)
	defer timeoutCh.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case ev := <-evCh:
		if ev.Reason != agent.PromptEndClean {
			return fmt.Errorf("prompt ended with reason %s", ev.Reason)
		}
		return nil
	case <-timeoutCh.C:
		return fmt.Errorf("timed out after %s", timeout)
	}
}

// Compile-time check: Factory satisfies SlashCommandFactory.
var _ command.SlashCommandFactory = (*Factory)(nil)
