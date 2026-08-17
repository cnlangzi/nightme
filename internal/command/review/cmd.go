// Package review implements the `/review` slash command.
//
// /review injects a code-review prompt into the current chat
// session's AgentSession. The chat agent (claude / codex / dsh /
// opencode / pi) reads the prompt, runs `git diff` itself, and
// outputs a structured `## Summary / ## Findings / ## Suggestions`
// review of the diff between the current branch and the default
// branch (= the diff a PR would have).
//
// Architecture (F-review.md v9):
//   - Architecture B: review runs in the current chat session
//     (no new process spawn), so the chat agent's context
//     (user's recent messages, prior tool calls) is preserved
//     and the review appears in the same thread as the user's
//     command.
//   - Zero qualifiers: /review takes no args. Extra args are
//     rejected inline (matches the /cwd / /think / /use pattern
//     — all check len(input.Args) directly, no separate parse
//     function).
//   - The bridge's Starter.Review method owns the actual
//     prompt injection. Most coding bridges delegate to
//     agent.DefaultReview which injects agent.StandardPrompt.
//     pty/bash returns agent.ErrReviewNotSupported because
//     bash can't do code review.
//   - Fix is conversational: after the review, the user types
//     "fix the blockers" and the same chat agent uses its
//     native Edit tools. /review does NOT auto-apply anything.
package review

import (
	"context"
	"fmt"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/timeouts"
)

// Factory is the command.SlashCommandFactory for /review.
//
// Holds *chatsession.Manager directly (mirrors /cwd, /think, /use)
// so the runtime can construct it via the package init() without
// pulling in extra runtime deps.
type Factory struct {
	mgr *chatsession.Manager
}

// init self-registers the /review command. Each command package's
// init() calls RegisterBuilder; the runtime orchestrator calls
// SetDeps once at startup to finalize every registered builder.
func init() {
	command.RegisterBuilder(func(d command.Deps) command.SlashCommandFactory {
		return NewFactory(d.Manager)
	})
}

// NewFactory constructs a Factory backed by mgr.
func NewFactory(mgr *chatsession.Manager) *Factory {
	return &Factory{mgr: mgr}
}

// Spec implements command.SlashCommandFactory.
func (f *Factory) Spec() command.Spec {
	return command.Spec{
		Name:    "review",
		Summary: "Review current branch vs default branch (PR mode, zero qualifiers)",
		Usage: `/review    # reviews current branch vs default branch (committed + uncommitted)
              # fix is conversational: type "fix the blockers" in chat afterwards`,
		Category: "workspace",
	}
}

// Handle implements command.SlashCommandFactory.
//
// Steps:
//  1. Inline `len(input.Args)` check — no separate parse function
//     (matches /cwd / /think / /use style).
//  2. Resolve the current chat's selected AgentSession.
//  3. Look up the corresponding Starter from agent.Builtins (it's
//     keyed by agent name).
//  4. Build an agent.ReviewContext with the chat session's
//     selected cwd and the AgentSession's SendBlocks callback.
//  5. Delegate to starter.Review — the bridge's Starter owns the
//     actual prompt injection (most call agent.DefaultReview).
//
// Returns Consumed=true and NO reply — the review appears
// asynchronously through the chat session's normal event pipeline
// (readpump → translate → emit), automatically tagged with
// agentbar / usagebar footer.
func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices,
	cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {

	// /review 零限定符 — input.Args[0] is the "review" command
	// itself, anything beyond that is an unexpected arg.
	// Inline check matches the /cwd / /think / /use pattern —
	// no separate parse function (F-review.md §2.4).
	if len(input.Args) > 1 {
		return command.Reply(ctx, rt, fmt.Sprintf(
			"❌ /review 不接受参数;去掉 %q", input.Args[1],
		)), nil
	}

	as := cs.SelectedAgentSession()
	if as == nil {
		return command.Reply(ctx, rt,
			"❌ 当前没有 active agent。先发条消息 / /use <name> 激活。",
		), nil
	}

	starter, err := agent.Builtins.Get(as.Agent)
	if err != nil {
		// Shouldn't happen — cs.SelectedAgentSession() returned a
		// live AS whose Agent name came from this same registry —
		// but guard against a race (AS activated with a
		// since-removed agent) with a friendly message.
		return command.Reply(ctx, rt, fmt.Sprintf(
			"❌ unknown agent %q", as.Agent,
		)), nil
	}

	rc := agent.ReviewContext{
		Workspace: cs.SelectedCwd(),
		Inject:    as.SendBlocks,
	}

	// /review is a one-shot review via RunOnce — wraps in
	// timeouts.Review (30 min, same as Agent / Shell / Hook).
	// RunOnce's subprocess boundary makes this safe: the
	// deadline only kills the review subprocess; the main
	// chat session is untouched. If we hit the timeout, the
	// user gets a friendly "review timed out" reply, not a
	// hung dispatcher.
	revCtx, cancel := context.WithTimeout(ctx, timeouts.Review)
	defer cancel()

	// starter.Review is the bridge's opportunity to customize
	// (e.g. claude could use its built-in /code-review trigger
	// instead of StandardPrompt in v2). v1 ships with all
	// 5 coding bridges delegating to agent.Review.
	if err := starter.Review(revCtx, rc); err != nil {
		// pty/bash returns ErrReviewNotSupported — surface a
		// specific "this agent can't do /review" message rather
		// than a generic bridge error.
		if err == agent.ErrReviewNotSupported {
			return command.Reply(ctx, rt, fmt.Sprintf(
				"❌ agent %q 暂不支持 /review(bash / pty 不是 coding agent,不能 review)\n   当前支持 /review 的 agent: claude, codex, dsh, opencode, pi\n   (用 /use <name> 切换)",
				as.Agent,
			)), nil
		}
		return command.Reply(ctx, rt, "❌ /review 失败: "+err.Error()), nil
	}

	// Successfully delegated to the bridge. The actual review
	// reply comes back asynchronously through the chat session's
	// normal event pipeline — we return Consumed=true and emit
	// no inline reply, so the user sees a single coherent review
	// in the chat thread (not two: a slash ack + the review).
	return &command.SlashOutput{Consumed: true}, nil
}