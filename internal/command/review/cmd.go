// Package review implements the `/review` slash command.
//
// /review runs a code review on the agent's session path — one
// session per rule group (independent context window, isolated from
// the main chat).
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
//     cancelled, the review sessions are closed, the goroutine
//     exits cleanly. No orphan work.
//   - The session is the isolation: review runs in
//     fresh sessions with their own context window. Main
//     chat's token budget is NOT polluted by review
//     reasoning (which can burn tens of thousands of tokens).
//   - Zero qualifiers: /review takes no positional args. The flags
//     are parsed by the shared lexer (command.ParseCmdArgs), which
//     rejects unknown flags and stray positionals.
//   - The runner is resolved in four tiers: the -a / --agent flag
//     of this invocation, the global `review.agent` setting, the
//     chat session's selected agent, and — when none of them names
//     an agent — the "no active agent" error.
//   - `-g / --global` persists the runner as `review.agent` in
//     ~/.nightme/config.yaml; `--unset` removes it again.
//   - The bridge's Starter.Review method calls agent.Review
//     (uses the shared session helper internally). Most coding
//     bridges delegate; pty/bash returns
//     agent.ErrReviewNotSupported — which the goroutine surfaces
//     via cs.Emitter().Send() as a friendly inline reply, not
//     silent failure.
//   - Fix is conversational: after the review appears in chat,
//     the user types "fix the blockers" and the main agent
//     uses its native Edit tools. /review does NOT auto-apply.
package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentsession"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/gateway/outbound"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/timeouts"
)

// Factory is the command.SlashCommandFactory for /review.
//
// Holds *chatsession.Manager directly (mirrors /cwd, /think, /use)
// so the runtime can construct it via the package init() without
// pulling in extra runtime deps.
type Factory struct{}

// init self-registers the /review command. Each command package's
// init() calls RegisterBuilder; the runtime orchestrator calls
// SetDeps once at startup to finalize every registered builder.
func init() {
	command.RegisterBuilder(func(d command.Deps) command.SlashCommandFactory {
		return NewFactory()
	})
}

// NewFactory constructs a Factory. command/* factories do not
// receive a *chatsession.Manager — cs comes from the dispatcher
// parameter at Handle time.
func NewFactory() *Factory {
	return &Factory{}
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
/review -a codex            # short form of --agent
/review -a codex -g         # also persist codex as the global review agent
                                 #   (-g / --global) for later /review runs
/review -g --unset          # remove the persisted global review agent`,
		Category: "workspace",
	}
}

// reviewCmdSpec declares /review's argv grammar. The shared lexer
// (command.ParseCmdArgs) enforces the "unknown flag" / arity
// contract for the whole /<cmd> surface.
var reviewCmdSpec = command.CmdSpec{
	Name:  "/review",
	Usage: "/review [-a|--agent <name>] [-g|--global] [--unset]",
	Flags: map[string]command.FlagSpec{
		"-a":       {Name: "agent", TakesValue: true},
		"--agent":  {Name: "agent", TakesValue: true},
		"-g":       {Name: "global"},
		"--global": {Name: "global"},
		"--unset":  {Name: "unset"},
	},
	MinArgs: 0,
	MaxArgs: 0,
}

// Spec is the parsed result of /review's argv.
//
// Agent picks the review runner for this invocation; Global
// persists that runner as the machine-wide default;
// Unset removes the persisted default. The findings always land in
// the chat session's current AS — only the runner differs.
type Spec struct {
	// Agent overrides the review runner. Empty means "use the
	// global scope, then the current selected AS's agent". When
	// set, the dispatcher looks up agent.Builtins.Get(Agent) and
	// uses that Starter's session to run the review; findings are
	// still injected to the current AS (as.SendBlocks) — only the
	// runner differs.
	Agent string

	// Global asks for Agent to be persisted as the global review
	// agent (the review.agent key of ~/.nightme/config.yaml).
	Global bool

	// Unset removes the persisted global review agent. It starts
	// no review.
	Unset bool
}

