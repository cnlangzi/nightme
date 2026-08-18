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

	// Parse args. /review supports ONE optional flag: --agent/-a
	// to specify which coding agent runs the review (overriding
	// the default of the current selected AS). Findings always
	// land in the current selected AS — see ReviewContext.Inject
	// below. Multiple /review commands in parallel run on
	// different agents and feed back into the same AS, matching
	// the user's "different reviewers, single chat" workflow.
	//
	// parseReviewArgs is the single source of truth for arg
	// validation (rejects unknown flags + positional names with
	// a specific error message). There is intentionally no inline
	// `len(input.Args) > 1` check — such a check would mistakenly
	// reject `/review --agent codex` (3 args) before the flag
	// parser can recognize `--agent`. v6 used an inline check +
	// zero qualifiers; v8 added `--agent` and the inline check
	// must NOT fire before parseReviewArgs.
	spec, err := parseReviewArgs(input.Args[1:])
	if err != nil {
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}

	// Resolve the inject target. /review always injects findings
	// back into the chat session's active AS (the
	// "different reviewers, single chat" workflow). The runner
	// (--agent) is a separate one-shot spawn — see below.
	//
	// Use LookupSelectedAgentSession (NOT SelectedAgentSession)
	// so the AS is auto-spawned when the chat session has a
	// selectedAgent + SelectedCwd but no live AS in the pool.
	// This happens after a daemon restart (the chat session
	// metadata is persisted, but the process exits) and after
	// /close — the user shouldn't have to send a plain message
	// or /use to revive the session before /review can run.
	as, err := cs.LookupSelectedAgentSession()
	if err != nil {
		switch {
		case errors.Is(err, chatsession.ErrNoSelectedCwd):
			return command.Reply(ctx, rt,
				"❌ no workspace set. Send /cwd <path> first.",
			), nil
		case errors.Is(err, chatsession.ErrNoSelectedAgent):
			return command.Reply(ctx, rt,
				"❌ no active agent configured. Run /use <agent> first.",
			), nil
		default:
			return command.Reply(ctx, rt, fmt.Sprintf(
				"❌ failed to activate agent: %v", err,
			)), nil
		}
	}

	// Resolve the review runner. --agent overrides the default
	// (current AS's agent); empty / missing means "use the
	// current AS's agent". The runner is a separate one-shot
	// spawn — its findings get routed to BOTH the AS (via
	// as.SendBlocks) and the channel (via em.Send) by the
	// goroutine below.
	runnerName := as.Agent
	if spec.Agent != "" {
		runnerName = spec.Agent
	}
	starter, err := agent.Builtins.Get(runnerName)
	if err != nil {
		// Only blame `--agent` when the user actually passed it —
		// otherwise runnerName came from the current selected AS
		// and the failure is about the chat's primary agent, not
		// the --agent override.
		suffix := ""
		if spec.Agent != "" {
			suffix = " (--agent)"
		}
		return command.Reply(ctx, rt, fmt.Sprintf(
			"❌ unknown agent %q%s", runnerName, suffix,
		)), nil
	}

	rc := agent.ReviewContext{
		Workspace: cs.SelectedCwd(),
	}

	// Capture the inputs the goroutine needs for an async reply
	// (ErrReviewNotSupported is the one case we surface back to
	// the user; everything else stays in logs). We grab them by
	// value so the closure doesn't pin SlashInput / agent
	// references for the full 30-min review budget.
	chatID := input.ChatID
	replyTo := input.MessageID
	workspace := cs.SelectedCwd()
	sendBlocks := as.SendBlocks // close over SendBlocks for Inject
	emitter := cs.Emitter()

	// v7: /review is fully async. Handle returns immediately
	// after launching a goroutine that does the actual review
	// work (RunOnce subprocess + distribute findings). The
	// dispatch worker is freed within microseconds; the chat
	// session's readpump continues processing events unblocked.
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
	// v9 distribution: on success, the bridge returns the raw
	// review text. The goroutine wraps it in FormatReviewMessage
	// and emits to TWO destinations:
	//
	//   1. as.SendBlocks → AS as a user turn → main agent sees
	//      findings, can act on "fix the blockers" follow-ups.
	//   2. em.Send → channel directly → user sees findings in
	//      chat immediately, without waiting for the AS's
	//      downstream reply (which may not echo them verbatim).
	//
	// On failure (incl. ErrReviewNotSupported), only the channel
	// gets a reply — no Inject, since there's no findings to act
	// on.
	go func() {
		revCtx, cancel := context.WithTimeout(cs.Context(), timeouts.Review)
		defer cancel()

		result, err := starter.Review(revCtx, rc)
		if err != nil {
			slog.Default().Warn("/review failed",
				"agent", runnerName,
				"err", err,
			)
			if emitter == nil {
				return
			}
			var text string
			switch {
			case errors.Is(err, agent.ErrReviewNotSupported):
				text = fmt.Sprintf("❌ agent %q does not support /review", runnerName)
			default:
				msg := err.Error()
				if len(msg) > 800 {
					msg = msg[:800] + "…(truncated)"
				}
				text = fmt.Sprintf("❌ /review failed (agent=%s):\n%s", runnerName, msg)
			}
			_ = emitter.Send(revCtx, messages.OutboundMessage{
				ChatID:  chatID,
				Kind:    messages.OutReply,
				ReplyTo: replyTo,
				Text:    text,
			})
			return
		}

		// Success: wrap once, route twice (AS + channel).
		formatted := agent.FormatReviewMessage(workspace, runnerName, result.Text)
		blocks := []agent.ContentBlock{{Type: agent.ContentText, Text: formatted}}

		// 1) Inject into AS as a user turn — main agent sees
		//    the review and can act on follow-ups.
		if err := sendBlocks(revCtx, blocks); err != nil {
			slog.Default().Warn("/review: AS inject failed",
				"agent", runnerName,
				"err", err,
			)
		}

		// 2) Send to the channel directly — user sees the
		//    findings without waiting for the AS's downstream
		//    reply. Skip silently if the chat session is
		//    closing (revCtx cancelled) or the emitter is gone.
		if emitter == nil {
			return
		}
		if revCtx.Err() != nil {
			return
		}
		_ = emitter.Send(revCtx, messages.OutboundMessage{
			ChatID:  chatID,
			Kind:    messages.OutReply,
			ReplyTo: replyTo,
			Text:    formatted,
		})
	}()

	// Return Consumed=true with no inline reply. The chat session
	// is now free: readpump continues processing events, the
	// dispatch worker is free, the user can send more messages.
	// The review findings arrive in chat asynchronously when the
	// goroutine completes (both AS-injected and channel-emitted).
	return &command.SlashOutput{Consumed: true}, nil
}