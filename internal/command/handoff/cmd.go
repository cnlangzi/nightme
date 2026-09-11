// Package handoff implements the `/handoff` slash command.
//
// /handoff serializes the current task into
// `<cwd>/.nightme/handoff.md` so the next AI coding agent (or a
// fresh session of the same one) can pick up the task with zero
// prior context. The Agent receives the embedded handoff prompt
// and writes the canonical file itself via its Write tool; this
// package queues the prompt, pre-creates the `.nightme` directory
// so the Agent doesn't burn a turn on mkdir, and post-verifies
// that the file landed on disk before posting a success reply.
package handoff

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
)

// handoffDir is the per-project directory the handoff lives in.
// Pre-created by /handoff so the Agent doesn't waste a turn on
// mkdir. Mirrored by internal/command/resume/handoffDir — both
// sides read/write the same path, so a rename here MUST be
// applied there too.
const handoffDir = ".nightme"

// handoffFilename is the on-disk filename written inside
// handoffDir. Fixed name so /resume can locate it without args.
// Lives under the chat's active CWD, not the user's $HOME — the
// handoff is per-project, not per-user.
const handoffFilename = "handoff.md"

// handoffPath is the full relative path; concatenation done once
// here so the runtime reply strings don't drift.
const handoffPath = handoffDir + string(filepath.Separator) + handoffFilename

// handoffPrompt is the Agent's task for /handoff. The Agent has
// the chat's full context; it produces a Markdown document
// conforming to the structure described below and writes it to
// ./.nightme/handoff.md via its Write tool.
const handoffPrompt = `You are performing a task handoff for the CURRENT task.
Your job is to create a durable handoff document for the current project so that another AI coding agent (or a fresh session of yourself) can continue the task with ZERO prior context and become productive within 2 minutes.
The canonical handoff file is:
./.nightme/handoff.md
This handoff belongs to the CURRENT PROJECT. Do not store it in the user's home directory or outside the current project.
Do not merely generate the handoff as chat output. You must actually create or overwrite ./.nightme/handoff.md with the final handoff content.
The purpose of this document is NOT to summarize the conversation. It is to serialize the current task state so another agent can safely continue from where the previous agent stopped.
Use only information available in the current conversation/session and current task context.
Preserve:
- the current task and success criteria
- completed work
- unfinished work
- current blockers
- concrete next actions
- confirmed-working approaches
- important unknowns and risks
- constraints that affect future execution
Prefer actionable state over narrative. Omit discussion that does not affect future execution.
You may infer implicit state only when it is directly supported by the sequence of actions, decisions, or outcomes in the available context.
Never invent facts, causes, files, commands, test results, implementation details, or decisions.

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

## Persistence Requirements
- Create the directory ./.nightme/ if it does not exist.
- Write the complete final handoff document to ./.nightme/handoff.md.
- Create the file if it does not exist.
- Overwrite the existing ./.nightme/handoff.md; do not append to an older handoff.
- The file content must be exactly the final handoff document and must begin with # Handoff.
- Verify after writing that ./.nightme/handoff.md exists and contains the newly generated handoff.
- Do not create or use ~/.handoff.md, ~/.nightme/handoff.md, ./handoff.md, or any other handoff location.

## Output Behavior
The handoff document belongs in ./.nightme/handoff.md, not in the chat response.
After successfully writing and verifying the file, respond only with a concise confirmation that the handoff was saved.
Do not print the entire handoff document in the response.
If writing or verifying ./.nightme/handoff.md fails, report the failure clearly and do not claim that the handoff was saved.

## Content Rules
- Write the handoff in the language predominantly used by the user.
- Preserve code, identifiers, commands, file paths, API names, and error messages verbatim.
- Replace all bracketed instructions with actual content. Never output the bracketed instructions themselves.
- Completed means actually done or verified, not merely discussed, proposed, or planned.
- In Progress means work has started but is not complete.
- Blocked means something currently prevents or materially delays progress.
- Do not list optional improvements, resolved issues, or inconveniences as blockers.
- Planned means not yet started and identified as a useful next action. Order items by execution priority or dependency when possible.
- Confirmed Working requires evidence from execution, testing, verification, or an explicit successful outcome.
- Unknowns / Risks should contain unresolved questions or credible risks that may affect future work.
- Do not claim an approach failed merely because it was replaced, discussed, or not chosen.
- Do not invent reasons for failures, decisions, blockers, or design choices.
- Preserve important negative knowledge when it materially affects future execution, but express it as current state or constraint rather than speculative failure history.
- Include file paths, symbols, commands, configuration names, and relevant error messages when they help the next agent continue the work.
- Prefer concrete references over vague descriptions. For example, prefer internal/command/handoff over “the command package”.
- Do not repeat the full history of the task.
- Compress the past into the smallest state representation that preserves correct continuation.
- Every item should help answer at least one of these questions:
  - What is done?
  - What is happening now?
  - What is blocking progress?
  - What should happen next?
  - What must the next agent know?
- If a section has no entries, write None.`

// handoffTimeout caps how long /handoff waits for the Agent to
// finish writing the document. 5 minutes is the budget; longer
// than /wiki init's was reserved for file-writing, but here the
// Agent still has to reason about chat history before it commits.
const handoffTimeout = 5 * time.Minute