// parseReviewArgs extracts /review's flags from argv. Returns an
// error for any unrecognised token, matching the dispatcher-level
// rejection pattern (don't be lenient with user input — better to
// error and have the user retry).
//
// Side-effect-free; takes only argv (slice of strings after
// the command name itself).
func parseReviewArgs(argv []string) (Spec, error) {
	parsed, err := command.ParseCmdArgs(argv, reviewCmdSpec)
	if err != nil {
		return Spec{}, err
	}
	spec := Spec{
		Agent:  strings.TrimSpace(parsed.Value("agent")),
		Global: parsed.Bool("global"),
		Unset:  parsed.Bool("unset"),
	}
	// An empty agent name is a mistake, not a request for the
	// default — "not given" and "given as empty" must stay
	// distinguishable.
	if parsed.Has("agent") && spec.Agent == "" {
		return Spec{}, errors.New("--agent requires a non-empty agent name")
	}
	return spec, nil
}

// validateReviewFlags rejects the flag combinations /review cannot
// act on. Each combination gets its own message so the user knows
// which flag to fix.
func validateReviewFlags(spec Spec) error {
	switch {
	case spec.Unset && spec.Agent != "":
		return errors.New("--unset and --agent are mutually exclusive")
	case spec.Unset && !spec.Global:
		return errors.New("--unset requires --global")
	case spec.Global && !spec.Unset && spec.Agent == "":
		return errors.New("--global requires --agent <name>")
	}
	return nil
}

// resolveRunner picks the agent that runs the review.
//
//	Tier 1  -a / --agent          this invocation
//	Tier 2  review.agent          global scope
//	Tier 3  the session's agent   the chat session's selected agent
//	Tier 4  none                  the caller reports "no active agent"
//
// A tier-2 name that is not a registered agent falls through to
// tier 3 — a stale name in the config file must not brick /review.
// The returned warning describes the substitution.
func resolveRunner(cliAgent, cfgAgent, sessionAgent string) (name string, warn string) {
	if cliAgent != "" {
		return cliAgent, ""
	}
	if cfgAgent != "" {
		if _, err := agent.Builtins.Get(cfgAgent); err == nil {
			return cfgAgent, ""
		}
		if sessionAgent == "" {
			return "", fmt.Sprintf("⚠️ review.agent %q is not a registered agent", cfgAgent)
		}
		return sessionAgent, fmt.Sprintf("⚠️ review.agent %q is not a registered agent — using %s", cfgAgent, sessionAgent)
	}
	return sessionAgent, ""
}

