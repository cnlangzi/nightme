// Package review implements the `/review` slash command.
//
// /review runs a code review in a fresh RunOnce subprocess
// (independent context window, isolated from the main chat).
// The captured findings are injected back into the main chat
// session as a user message.
//
// Architecture (F-review.md v7):
//   - **Fully async** dispatcher: Handle launches a goroutine
//     to do the review work and returns Consumed=true
//     immediately. The dispatch worker is freed within
//     microseconds; the chat session's readpump is never
//     blocked. Multiple /review commands can run in parallel.
//   - The goroutine uses cs.Context() as its parent — when
//     the chat session closes (e.g. /close), the context is
//     cancelled, RunOnce's subprocess is killed, the goroutine
//     exits cleanly. No orphan work.
//   - RunOnce itself is the isolation: review runs in a
//     fresh subprocess with its own context window. Main
//     chat's token budget is NOT polluted by review
//     reasoning (which can burn tens of thousands of tokens).
//   - Zero qualifiers: /review takes no args. Extra args are
//     rejected inline (matches /cwd / /think / /use).
//   - The bridge's Starter.Review method calls agent.Review
//     (uses s.RunOnce internally). Most coding bridges
//     delegate; pty/bash returns agent.ErrReviewNotSupported —
//     which the goroutine surfaces via cs.Emitter().Send() as
//     a friendly inline reply, not silent failure.
//   - Fix is conversational: after the review appears in chat,
//     the user types "fix the blockers" and the main agent
//     uses its native Edit tools. /review does NOT auto-apply.
package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
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
		Summary: "Review current branch vs default branch (PR mode)",
		Usage: `/review                  # run review with the current selected agent,
                                 #   inject findings back to the same chat
/review --agent codex      # run review with codex (or any registered agent),
                                 #   findings still go to the current selected agent
/review -a codex            # short form of --agent`,
		Category: "workspace",
	}
}

// Spec is the parsed result of /review's argv. v8 introduced a
// single optional flag (--agent) that lets the user run the
// review on a different agent than the current selected one;
// findings always land in the current AS. All other args are
// rejected inline at the dispatch layer.
type Spec struct {
	// Agent overrides the review runner. Empty means "use the
	// current selected AS's agent" (the default). When set, the
	// dispatcher looks up agent.Builtins.Get(Agent) and uses that
	// Starter's RunOnce to run the review; findings are still
	// injected to the current AS (as.SendBlocks) — only the
	// runner differs.
	Agent string
}

// parseReviewArgs extracts the optional --agent / -a flag from
// argv. Returns an error for any unrecognised token, matching
// the dispatcher-level rejection pattern (don't be lenient
// with user input — better to error and have the user retry).
//
// Side-effect-free; takes only argv (slice of strings after
// the command name itself).
func parseReviewArgs(argv []string) (Spec, error) {
	var spec Spec
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch a {
		case "-a", "--agent":
			if i+1 >= len(argv) {
				return Spec{}, fmt.Errorf("--agent requires a value")
			}
			i++
			spec.Agent = argv[i]
		default:
			return Spec{}, fmt.Errorf("unknown arg %q (only --agent / -a supported)", a)
		}
	}
	return spec, nil
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
//     actual prompt injection (most call agent.Review).
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

	// Parse args. /review supports ONE optional flag: --agent/-a
	// to specify which coding agent runs the review (overriding
	// the default of the current selected AS). Findings always
	// land in the current selected AS — see ReviewContext.Inject
	// below. Multiple /review commands in parallel run on
	// different agents and feed back into the same AS, matching
	// the user's "different reviewers, single chat" workflow.
	//
	// All other args (positional names, other flags) are
	// rejected — matches /cwd / /think / /use inline check.
	spec, err := parseReviewArgs(input.Args[1:])
	if err != nil {
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}

	as := cs.SelectedAgentSession()
	if as == nil {
		return command.Reply(ctx, rt,
			"❌ 当前没有 active agent。先发条消息 / /use <name> 激活。",
		), nil
	}

	// Resolve the review runner. --agent overrides the default
	// (current AS's agent); empty / missing means "use the
	// current AS's agent". Either way the findings are injected
	// to the *current* AS via rc.Inject = as.SendBlocks.
	runnerName := as.Agent
	if spec.Agent != "" {
		runnerName = spec.Agent
	}
	starter, err := agent.Builtins.Get(runnerName)
	if err != nil {
		return command.Reply(ctx, rt, fmt.Sprintf(
			"❌ unknown agent %q (--agent)", runnerName,
		)), nil
	}

	rc := agent.ReviewContext{
		Workspace: cs.SelectedCwd(),
		Inject:    as.SendBlocks, // ALWAYS current AS, not the runner
	}

	// Capture the inputs the goroutine needs for an async reply
	// (ErrReviewNotSupported is the one case we surface back to
	// the user; everything else stays in logs). We grab them by
	// value so the closure doesn't pin SlashInput for the full
	// 30-min review budget.
	chatID := input.ChatID
	replyTo := input.MessageID

	// v7: /review is fully async. Handle returns immediately
	// after launching a goroutine that does the actual review
	// work (RunOnce subprocess + inject findings). The dispatch
	// worker is freed within microseconds; the chat session's
	// readpump continues processing events unblocked.
	//
	// Lifecycle:
	//   - The goroutine uses cs.Context() as its parent — when
	//     the chat session closes (e.g. /close), the context is
	//     cancelled, RunOnce's subprocess is killed, and the
	//     goroutine exits cleanly. No orphan work.
	//   - revCtx wraps cs.Context() with timeouts.Review (30 min),
	//     same as /gtw commit's Agent timeout. RunOnce's
	//     subprocess boundary makes this safe: the deadline
	//     only kills the review subprocess; the main chat is
	//     untouched.
	//
	// Error handling: most errors that happen AFTER Handle returns
	// can't be surfaced as inline replies (the chat reply is
	// gone), so they stay in logs. The one exception is
	// ErrReviewNotSupported — that error is fully predictable
	// (we know upfront pty/bash can't review) and the user
	// deserves a friendly inline reply, not silent failure.
	// We surface it via cs.Emitter().Send() on the chat's
	// emitter so the message lands in the right chat.
	go func() {
		revCtx, cancel := context.WithTimeout(cs.Context(), timeouts.Review)
		defer cancel()
		err := starter.Review(revCtx, rc)
		if err == nil {
			return
		}
		slog.Default().Warn("/review failed",
			"agent", runnerName,
			"err", err,
		)
		if errors.Is(err, agent.ErrReviewNotSupported) {
			em := cs.Emitter()
			if em == nil {
				return
			}
			_ = em.Send(revCtx, messages.OutboundMessage{
				ChatID:  chatID,
				Kind:    messages.OutReply,
				ReplyTo: replyTo,
				Text:    fmt.Sprintf("❌ agent %q 暂不支持 /review", runnerName),
			})
		}
	}()

	// Return Consumed=true with no inline reply. The chat session
	// is now free: readpump continues processing events, the
	// dispatch worker is free, the user can send more messages.
	// The review findings arrive in chat asynchronously when the
	// goroutine completes and rc.Inject pushes them into the
	// main AS as a user message.
	return &command.SlashOutput{Consumed: true}, nil
}