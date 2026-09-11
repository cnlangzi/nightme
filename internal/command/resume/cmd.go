// Package resume implements the `/resume` slash command.
//
// /resume is the companion to /handoff: in a fresh chat session
// (or a session that wants to keep the existing context but treat
// the captured handoff as authoritative), it reads
// <cwd>/handoff.md and queues the embedded resume prompt +
// document body as a single discrete Prompt. The Agent then
// reads the embedded handoff and continues the work productively
// without any further human ceremony.
//
// Companion semantics:
//
//   - /handoff — current Agent context → ./handoff.md
//   - /resume  — ./handoff.md        → current Agent context
//
// The two commands are designed to round-trip across sessions:
// /handoff in session A produces handoff.md; the user (or a
// start-up hook) loads handoff.md into session B's CWD and runs
// /resume to continue. No shared runtime state is required.
//
// Reply kind: OutReply (mirrors /queue and /steer — /resume is a
// per-turn continuation that injects a message into the Agent's
// input stream, so the channel adapter folds the ack into the
// same rolling-log card as the Agent's reply rather than emitting
// a separate one-shot bubble).
//
// Factory holds *chatsession.Manager indirectly (cs comes from
// the dispatcher parameter at Handle time).
package resume

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
)

// handoffFile is the on-disk filename /resume reads. Kept in
// sync with internal/command/handoff/cmd.go's handoffFile —
// /handoff writes this name and /resume reads the same name so
// neither side needs args.
const handoffFile = "handoff.md"

// Factory is the command.SlashCommandFactory for /resume.
type Factory struct{}

// NewFactory constructs a Factory. command/* factories do not
// receive a *chatsession.Manager — cs comes from the dispatcher
// parameter at Handle time.
func NewFactory() *Factory { return &Factory{} }

// init self-registers the /resume builder. SetDeps (called once
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
		Name:     "resume",
		Summary:  "Continue the task described by ./handoff.md in the current session.",
		Usage:    "/resume",
		Category: "session",
	}
}

// resumeSpec declares /resume's argv grammar for the shared lexer
// (issue #291): no flags, no positional args. /resume is
// single-action; any arg is a usage error rather than silently
// dropped, mirroring /stop and /handoff's contracts.
var resumeSpec = command.CmdSpec{
	Name:    "/resume",
	Usage:   "/resume",
	MinArgs: 0,
	MaxArgs: 0,
}

// Handle implements command.SlashCommandFactory.
//
// Flow:
//
//  1. ChatSession + active CWD preflight (RequireActiveCwd).
//  2. Active-agent preflight (SelectedAgent + LookupSelectedAgentSession)
//     — same shape as /wiki init and /handoff: QueueUserMessage
//     → TryFlush only rewind on a missing selectedAS, never
//     look it up.
//  3. Reject trailing args / any flag via ParseCmdArgs.
//  4. Read <cwd>/handoff.md. Missing or unreadable file →
//     reply "no handoff; run /handoff first" (NOT a fatal
//     error — the user can run /handoff and retry).
//  5. Build a MessageKindQueue Message whose Body is the
//     embedded resume prompt + the file contents, then
//     cs.QueueUserMessage enqueues it as its own discrete
//     Prompt batch.
//  6. Reply with OutReply ack so the channel folds it into the
//     same rolling-log card as the Agent's continuation.
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

	if _, err := command.ParseCmdArgs(input.Args[1:], resumeSpec); err != nil {
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}

	handoffPath := filepath.Join(cwd, handoffFile)
	body, err := os.ReadFile(handoffPath)
	if err != nil {
		if os.IsNotExist(err) {
			return command.OutReply(input,
				fmt.Sprintf("❌ /resume: %s not found in workspace; run /handoff first.", handoffFile)), nil
		}
		return command.OutReply(input,
			fmt.Sprintf("❌ /resume: read %s failed: %v", handoffPath, err)), nil
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return command.OutReply(input,
			fmt.Sprintf("❌ /resume: %s is empty; run /handoff again.", handoffFile)), nil
	}

	msg := chatsession.Message{
		ID:     input.MessageID,
		ChatID: input.ChatID,
		Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: buildResumeBody(string(body))}},
		Kind:   chatsession.MessageKindQueue,
	}
	if err := cs.QueueUserMessage(msg); err != nil {
		return command.OutReply(input, fmt.Sprintf("Queue failed: %v", err)), nil
	}

	return command.OutReply(input, "🔄 Resuming task from ./handoff.md…"), nil
}