// Handle implements command.SlashCommandFactory.
//
// Steps:
//  1. Parse the flags (shared lexer) and reject the flag
//     combinations /review cannot act on.
//  2. Apply the global scope: `-g` persists the runner,
//     `--unset` removes it. The write happens before the chat
//     session is consulted — the persisted setting is machine-wide
//     and does not depend on whether this chat has a workspace.
//  3. Resolve the current chat's selected AgentSession.
//  4. Resolve the runner: -a / --agent → global scope → the chat
//     session's agent.
//  5. Look up the corresponding Starter from agent.Builtins (it's
//     keyed by agent name).
//  6. Build an agent.StartConfig with the chat session's
//     selected cwd.
//  7. Delegate to starter.Review — the bridge's Starter owns the
//     actual prompt invocation (most call agent.Review).
//
// Returns Consumed=true. The review itself appears asynchronously
// through the chat session's normal event pipeline (readpump →
// translate → emit), automatically tagged with agentbar / usagebar
// footer; the notices about the global scope travel with this
// command's own reply.
func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices,
	mgr *chatsession.Manager, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {

	// Parse the flags. /review supports --agent/-a (the runner of
	// this invocation), -g/--global (persist that runner) and
	// --unset (remove the persisted runner). Findings always land
	// in the current selected AS (via as.SendBlocks in the
	// goroutine below). Multiple /review commands in parallel run
	// on different agents and feed back into the same AS, matching
	// the user's "different reviewers, single chat" workflow.
	//
	// parseReviewArgs is the single source of truth for arg
	// validation (rejects unknown flags + positional names with
	// a specific error message). There is intentionally no inline
	// `len(input.Args) > 1` check — such a check would mistakenly
	// reject `/review --agent codex` (3 args) before the flag
	// parser can recognize `--agent`.
	spec, err := parseReviewArgs(input.Args[1:])
	if err != nil {
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}
	if err := validateReviewFlags(spec); err != nil {
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}

	// Apply the global scope before the chat session is consulted.
	notices, done, err := applyGlobalScope(spec)
	if err != nil {
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}
	if done {
		return command.Reply(ctx, rt, strings.Join(notices, "\n")), nil
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
			return replyWith(ctx, rt, notices,
				"❌ no workspace set. Send /cwd <path> first.",
			), nil
		case errors.Is(err, chatsession.ErrNoSelectedAgent):
			return replyWith(ctx, rt, notices,
				"❌ no active agent configured. Run /use <agent> first.",
			), nil
		default:
			return replyWith(ctx, rt, notices, fmt.Sprintf(
				"❌ failed to activate agent: %v", err,
			)), nil
		}
	}

	// Resolve the review runner. Tier 1 is the -a / --agent flag,
	// tier 2 the global scope, tier 3 the current AS's agent. The
	// global scope is only read when tier 1 did not decide — a
	// per-invocation flag always wins over the persisted default.
	//
	// The runner is a separate one-shot spawn — its findings get
	// routed to BOTH the AS (via as.SendBlocks) and the channel
	// (via em.Send) by the goroutine below.
	globalAgent := ""
	if spec.Agent == "" {
		path := config.DefaultPath()
		var readErr error
		globalAgent, readErr = readGlobalReviewAgent(path)
		if readErr != nil {
			notices = append(notices, fmt.Sprintf(
				"⚠️ cannot read review.agent from %s: %v — using %s",
				path, readErr, as.Agent,
			))
		}
	}
	runnerName, warn := resolveRunner(spec.Agent, globalAgent, as.Agent)
	if warn != "" {
		notices = append(notices, warn)
	}
	if runnerName == "" {
		return replyWith(ctx, rt, notices,
			"❌ no active agent configured. Run /use <agent> first.",
		), nil
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
		return replyWith(ctx, rt, notices, fmt.Sprintf(
			"❌ unknown agent %q%s", runnerName, suffix,
		)), nil
	}

	rc := agent.StartConfig{
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
	// work (review sessions + distribute findings). The
	// dispatch worker is freed within microseconds; the chat
	// session's readpump continues processing events unblocked.
	//
	// Lifecycle:
	//   - The goroutine uses cs.Context() as its parent — when
	//     the chat session closes (e.g. /close), the context is
	//     cancelled, the review sessions are closed, and the
	//     goroutine exits cleanly. No orphan work.
	//   - revCtx wraps cs.Context() with timeouts.Review (30 min),
	//     same as /gtw commit's Agent timeout. The session
	//     boundary makes this safe: the deadline only closes the
	//     review sessions; the main chat is untouched.
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

		// Wire up the sink so the user sees the review agent's
		// intermediate events (thinking, tool calls, …) streaming
		// into the chat while /review runs. Sink is purely
		// observational — the formatted review text still flows
		// through FormatReviewMessage + sendBlocks (AS inject)
		// and emitter.Send (channel emit) below as two separate
		// product paths. No conflict: sink shows the process,
		// the formatted text is the deliverable.
		//
		// defer finalize() (F-63 follow-up fix-gtw-command-done):
		// the sink's drain goroutine runs on its own ctx so the
		// terminal OutHeartbeat's renderLocked is not canceled
		// when revCtx's WithTimeout fires at goroutine return.
		sink, finalize := outbound.StreamRunOnceToEmitter(revCtx, emitter, cs, slog.Default(), chatID, replyTo, runnerName)
		defer finalize()

		result, err := starter.Review(revCtx, rc, agent.WithEventSink(sink))
		if err != nil {
			if emitter == nil {
				return
			}
			var text string
			switch {
			case errors.Is(err, agent.ErrNoDiff):
				// Pre-compute found zero reviewable + zero
				// untracked files in the workspace — clean
				// working tree, no committed/staged/untracked
				// changes vs the default branch. The agent was
				// NOT spawned (short-circuit inside
				// ReviewWithPrompt / ReviewWithOcr), so we don't
				// slog.Warn — this is the expected graceful path,
				// not a failure. Use ℹ️ instead of ❌ so the user
				// sees "nothing to do", not "something broke".
				text = fmt.Sprintf(
					"ℹ️ no diff to review in %s (working tree clean, no staged/committed/untracked files in scope)",
					workspace)
				slog.Default().Info("/review: empty diff, no agent spawn",
					"agent", runnerName,
					"workspace", workspace)
			case errors.Is(err, agent.ErrReviewNotSupported):
				slog.Default().Warn("/review failed",
					"agent", runnerName,
					"err", err,
				)
				text = fmt.Sprintf("❌ agent %q does not support /review", runnerName)
			default:
				slog.Default().Warn("/review failed",
					"agent", runnerName,
					"err", err,
				)
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
		//
		// WithReplyTo(replyTo) tells the readpump to stamp the
		// /review slash command's message_id onto every AgentEvent
		// the main agent emits in response to this injected turn.
		// Without the override, those events anchor to the prior
		// prompt's LastMessageID (whatever the user message was
		// before /review), and the chat channel renders the fix
		// replies as a separate rolling card instead of folding
		// them into the /review placeholder.
		if err := sendBlocks(revCtx, blocks, agentsession.WithReplyTo(replyTo)); err != nil {
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
		_ = emitter.Send(revCtx, buildTerminalReply(chatID, replyTo, formatted, runnerName, result))
	}()

	// Return Consumed=true. The chat session is now free: readpump
	// continues processing events, the dispatch worker is free, the
	// user can send more messages. The review findings arrive in
	// chat asynchronously when the goroutine completes (both
	// AS-injected and channel-emitted); the notices about the
	// global scope travel with this reply so the user learns about
	// the persisted setting right away.
	return replyWith(ctx, rt, notices, ""), nil
}

// applyGlobalScope performs the global-scope write of one /review
// invocation: `-g` persists the runner, `--unset` removes it.
//
// It returns the notices for the reply and whether the command is
// done — the unset path starts no review. A failed write is a
// notice, not an error: the review is the primary action and must
// still run.
func applyGlobalScope(spec Spec) (notices []string, done bool, err error) {
	if !spec.Global && !spec.Unset {
		return nil, false, nil
	}
	path := config.DefaultPath()
	switch {
	case spec.Unset:
		if err := config.UnsetReviewAgent(path); err != nil {
			return nil, false, fmt.Errorf("failed to unset review.agent: %w", err)
		}
		return []string{"✅ review.agent unset (global)", "→ file: " + path}, true, nil
	case spec.Global:
		// The name must be a registered agent — a persisted name
		// that no agent answers to would only fail later, on every
		// /review run.
		if _, err := agent.Builtins.Get(spec.Agent); err != nil {
			return nil, false, fmt.Errorf("unknown agent %q", spec.Agent)
		}
		if err := config.SetReviewAgent(path, spec.Agent); err != nil {
			return []string{fmt.Sprintf(
				"⚠️ failed to save review.agent: %v — reviewing with %s anyway",
				err, spec.Agent,
			)}, false, nil
		}
		return []string{
			fmt.Sprintf("✅ review.agent = %s (global)", spec.Agent),
			"→ file: " + path,
		}, false, nil
	}
	return nil, false, nil
}

// readGlobalReviewAgent reads the persisted review.agent from the
// config file at path.
//
// config.Load (not config.LoadDefault) is deliberate: LoadDefault
// probes the built-in agents and may persist a detected primary as
// a side effect, which a slash command must not cause.
func readGlobalReviewAgent(path string) (string, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(cfg.Review.Agent), nil
}

// replyWith builds the command's single reply: the notices the user
// needs to see (the persistence confirmation, the warnings) followed
// by the error line when the command failed. A command that failed
// after a successful write still reports the write.
func replyWith(ctx context.Context, rt command.RuntimeServices, notices []string, errLine string) *command.SlashOutput {
	lines := make([]string, 0, len(notices)+1)
	lines = append(lines, notices...)
	if errLine != "" {
		lines = append(lines, errLine)
	}
	return command.Reply(ctx, rt, strings.Join(lines, "\n"))
}

// buildTerminalReply assembles the /review terminal OutboundMessage.
// The caller passes the already-formatted text (see
// FormatReviewMessage) so the wrap is computed once for both the
// AS-inject path and the channel-emit path. Stamp policy lives in
// messages.StampRunResult; this helper is just the message
// construction + stamp plumbing.
func buildTerminalReply(chatID, replyTo, formatted, runnerName string, result agent.RunResult) messages.OutboundMessage {
	out := messages.OutboundMessage{
		ChatID:  chatID,
		Kind:    messages.OutReply,
		ReplyTo: replyTo,
		Text:    formatted,
	}
	messages.StampRunResult(&out, runnerName, result)
	return out
}
