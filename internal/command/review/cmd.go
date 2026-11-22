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
//   - The goroutine uses cs.Context() as its parent — when the
//     chat session's context is cancelled, the review sessions are
//     closed and the goroutine exits cleanly. No orphan work.
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
//   - `-c / --continue` finishes the groups of the last review in
//     this chat that did NOT complete: a group that reached the agent
//     is resumed (its agent session is reopened and sent the text
//     "continue"), a group that never reached the agent is re-run (a
//     fresh session carries the prompt the group was asked to run).
//     The merged result is delivered exactly like a review's. The
//     resume state lives in the factory's memory, keyed by chat id —
//     nothing is written to disk.
//   - `--abort` stops the review (or the continuation) that is
//     running in this chat: the run's context is cancelled, which
//     closes the agent sessions of its groups. The stopped groups
//     stay resumable — `--continue` reopens them and sends them the
//     text "continue". A review has no wall-clock deadline: it runs
//     until the chat session's context is cancelled (the daemon
//     shutting down) or the user aborts it.
//   - `--timeout <dur>` bounds how long a review group may stay
//     silent. A group whose subagent produces no event for that long
//     is closed, reported as timed out, and stays resumable. The
//     threshold comes from the flag, else from `review.timeout` in
//     the config file, else from the built-in default (15m).
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
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/gateway/outbound"
	"github.com/cnlangzi/nightme/internal/gitbase"
	"github.com/cnlangzi/nightme/internal/messages"
)

// Factory is the command.SlashCommandFactory for /review.
//
// Holds *chatsession.Manager directly (mirrors /cwd, /think, /use)
// so the runtime can construct it via the package init() without
// pulling in extra runtime deps.
//
// The factory also holds the resume state of the last review per
// chat (see record.go), the count of the reviews currently running
// per chat and the registry of the runs the user can abort (see
// abort.go). The factory is built once per daemon process, so the
// state lives for the lifetime of the process and is lost on
// restart.
type Factory struct {
	mu      sync.Mutex
	records map[string]reviewRecord
	running map[string]int
	runs    map[string][]*runHandle
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
		runs:    make(map[string][]*runHandle),
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
/review -c                  # continue the groups of the last review in this chat
                                 #   that failed: a group that reached the agent is
                                 #   resumed, a group that never started is re-run
/review -c -a codex        # continue with codex taking the failed groups over:
                                 #   the groups are re-run with their stored prompt
/review --abort            # stop the review running in this chat
/review --timeout 5m       # close a subagent that says nothing for 5 minutes
                                 #   (0 disables the timeout)`,
		Category: "workspace",
	}
}

// reviewCmdSpec declares /review's argv grammar. The shared lexer
// (command.ParseCmdArgs) enforces the "unknown flag" / arity
// contract for the whole /<cmd> surface.
var reviewCmdSpec = command.CmdSpec{
	Name:  "/review",
	Usage: "/review [-a|--agent <name>] [-g|--global] [--unset] [-c|--continue] [--abort] [-t|--timeout <dur>]",
	Flags: map[string]command.FlagSpec{
		"-a":         {Name: "agent", TakesValue: true},
		"--agent":    {Name: "agent", TakesValue: true},
		"-g":         {Name: "global"},
		"--global":   {Name: "global"},
		"--unset":    {Name: "unset"},
		"-c":         {Name: "continue"},
		"--continue": {Name: "continue"},
		"--abort":    {Name: "abort"},
		"-t":         {Name: "timeout", TakesValue: true},
		"--timeout":  {Name: "timeout", TakesValue: true},
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
	//
	// Agent overrides that runner: another agent takes the groups
	// over, re-running them with the prompt they were asked to run.
	Continue bool

	// Abort stops the review (or the continuation) that is running
	// in this chat. It starts no review and needs neither a
	// workspace nor a selected agent.
	Abort bool

	// Timeout is the per-group silence threshold of this invocation:
	// how long a review group may go without an event before it is
	// closed and reported as timed out. Zero disables the timeout.
	//
	// TimeoutSet says whether the flag was given: not given means the
	// config value, then the built-in default, decide.
	Timeout    time.Duration
	TimeoutSet bool
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
		Abort:    parsed.Bool("abort"),
	}
	// An empty agent name is a mistake, not a request for the
	// default — "not given" and "given as empty" must stay
	// distinguishable.
	if parsed.Has("agent") && spec.Agent == "" {
		return Spec{}, errors.New("--agent requires a non-empty agent name")
	}
	if parsed.Has("timeout") {
		d, err := time.ParseDuration(strings.TrimSpace(parsed.Value("timeout")))
		if err != nil {
			return Spec{}, errors.New("--timeout requires a duration like 15m")
		}
		if d < 0 {
			return Spec{}, errors.New("--timeout must not be negative")
		}
		spec.Timeout = d
		spec.TimeoutSet = true
	}
	return spec, nil
}

