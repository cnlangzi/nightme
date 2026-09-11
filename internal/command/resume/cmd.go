// Package resume implements the `/resume` slash command.
//
// /resume is the companion to /handoff: in a fresh chat session
// (or a session that wants to keep the existing context but treat
// the captured handoff as authoritative), it queues the embedded
// resume prompt for the Agent. The prompt's step 1 instructs the
// Agent to read <cwd>/.nightme/handoff.md itself, so this package
// does NOT inline the file body — the canonical disk source is
// the only authoritative copy.
//
// Companion semantics:
//
//   - /handoff — current Agent context → ./.nightme/handoff.md
//   - /resume  — ./.nightme/handoff.md  → current Agent context
//
// Reply kind: OutReply (mirrors /queue and /steer — /resume is a
// per-turn continuation that injects a message into the Agent's
// input stream, so the channel adapter folds the ack into the
// same rolling-log card as the Agent's reply rather than emitting
// a separate one-shot bubble).
package resume

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
)

// handoffDir is the per-project directory the handoff lives in.
// Mirrored by internal/command/handoff/cmd.go's handoffDir — both
// sides read/write the same path, so a rename here MUST be
// applied there too.
const handoffDir = ".nightme"

// handoffFilename is the on-disk filename /resume reads inside
// handoffDir. Fixed name so /resume can locate it without args.
// Lives under the chat's active CWD, not the user's $HOME.
const handoffFilename = "handoff.md"

// handoffPath is the full relative path; concatenation done once
// here so the runtime reply strings don't drift.
const handoffPath = handoffDir + string(filepath.Separator) + handoffFilename

// Factory is the command.SlashCommandFactory for /resume.
type Factory struct{}

// NewFactory constructs a Factory.
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
		Summary:  "Continue the task described by ./.nightme/handoff.md in the current session.",
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
//  2. Active-agent preflight (SelectedAgent + LookupSelectedAgentSession).
//  3. Reject trailing args / any flag via ParseCmdArgs.
//  4. input.MessageID guard — ChatSession.QueueUserMessage
//     silently no-ops on empty ID; mirrors the guard at
//     queue/cmd.go:143.
//  5. Stat ./.nightme/handoff.md. Missing or unreadable →
//     OutReply error hinting at /handoff (NOT a fatal error —
//     the user can run /handoff and retry).
//  6. Queue the embedded resumePromptPrefix as a discrete
//     MessageKindQueue Prompt batch. The Agent reads the
//     handoff from disk per the prompt's step 1; no inline
//     copy of the file body is appended.
//  7. Reply with OutReply ack so the channel folds it into the
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

	if input.MessageID == "" {
		return command.OutReply(input,
			"Internal: missing message id; /resume did not enqueue."), nil
	}

	handoffAbsPath := filepath.Join(cwd, handoffPath)
	info, err := os.Stat(handoffAbsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return command.OutReply(input,
				fmt.Sprintf("❌ /resume: %s not found in workspace; run /handoff first.", handoffPath)), nil
		}
		return command.OutReply(input,
			fmt.Sprintf("❌ /resume: stat %s failed: %v", handoffAbsPath, err)), nil
	}
	if info.Size() == 0 {
		return command.OutReply(input,
			fmt.Sprintf("❌ /resume: %s is empty; run /handoff again.", handoffPath)), nil
	}

	msg := chatsession.Message{
		ID:     input.MessageID,
		ChatID: input.ChatID,
		Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: resumePromptPrefix}},
		Kind:   chatsession.MessageKindQueue,
	}
	if err := cs.QueueUserMessage(msg); err != nil {
		return command.OutReply(input, fmt.Sprintf("Queue failed: %v", err)), nil
	}

	return command.OutReply(input, "🔄 Resuming task from ./.nightme/handoff.md…"), nil
}

