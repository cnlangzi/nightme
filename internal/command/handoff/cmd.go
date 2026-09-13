// Package handoff implements the `/handoff` slash command.
//
// /handoff serializes the current task into
// `$HOME/.nightme/handoff/<name>.md` so the next AI coding agent
// (or a fresh session of the same one) can pick up the task with
// zero prior context — from any cwd, not just the one the
// previous agent was sitting in. The Agent receives the embedded
// handoff prompt and writes the canonical file itself via its
// Write tool; this package queues the prompt, pre-creates the
// per-user handoff directory so the Agent doesn't burn a turn
// on mkdir, and post-verifies that the file landed on disk
// before posting a success reply.
package handoff

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/nightmedir"
)

// handoffPrompt placeholder. `{{...}}` is the chosen style
// because it (a) doesn't collide with markdown / HTML tag
// parsing that an Agent might apply to the prompt, and (b)
// matches common prompt-template conventions so an Agent that
// has seen templated prompts before will recognize them as
// substitution targets rather than literal text.
//
// The `_ABS` suffix signals that the runtime substitutes an
// absolute filesystem path. Without that signal an Agent might
// write relative paths (./<name>.md) thinking "ABS" means
// "abstract / non-literal".
const placeholderHandoffFileAbs = "{{HANDOFF_FILE_ABS}}"

// RenderHandoffPrompt substitutes the absolute per-user handoff
// path into handoffPrompt. Exposed (rather than inlined in
// Handle) so tests can pin the placeholder contract: no {{...}}
// survives and the substituted text is platform-canonical.
//
// name must already pass nightmedir.ValidateHandoffName;
// RenderHandoffPrompt does not re-validate so a bad name shows
// up as a literal path in the rendered prompt rather than
// silently rounding to something usable.
func RenderHandoffPrompt(name string) (string, error) {
	abs, err := nightmedir.HandoffFilePath(name)
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(handoffPrompt, placeholderHandoffFileAbs, abs), nil
}