// resumePromptPrefix is the fixed preamble /resume sends to the
// Agent. The Agent receives the rest of the body inline below
// (a separator + the handoff document), so it has both the
// resume protocol and the captured state in a single Prompt.
//
// The handoff document body is appended verbatim after the
// separator; the Agent reads it as-is. The Agent's only job is
// to follow the protocol in resumePromptPrefix.
const resumePromptPrefix = `You are resuming an existing task from a previous AI coding agent.
A handoff document is available at: ./handoff.md

Your job is to continue the task from the exact state described in that document, without unnecessarily repeating work that has already been completed.

The previous agent had different context from you. Treat the handoff as the primary source of task state, but verify important claims against the current workspace before acting. The handoff may describe work that was completed, partially completed, blocked, planned, or merely believed to be working.

Do not restart the task from scratch.

## Resume Procedure
Follow this order:
1. Read ./handoff.md.
2. Identify the current task, success criteria, completed work, in-progress work, blockers, and planned next actions.
3. Inspect the current workspace as needed to verify the handoff's important claims.
4. Identify the earliest unfinished action that can make meaningful progress.
5. Continue executing the task from that point.
6. Reuse confirmed-working approaches and respect documented constraints.
7. Do not repeat completed work unless verification shows that it is incomplete, broken, or inconsistent with the current workspace.
8. If a documented blocker is still present, investigate whether it can now be resolved before moving on.
9. If the handoff contains stale or incorrect information, trust the current workspace and actual execution results over the document, and adapt accordingly.

## Continuation Rules
- Completed items are presumed done, but verify them when doing so is necessary to safely continue.
- In Progress is normally the highest-priority continuation point.
- Blocked items should be revisited when the blocker may now be resolvable.
- Planned items are the fallback execution queue after unfinished in-progress work and actionable blockers.
- Confirmed Working contains approaches that should be preferred when still applicable.
- Unknowns / Risks are things to investigate only when they materially affect the next step.
- Do not redo work merely to reproduce the previous agent's process.
- Do not follow planned steps blindly when the current workspace shows that a different order is necessary.
- Preserve existing implementation decisions unless there is concrete evidence that they are incorrect.
- When the handoff and the current workspace disagree, use observable current state as the source of truth.

## Execution Principles
Act as the engineer taking over an interrupted task, not as a reviewer writing a report.
Be decisive and continue execution whenever the next action is sufficiently clear.
Prefer:
- inspecting existing code over re-deriving context from scratch
- continuing partial work over restarting it
- using existing patterns over introducing new ones
- verifying assumptions with concrete evidence
- resolving blockers before speculative improvements
- completing the smallest coherent next step before broad refactoring
Do not:
- summarize the handoff back to the user
- explain the handoff document unless necessary to resolve ambiguity
- ask for information that can be discovered from the repository, workspace, or available tools
- repeat already-completed implementation work without evidence that it needs to be redone
- invent missing context
- treat handoff.md as more authoritative than the actual repository or execution results

## When the Handoff Is Incomplete
If ./handoff.md is missing, unreadable, empty, or clearly malformed:
1. Inspect the current repository and working state.
2. Infer the task only from available evidence.
3. Continue only when the intended task and next action are sufficiently clear.
4. Do not fabricate a handoff or pretend that previous context is known.

## When the Handoff Is Stale
If the current workspace has advanced beyond the handoff:
- Preserve the newer state.
- Do not roll back completed work merely to match the document.
- Continue from the actual current state.
- Treat the handoff as historical context rather than the source of truth.

## Output Behavior
Do not produce a handoff or progress summary before working.
Start by reading ./handoff.md and inspecting the relevant current state.
Then continue executing the task.

Your objective is simple:
Make the next correct change toward completing the task.

---

The captured handoff document from the previous agent follows below this line. Treat it as the primary source of task state, but verify its important claims against the current workspace before acting on them.
`

// buildResumeBody concatenates the resume protocol with the
// captured handoff body in a single Prompt. A blank line + a
// fenced-style separator inside the literal separates the two so
// the Agent can tell where the protocol ends and the handoff
// begins.
func buildResumeBody(handoffBody string) string {
	var b strings.Builder
	b.Grow(len(resumePromptPrefix) + len(handoffBody) + 64)
	b.WriteString(resumePromptPrefix)
	if handoffBody != "" {
		// Normalize: trim trailing whitespace on the handoff
		// body so the separator reads cleanly. The body is
		// already plain Markdown (no fences), so a single
		// trailing newline is enough.
		b.WriteString(strings.TrimRight(handoffBody, "\r\n"))
		b.WriteString("\n")
	}
	return b.String()
}

// Compile-time check: Factory satisfies SlashCommandFactory.
var _ command.SlashCommandFactory = (*Factory)(nil)