// Factory is the command.SlashCommandFactory for /handoff.
type Factory struct{}

// NewFactory constructs a Factory.
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
		Summary:  "Serialize the current task into ./.nightme/handoff.md so the next agent can continue.",
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
//  2. Active-agent preflight (SelectedAgent + LookupSelectedAgentSession).
//  3. Reject trailing args / any flag via ParseCmdArgs.
//  4. input.MessageID guard — ChatSession.QueueUserMessage
//     silently no-ops on empty ID; /queue has the same guard at
//     queue/cmd.go:143. Without it, a synthetic inbound would
//     get the ack while enqueuing nothing.
//  5. MkdirAll <cwd>/.nightme so the Agent doesn't waste a turn
//     creating the directory itself.
//  6. Queue the embedded handoff prompt as a discrete
//     MessageKindQueue Prompt batch.
//  7. Spawn a goroutine on a detached context.Background: the
//     slash lifetime is short, but we still need to wait for the
//     Agent to finish so we can stat the file. The goroutine
//     posts the final reply via the chat's Emitter.
//
// Reply kind: command.Reply (→ OutCommandReply) for the ack and
// OutReply (via Emitter) for the final reply.
func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices,
	mgr *chatsession.Manager, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {

	if cs == nil {
		return command.Reply(ctx, rt, "No active chat session."), nil
	}
	cwd, failOut := command.RequireActiveCwd(cs)
	if failOut != nil {
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

	if input.MessageID == "" {
		return command.Reply(ctx, rt,
			"Internal: missing message id; /handoff did not enqueue."), nil
	}

	if err := os.MkdirAll(filepath.Join(cwd, handoffDir), 0o755); err != nil {
		return command.Reply(ctx, rt,
			fmt.Sprintf("❌ /handoff: cannot create %s: %v", handoffDir, err)), nil
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
		final := verifyHandoff(workCtx, cs, input.MessageID, filepath.Join(cwd, handoffPath))
		em := cs.Emitter()
		if em == nil {
			slog.Warn("handoff: emitter nil; final reply dropped",
				"chat_id", input.ChatID, "message_id", input.MessageID,
				"final", final)
			return
		}
		if err := em.Send(context.Background(), messages.OutboundMessage{
			ChatID: input.ChatID, ReplyTo: input.MessageID,
			Kind: messages.OutReply, Text: final,
		}); err != nil {
			slog.Warn("handoff: final reply send failed",
				"chat_id", input.ChatID, "message_id", input.MessageID,
				"err", err)
		}
	}()

	return command.Reply(ctx, rt, "⏳ /handoff queued; agent is writing ./.nightme/handoff.md."), nil
}

// verifyHandoff waits for the Agent's PromptEnd signal then
// checks the canonical handoff file on disk. The Agent writes
// the file itself via its Write tool (per the embedded prompt's
// Persistence Requirements); this function does no text capture
// and no file write — it is a pure verification gate so the
// user gets explicit feedback when the Agent fails to follow
// the persistence rules.
//
// Returns the user-facing reply text — either a success summary
// naming the verified file, or a ❌ error line. The caller posts
// this through the chat's Emitter as OutReply.
func verifyHandoff(ctx context.Context, cs *chatsession.ChatSession, msgID, absPath string) string {
	if err := waitForPromptEnd(ctx, cs, msgID, handoffTimeout); err != nil {
		return "❌ /handoff: " + err.Error()
	}
	return verifyHandoffFile(absPath)
}

// verifyHandoffFile is the pure verification gate: stat the
// canonical handoff path and return the user-facing reply text.
// Split out from verifyHandoff so the file-check logic is
// testable from an internal test file (same package, lowercase
// call site — see verify_internal_test.go) without spinning up
// an AgentEventBus / PromptEndBus round-trip.
func verifyHandoffFile(absPath string) string {
	info, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Sprintf("❌ /handoff: agent did not write %s; rerun /handoff or paste the handoff into the file manually.", handoffPath)
		}
		return fmt.Sprintf("❌ /handoff: stat %s failed: %v", absPath, err)
	}
	if info.Size() == 0 {
		return fmt.Sprintf("❌ /handoff: %s exists but is empty; rerun /handoff.", handoffPath)
	}
	return fmt.Sprintf("✅ /handoff\n\n%s saved (%d bytes).", handoffPath, info.Size())
}

// waitForPromptEnd blocks until PromptEndBus delivers an event
// for userMsgID, or ctx is cancelled, or the timeout elapses.
// Returns nil on clean termination; non-nil otherwise. Mirrors
// the helper in internal/command/wiki/init.go; kept local
// because /handoff is the only call site (after the
// prompt/collector split) and sharing it would mean giving
// /wiki init a new dependency on this package for nothing.
func waitForPromptEnd(ctx context.Context, cs *chatsession.ChatSession, userMsgID string, timeout time.Duration) error {
	if cs == nil || cs.PromptEndBus == nil {
		return fmt.Errorf("PromptEndBus unavailable")
	}
	evCh := make(chan chatsession.PromptEndedEvent, 1)
	unsub := cs.PromptEndBus.Subscribe(func(e chatsession.PromptEndedEvent) bool {
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