// handoffPrompt is the Agent's task for /handoff. The Agent has
// the chat's full context; it produces a Markdown document
// conforming to the structure described below and writes it to
// the absolute handoff path that the runtime substitutes in via
// the {{HANDOFF_FILE_ABS}} placeholder.
//
// Path placeholder:
//
//	{{HANDOFF_FILE_ABS}} →  nightmedir.HandoffFilePath(name)
//
// Why absolute paths in the prompt: the Agent's Write tool can
// then call without having to reconstruct
// "~/.nightme/handoff/<name>.md" from relative terms, which
// removes a class of mistakes (writing to cwd-relative paths,
// dropping the .md suffix, etc.). See
// internal/nightmedir.HandoffRelPath for the slash-form variant
// that stays in user-visible reply text.
//
// RenderHandoffPrompt performs the substitution at Handle time
// after nightmedir.ValidateHandoffName has accepted the name.
// Tests pin both the placeholder name and the "no placeholder
// survives" contract.
const handoffPrompt = `You are performing a task handoff for the CURRENT task.
Your job is to create a durable, NAMED handoff document so that another AI coding agent (or a fresh session of yourself) can continue the task from ZERO prior context and become productive within 2 minutes — including from a different working directory than the one you are in now.
The canonical handoff file is:
{{HANDOFF_FILE_ABS}}
That path lives under the user's home directory and is shared across all projects the user might switch into. Do not store the handoff inside the current project; do not write to any other path.
Do not merely generate the handoff as chat output. You must actually create or overwrite {{HANDOFF_FILE_ABS}} with the final handoff content.
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
- Write the complete final handoff document to {{HANDOFF_FILE_ABS}}.
- Create the file if it does not exist.
- Overwrite the existing {{HANDOFF_FILE_ABS}}; do not append to an older handoff.
- The file content must be exactly the final handoff document and must begin with # Handoff.
- Verify after writing that {{HANDOFF_FILE_ABS}} exists and contains the newly generated handoff.

## Output Behavior
The handoff document belongs at {{HANDOFF_FILE_ABS}}, not in the chat response.
After successfully writing and verifying the file, respond only with a concise confirmation that the handoff was saved.
Do not print the entire handoff document in the response.
If writing or verifying {{HANDOFF_FILE_ABS}} fails, report the failure clearly and do not claim that the handoff was saved.

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
		Summary:  "Serialize the current task into ~/.nightme/handoff/<name>.md so the next agent can continue from any directory.",
		Usage:    "/handoff <name>",
		Category: "session",
	}
}

// handoffSpec declares /handoff's argv grammar for the shared
// lexer (issue #291): no flags, exactly one positional arg (the
// handoff name). /handoff is single-action; a missing or extra
// arg is a usage error rather than silently dropped, mirroring
// /stop's contract.
var handoffSpec = command.CmdSpec{
	Name:    "/handoff",
	Usage:   "/handoff <name>",
	MinArgs: 1,
	MaxArgs: 1,
}

// Handle implements command.SlashCommandFactory.
//
// Flow:
//
//  1. ChatSession + active CWD preflight (RequireActiveCwd).
//     cwd is still required — the task itself is cwd-scoped even
//     though the handoff document lives under $HOME — but the
//     path of the handoff file no longer depends on it.
//  2. Active-agent preflight (SelectedAgent + LookupSelectedAgentSession).
//  3. Reject missing / extra args via ParseCmdArgs.
//  4. input.MessageID guard — ChatSession.QueueUserMessage
//     silently no-ops on empty ID; /queue has the same guard at
//     queue/cmd.go:143. Without it, a synthetic inbound would
//     get the ack while enqueuing nothing.
//  5. ValidateHandoffName — character set / length / sentinel
//     rules. Done before EnsureHandoffDir so a bad name never
//     creates a directory on disk.
//  6. EnsureHandoffDir — MkdirAll of $HOME/.nightme/handoff so
//     the Agent doesn't waste a turn on mkdir. Per-user
//     .gitignore is irrelevant (home is not inside a repo).
//  7. Queue the rendered handoff prompt as a discrete
//     MessageKindQueue Prompt batch.
//  8. Spawn a goroutine on a detached context.Background: the
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
	if _, failOut := command.RequireActiveCwd(cs); failOut != nil {
		return failOut, nil
	}

	if cs.SelectedAgent() == "" {
		return command.Reply(ctx, rt, "❌ no active agent; run /use <agent> first"), nil
	}
	if _, err := cs.LookupSelectedAgentSession(); err != nil {
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}

	parsed, err := command.ParseCmdArgs(input.Args[1:], handoffSpec)
	if err != nil {
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}

	if input.MessageID == "" {
		return command.Reply(ctx, rt,
			"Internal: missing message id; /handoff did not enqueue."), nil
	}

	name := parsed.Arg(0)
	if err := nightmedir.ValidateHandoffName(name); err != nil {
		return command.Reply(ctx, rt, "❌ /handoff: "+err.Error()), nil
	}
	if err := nightmedir.EnsureHandoffDir(); err != nil {
		return command.Reply(ctx, rt,
			fmt.Sprintf("❌ /handoff: cannot create handoff dir: %v", err)), nil
	}
	absPath, err := nightmedir.HandoffFilePath(name)
	if err != nil {
		return command.Reply(ctx, rt,
			fmt.Sprintf("❌ /handoff: resolve handoff path: %v", err)), nil
	}
	prompt, err := RenderHandoffPrompt(name)
	if err != nil {
		return command.Reply(ctx, rt,
			fmt.Sprintf("❌ /handoff: render prompt: %v", err)), nil
	}

	msg := chatsession.Message{
		ID:     input.MessageID,
		ChatID: input.ChatID,
		Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: prompt}},
		Kind:   chatsession.MessageKindQueue,
	}
	if err := cs.QueueUserMessage(msg); err != nil {
		return command.Reply(ctx, rt, fmt.Sprintf("Queue failed: %v", err)), nil
	}

	workCtx, cancel := context.WithCancel(context.Background())
	go func() {
		defer cancel()
		final := verifyHandoff(workCtx, cs, input.MessageID, absPath, name)
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

	return command.Reply(ctx, rt, fmt.Sprintf("⏳ /handoff queued; agent is writing %s.", nightmedir.HandoffRelPath(name))), nil
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
func verifyHandoff(ctx context.Context, cs *chatsession.ChatSession, msgID, absPath, name string) string {
	if err := waitForPromptEnd(ctx, cs, msgID, handoffTimeout); err != nil {
		return "❌ /handoff: " + err.Error()
	}
	return verifyHandoffFile(absPath, name)
}

// verifyHandoffFile is the pure verification gate: stat the
// canonical handoff path and return the user-facing reply text.
// Split out from verifyHandoff so the file-check logic is
// testable from an internal test file (same package, lowercase
// call site — see verify_internal_test.go) without spinning up
// an AgentEventBus / PromptEndBus round-trip.
//
// absPath and relName pair: the absolute path for the Stat, the
// already-validated name for the slash-form text echoed back to
// the user. relName is the name without the ".md" suffix; this
// matches HandoffRelPath's contract.
func verifyHandoffFile(absPath, name string) string {
	rel := nightmedir.HandoffRelPath(name)
	info, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Sprintf("❌ /handoff: agent did not write %s; rerun /handoff or paste the handoff into the file manually.", rel)
		}
		return fmt.Sprintf("❌ /handoff: stat %s failed: %v", absPath, err)
	}
	if info.Size() == 0 {
		return fmt.Sprintf("❌ /handoff: %s exists but is empty; rerun /handoff.", rel)
	}
	return fmt.Sprintf("✅ /handoff\n\n%s saved (%d bytes).", rel, info.Size())
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
