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
//   - `-c / --continue` resumes the groups of the last review in
//     this chat that did NOT complete: it reopens their agent
//     sessions (the session id recorded with the run), sends them
//     the text "continue", and delivers the merged result exactly
//     like a review's. The resume state lives in the factory's
//     memory, keyed by chat id — nothing is written to disk.
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
	"sync"

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
//
// The factory also holds the resume state of the last review per
// chat (see record.go) and the count of the reviews currently
// running per chat. The factory is built once per daemon process, so
// the state lives for the lifetime of the process and is lost on
// restart.
type Factory struct {
	mu      sync.Mutex
	records map[string]reviewRecord
	running map[string]int
	nextID  uint64
}

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
	return &Factory{
		records: make(map[string]reviewRecord),
		running: make(map[string]int),
	}
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
/review -g --unset          # remove the persisted global review agent
/review -c                  # resume the groups of the last review in this chat
                                 #   that failed (sends "continue" to them)`,
		Category: "workspace",
	}
}

// reviewCmdSpec declares /review's argv grammar. The shared lexer
// (command.ParseCmdArgs) enforces the "unknown flag" / arity
// contract for the whole /<cmd> surface.
var reviewCmdSpec = command.CmdSpec{
	Name:  "/review",
	Usage: "/review [-a|--agent <name>] [-g|--global] [--unset] [-c|--continue]",
	Flags: map[string]command.FlagSpec{
		"-a":         {Name: "agent", TakesValue: true},
		"--agent":    {Name: "agent", TakesValue: true},
		"-g":         {Name: "global"},
		"--global":   {Name: "global"},
		"--unset":    {Name: "unset"},
		"-c":         {Name: "continue"},
		"--continue": {Name: "continue"},
	},
	MinArgs: 0,
	MaxArgs: 0,
}

