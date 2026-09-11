// Package resume implements the `/resume` slash command.
//
// /resume is the companion to /handoff: in a fresh chat session
// (or a session that wants to keep the existing context but treat
// the captured handoff as authoritative), it queues the embedded
// resume prompt for the Agent. The prompt's step 1 instructs the
// Agent to read the named handoff file itself, so this package
// does NOT inline the file body — the canonical disk source is
// the only authoritative copy.
//
// Companion semantics:
//
//   - /handoff <name> — current Agent context → ~/.nightme/handoff/<name>.md
//   - /resume  <name> — ~/.nightme/handoff/<name>.md       → current Agent context
//
// The handoff document lives under the user's home directory so
// the resuming session does not need to be in the same cwd the
// previous agent used; the prompt explicitly tells the Agent the
// cwd may differ.
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
	"strings"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/nightmedir"
)

// handoffPrompt placeholder. Matches the convention used by the
// /handoff package — the runtime substitutes the absolute path
// before the Agent sees the prompt so the Agent's Read tool can
// call without having to reconstruct the relative path.
//
// Same `{{...}}` style and `_ABS` suffix as the handoff
// package; the names are kept identical so a single
// "path placeholders" mental model applies across both
// commands.
const placeholderHandoffFileAbs = "{{HANDOFF_FILE_ABS}}"

// RenderResumePrompt substitutes the absolute per-user handoff
// path into resumePromptPrefix. Symmetric with the /handoff
// package's RenderHandoffPrompt — tests pin the "no placeholder
// survives" contract and the absolute-path semantics.
//
// name must already pass nightmedir.ValidateHandoffName;
// RenderResumePrompt does not re-validate so a bad name shows
// up as a literal path in the rendered prompt rather than
// silently rounding to something usable.
func RenderResumePrompt(name string) (string, error) {
	abs, err := nightmedir.HandoffFilePath(name)
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(resumePromptPrefix, placeholderHandoffFileAbs, abs), nil
}

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
		Summary:  "Continue the task described by ~/.nightme/handoff/<name>.md in the current session.",
		Usage:    "/resume <name>",
		Category: "session",
	}
}

// resumeSpec declares /resume's argv grammar for the shared lexer
// (issue #291): no flags, exactly one positional arg (the
// handoff name). /resume is single-action; a missing or extra
// arg is a usage error rather than silently dropped, mirroring
// /stop and /handoff's contracts.
var resumeSpec = command.CmdSpec{
	Name:    "/resume",
	Usage:   "/resume <name>",
	MinArgs: 1,
	MaxArgs: 1,
}

// Handle implements command.SlashCommandFactory.
//
// Flow:
//
//  1. ChatSession + active CWD preflight (RequireActiveCwd).
//     cwd is still required even though the handoff document
//     lives under $HOME — without a chat-scoped cwd there is no
//     useful "where to continue" target for the resuming Agent.
//  2. Active-agent preflight (SelectedAgent + LookupSelectedAgentSession).
//  3. Reject missing / extra args via ParseCmdArgs.
//  4. input.MessageID guard — ChatSession.QueueUserMessage
//     silently no-ops on empty ID; mirrors the guard at
//     queue/cmd.go:143.
//  5. ValidateHandoffName — character set / length / sentinel
//     rules. Done before Stat so a bad name short-circuits with
//     a clean usage error instead of a confusing "not found".
//  6. Stat the named handoff. Missing or unreadable → OutReply
//     error hinting at /handoff (NOT a fatal error — the user
//     can run /handoff <name> and retry).
//  7. Queue the embedded resumePromptPrefix as a discrete
//     MessageKindQueue Prompt batch. The Agent reads the
//     handoff from disk per the prompt's step 1; no inline
//     copy of the file body is appended.
//  8. Reply with OutReply ack so the channel folds it into the
//     same rolling-log card as the Agent's continuation.
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

	parsed, err := command.ParseCmdArgs(input.Args[1:], resumeSpec)
	if err != nil {
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}

	if input.MessageID == "" {
		return command.OutReply(input,
			"Internal: missing message id; /resume did not enqueue."), nil
	}

	name := parsed.Arg(0)
	if err := nightmedir.ValidateHandoffName(name); err != nil {
		return command.OutReply(input, "❌ /resume: "+err.Error()), nil
	}
	handoffAbsPath, err := nightmedir.HandoffFilePath(name)
	if err != nil {
		return command.OutReply(input,
			fmt.Sprintf("❌ /resume: resolve handoff path: %v", err)), nil
	}
	info, err := os.Stat(handoffAbsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return command.OutReply(input,
				fmt.Sprintf("❌ /resume: %s not found; run /handoff <name> first.", nightmedir.HandoffRelPath(name))), nil
		}
		return command.OutReply(input,
			fmt.Sprintf("❌ /resume: stat %s failed: %v", handoffAbsPath, err)), nil
	}
	if info.Size() == 0 {
		return command.OutReply(input,
			fmt.Sprintf("❌ /resume: %s is empty; rerun /handoff %s.", nightmedir.HandoffRelPath(name), name)), nil
	}

	prompt, err := RenderResumePrompt(name)
	if err != nil {
		return command.OutReply(input,
			fmt.Sprintf("❌ /resume: render prompt: %v", err)), nil
	}
	msg := chatsession.Message{
		ID:     input.MessageID,
		ChatID: input.ChatID,
		Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: prompt}},
		Kind:   chatsession.MessageKindQueue,
	}
	if err := cs.QueueUserMessage(msg); err != nil {
		return command.OutReply(input, fmt.Sprintf("Queue failed: %v", err)), nil
	}

	return command.OutReply(input, fmt.Sprintf("🔄 Resuming task from %s…", nightmedir.HandoffRelPath(name))), nil
}

// resumePromptPrefix is the verbatim preamble /resume sends to
// the Agent. The {{HANDOFF_FILE_ABS}} placeholder resolves to
// the absolute per-user handoff path via RenderResumePrompt;
// the prompt intentionally mentions only the canonical path and
// no forbidden-path list — giving the Agent a roster of wrong
// paths to NOT use invites it to remember those paths and pick
// one by mistake. Same logic the /handoff package applies.
const resumePromptPrefix = `You are resuming an existing task from a previous AI coding agent.
The current working directory may differ from the one the previous agent was in; that is fine — the handoff document is not tied to any project.
A handoff document for the task lives at the absolute path:
{{HANDOFF_FILE_ABS}}
That file is the canonical handoff state for the task you want to continue.

Your job is to continue the existing task from the state described in that file. Do NOT restart the task from scratch.

The previous agent had different context from you. Treat the handoff as the primary source of task state, but verify important claims against the current workspace before acting. The handoff is a state snapshot, not an absolute source of truth.

## Resume Procedure
Follow this order:
1. Read {{HANDOFF_FILE_ABS}}.
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
If {{HANDOFF_FILE_ABS}} does not exist:
1. Inspect the current project and working state.
2. Determine whether the task can be identified from the current context.
3. Continue when the intended task and next action are sufficiently clear.
4. Do not fabricate a missing handoff.
If {{HANDOFF_FILE_ABS}} exists but is empty, malformed, or clearly incomplete:
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
Start by reading {{HANDOFF_FILE_ABS}}. Then inspect the relevant current project state and continue executing the task.

Your primary objective is:
Continue the existing task from the correct current state and make the next correct change toward completing it.
Only respond to the user when a meaningful execution result, blocker, required decision, or other user-facing information needs to be communicated.
`

// Compile-time check: Factory satisfies SlashCommandFactory.
var _ command.SlashCommandFactory = (*Factory)(nil)