// resumePromptPrefix is the verbatim preamble /resume sends to
// the Agent. Step 1 instructs the Agent to read ./.nightme/handoff.md
// itself; this package does not inline the file body, so the
// disk source is the single authoritative read.
const resumePromptPrefix = `You are resuming an existing task from a previous AI coding agent.
The current project may contain a handoff document at:
./.nightme/handoff.md
This file is the canonical handoff state for the CURRENT PROJECT.

Your job is to continue the existing task from the state described in that file. Do NOT restart the task from scratch.

The previous agent had different context from you. Treat the handoff as the primary source of task state, but verify important claims against the current workspace before acting. The handoff is a state snapshot, not an absolute source of truth.

## Resume Procedure
Follow this order:
1. Read ./.nightme/handoff.md.
2. Identify:
  - the current task and success criteria
  - what has been completed
  - what is still in progress
  - what is currently blocked
  - what the planned next actions are
  - confirmed-working approaches and relevant constraints
  - important unknowns or risks
3. Inspect the current project/workspace as needed to verify the important claims in the handoff.
4. Compare the handoff state with the actual current state of the project.
5. Identify the earliest unfinished action that can make meaningful progress toward the task's success criteria.
6. Continue executing the task from that point.
7. Reuse confirmed-working approaches and respect documented constraints.
8. Resolve actionable blockers before moving on to lower-priority planned work when appropriate.
9. Do not repeat completed work unless verification shows that it is incomplete, broken, reverted, or inconsistent with the current workspace.
10. Adapt the planned sequence when the actual project state makes a different order necessary.

## State Interpretation
Use these sections according to the following rules:
- Completed means work that the previous agent states is complete or verified.
- In Progress is the preferred continuation point when the work is genuinely unfinished.
- Blocked identifies conditions that currently prevent or materially delay progress. Re-check whether those blockers can now be resolved.
- Planned is the execution queue for work that has not yet started.
- Confirmed Working identifies approaches that have been verified and should be reused when still applicable.
- Unknowns / Risks identifies uncertainty that may affect execution. Investigate it when it becomes relevant to the next action.
Do not treat Planned as a rigid script.
The current workspace may reveal that:
- a planned item is already complete
- an in-progress item is actually finished
- a blocker has been resolved
- a different action is now required
- the handoff is stale
Use the actual current state to determine what to do next.

## Source of Truth
When the handoff and the current workspace disagree:
- Trust observable current workspace state over stale claims in the handoff.
- Do not roll back working changes merely to match the handoff.
- Do not assume that a claimed completion is correct without sufficient evidence when that verification matters.
- Preserve valid work already present in the repository.
- Continue from the actual current state.
Never invent missing context.
Do not invent:
- requirements
- implementation details
- test results
- files or symbols
- commands or command output
- design decisions
- reasons for previous decisions

## Execution Principles
Act as the engineer taking over an interrupted task, not as a reviewer of the previous agent.
Be decisive and continue execution whenever the next action is sufficiently clear.
Prefer:
- inspecting the existing implementation over re-deriving the entire task
- continuing partial work over restarting it
- using established project patterns over introducing unnecessary new ones
- verifying assumptions with concrete evidence
- resolving actionable blockers before speculative improvements
- completing the smallest coherent next step before broad refactoring
Do not:
- summarize the handoff back to the user before working
- reproduce the conversation history
- rewrite the handoff unnecessarily
- redo completed implementation without evidence that it needs to be redone
- ask the user for information that can be discovered from the project or available tools
- make unrelated improvements
- restart from a clean slate merely because the codebase is unfamiliar

## Handoff File Errors
If ./.nightme/handoff.md does not exist:
1. Inspect the current project and working state.
2. Determine whether the task can be identified from the current context.
3. Continue when the intended task and next action are sufficiently clear.
4. Do not fabricate a missing handoff.
If ./.nightme/handoff.md exists but is empty, malformed, or clearly incomplete:
1. Use whatever valid information it contains.
2. Inspect the current project to reconstruct the missing state.
3. Continue from the earliest actionable unfinished work.
4. Do not assume missing information.
If the handoff is stale:
- Preserve newer work in the current workspace.
- Do not revert changes merely to match the document.
- Continue from the actual current state.

## Output Behavior
Do not output a summary of the handoff before executing the task.
Do not ask for confirmation before continuing when the next action is sufficiently clear.
Start by reading:
./.nightme/handoff.md
Then inspect the relevant current project state and continue executing the task.

Your primary objective is:
Continue the existing task from the correct current state and make the next correct change toward completing it.
Only respond to the user when a meaningful execution result, blocker, required decision, or other user-facing information needs to be communicated.
`

// Compile-time check: Factory satisfies SlashCommandFactory.
var _ command.SlashCommandFactory = (*Factory)(nil)