// Spec is the parsed result of /review's argv.
//
// Agent picks the review runner for this invocation; Global
// persists that runner as the machine-wide default;
// Unset removes the persisted default; Continue resumes the groups
// of the last review in this chat that did not complete. The
// findings always land in the chat session's current AS — only the
// runner differs.
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

	// Continue resumes the review groups of the last review in this
	// chat that failed. The runner is the agent recorded with that
	// run — a session belongs to one agent and cannot be moved to
	// another — so the four-tier runner resolution does not apply.
	Continue bool
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
		Agent:    strings.TrimSpace(parsed.Value("agent")),
		Global:   parsed.Bool("global"),
		Unset:    parsed.Bool("unset"),
		Continue: parsed.Bool("continue"),
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
//
// The --continue checks come first: --continue resumes an existing
// runner, so the flags that choose or persist a runner have nothing
// to add to it.
func validateReviewFlags(spec Spec) error {
	switch {
	case spec.Continue && spec.Agent != "":
		return errors.New("--continue and --agent are mutually exclusive")
	case spec.Continue && spec.Global:
		return errors.New("--continue and --global are mutually exclusive")
	case spec.Continue && spec.Unset:
		return errors.New("--continue and --unset are mutually exclusive")
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
//  3. Dispatch: `--continue` resumes the last review in this chat,
//     everything else starts a new one.
//
// Returns Consumed=true. The review itself appears asynchronously
// through the chat session's normal event pipeline (readpump →
// translate → emit), automatically tagged with agentbar / usagebar
// footer; the notices about the global scope travel with this
// command's own reply.
func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices,
	mgr *chatsession.Manager, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {

	// Parse the flags. /review supports --agent/-a (the runner of
	// this invocation), -g/--global (persist that runner),
	// --unset (remove the persisted runner) and -c/--continue
	// (resume the last review in this chat). Findings always land
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
		chatsession.ReportOutcome(ctx, command.FailedOutcome("usage", "the command line is not valid"))
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}
	if err := validateReviewFlags(spec); err != nil {
		chatsession.ReportOutcome(ctx, command.FailedOutcome("usage", "the flag combination is not valid"))
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}

	// Apply the global scope before the chat session is consulted.
	notices, done, writeFailed, err := applyGlobalScope(spec)
	if err != nil {
		// An agent name that is not registered is a different
		// failure than a failed write — the program branches on it.
		reason := "config_error"
		summary := "the review agent could not be persisted"
		if errors.Is(err, errUnknownReviewAgent) {
			reason = "unknown_agent"
			summary = "the review agent is not a registered agent"
		}
		chatsession.ReportOutcome(ctx, command.FailedOutcome(reason, summary))
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}
	if writeFailed {
		// The review still runs, but persisting the runner did
		// not happen — half of what was asked. The review's own
		// verdict merges underneath this one.
		chatsession.ReportOutcome(ctx, messages.Outcome{
			Status:  messages.OutcomeIncomplete,
			Summary: "the review agent could not be persisted",
			Data:    map[string]string{"reason": "config_write_failed"},
		})
	}
	if done {
		// --unset: the persisted runner is gone and no review runs.
		chatsession.ReportOutcome(ctx, messages.Outcome{
			Status:  messages.OutcomeOK,
			Summary: "the global review agent was removed",
			Data:    map[string]string{"scope": "global"},
		})
		return command.Reply(ctx, rt, strings.Join(notices, "\n")), nil
	}

	if spec.Continue {
		return f.handleContinue(ctx, rt, cs, input, notices)
	}
	return f.handleReview(ctx, rt, cs, input, spec, notices)
}

// handleReview starts a new review.
//
// Steps:
//  1. Resolve the current chat's selected AgentSession.
//  2. Resolve the runner: -a / --agent → global scope → the chat
//     session's agent.
//  3. Look up the corresponding Starter from agent.Builtins (it's
//     keyed by agent name).
//  4. Build an agent.StartConfig with the chat session's
//     selected cwd.
//  5. Delegate to starter.Review — the bridge's Starter owns the
//     actual prompt invocation (most call agent.Review).
func (f *Factory) handleReview(ctx context.Context, rt command.RuntimeServices,
	cs *chatsession.ChatSession, input command.SlashInput, spec Spec, notices []string) (*command.SlashOutput, error) {

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
			chatsession.ReportOutcome(ctx, command.FailedOutcome("no_cwd", "no workspace is set"))
			return replyWith(ctx, rt, notices,
				"❌ no workspace set. Send /cwd <path> first.",
			), nil
		case errors.Is(err, chatsession.ErrNoSelectedAgent):
			chatsession.ReportOutcome(ctx, command.FailedOutcome("no_agent", "no agent is selected"))
			return replyWith(ctx, rt, notices,
				"❌ no active agent configured. Run /use <agent> first.",
			), nil
		default:
			chatsession.ReportOutcome(ctx, command.FailedOutcome("error", "the agent could not be activated"))
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
		chatsession.ReportOutcome(ctx, command.FailedOutcome("no_agent", "no agent is selected"))
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
		chatsession.ReportOutcome(ctx, command.FailedOutcomeData("unknown_agent",
			"the review runner is not a registered agent",
			map[string]string{"agent": runnerName}))
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

	// The review outlives the command: the command returns as soon
	// as the goroutine is launched, but the review itself runs for
	// minutes and then feeds the findings back into the chat's
	// agent as a turn of its own. Take a hold on the message's
	// outcome so the settled signal waits for the whole thing
	// instead of reporting the command as done while the agent
	// work is still running.
	//
	// The hold is taken HERE, not inside the goroutine: the
	// dispatcher's own hold on the same message is released when
	// Handle returns, and a hold taken later would arrive after
	// that release — the message would settle as unknown before
	// the review even started.
	work := cs.Hold(replyTo)

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
	//
	// The chat is marked as running a review while the review is in
	// flight: a `--continue` for this chat is refused then — the two
	// flows would fight over the same group sessions. Overlapping
	// reviews are allowed, as before, so the marker is a counter.
	f.startReview(chatID)
	go func() {
		revCtx, cancel := context.WithTimeout(cs.Context(), timeouts.Review)
		defer cancel()

		// The review's own hold: released when the goroutine
		// returns, so the message settles only after the review
		// work AND the injected turn it hands the findings to are
		// both done.
		defer work.Release()

		// The chat stops counting as "running a review" once the
		// review itself is done — the delivery below is only the
		// user-facing rendering of the result.
		finished := false
		finish := func() {
			if finished {
				return
			}
			finished = true
			f.finishReview(chatID)
		}
		defer finish()

		// The per-group outcomes of this run — the resume state a
		// later `--review --continue` picks up.
		collector := &outcomeCollector{}

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

		result, err := starter.Review(revCtx, rc,
			agent.WithEventSink(sink),
			agent.WithReviewJobObserver(collector.observe),
		)

		// The record is replaced with this run's outcomes BEFORE the
		// result is delivered: by the time the user sees the
		// findings, the state a later `--continue` reads is already
		// this run's.
		//
		// A run that produced no group outcomes — the review found
		// no diff, or the bridge cannot review at all — leaves the
		// chat's resume state alone: there is nothing new to resume
		// from, and the previous run's unfinished groups are still
		// unfinished.
		groupOutcomes := collector.results()
		if len(groupOutcomes) > 0 {
			f.record(chatID, runnerName, workspace, groupOutcomes)
		}
		finish()

		if err != nil {
			// The review failed — say so in the machine-readable
			// channel too. A driver that only reads the settled
			// message must not see "unknown" for a review that
			// visibly failed.
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
				work.Report(messages.Outcome{
					Status:  messages.OutcomeNoop,
					Summary: "no diff to review",
					Data:    map[string]string{"agent": runnerName, "reason": "no_diff"},
				})
			case errors.Is(err, agent.ErrReviewNotSupported):
				slog.Default().Warn("/review failed",
					"agent", runnerName,
					"err", err,
				)
				text = fmt.Sprintf("❌ agent %q does not support /review", runnerName)
				work.Report(messages.Outcome{
					Status:  messages.OutcomeFailed,
					Summary: "the agent does not support /review",
					Data:    map[string]string{"agent": runnerName, "reason": "not_supported"},
				})
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
				work.Report(messages.Outcome{
					Status:  messages.OutcomeFailed,
					Summary: err.Error(),
					Data:    map[string]string{"agent": runnerName, "reason": "error"},
				})
			}
			if emitter == nil {
				return
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

		// The review itself succeeded. The verdict the consumer
		// ends up with is the merged one: a failed injection
		// outranks this, and the injected turn's own end decides
		// the rest.
		//
		// A group that failed does not fail the review — the
		// surviving groups' findings are still useful — but it is
		// not a clean success either. The group counts come from
		// the collector, never from the review text.
		groups, unfinished := groupCounts(groupOutcomes)
		status := messages.OutcomeOK
		summary := "review delivered"
		if unfinished > 0 {
			status = messages.OutcomeIncomplete
			summary = fmt.Sprintf("%d of %d groups unfinished", unfinished, groups)
		}
		data := map[string]string{"agent": runnerName}
		if groups > 0 {
			data["groups"] = fmt.Sprintf("%d", groups)
		}
		if unfinished > 0 {
			data["unfinished_groups"] = fmt.Sprintf("%d", unfinished)
		}
		work.Report(messages.Outcome{
			Status:  status,
			Summary: summary,
			Data:    data,
		})

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
		//
		// The injected turn is agent work of its own, and it can
		// run for a long time after the review is done. Hold the
		// message until that turn ends — the turn's prompt-end
		// carries replyTo (the WithReplyTo override becomes the
		// prompt's anchor), so the hold releases on exactly that
		// event.
		//
		// A failed injection means no turn will run, so the hold
		// is released right away instead of waiting for a turn
		// that never comes.
		injectWork := cs.HoldUntilPromptEnd(replyTo)
		if err := sendBlocks(revCtx, blocks, agentsession.WithReplyTo(replyTo)); err != nil {
			slog.Default().Warn("/review: AS inject failed",
				"agent", runnerName,
				"err", err,
			)
			// The findings were produced but never reached the
			// agent — the review is not complete.
			work.Report(messages.Outcome{
				Status:  messages.OutcomeIncomplete,
				Summary: "review findings could not be delivered to the agent",
				Data:    map[string]string{"agent": runnerName, "reason": "inject_failed"},
			})
			injectWork.Release()
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

// continueText is the text sent to each resumed session.
//
// The literal "continue": the resumed session still holds the
// original review prompt — including the output format the agent
// was asked for — so the agent knows what to produce without a
// second prompt spelling it out.
const continueText = "continue"

// handleContinue resumes the review groups of the last review in this
// chat that did not complete.
//
// The runner is the agent recorded with the previous run — a session
// belongs to one agent and cannot be moved to another, so the
// four-tier runner resolution does not apply. The resumed sessions
// run in the recorded workspace: the session belongs to that
// workspace, and resuming it against another working directory would
// point the agent at the wrong files.
//
// The merged result is delivered exactly like a review's: wrapped
// with agent.FormatReviewMessage, injected into the chat session's
// current AS as a user turn, and emitted to the channel.
func (f *Factory) handleContinue(ctx context.Context, rt command.RuntimeServices,
	cs *chatsession.ChatSession, input command.SlashInput, notices []string) (*command.SlashOutput, error) {
	chatID := input.ChatID

	rec, ok := f.lookup(chatID)
	if !ok {
		chatsession.ReportOutcome(ctx, command.FailedOutcome("no_review", "this chat has no review to continue"))
		return replyWith(ctx, rt, notices,
			"❌ no review to continue — run /review first",
		), nil
	}
	// The chat is marked busy for the whole continuation: two
	// concurrent --continue commands must not both resume the same
	// group sessions. The check and the mark happen under one lock.
	if !f.startContinuation(chatID) {
		chatsession.ReportOutcome(ctx, command.FailedOutcome("already_running",
			"a review is already running in this chat"))
		return replyWith(ctx, rt, notices,
			"❌ a review is already running in this chat",
		), nil
	}
	// From here on the chat counts as busy. Every early return
	// releases the mark; the goroutine releases it when the
	// continuation ends.
	release := func() { f.finishReview(chatID) }

	jobs, why := rec.resumable()
	if why != "" {
		release()
		// The ℹ️ cards here mean "there was nothing to resume" —
		// the last review finished, or its one retry is spent.
		chatsession.ReportOutcome(ctx, command.NoopOutcome("there was nothing to resume", nil))
		return replyWith(ctx, rt, notices, why), nil
	}

	runnerName := rec.agent
	starter, err := agent.Builtins.Get(runnerName)
	if err != nil {
		release()
		chatsession.ReportOutcome(ctx, command.FailedOutcomeData("unknown_agent",
			"the review runner is not a registered agent",
			map[string]string{"agent": runnerName}))
		return replyWith(ctx, rt, notices, fmt.Sprintf(
			"❌ unknown agent %q", runnerName,
		)), nil
	}

	// Resolve the inject target — the chat session's active AS.
	// Unchanged from the review path: the findings land in the
	// current AS, whatever agent ran the review.
	as, err := cs.LookupSelectedAgentSession()
	if err != nil {
		release()
		switch {
		case errors.Is(err, chatsession.ErrNoSelectedCwd):
			chatsession.ReportOutcome(ctx, command.FailedOutcome("no_cwd", "no workspace is set"))
			return replyWith(ctx, rt, notices,
				"❌ no workspace set. Send /cwd <path> first.",
			), nil
		case errors.Is(err, chatsession.ErrNoSelectedAgent):
			chatsession.ReportOutcome(ctx, command.FailedOutcome("no_agent", "no agent is selected"))
			return replyWith(ctx, rt, notices,
				"❌ no active agent configured. Run /use <agent> first.",
			), nil
		default:
			chatsession.ReportOutcome(ctx, command.FailedOutcome("error", "the agent could not be activated"))
			return replyWith(ctx, rt, notices, fmt.Sprintf(
				"❌ failed to activate agent: %v", err,
			)), nil
		}
	}

	// The resumed sessions run in the recorded workspace, not in the
	// chat session's current one.
	rc := agent.StartConfig{
		Workspace: rec.workspace,
	}

	// Capture the inputs the goroutine needs for an async reply. We
	// grab them by value so the closure doesn't pin SlashInput / agent
	// references for the full 30-min review budget.
	replyTo := input.MessageID
	workspace := rec.workspace
	sendBlocks := as.SendBlocks // close over SendBlocks for Inject
	emitter := cs.Emitter()

	// The continuation outlives the command exactly like a plain
	// review does — same hold, same reason. See handleReview.
	work := cs.Hold(replyTo)

	// The continuation is a review run like any other: the chat
	// stays marked as busy until the continuation itself is done.
	go func() {
		revCtx, cancel := context.WithTimeout(cs.Context(), timeouts.Review)
		defer cancel()

		defer work.Release()

		// The chat stops counting as "running a review" once the
		// continuation itself is done — the delivery below is only
		// the user-facing rendering of the result.
		finished := false
		finish := func() {
			if finished {
				return
			}
			finished = true
			release()
		}
		defer finish()

		// The per-group outcomes of this run.
		collector := &outcomeCollector{}

		// The sink is purely observational — the formatted text
		// still flows through FormatReviewMessage + sendBlocks (AS
		// inject) and emitter.Send (channel emit) below as two
		// separate product paths.
		sink, finalize := outbound.StreamRunOnceToEmitter(revCtx, emitter, cs, slog.Default(), chatID, replyTo, runnerName)
		defer finalize()

		result, err := agent.ContinueReviewJobs(revCtx, starter, rc, jobs, continueText,
			agent.WithEventSink(sink),
			agent.WithReviewJobObserver(collector.observe),
		)

		// The record is replaced with this run's outcomes BEFORE the
		// result is delivered, so a second `--continue` finds
		// nothing left to resume. A group that fails again has
		// spent its one resume attempt — the failure stays in the
		// record, the retry does not.
		//
		// The write is conditional on the record still being the one
		// this continuation resumed from: a review that finished in
		// the meantime owns the chat's resume state.
		groupOutcomes := collector.spentResults()
		f.recordIfCurrent(chatID, rec.id, runnerName, workspace, groupOutcomes)
		finish()

		if err != nil {
			slog.Default().Warn("/review --continue failed",
				"agent", runnerName,
				"err", err,
			)
			work.Report(messages.Outcome{
				Status:  messages.OutcomeFailed,
				Summary: err.Error(),
				Data:    map[string]string{"agent": runnerName, "reason": "error"},
			})
			if emitter == nil {
				return
			}
			msg := err.Error()
			if len(msg) > 800 {
				msg = msg[:800] + "…(truncated)"
			}
			_ = emitter.Send(revCtx, messages.OutboundMessage{
				ChatID:  chatID,
				Kind:    messages.OutReply,
				ReplyTo: replyTo,
				Text: fmt.Sprintf("❌ /review continue failed (agent=%s):\n%s",
					runnerName, msg),
			})
			return
		}

		// Success: wrap once, route twice (AS + channel) — the same
		// delivery a normal review gets.
		formatted := agent.FormatReviewMessage(workspace, runnerName, result.Text)
		blocks := []agent.ContentBlock{{Type: agent.ContentText, Text: formatted}}

		// A resumed group that failed again did not finish its
		// work — the continuation is not a clean success. The
		// counts come from the collector, never from the text.
		//
		// A continuation fans out over the groups it RESUMED, not
		// over every group of the review it continues — so its
		// count goes under `resumed_groups`, and `groups` keeps
		// meaning "the groups of the review".
		resumed, unfinished := groupCounts(groupOutcomes)
		status := messages.OutcomeOK
		summary := "review continued"
		if unfinished > 0 {
			status = messages.OutcomeIncomplete
			summary = fmt.Sprintf("%d of %d resumed groups unfinished", unfinished, resumed)
		}
		data := map[string]string{"agent": runnerName}
		if resumed > 0 {
			data["resumed_groups"] = fmt.Sprintf("%d", resumed)
		}
		if unfinished > 0 {
			data["unfinished_groups"] = fmt.Sprintf("%d", unfinished)
		}
		work.Report(messages.Outcome{
			Status:  status,
			Summary: summary,
			Data:    data,
		})

		// Same contract as a plain review: the injected turn is
		// agent work of its own, so the message settles when that
		// turn ends — and immediately when the injection fails.
		injectWork := cs.HoldUntilPromptEnd(replyTo)
		if err := sendBlocks(revCtx, blocks, agentsession.WithReplyTo(replyTo)); err != nil {
			slog.Default().Warn("/review --continue: AS inject failed",
				"agent", runnerName,
				"err", err,
			)
			work.Report(messages.Outcome{
				Status:  messages.OutcomeIncomplete,
				Summary: "review findings could not be delivered to the agent",
				Data:    map[string]string{"agent": runnerName, "reason": "inject_failed"},
			})
			injectWork.Release()
		}

		if emitter == nil {
			return
		}
		if revCtx.Err() != nil {
			return
		}
		_ = emitter.Send(revCtx, buildTerminalReply(chatID, replyTo, formatted, runnerName, result))
	}()

	return replyWith(ctx, rt, notices, ""), nil
}

// errUnknownReviewAgent is the failure of a -g flag that names an
// agent no registered starter answers to. It is distinct from a
// failed write: nothing was written, and the name itself is wrong.
var errUnknownReviewAgent = errors.New("unknown agent")

// applyGlobalScope performs the global-scope write of one /review
// invocation: `-g` persists the runner, `--unset` removes it.
//
// It returns the notices for the reply and whether the command is
// done — the unset path starts no review. A failed write is a
// notice, not an error: the review is the primary action and must
// still run.
func applyGlobalScope(spec Spec) (notices []string, done bool, writeFailed bool, err error) {
	if !spec.Global && !spec.Unset {
		return nil, false, false, nil
	}
	path := config.DefaultPath()
	switch {
	case spec.Unset:
		if err := config.UnsetReviewAgent(path); err != nil {
			return nil, false, false, fmt.Errorf("failed to unset review.agent: %w", err)
		}
		return []string{"✅ review.agent unset (global)", "→ file: " + path}, true, false, nil
	case spec.Global:
		// The name must be a registered agent — a persisted name
		// that no agent answers to would only fail later, on every
		// /review run.
		if _, err := agent.Builtins.Get(spec.Agent); err != nil {
			return nil, false, false, fmt.Errorf("%w %q", errUnknownReviewAgent, spec.Agent)
		}
		if err := config.SetReviewAgent(path, spec.Agent); err != nil {
			return []string{fmt.Sprintf(
				"⚠️ failed to save review.agent: %v — reviewing with %s anyway",
				err, spec.Agent,
			)}, false, true, nil
		}
		return []string{
			fmt.Sprintf("✅ review.agent = %s (global)", spec.Agent),
			"→ file: " + path,
		}, false, false, nil
	}
	return nil, false, false, nil
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