// validateReviewFlags rejects the flag combinations /review cannot
// act on. Each combination gets its own message so the user knows
// which flag to fix.
//
// The --timeout checks come first: a timeout bounds a run that starts,
// so the flags that start no run have nothing to bound.
//
// The --abort checks come next: the abort targets the run that is
// already in flight, so the flags that choose or persist a runner
// have nothing to add to it.
//
// The --continue checks come next: --continue picks the last review
// of this chat back up. It may name the agent that takes it over, so
// --agent is not in the way — but the flags that persist a runner
// have nothing to add to a review that is already running.
func validateReviewFlags(spec Spec) error {
	switch {
	case spec.TimeoutSet && spec.Abort:
		return errors.New("--timeout and --abort are mutually exclusive")
	case spec.TimeoutSet && spec.Unset:
		return errors.New("--timeout and --unset are mutually exclusive")
	case spec.Abort && spec.Continue:
		return errors.New("--abort and --continue are mutually exclusive")
	case spec.Abort && spec.Agent != "":
		return errors.New("--abort and --agent are mutually exclusive")
	case spec.Abort && spec.Global:
		return errors.New("--abort and --global are mutually exclusive")
	case spec.Abort && spec.Unset:
		return errors.New("--abort and --unset are mutually exclusive")
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

	// The abort targets the run that is already in flight, so it
	// needs neither a workspace nor a selected agent — it is
	// dispatched before the chat session is consulted.
	if spec.Abort {
		return f.handleAbort(ctx, rt, input)
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

	// The silence threshold of this run: the flag, else the config
	// file, else the built-in default. A config value that cannot be
	// read is a warning — the review still runs.
	timeout, timeoutNotice := resolveReviewTimeout(spec)
	if timeoutNotice != "" {
		notices = append(notices, timeoutNotice)
	}

	if spec.Continue {
		return f.handleContinue(ctx, rt, cs, input, spec, notices, timeout)
	}
	return f.handleReview(ctx, rt, cs, input, spec, notices, timeout)
}

// threadShowsFindings reports whether the chat's /think lets the
// review's findings reach the thread.
//
// The findings ride the thinking kind, and the gate drops that kind
// while /think is off — the default. With the thread empty, the
// agent's answer is the reader's only view of what the findings say.
func threadShowsFindings(cs *chatsession.ChatSession) bool {
	return cs != nil && cs.ThinkMode() != chatsession.ThinkModeHide
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
	cs *chatsession.ChatSession, input command.SlashInput, spec Spec, notices []string,
	timeout time.Duration) (*command.SlashOutput, error) {

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
	// references for the whole review run.
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
	//   - The goroutine uses cs.Context() as its parent — when the
	//     chat session's context is cancelled, the review sessions
	//     are closed and the goroutine exits cleanly. No orphan
	//     work.
	//   - The run has NO wall-clock deadline: it ends when the chat
	//     closes or when the user aborts it. The context is
	//     cancellable with a cause, so the run can tell an abort
	//     from a chat close.
	//
	// v9 distribution: on success, the bridge returns the raw
	// review text. The goroutine wraps it in FormatReviewMessage
	// and injects it into the AS as a user turn — the main agent
	// sees the findings and can act on "fix the blockers"
	// follow-ups.
	//
	// The channel does NOT get the merged text: the findings reach
	// it through the coalesced stream the sink carries (each batch
	// labeled with its group), framed by the plan line before it
	// and the verdict line after it. The user reads the findings
	// once.
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

	// The run: no deadline, cancellable with the abort sentinel so
	// `/review --abort` can stop the run. It is registered BEFORE the
	// goroutine starts, so an abort that arrives right after the
	// command returned cannot race the registration.
	revCtx, handle := f.newRun(chatID, cs.Context(), runnerName, "review")

	go func() {
		// The defer order is the run's shutdown contract: the
		// running marker is released and the message's hold is given
		// back first, and `done` closes LAST — an abort that waits on
		// it is guaranteed to find the run finished and its record
		// written.
		defer close(handle.done)
		defer f.endRun(chatID, handle)
		defer handle.cancel(nil)

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
		// into the chat while /review runs. The sink shows the
		// process; the merged review text is the main agent's
		// input and never reaches the channel.
		//
		// defer drain() (F-63 follow-up fix-gtw-command-done):
		// the sink's drain goroutine runs on its own ctx so the
		// terminal OutHeartbeat's render is not canceled
		// when revCtx is cancelled at goroutine return.
		//
		// The drain is called once more below, right after the
		// fan-out ends: the groups' flushed batches are still queued
		// behind the drain goroutine, and the review's own closing
		// lines must land after them.
		sink, finalize := outbound.StreamRunOnceToEmitter(revCtx, emitter, cs, slog.Default(), chatID, replyTo, runnerName)
		var drainOnce sync.Once
		drain := func() { drainOnce.Do(finalize) }
		defer drain()

		result, err := starter.Review(revCtx, rc,
			agent.WithEventSink(sink),
			agent.WithReviewJobObserver(logFailedGroups("/review", runnerName, collector.observe)),
			// The plan fires once, before the first group starts —
			// the user learns the group list before any group
			// produces output.
			agent.WithReviewPlanObserver(planObserver(revCtx, emitter, collector, chatID, replyTo, runnerName)),
			// The silence threshold of one group: a subagent that
			// stops producing anything is closed on its own.
			agent.WithReviewGroupTimeout(timeout),
		)

		// The fan-out is over: the groups' sessions are closed and the
		// findings are in hand. There is nothing left for an abort to
		// stop, so the delivery below runs undisturbed.
		f.finishFanout(chatID, handle)

		// The record is updated with this run's results BEFORE the
		// result is delivered: the findings of the groups that
		// finished wait there until every group is in, and the state
		// a later `--continue` reads is already this run's.
		//
		// A run that produced no group outcomes — the review found
		// no diff, or the bridge cannot review at all — leaves the
		// chat's record alone: there is nothing new to add, and the
		// previous run's unfinished groups are still unfinished.
		groupOutcomes := collector.results()
		var rec reviewRecord
		if len(groupOutcomes) > 0 {
			rec = f.record(chatID, runnerName, workspace, reviewRun{
				outcomes:      groupOutcomes,
				contributions: collector.said(),
				plan:          collector.plan(),
			})
		} else {
			rec, _ = f.lookup(chatID)
		}
		finish()

		// The user aborted this run: the agent sessions are closed,
		// the findings that had already arrived stay in the thread,
		// and the stopped groups stay resumable — the record above
		// holds them with their session ids.
		//
		// The abort branch comes BEFORE the failure branch: after an
		// abort the fan-out returns a context-cancellation error, and
		// the user must see "aborted", not "❌ /review failed".
		if aborted(revCtx) {
			// The sink is drained first: the findings that had
			// already been queued land before the notice.
			drain()
			reportAborted(revCtx, work, emitter, chatID, replyTo, runnerName, "review", groupOutcomes)
			return
		}

		// A run that failed before the fan-out — no diff, an unknown
		// base, an agent that cannot review — has no groups and
		// nothing to deliver. A run whose groups all failed is not
		// this case: the review is pending, and the card says what is
		// still missing.
		if err != nil && len(groupOutcomes) == 0 {
			// The review failed — say so in the machine-readable
			// channel too. A driver that only reads the settled
			// message must not see "unknown" for a review that
			// visibly failed.
			var text string
			// The failure happened before the fan-out — no diff, an
			// unknown base, an agent that cannot review — so there is
			// no group and no work to pick up: the outcome carries no
			// unfinished count.
			data := unfinishedData(map[string]string{"agent": runnerName, "delivered": "false"}, groupOutcomes)
			switch {
			case errors.Is(err, agent.ErrNoDiff):
				// Pre-compute found zero reviewable files in the
				// workspace — clean working tree, no
				// committed/staged changes vs the default branch.
				// The agent was NOT spawned (short-circuit inside
				// ReviewWithPrompt / ReviewWithOcr), so we don't
				// slog.Warn — this is the expected graceful path,
				// not a crash.
				//
				// A review runs because there is a diff; an empty
				// diff means the premise does not hold, so the
				// outcome is a failure: the caller decides what to
				// do with a review that never looked at anything.
				text = fmt.Sprintf(
					"❌ no diff to review in %s (working tree clean, no staged/committed files in scope)",
					workspace)
				slog.Default().Info("/review: empty diff, no agent spawn",
					"agent", runnerName,
					"workspace", workspace)
				work.Report(command.FailedOutcomeData("no_diff", "no diff to review",
					data,
					"there is no diff to review: the working tree is clean and the branch carries no change against the base"))
			case errors.Is(err, agent.ErrBaseUnknown):
				// The repository has an origin remote, but the base
				// branch could neither be read nor repaired — so the
				// committed diff was NOT reviewed. Saying "no diff"
				// would claim there was nothing to review; the truth
				// is that nothing was examined.
				text = baseUnknownMessage(workspace, err)
				slog.Default().Warn("/review: base branch unknown",
					"agent", runnerName,
					"workspace", workspace,
					"err", err,
				)
				work.Report(command.FailedOutcomeData("base_unknown",
					"the base branch could not be determined",
					data,
					"the base branch could neither be read nor repaired: the committed diff was not reviewed"))
			case errors.Is(err, agent.ErrReviewNotSupported):
				slog.Default().Warn("/review failed",
					"agent", runnerName,
					"err", err,
				)
				text = fmt.Sprintf("❌ agent %q does not support /review", runnerName)
				work.Report(command.FailedOutcomeData("not_supported",
					"the agent does not support /review",
					data,
					"the agent cannot review: "+err.Error()))
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
				work.Report(command.FailedOutcomeData("error", err.Error(),
					timeoutOutcomeData(data, groupOutcomes),
					"the review failed: "+err.Error()))
			}
			if emitter == nil {
				return
			}
			_, _ = emitter.Send(revCtx, messages.OutboundMessage{
				ChatID:  chatID,
				Kind:    messages.OutReply,
				ReplyTo: replyTo,
				Text:    text,
			})
			return
		}

		if len(groupOutcomes) == 0 {
			// The run produced no group: there is nothing to deliver.
			// The review the chat is in the middle of keeps its
			// findings, and they wait for the round that completes
			// it.
			work.Report(messages.Outcome{
				Status:  messages.OutcomeOK,
				Summary: "the review found nothing to review",
				Data:    map[string]string{"agent": runnerName, "delivered": "false"},
			})
			return
		}

		// Every group of the run failed. That is not the end of the
		// review — the groups that failed can still be picked up by
		// a continuation, and the card says so.
		if err != nil {
			slog.Default().Warn("/review: every group failed",
				"agent", runnerName,
				"err", err,
			)
		}

		// The delivery: the review is handed to the chat — once the
		// findings are in and every group is in.
		//
		// The merged document is the agent's input — it is complete,
		// it carries the failed-group markers, and "fix the
		// blockers" reads it. The channel does NOT get it: the
		// findings already reached the chat through the coalesced
		// stream, and a second copy would say the same thing twice.
		//
		// The card's digest carries only the locations, so the
		// agent's answer is where the findings' text reaches the
		// reader when /think keeps the thread empty.
		data := map[string]string{"agent": runnerName}
		if groups := len(rec.outcomes); groups > 0 {
			data["groups"] = fmt.Sprintf("%d", groups)
		}
		f.deliver(reviewDelivery{
			ctx:        revCtx,
			cs:         cs,
			work:       work,
			emitter:    emitter,
			sink:       sink,
			drain:      drain,
			chatID:     chatID,
			replyTo:    replyTo,
			runner:     runnerName,
			sendBlocks: sendBlocks,
			rec:        rec,
			result:     result,
			notes:      result.Notes,
			data:       timeoutOutcomeData(data, groupOutcomes),
			notice:     reviewTimeoutNotice(groupOutcomes, timeout),
		})
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
// second prompt spelling it out. A group that never reached the
// agent is re-run instead: it gets its stored prompt, not this text.
const continueText = "continue"

// handleContinue finishes the review groups of the last review in this
// chat that did not complete.
//
// A group that reached the agent is resumed; a group that never
// reached the agent is re-run with the prompt it was asked to run.
//
// The runner is the agent recorded with the previous run — a session
// belongs to one agent and cannot be moved to another, so the
// four-tier runner resolution does not apply. The sessions run in the
// recorded workspace: the session belongs to that workspace, and
// resuming it against another working directory would point the
// agent at the wrong files.
//
// An --agent names another runner: the groups are taken over. The
// takeover re-runs every group with the prompt it was asked to run —
// the session belongs to the agent that ran it and cannot be moved.
//
// The merged result is delivered exactly like a review's: wrapped
// with agent.FormatReviewMessage and injected into the chat session's
// current AS as a user turn. The channel gets the continued groups'
// coalesced stream, framed by the plan line and the verdict line.
func (f *Factory) handleContinue(ctx context.Context, rt command.RuntimeServices,
	cs *chatsession.ChatSession, input command.SlashInput, spec Spec, notices []string,
	timeout time.Duration) (*command.SlashOutput, error) {
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

	// The runner of this continuation: the agent of the last review,
	// unless another one is named. Another one takes the groups over.
	runnerName := rec.agent
	takeover := false
	if spec.Agent != "" {
		runnerName = spec.Agent
		takeover = spec.Agent != rec.agent
	}

	jobs, why := rec.resumable(takeover)
	if why != "" {
		release()
		// The ℹ️ cards here mean "there was nothing to resume" —
		// the last review finished, or its one retry is spent.
		chatsession.ReportOutcome(ctx, command.NoopOutcome("there was nothing to resume", nil))
		return replyWith(ctx, rt, notices, why), nil
	}

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

	// The sessions run in the recorded workspace, not in the chat
	// session's current one.
	rc := agent.StartConfig{
		Workspace: rec.workspace,
	}

	// Capture the inputs the goroutine needs for an async reply. We
	// grab them by value so the closure doesn't pin SlashInput / agent
	// references for the whole continuation run.
	replyTo := input.MessageID
	workspace := rec.workspace
	sendBlocks := as.SendBlocks // close over SendBlocks for Inject
	emitter := cs.Emitter()

	// The continuation outlives the command exactly like a plain
	// review does — same hold, same reason. See handleReview.
	work := cs.Hold(replyTo)

	// The continuation is a review run like any other: the chat
	// stays marked as busy until the continuation itself is done.
	//
	// The run: no deadline, cancellable with the abort sentinel so
	// `/review --abort` can stop it too. It is registered BEFORE the
	// goroutine starts, so an abort that arrives right after the
	// command returned cannot race the registration.
	revCtx, handle := f.newRun(chatID, cs.Context(), runnerName, "continue")

	go func() {
		// The defer order is the run's shutdown contract: the
		// running marker is released and the message's hold is given
		// back first, and `done` closes LAST — an abort that waits on
		// it is guaranteed to find the run finished and its record
		// written.
		defer close(handle.done)
		defer f.endRun(chatID, handle)
		defer handle.cancel(nil)

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

		// The sink shows the process — the merged text is the main
		// agent's input and never reaches the channel.
		//
		// The drain is called once more below, right after the
		// fan-out ends: the groups' flushed batches are still queued
		// behind the drain goroutine, and the continuation's own
		// lines must land after them.
		sink, finalize := outbound.StreamRunOnceToEmitter(revCtx, emitter, cs, slog.Default(), chatID, replyTo, runnerName)
		var drainOnce sync.Once
		drain := func() { drainOnce.Do(finalize) }
		defer drain()

		result, err := agent.ContinueReviewJobs(revCtx, starter, rc, jobs, continueText,
			agent.WithEventSink(sink),
			agent.WithReviewJobObserver(logFailedGroups("/review --continue", runnerName, collector.observe)),
			// The plan names the continued groups, before the first
			// one produces output.
			agent.WithReviewPlanObserver(planObserver(revCtx, emitter, collector, chatID, replyTo, runnerName)),
			// The silence threshold of one group: a resumed subagent
			// that stops producing anything is closed on its own.
			agent.WithReviewGroupTimeout(timeout),
		)

		// The fan-out is over: the resumed sessions are closed and
		// their findings are in hand. There is nothing left for an
		// abort to stop, so the delivery below runs undisturbed.
		f.finishFanout(chatID, handle)

		// The record is updated with this run's results BEFORE the
		// result is delivered, so a second `--continue` finds
		// nothing left to resume. A group that fails again has
		// spent its one recovery attempt — the failure stays in the
		// record, the retry does not.
		//
		// A TAKEOVER is the exception: another agent picked the
		// groups up, and the operator's own recovery attempt is
		// still theirs to spend. The group stays resumable.
		//
		// An ABORTED continuation is the exception too: the groups
		// were stopped, not exhausted, so they keep their resume
		// attempt and stay resumable.
		abortedRun := aborted(revCtx)
		groupOutcomes := collector.results()
		switch {
		case abortedRun:
			// A group the abort caught before it spawned reports no
			// session id — the resume never got that far — but the
			// session it was reopening is still the one the record
			// holds. Without this the stopped group would look like
			// it never reached the agent, and would not be
			// resumable.
			carrySessionIDs(groupOutcomes, jobs)
		case !takeover:
			// A takeover does not spend the operator's recovery
			// attempt: the group failed under another agent, and the
			// operator may still pick it up.
			groupOutcomes = collector.spentResults()
		}
		// A resumed group keeps its session but loses its prompt: the
		// resume sent the text "continue", not the prompt the group
		// was asked to run. The prompt rides along from the record it
		// came from — it is the only way to run the group on another
		// agent.
		carryPrompts(groupOutcomes, rec.outcomes)
		merged := f.record(chatID, runnerName, workspace, reviewRun{
			outcomes:      groupOutcomes,
			contributions: collector.said(),
			plan:          collector.plan(),
		})
		finish()

		// The user aborted this run: the resumed sessions are closed
		// and the stopped groups stay resumable — the record above
		// holds them with their session ids.
		//
		// The abort branch comes BEFORE the failure branch: after an
		// abort the fan-out returns a context-cancellation error, and
		// the user must see "aborted", not "❌ /review continue
		// failed".
		if abortedRun {
			// The sink is drained first: the findings that had
			// already been queued land before the notice.
			drain()
			reportAborted(revCtx, work, emitter, chatID, replyTo, runnerName, "continuation", groupOutcomes)
			return
		}

		// The continuation failed as a run: every group it continued
		// failed again. That is not the end of the review — the
		// groups that survived are still worth delivering, and the
		// groups that died are named in the card.
		if err != nil {
			slog.Default().Warn("/review --continue failed",
				"agent", runnerName,
				"err", err,
			)
		}

		// The delivery: the review is handed to the chat — once the
		// findings are in and every group is in.
		//
		// The merged document is the agent's input — it is complete,
		// it carries the failed-group markers, and "fix the
		// blockers" reads it. The channel does NOT get it: the
		// findings already reached the chat through the coalesced
		// stream, and a second copy would say the same thing twice.
		//
		// A continuation fans out over the groups it CONTINUED, not
		// over every group of the review it continues — so its count
		// goes under `continued_groups`.
		continued, _ := groupCounts(groupOutcomes)
		data := map[string]string{"agent": runnerName}
		if continued > 0 {
			data["continued_groups"] = fmt.Sprintf("%d", continued)
		}
		f.deliver(reviewDelivery{
			ctx:        revCtx,
			cs:         cs,
			work:       work,
			emitter:    emitter,
			sink:       sink,
			drain:      drain,
			chatID:     chatID,
			replyTo:    replyTo,
			runner:     runnerName,
			sendBlocks: sendBlocks,
			rec:        merged,
			result:     result,
			notes:      result.Notes,
			data:       timeoutOutcomeData(data, groupOutcomes),
			notice:     reviewTimeoutNotice(groupOutcomes, timeout),
		})
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

// baseUnknownMessage renders the base-unknown failure: the workspace,
// the fact that the committed diff was not reviewed, and the command
// that fixes the cause.
//
// The remedy depends on why the base is unknown: an unreachable
// remote needs the symbolic ref set, a missing remote-tracking ref
// needs the fetch that creates it.
func baseUnknownMessage(workspace string, err error) string {
	reason := gitbase.ReasonRemoteUnreachable
	var unknown *agent.BaseUnknownError
	if errors.As(err, &unknown) && unknown.Reason != "" {
		reason = unknown.Reason
	}
	cause := "refs/remotes/origin/HEAD is missing and the remote query failed"
	fix := fmt.Sprintf("git -C %s remote set-head origin -a", shellPath(workspace))
	if reason == gitbase.ReasonMissingTrackingRef {
		cause = "refs/remotes/origin/HEAD is missing and the local remote-tracking ref is absent"
		fix = fmt.Sprintf("git -C %s fetch origin", shellPath(workspace))
	}
	return fmt.Sprintf("❌ could not determine the base branch for %s — %s. "+
		"The committed diff was not reviewed. Fix: %s", workspace, cause, fix)
}

// shellPath quotes a filesystem path for a copy-paste shell command.
//
// A path made only of safe characters stays bare, so the common case
// reads `git -C /home/me/repo`; anything else is quoted in the
// platform's shell style, so pasting the command still works for a
// workspace whose path holds a space or a shell metacharacter.
func shellPath(path string) string {
	if path != "" && strings.IndexFunc(path, func(r rune) bool {
		return !strings.ContainsRune(shellSafeChars(), r)
	}) == -1 {
		return path
	}
	if runtime.GOOS == "windows" {
		// cmd.exe knows double quotes; a path cannot hold a double
		// quote of its own.
		return `"` + path + `"`
	}
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

// shellSafeChars lists the characters a path may hold without
// quoting, in either shell.
//
// These are the characters a real path is made of: cmd.exe expands
// %VAR% and POSIX expands $VAR, $HOME and backticks — none of them
// are in this set, so a path holding only these needs no quoting.
func shellSafeChars() string {
	return "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-./\\:@+=~"
}
