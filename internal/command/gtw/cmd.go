package gtw

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/timeouts"
	"github.com/cnlangzi/nightme/pkg/outcome"
)

// withCommandTimeout applies the per-/gtw-command wall-clock
// budget (timeouts.Command) when the caller did not already set a
// deadline on parent. Mirrors the runCmd contract — "caller's
// deadline always wins" — so the runtime can pass a stricter ctx
// (e.g. an upper-bound for the whole inbound dispatcher chain)
// without being silently overridden by gtw.
//
// The wrap sits above all per-step budgets (CLI / Hook / Agent)
// so a hang that no single one of them covers — a stuck
// worktree-move branch, an agent session that never returns, a
// inner select loop waiting on a signal — still gets a cap and
// releases the per-chat run lock. The returned cancel MUST be
// deferred by the caller; missing it would leak the timer.
//
// When parent already has a deadline, the returned cancel is a
// no-op so the defer cost stays uniform across callers.
//
// Called once from Factory.Handle — placing the wrap at the
// top of Handle lets every /gtw subcommand inherit the cap for
// free without each run* having to repeat the two-line incant.
// Tests run the helper directly to pin the two-branch contract
// (applies when no deadline, returns parent unchanged when one
// is set).
func withCommandTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	if _, hasDeadline := parent.Deadline(); hasDeadline {
		return parent, func() {}
	}
	return context.WithTimeout(parent, timeouts.Command)
}

// requireCwd is command.RequireActiveCwd plus the outcome the
// refusal produces: a command that needs a workspace and has none
// failed, and a program driving it must be able to tell that apart
// from a command that ran and found nothing to do.
func requireCwd(ctx context.Context, cs *chatsession.ChatSession) (string, *command.SlashOutput) {
	cwd, failOut := command.RequireActiveCwd(cs)
	if failOut == nil {
		return cwd, nil
	}
	reportOutcome(ctx, command.FailedOutcome(http.StatusBadRequest, "the command needs an active workspace"))
	return "", failOut
}

// Factory implements command.SlashCommandFactory for /gtw.
//
// It holds the per-process *Manager (gtw state) and the
// runtime's HandlerDeps (Git / Prober /
// Detect / Now). The runtime constructs one Factory at startup
// and registers it with command.Registry.
//
// Wire-up example:
//
//	mgr := gtw.NewManager()
//	factory := gtw.NewFactoryWithDeps(mgr, deps)
//	reg := command.NewRegistry()
//	reg.Register(factory)
//
// v1.5: Manager.SetHandlerDeps was removed. The slash-command
// path takes deps via the Factory (NewFactoryWithDeps /
// Factory.SetHandlerDeps); Manager itself only owns the per-chat
// run lock.
type Factory struct {
	mgr  *Manager
	deps HandlerDeps
}

// NewFactory constructs a Factory backed by mgr. Use
// Factory.SetHandlerDeps afterwards (or NewFactoryWithDeps)
// to wire the runtime's HandlerDeps.
func NewFactory(mgr *Manager) *Factory {
	return &Factory{mgr: mgr}
}

// NewFactoryWithDeps constructs a Factory and primes it with
// the runtime's HandlerDeps.
func NewFactoryWithDeps(mgr *Manager, deps HandlerDeps) *Factory {
	f := &Factory{mgr: mgr}
	f.SetHandlerDeps(deps)
	return f
}

// init self-registers the gtw command. Phase 2.3: each
// command package's init() calls RegisterBuilder; the runtime
// orchestrator calls SetDeps once at startup to finalize
// every registered builder. GTWExt carries gtw.HandlerDeps
// (typed as `any` in command.Deps to avoid command ↔ gtw
// import cycle).
//
// gtw reads only d.GTWExt. Every *chatsession.ChatSession
// reference is supplied passively: slash commands receive cs
// from the dispatcher parameter. No cs lookup, cache, or
// chat-session store lives in this package — by construction
// there is no path for stale-cache / cross-channel-cwd-loss
// bugs.
func init() {
	command.RegisterBuilder(func(d command.Deps) command.SlashCommandFactory {
		handlerDeps, _ := d.GTWExt.(HandlerDeps)
		mgr := NewManager()
		return NewFactoryWithDeps(mgr, handlerDeps)
	})
}

// SetHandlerDeps primes the factory with runtime deps.
//
// The record store comes from the manager the factory owns: the
// `--continue` flags of /gtw commit and /gtw pr pick their run up
// from it.
func (f *Factory) SetHandlerDeps(deps HandlerDeps) {
	if deps.records == nil {
		deps.records = f.mgr.records
	}
	f.deps = deps
}

// hookContext derives the hook context of one gtw run.
//
// A continuation works on the workspace of the attempt it picks up
// — the chat's cwd may have moved since — so its hooks see that
// workspace rather than the chat's current one.
func (f *Factory) hookContext(ctx context.Context, cs *chatsession.ChatSession, chatID, command string, cont bool) HookContext {
	if cont {
		if rec, ok := f.deps.records.Lookup(chatID, command); ok {
			return HookContext{
				Command:  command,
				RepoRoot: rec.ctx.RepoRoot,
				Worktree: rec.ctx.Worktree,
				Branch:   rec.ctx.Branch,
			}
		}
	}
	return f.deriveHookContext(ctx, cs, command)
}

// deriveHookContext best-effort populates a HookContext from
// current chat state. Tries .nightme/gtw.yml first (authoritative
// when present); falls back to the canonical RepoRoot helper +
// `git rev-parse --abbrev-ref HEAD` on SelectedCwd.
//
// Any derivation failure is silent — empty fields are skipped by
// HookContext.ToEnv and hooks see "not set" rather than fake
// values. This keeps the "hooks are additive, never block main
// flow" iron rule intact.
//
// For commands that don't have a yml yet (runFix pre-creation,
// runSync on a manual branch) this gives hooks the rough shape:
// repo == worktree, branch from HEAD. Better-than-nothing —
// `codegraph init` etc. work.
func (f *Factory) deriveHookContext(ctx context.Context, cs *chatsession.ChatSession, command string) HookContext {
	hc := HookContext{Command: command}
	if cs == nil {
		return hc
	}
	cwd := cs.SelectedCwd()
	if cwd == "" {
		return hc
	}
	// yml path — exact values from the active fix.
	if c, err := ReadGTWYml(cwd); err == nil && c.Worktree != "" {
		hc.RepoRoot = c.RepoRoot
		hc.Worktree = c.Worktree
		hc.Branch = c.Branch
		return hc
	}
	if f.deps.Git == nil {
		return hc
	}
	// Fallback: use the canonical RepoRoot helper (which
	// distinguishes ErrNotInGitRepo). sync-style commands operate
	// on the main checkout, so repo == worktree is correct.
	if repo, err := RepoRoot(ctx, cwd, f.deps.Git); err == nil && repo != "" {
		hc.RepoRoot = repo
		hc.Worktree = repo
	}
	if out, _, err := f.deps.Git.Run(ctx, cwd, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		if trimmed := strings.TrimSpace(out); trimmed != "" && trimmed != "HEAD" {
			hc.Branch = trimmed
		}
	}
	return hc
}

// withHooks wraps an inner command run with the before/after
// hook lists from the user-level config. The hcFn closure is
// called TWICE — once before main() to capture pre-hook state,
// once after main() returns so the post-hook sees whatever main()
// produced. This is what lets /gtw fix's after-hook see the
// newly-created worktree: runFix mutates its captured HookContext
// from inside the main() closure (after WriteGTWYml), and withHooks
// reads it back via hcFn().
//
// The single-source-of-truth "cwd" the hook subprocess actually
// runs in is also re-read after main() — runFix's SetSelectedCwd
// moves chat cwd into the worktree, and post-hooks should run
// there (so $GTW_WORKTREE == $PWD, hooks can `cd $GTW_WORKTREE`
// without surprises). Pre-hooks run from the user's original cwd.
//
// Empty before+after short-circuits: no hcFn invocation, no
// git enrichment, no RunHooks calls. main() still runs and
// loadNotes still flow through to the user — the short-circuit
// only skips the (relatively expensive) env-derivation +
// shell-spawn paths. This avoids 2-4 wasted git invocations per
// command on a config that doesn't use hooks at all.
func (f *Factory) withHooks(
	ctx context.Context,
	cs *chatsession.ChatSession,
	chatID, messageID string,
	loadNotes LoadNotes,
	hcFn func() HookContext,
	before, after []Hook,
	main func() error,
) error {
	// Fast-path: no hooks configured → skip hcFn, DefaultBranch,
	// and RunHooks entirely. main() still runs (the actual gtw
	// command is the whole point) and loadNotes still flow through
	// to the user. This avoids 2-4 wasted git invocations per
	// command on a config that doesn't use hooks at all.
	if len(before) == 0 && len(after) == 0 {
		mainErr := main()
		if block := formatLoadNotes(loadNotes); block != "" {
			em := messages.Emitter(nil)
			if cs != nil {
				em = cs.Emitter()
			}
			_ = reply(ctx, em, chatID, messageID, block)
		}
		return mainErr
	}

	preCwd := ""
	if cs != nil {
		preCwd = cs.SelectedCwd()
	}
	preHC := hcFn()
	// Enrich DefaultBranch from the pre-state. Failure is
	// silent — "no default branch info" must NEVER block main
	// flow; that would re-introduce the v0 "yml missing → hard
	// fail" anti-pattern this whole feature was designed to
	// avoid.
	if preHC.RepoRoot != "" && f.deps.Git != nil {
		if db, derr := DefaultBranch(ctx, preHC.RepoRoot, f.deps.Git); derr == nil {
			preHC.DefaultBranch = db
		}
	}
	pre := RunHooks(ctx, before, preHC, preCwd)

	mainErr := main()

	// Re-read chat cwd (main() may have moved it via
	// SetSelectedCwd) and re-invoke hcFn (main() may have mutated
	// the captured HookContext — see /gtw fix).
	postCwd := preCwd
	if cs != nil {
		postCwd = cs.SelectedCwd()
	}
	postHC := hcFn()
	if postHC.RepoRoot != "" && f.deps.Git != nil {
		if db, derr := DefaultBranch(ctx, postHC.RepoRoot, f.deps.Git); derr == nil {
			postHC.DefaultBranch = db
		}
	}
	post := RunHooks(ctx, after, postHC, postCwd)

	var em messages.Emitter
	if cs != nil {
		em = cs.Emitter()
	}
	// Reply 1: load-notes + before-hooks. Combined so "your yml
	// had warnings" sits next to "here's what ran before the
	// action" — both are pre-action context.
	if block := formatLoadNotes(loadNotes) + FormatResults("before", pre); block != "" {
		_ = reply(ctx, em, chatID, messageID, block)
	}
	// Reply 2: after-hooks. Separate reply so it lands AFTER the
	// before-hooks reply in chat order — the user reads "before"
	// then "after", matching what actually happened.
	if block := FormatResults("after", post); block != "" {
		_ = reply(ctx, em, chatID, messageID, block)
	}
	return mainErr
}

// formatLoadNotes renders load-time diagnostics (read/parse
// errors when loading ~/.nightme/gtw.yml) into a small block
// that precedes hook output in the consolidated reply.
//
// Returns "" when there are no notes — callers concatenate
// without nil checks.
func formatLoadNotes(notes LoadNotes) string {
	if !notes.HasWarnings() {
		return ""
	}
	return "⚠️ hooks config\n" + strings.Join(notes.Warnings, "\n") + "\n"
}

// Spec implements command.SlashCommandFactory.
//
// Subcommands carries per-subcommand usage so /help can render
// each subcommand with its own usage hint rather than cramming
// the parent's Usage. The parent's Usage field stays as the
// terse /gtw help overview (the format /help and unknown-command
// replies both read from).
func (f *Factory) Spec() command.Spec {
	return command.Spec{
		Name:    "gtw",
		Aliases: []string{"team"},
		Summary: "GTW: Git-driven team workflow (claim, label, worktree).",
		Usage: "/gtw plan [topic]                   put the agent in plan mode: settle the design before any change\n" +
			"/gtw new [<material>]                have the agent draft issues from this chat's discussion, with the material in the same message\n" +
			"/gtw new -y                         create the issues from the reviewed drafts\n" +
			"/gtw fix <issue-id> [-y|--yes]     claim, label, and create a worktree\n" +
			"/gtw fix * [-y|--yes]              pick the next assigned issue and fix it\n" +
			"/gtw fix --name <branch>            create a local worktree (no issue)\n" +
			"/gtw assign <issue-id> [--remove|-r] attach or remove the `nightme/assigned` queue marker\n" +
			"/gtw close [--reset]                finalize the issue labels, tear down the worktree, and sync main\n" +
			"/gtw back                           exit the fix worktree (cwd → repoRoot) and sync main — worktree preserved\n" +
			"/gtw back <branch>                  enter the worktree whose branch starts with <branch> (cwd → worktree); repairs a missing .nightme/gtw.yml\n" +
			"/gtw commit [-a <agent>]            commit uncommitted work via the configured agent (no push)\n" +
			"/gtw commit -c                      continue the last failed /gtw commit of this chat\n" +
			"/gtw commit -a codex -g             make codex the default agent for /gtw commit\n" +
			"/gtw push                           push the worktree branch (clean only — refuses dirty)\n" +
			"/gtw pr [-a <agent>] [--ready]      generate PR title+body, then open the PR\n" +
			"/gtw pr -a claude                   override which agent runs the one-shot\n" +
			"/gtw pr -c                          continue the last failed /gtw pr of this chat\n" +
			"/gtw pr view [number]               read the merge state of a pull request (read-only)\n" +
			"/gtw config lang [value]            show or set the output language for this repository\n" +
			"/gtw config -g lang [value]         show or set the user-level output language\n" +
			"/gtw sync                           checkout the default branch and pull --rebase from origin\n" +
			"/gtw abort                          cancel the /gtw command currently running in this chat",
		Subcommands: []command.SubcommandSpec{
			{
				Name:    "plan",
				Summary: "put the current agent in plan mode to settle a design before any change",
				Usage:   "/gtw plan [topic]",
			},
			{
				Name:    "new",
				Summary: "draft issues from this chat's discussion via the current agent; the material after the command rides along; `-y` creates them",
				Usage:   newCmdSpec.Usage,
			},
			{
				Name:    "fix",
				Summary: "claim an issue, create a worktree, dispatch to the agent",
				Usage:   fixCmdSpec.Usage,
			},
			{
				Name:    "assign",
				Summary: "attach the `nightme/assigned` queue marker to an issue, or take it off again",
				Usage:   assignCmdSpec.Usage,
			},
			{Name: "close", Summary: "finalize the issue labels, tear down the worktree, delete the branch, and sync main", Usage: closeCmdSpec.Usage},
			{Name: "back", Summary: "exit the fix worktree (cwd → repoRoot) and sync main; `/gtw back <branch>` jumps into the worktree of that branch", Usage: "/gtw back [<branch>]"},
			{Name: "commit", Summary: "commit uncommitted work via the configured agent", Usage: commitCmdSpec.Usage},
			{Name: "push", Summary: "push the worktree branch (clean only — refuses dirty)"},
			{Name: "pr", Summary: "generate PR title+body, then open the PR", Usage: prCmdSpec.Usage},
			{
				Name:    "pr view",
				Summary: "read the merge state of a pull request (read-only)",
				Usage:   "/gtw pr view [number]",
				Hidden:  true,
			},
			{Name: "config", Summary: "show or set the gtw output language (repo scope by default, -g for the user scope)", Usage: configCmdSpec.Usage},
			{Name: "sync", Summary: "checkout the default branch and pull --rebase from origin"},
			{Name: "abort", Summary: "cancel the gtw command currently running in this chat"},
		},
	}
}

// Handle implements command.SlashCommandFactory. Routes by
// the first arg AFTER the command name to the per-subcommand
// handler.
//
// F-51 invariant: commander.Dispatch always prefixes the argv
// with the command name (Args[0] = "gtw"). The subcommand
// ("fix" / "list" / "reset") is therefore Args[1], not
// Args[0]. Tests written against this factory use the
// "Args[0] = subcommand" convention because the commander
// is bypassed; production callers must go through commander.
func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices, mgr *chatsession.Manager, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {
	if len(input.Args) < 2 {
		reportOutcome(ctx, command.FailedOutcome(http.StatusBadRequest, "no /gtw subcommand was given"))
		return &command.SlashOutput{
			Reply:    f.Spec().Usage,
			Consumed: true,
		}, nil
	}

	// Whole-pipeline wall-clock cap (timeouts.Command). Applied
	// HERE — once — instead of at the top of each run* function,
	// so every /gtw subcommand (including /gtw new, which uses an
	// early-return path that skips the run lock below) inherits
	// the cap for free. New subcommands pick it up automatically.
	// RunFix / RunClose / RunCommit / RunPR / RunAgentFor already
	// wrap the inner subprocess calls with their own narrower
	// budgets (CLI=5m, Agent=30m, Hook=30m) — this is the outer
	// net for anything those budgets don't cover. Sits AFTER the
	// usage-reply short-circuit above so /help stays fast.
	ctx, cancelCmd := withCommandTimeout(ctx)
	defer cancelCmd()

	// Per-chat run lock: previous /gtw for this chatID must
	// complete before the next one starts. F-59 made every
	// slash command async (a fresh goroutine per inbound), so
	// without serialisation two /gtw calls landing back-to-back
	// would race on Manager.drafts / the worktree directory /
	// cs.SelectedCwd / the agent session.
	//
	// (v1.5 removed the Manager.states layer; the run lock is
	// still required because the worktree, AS pool, and chat
	// cwd are per-chat and concurrently written by /gtw fix.)
	//
	// chatID == "" → runLockFor returns nil and we no-op (tests
	// drive Handle directly with empty ChatID; production always
	// has one). defer covers all return paths below (unknown
	// subcommand, subcommand errors, normal completion).
	//
	// Cross-chat independence: chat A's long /gtw fix does not
	// block chat B's /gtw commit; the lock is per chatID.
	//
	// /gtw plan and /gtw new take no lock: they touch neither the
	// worktree nor the chat cwd — plan only queues a prompt, new
	// only the chat's own issue draft. /gtw config likewise only
	// reads and writes ~/.nightme/gtw.yml — that file is shared by
	// every chat, so its read-modify-write is serialised by
	// configWriteMu rather than by the per-chat lock. /gtw abort
	// takes no lock either — it must not queue behind the very
	// command it has to cancel. /gtw assign flips a label on the
	// tracker and touches nothing else, so it needs no lock either
	// — see RunAssign for the label-write window that leaves open.
	switch input.Args[1] {
	case "plan":
		return f.runPlan(ctx, cs, input)
	case "new":
		return f.runNew(ctx, rt, cs, input)
	case "config":
		return f.runConfig(ctx, cs, input)
	case "abort":
		return f.runAbort(ctx, cs, input)
	case "assign":
		return f.runAssign(ctx, cs, input)
	}

	// The run's own context: cancellable with ErrAborted so
	// /gtw abort can end the command and tear down its
	// subprocesses. Every subcommand handler below receives runCtx
	// (not ctx), so the abort cause is visible to the whole call
	// tree.
	runCtx, cancelRun := context.WithCancelCause(ctx)
	defer cancelRun(nil)

	if mu := f.mgr.runLockFor(input.ChatID); mu != nil {
		mu.Lock()
		defer mu.Unlock()
	}
	// Registered AFTER the lock is taken, so the registry names
	// the chain holder (the command blocking every later /gtw in
	// this chat) and never a queued waiter.
	release := f.mgr.beginRun(input.ChatID, input.Args[1], cancelRun)
	defer release()

	var out *command.SlashOutput
	var err error
	switch input.Args[1] {
	case "fix":
		out, err = f.runFix(runCtx, rt, cs, input)
	case "close":
		out, err = f.runClose(runCtx, rt, cs, input)
	case "back":
		out, err = f.runBack(runCtx, rt, cs, input)
	case "commit":
		out, err = f.runCommit(runCtx, rt, cs, input)
	case "push":
		out, err = f.runPush(runCtx, rt, cs, input)
	case "pr":
		if isPRView(input.Args) {
			out, err = f.runPRView(runCtx, cs, input)
		} else {
			out, err = f.runPR(runCtx, rt, cs, input)
		}
	case "sync":
		out, err = f.runSync(runCtx, rt, cs, input)
	default:
		reportOutcome(ctx, command.FailedOutcome(http.StatusBadRequest,
			"unknown /gtw subcommand "+input.Args[1]))
		return &command.SlashOutput{
			Reply:    "Unknown subcommand: " + input.Args[1] + "\n" + f.Spec().Usage,
			Consumed: true,
		}, nil
	}
	if aborted(runCtx) {
		// Cancelled by /gtw abort: the abort card is the only
		// message this run produces. The outcome is `cancelled` —
		// the run did not fail, it was stopped.
		chatsession.ReportOutcome(ctx, outcome.Outcome{
			Success:    false,
			Status:     outcome.StatusClientClosedRequest,
			StatusText: "the command was cancelled by /gtw abort",
		})
		return &command.SlashOutput{Consumed: true}, nil
	}
	return out, err
}

// gtwTail returns the free-form text the user typed after the
// "/gtw <subcommand>" prefix, with its line structure intact, and
// the part of it that sits on the command's own line.
//
// The raw text wins: it is the message as the user typed it. Only
// when the caller filled Args alone — programmatic callers and tests
// — does the whitespace-joined argv stand in for it.
func gtwTail(input command.SlashInput) (tail, line string) {
	if input.Text == "" {
		argv := strings.Join(input.Args[2:], " ")
		return argv, argv
	}
	return command.CommandTail(input.Text, 2)
}

// runPlan handles `/gtw plan [topic]`. The topic is free-form
// prose for the agent, so it bypasses command.ParseCmdArgs like
// /steer does: a leading "-" is data, not a flag. It comes from the
// raw text, so a multi-line topic keeps its line structure.
func (f *Factory) runPlan(ctx context.Context, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {
	if _, failOut := requireCwd(ctx, cs); failOut != nil {
		return failOut, nil
	}
	topic, _ := gtwTail(input)
	_, _ = RunPlan(ctx, cs, f.deps, input.ChatID, input.MessageID, topic)
	return &command.SlashOutput{Consumed: true}, nil
}

// runAssign handles `/gtw assign <issue-id> [--remove|-r]`: it puts
// the queue marker `nightme/assigned` on an issue, or takes it off.
//
// The command touches the tracker and nothing else — no worktree, no
// branch, no agent, no state file — so it takes no run lock, fires no
// hooks and dispatches no agent.
func (f *Factory) runAssign(ctx context.Context, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {
	args, err := parseAssignArgs(input.Args[2:])
	if err != nil {
		reportOutcome(ctx, command.FailedOutcome(http.StatusBadRequest, "the command line is not valid"))
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ %v", err),
			Consumed: true,
		}, nil
	}

	_, _ = RunAssign(ctx, cs, f.deps, input.ChatID, input.MessageID, args.IssueID, args.Remove)
	return &command.SlashOutput{Consumed: true}, nil
}

// runNew handles the two steps of /gtw new: `/gtw new [<material>]`
// asks the current agent to draft the issues (RunNewDraft), `/gtw
// new -y` creates them from the reviewed drafts (RunNewCreate). The
// material is the text the user typed after the command; it reaches
// the agent in the same message as the task. Only the creation step
// is wrapped in withHooks — new.before / new.after describe "issues
// are being created", which drafting does not do. They run once per
// `/gtw new -y`, however many drafts it creates.
func (f *Factory) runNew(ctx context.Context, _ command.RuntimeServices, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {
	tail, line := gtwTail(input)
	args, err := parseNewIssueArgs(tail, line)
	if err != nil {
		reportOutcome(ctx, command.FailedOutcome(http.StatusBadRequest, "the command line is not valid"))
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ %v", err),
			Consumed: true,
		}, nil
	}
	if _, failOut := requireCwd(ctx, cs); failOut != nil {
		return failOut, nil
	}

	cfg, loadNotes := Load()

	if !args.Yes {
		// The draft path has no withHooks wrapper, so the load
		// notes ride out as their own reply — a malformed yml
		// would otherwise silently fall back to the default
		// output language.
		if notes := formatLoadNotes(loadNotes); notes != "" {
			_ = reply(ctx, cs.Emitter(), input.ChatID, input.MessageID, notes)
		}
		_, _ = RunNewDraft(ctx, cs, f.deps, f.mgr, input.ChatID, input.MessageID, cfg, args.Answers)
		return &command.SlashOutput{Consumed: true}, nil
	}

	hc := f.deriveHookContext(ctx, cs, "new")
	hcFn := func() HookContext { return hc }
	err = f.withHooks(ctx, cs, input.ChatID, input.MessageID,
		loadNotes, hcFn, cfg.New.Hooks.Before, cfg.New.Hooks.After,
		func() error {
			_, e := RunNewCreate(ctx, cs, f.deps, input.ChatID, input.MessageID)
			return e
		})
	if err != nil {
		reportOutcome(ctx, command.FailedOutcome(http.StatusInternalServerError, "the issues could not be created"))
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ /gtw new failed: %v", err),
			Consumed: true,
		}, nil
	}
	return &command.SlashOutput{Consumed: true}, nil
}

// runFix handles `/gtw fix <issue-id>` and `/gtw fix --name <branch>`.
// F-XX splits the entry into two modes at the factory boundary
// so RunFix sees a single Mode constant.
//
// F-XX adds `-y` / `--yes` to bypass assess-first dispatch and
// go straight to the Execute prompt.
//
// F-51: command.Commander prefixes Args with the command name
// ("gtw"), then the subcommand ("fix"), then the subcommand's
// args. So Args[2] is the first user-supplied token.
//
// Usage hints come from fixCmdSpec.Usage — the parser and this
// method both read from that single source. Adding or retiring
// a flag means updating fixCmdSpec; the inline-usage reply
// (empty argv) and parse-error replies stay in lockstep.
func (f *Factory) runFix(ctx context.Context, _ command.RuntimeServices, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {
	if len(input.Args) < 3 {
		reportOutcome(ctx, command.FailedOutcome(http.StatusBadRequest, "no issue id or branch name was given"))
		return &command.SlashOutput{
			Reply:    fixCmdSpec.Usage,
			Consumed: true,
		}, nil
	}

	args, err := parseFixArgs(input.Args[2:])
	if err != nil {
		// Errors from command.ParseCmdArgs already carry the
		// ". Usage: ..." suffix via spec.usageTail(); our
		// post-parse checks (mutual exclusion, missing arg) do
		// the same via spec.UsageTail(). A single "❌ " prefix
		// is all the caller needs to layer on top.
		reportOutcome(ctx, command.FailedOutcome(http.StatusBadRequest, "the command line is not valid"))
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ %v", err),
			Consumed: true,
		}, nil
	}

	// Local-mode quick validation: the slug must not be empty
	// after normalisation. Doing this here (before RunFix) keeps
	// the SlashOutput path simple when no channel is wired in
	// tests. We cache the derived branch name so we don't pay
	// DeriveBranchFromName twice (validation + hc.Branch fill).
	var predictedBranch string
	if args.Mode == ModeLocal {
		derived, derr := DeriveBranchFromName(args.RawArg)
		if derr != nil {
			reportOutcome(ctx, command.FailedOutcome(http.StatusBadRequest,
				"the branch name could not be derived"))
			return &command.SlashOutput{
				Reply:    fmt.Sprintf("❌ %v", derr),
				Consumed: true,
			}, nil
		}
		predictedBranch = derived
	}
	// ID-mode quick validation: pre-validate locally with
	// parseIssueID (not strconv.Atoi) so "#42" is accepted —
	// the GitHub/GitLab convention users have muscle memory for.
	// The automatic mode names no issue, so it has no id to
	// validate.
	if args.Mode == ModeRemote && !args.Auto {
		if _, err := parseIssueID(args.RawArg); err != nil {
			reportOutcome(ctx, command.FailedOutcome(http.StatusBadRequest, "the issue id is not valid"))
			return &command.SlashOutput{
				Reply:    fmt.Sprintf("Invalid issue id: %q (%v)", args.RawArg, err),
				Consumed: true,
			}, nil
		}
	}

	// cs is supplied by the dispatcher — the same ChatSession
	// that /cwd, /use, /close and other slash commands see in
	// the same chat. No second lookup, no cache that could go
	// stale.
	_, failOut := requireCwd(ctx, cs)
	if failOut != nil {
		return failOut, nil
	}

	// Load user-level hook config (silent if missing). The
	// before/after lists wrap the actual RunFix call below; any
	// load-time warnings ride along in the consolidated reply.
	cfg, loadNotes := Load()

	// RunFix signature: (ctx, mode, cs, deps, chatID,
	// messageID, args, yes). Reply is sent inline via
	// cs.Emitter(); *Result only carries Consumed / Dropped for
	// the runtime. The withHooks wrapper fires before/after
	// hooks around the call and ships the hook output as a
	// follow-up reply (per wip/gtw-hooks.md always-echo policy).
	// Pre-fill what we know. The hc variable is captured by the
	// hcFn closure below; when main() reassigns it on success
	// (cs.SelectedCwd has moved into the worktree and WriteGTWYml
	// wrote .nightme/gtw.yml, so a fresh deriveHookContext reads
	// the full state), the next hcFn() call — for the post-hook
	// — sees the new value. Pre-hook sees the predicted state.
	hc := f.deriveHookContext(ctx, cs, "fix")
	hc.Branch = predictedBranch // empty string for ModeRemote
	hcFn := func() HookContext { return hc }
	err = f.withHooks(ctx, cs, input.ChatID, input.MessageID,
		loadNotes, hcFn, cfg.Fix.Hooks.Before, cfg.Fix.Hooks.After,
		func() error {
			_, e := RunFix(ctx, args.Mode, cs, f.deps,
				input.ChatID, input.MessageID,
				[]string{args.RawArg}, args.Yes, args.Auto)
			if e == nil {
				// Re-derive so post-hook sees the worktree
				// that RunFix just created + yml-wrote.
				hc = f.deriveHookContext(ctx, cs, "fix")
			}
			return e
		})
	if err != nil {
		reportOutcome(ctx, command.FailedOutcome(http.StatusInternalServerError, "the worktree could not be created"))
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ /gtw fix failed: %v", err),
			Consumed: true,
		}, nil
	}
	return &command.SlashOutput{Consumed: true}, nil
}

// fixArgs bundles the parsed argv tail of `/gtw fix <...>`.
// Splitting it into a struct (rather than separate return
// values) keeps the parser functions readable as we add more
// flags — `--yes` / `-y` is the first, future flags
// (`--no-dispatch`, `--base <ref>`) can land here without
// breaking signatures again.
type fixArgs struct {
	Mode   Mode
	RawArg string // issue id (ModeRemote) or branch name (ModeLocal)
	Yes    bool   // --yes / -y: dispatch Execute Prompt instead of Assess
	Auto   bool   // the positional was `*`: pick the next assigned issue
}

// fixCmdSpec is the argv grammar for /gtw fix. Lexer is the
// shared command.ParseCmdArgs (issue #291) — see
// internal/command/args.go for the contract.
//
// Two valid shapes (one positional OR one --name value, never
// both):
//
//	<issue-id>              ModeRemote (issue id from positional)
//	*                       ModeRemote (the next assigned issue)
//	--name <branch>         ModeLocal  (branch from --name value)
//
// Both shapes accept an optional -y/--yes to dispatch the
// Execute prompt instead of the Assess prompt. The mutual
// exclusion between the two shapes is enforced in
// parseFixArgs' post-parse step (the shared lexer can't
// express it because it doesn't know which positional pattern
// a command accepts).
var fixCmdSpec = command.CmdSpec{
	Name: "/gtw fix",
	// Single-line Usage keeps the ". Usage: …" suffix readable
	// when appended to parse-error messages — multi-line
	// Usage would land the second half mid-sentence in
	// every error reply.
	Usage: "/gtw fix <issue-id> [-y|--yes] | /gtw fix * [-y|--yes] | /gtw fix --name <branch> | -n <branch>",
	Flags: map[string]command.FlagSpec{
		"-y":     {Name: "yes"},
		"--yes":  {Name: "yes"},
		"-n":     {Name: "name", TakesValue: true},
		"--name": {Name: "name", TakesValue: true},
	},
	// 0..1 positionals: <issue-id> alone for Remote (1
	// positional); --name <branch> consumes its value (0
	// positionals left). MaxArgs=1 also catches
	// `/gtw fix 42 extra`.
	MinArgs: 0,
	MaxArgs: 1,
}

// parseFixArgs lexes the argv tail of `/gtw fix <...>` via the
// shared command.ParseCmdArgs and decides which Mode the user
// is asking for. Returns *fixArgs{Mode, RawArg, Yes, Auto}; any
// failure comes straight from command.ParseCmdArgs and already
// carries the Usage tail (issue #291 contract).
//
// Two /gtw fix-specific concerns live outside the shared
// lexer:
//
//  1. Mutual exclusion between --name <branch> and a bare
//     positional. The shared lexer can't express it because it
//     doesn't know which positional pattern a command accepts.
//     We use parsed.Has("name") for flag presence (so an empty
//     --name value still counts as "the flag was used") rather
//     than parsed.Value("name") != "" which would conflate
//     absent with empty.
//  2. Empty / whitespace-only --name value. The shared lexer
//     happily consumes the next token verbatim (even an empty
//     one); we surface this as a user-facing error rather than
//     handing an empty branch name to DeriveBranchFromName.
//
// The positional `*` selects the automatic mode: the issue is
// picked from the queue instead of named by the user. It is a
// positional, not a flag, so `--name` and `*` are mutually
// exclusive like an issue id and a branch name are.
func parseFixArgs(argv []string) (fixArgs, error) {
	parsed, err := command.ParseCmdArgs(argv, fixCmdSpec)
	if err != nil {
		return fixArgs{}, err
	}

	hasName := parsed.Has("name")
	hasPositional := parsed.NArgs() > 0

	// Mutual exclusion: --name flag presence and a bare
	// positional cannot be combined.
	if hasName && hasPositional {
		return fixArgs{}, fmt.Errorf(
			"%s takes either <issue-id> or --name <branch>, not both%s",
			fixCmdSpec.Name, fixCmdSpec.UsageTail())
	}

	name := strings.TrimSpace(parsed.Value("name"))
	positional := strings.TrimSpace(parsed.Arg(0))

	switch {
	case hasName:
		if name == "" {
			return fixArgs{}, fmt.Errorf(
				"%s: --name requires a non-empty branch name%s",
				fixCmdSpec.Name, fixCmdSpec.UsageTail())
		}
		return fixArgs{Mode: ModeLocal, RawArg: name, Yes: parsed.Bool("yes")}, nil
	case hasPositional:
		if positional == autoSelector {
			return fixArgs{Mode: ModeRemote, RawArg: autoSelector, Auto: true, Yes: parsed.Bool("yes")}, nil
		}
		return fixArgs{Mode: ModeRemote, RawArg: positional, Yes: parsed.Bool("yes")}, nil
	default:
		return fixArgs{}, fmt.Errorf(
			"%s needs <issue-id> or --name <branch>%s",
			fixCmdSpec.Name, fixCmdSpec.UsageTail())
	}
}

// closeCmdSpec is the argv grammar for /gtw close.
//
// `--reset` clears the issue's `nightme/*` state labels without
// marking the work done: the default close puts `nightme/done` on the
// issue, `--reset` only takes the state labels off.
//
// The flag is boolean — it takes no value — so `--reset` alone is the
// whole argument.
var closeCmdSpec = command.CmdSpec{
	Name:  "/gtw close",
	Usage: "/gtw close [--reset]",
	Flags: map[string]command.FlagSpec{
		"--reset": {Name: "reset"},
	},
	MinArgs: 0,
	MaxArgs: 0,
}

// runClose handles `/gtw close`. Tears down the worktree
// created by `/gtw fix`, deletes the local branch, switches CWD
// back to the main repo, clears gtw state, and then runs the
// upstream sync (same card /gtw sync emits). Two cards land in
// the chat: one for the local teardown, one for the sync.
// See wip/gtw.md §14.5 for the full flow and RunClose for the
// implementation.
//
// One flag: `--reset` clears the issue's state labels instead of
// marking the work done. Close is otherwise intentionally
// all-or-nothing — if the worktree is dirty the user must commit /
// stash / discard before re-running; we don't expose a
// force-escape hatch. Neither /gtw close nor /gtw fix exposes a
// --force: stale worktree paths are cleaned manually via
// `git worktree remove --force <path>` or by re-running
// `/gtw close` after the user has unblocked the worktree.
//
// The close also finalises the pull request the branch opened: its
// `nightme/*` state labels come off, so the tracker shows the work
// as finished on both sides. That step only warns when it fails —
// the PR's labels are bookkeeping, and the teardown must not wait
// for them.
//
// No shim is needed; RunClose reads its state from the
// cwd-scoped yml directly. Deps are forwarded verbatim; the
// reply path is RunClose's own cs.Emitter() (no extra wiring).
// Wrapped in withHooks so close.before / close.after fire.
func (f *Factory) runClose(ctx context.Context, _ command.RuntimeServices, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {
	// Issue #291: /gtw close takes no positional args, so anything
	// in the tail that is not `--reset` is a user mistake — most
	// importantly `/gtw close --force`, which the F-XX removal
	// note explicitly tells users not to expect. Before this gate
	// the token was silently swallowed and the close ran anyway, so
	// the user got no signal that --force did nothing.
	parsed, err := command.ParseCmdArgs(input.Args[2:], closeCmdSpec)
	if err != nil {
		reportOutcome(ctx, command.FailedOutcome(http.StatusBadRequest, "the command line is not valid"))
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ %v", err),
			Consumed: true,
		}, nil
	}
	reset := parsed.Bool("reset")

	cfg, loadNotes := Load()

	hc := f.deriveHookContext(ctx, cs, "close")
	hcFn := func() HookContext { return hc }
	err = f.withHooks(ctx, cs, input.ChatID, input.MessageID,
		loadNotes, hcFn, cfg.Close.Hooks.Before, cfg.Close.Hooks.After,
		func() error {
			res, e := RunClose(ctx, cs, f.deps, input.ChatID, input.MessageID, reset)
			_ = res // RunClose already sent the reply via cs.Emitter()
			// RunClose tears down the worktree + branch +
			// .nightme/gtw.yml and resets cs.SelectedCwd
			// back to repoRoot. Post-hook env then reflects
			// the post-close state: yml is gone so the git
			// fallback fires, and since cwd is now repoRoot,
			// GTW_WORKTREE == GTW_REPO_ROOT in the env.
			// That's semantically muddled (the label
			// "WORKTREE" is misleading when there's no
			// separate worktree) but it's the path the user
			// is now in, and hooks can detect
			// "no active fix" by checking the yml-equivalent
			// — see gtw/README.md §0. The alternative
			// (leaving GTW_WORKTREE empty) would force every
			// hook to handle "post-close" specially, which
			// is worse than a slightly confusing label.
			if e == nil {
				hc = f.deriveHookContext(ctx, cs, "close")
			}
			return e
		})
	if err != nil {
		reportOutcome(ctx, command.FailedOutcome(http.StatusInternalServerError, "the worktree could not be closed"))
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ /gtw close failed: %v", err),
			Consumed: true,
		}, nil
	}
	return &command.SlashOutput{Consumed: true}, nil
}

// runBack handles `/gtw back`. Non-destructive counterpart to
// runClose: switches the chat's SelectedCwd back to the main
// repo without removing the worktree, the branch, or the .nightme/
// gtw.yml, then runs the upstream sync so the next /gtw fix
// starts on a clean baseline.
//
// The worktree stays alive so the user can `/cwd` back into it
// to resume — back is a "pause and step out" gesture, not a
// teardown. See RunBack (back.go) for the implementation.
//
// No flags — back is intentionally minimal. Unlike close, back
// does NOT refuse on a dirty worktree (the uncommitted work is
// exactly the point of being able to come back to it). Like
// close, back takes no positional args; the same ParseCmdArgs
// gate rejects typo-flags like `/gtw back --force` instead of
// silently ignoring them (issue #291).
//
// Construction mirrors runClose: same withHooks wrap so
// back.before / back.after fire, same hc re-derive after main()
// so the post-hook env reflects the post-back state. Back does
// NOT touch the Manager slot (the fix is still in flight per
// the yml; clearing would silently break reactions if the user
// /cwd's back), does NOT evict agent sessions (they're pinned
// to a still-alive worktree), and does NOT call /new (back is
// reversible — the user might resume in minutes).
func (f *Factory) runBack(ctx context.Context, _ command.RuntimeServices, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {
	parsed, err := command.ParseCmdArgs(input.Args[2:], command.CmdSpec{
		Name:    "/gtw back",
		Usage:   "/gtw back [<branch>]",
		MinArgs: 0,
		MaxArgs: 1,
	})
	if err != nil {
		reportOutcome(ctx, command.FailedOutcome(http.StatusBadRequest, "the command line is not valid"))
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ %v", err),
			Consumed: true,
		}, nil
	}

	// Preflight: back operates on the current workspace
	// (cs.SelectedCwd is the only handle we have on which
	// worktree to step out of). Mirror runSync's pattern
	// (cmd.go) — the handler rejects empty-cwd before
	// RunBack is ever called, so RunBack can trust its input.
	// RunBack also has a defensive guard for direct callers
	// (tests, future call sites) — belt and suspenders.
	if _, failOut := requireCwd(ctx, cs); failOut != nil {
		return failOut, nil
	}

	cfg, loadNotes := Load()

	// --- /gtw back (no arg): exit the worktree → repoRoot ------
	// The original non-destructive path; cwd must be inside a
	// worktree (RunBack's own gate refuses otherwise).
	if parsed.NArgs() == 0 {
		hc := f.deriveHookContext(ctx, cs, "back")
		hcFn := func() HookContext { return hc }
		err := f.withHooks(ctx, cs, input.ChatID, input.MessageID,
			loadNotes, hcFn, cfg.Back.Hooks.Before, cfg.Back.Hooks.After,
			func() error {
				res, e := RunBack(ctx, cs, f.deps, input.ChatID, input.MessageID)
				_ = res // RunBack already sent the reply via cs.Emitter()
				// RunBack moves cs.SelectedCwd back to repoRoot but
				// leaves the worktree on disk, so the yml is still
				// readable — deriveHookContext will keep populating
				// Worktree / Branch from the yml rather than the
				// git fallback. Re-derive so post-hook env reflects
				// the post-back state, matching runClose's pattern.
				if e == nil {
					hc = f.deriveHookContext(ctx, cs, "back")
				}
				return e
			})
		if err != nil {
			reportOutcome(ctx, command.FailedOutcome(http.StatusInternalServerError, "the worktree could not be entered"))
			return &command.SlashOutput{
				Reply:    fmt.Sprintf("❌ /gtw back failed: %v", err),
				Consumed: true,
			}, nil
		}
		return &command.SlashOutput{Consumed: true}, nil
	}

	// --- /gtw back <branch>: enter the worktree of that branch --
	// Inverse direction: cwd must BE a root of the repository —
	// the main checkout or any worktree of it
	// (RunBackToWorktree's own gate refuses otherwise). Hooks
	// reuse cfg.Back.Hooks — the user's "before / after back"
	// rules apply symmetrically to both directions.
	// TrimSpace + empty-arg guard live inside RunBackToWorktree;
	// duplicating them here would drift the wording over time.
	// The handler's only job is parse + dispatch — the gate is
	// the function's.
	name := strings.TrimSpace(parsed.Arg(0))
	hc := f.deriveHookContext(ctx, cs, "back")
	hcFn := func() HookContext { return hc }
	err = f.withHooks(ctx, cs, input.ChatID, input.MessageID,
		loadNotes, hcFn, cfg.Back.Hooks.Before, cfg.Back.Hooks.After,
		func() error {
			_, e := RunBackToWorktree(ctx, cs, f.deps, input.ChatID, input.MessageID, name)
			if e == nil {
				hc = f.deriveHookContext(ctx, cs, "back")
			}
			return e
		})
	if err != nil {
		reportOutcome(ctx, command.FailedOutcome(http.StatusInternalServerError, "the worktree could not be entered"))
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ /gtw back failed: %v", err),
			Consumed: true,
		}, nil
	}
	return &command.SlashOutput{Consumed: true}, nil
}

// runPush handles `/gtw push`. Reads the yml at the current
// CWD to find the worktree, pushes the branch to origin.
//
// F-XX (commit/push split): push no longer auto-commits. A
// dirty worktree is a hard refusal — the user runs `/gtw
// commit` first, then `/gtw push`. Push's agent path (Branch 2
// of the legacy flow) moved to `/gtw commit`, so the
// `push.agent:` yml key is ignored when present (parser keeps
// it for schema compatibility but the dispatcher no longer
// reads it).
//
// No draft shim needed — push doesn't touch reaction cards.
// Just the same HandlerDeps as everywhere else. Wrapped in
// withHooks so push.before / push.after from ~/.nightme/gtw.yml
// fire.
func (f *Factory) runPush(ctx context.Context, _ command.RuntimeServices, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {
	args, err := parsePushArgs(input.Args[2:])
	if err != nil {
		reportOutcome(ctx, command.FailedOutcome(http.StatusBadRequest, "the command line is not valid"))
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ %v", err),
			Consumed: true,
		}, nil
	}

	cfg, loadNotes := Load()

	hc := f.deriveHookContext(ctx, cs, "push")
	hcFn := func() HookContext { return hc }
	err = f.withHooks(ctx, cs, input.ChatID, input.MessageID,
		loadNotes, hcFn, cfg.Push.Hooks.Before, cfg.Push.Hooks.After,
		func() error {
			res, e := dispatchPush(ctx, cs, f.deps, input.ChatID, input.MessageID, args)
			_ = res // dispatchPush already sent the reply via cs.Emitter()
			// dispatchPush doesn't move cs.SelectedCwd, so
			// re-derive is a no-op here — but we still call
			// it so the post-hook env is always fresh (e.g.
			// if a future change mutates cwd during push).
			if e == nil {
				hc = f.deriveHookContext(ctx, cs, "push")
			}
			return e
		})
	if err != nil {
		reportOutcome(ctx, command.FailedOutcome(http.StatusInternalServerError, "the branch could not be pushed"))
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ /gtw push failed: %v", err),
			Consumed: true,
		}, nil
	}
	return &command.SlashOutput{Consumed: true}, nil
}

// runCommit handles `/gtw commit`. Reads the yml at the current
// CWD to find the worktree, dispatches the configured one-shot
// agent to commit uncommitted changes (no push). Wrapped in
// withHooks so commit.before / commit.after from
// ~/.nightme/gtw.yml fire.
//
// Mirror of runPush with the Agent plumbed through to the
// dispatcher. Doesn't touch gtw state or reaction cards, so no
// draft shim is needed.
//
// The three scope flags work the way /review's do: `--continue`
// picks the last failed commit of this chat back up, `-g` persists
// the agent of this invocation as `commit.agent`, and `--unset`
// removes it again (and starts no commit).
func (f *Factory) runCommit(ctx context.Context, _ command.RuntimeServices, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {
	args, err := parseCommitArgs(input.Args[2:])
	if err != nil {
		reportOutcome(ctx, command.FailedOutcome(http.StatusBadRequest, "the command line is not valid"))
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ %v", err),
			Consumed: true,
		}, nil
	}
	if err := validateGTWFlags(args.Agent, args.Global, args.Unset, args.Continue); err != nil {
		reportOutcome(ctx, command.FailedOutcome(http.StatusBadRequest, "the flag combination is not valid"))
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ %v%s", err, commitCmdSpec.UsageTail()),
			Consumed: true,
		}, nil
	}

	// The global scope: `-g` persists the agent, `--unset` removes
	// it. The write happens before the command runs — the persisted
	// setting is machine-wide and does not depend on the chat's
	// workspace.
	notices, done, writeFailed, err := applyGlobalScope(subCommit, args.Agent, args.Global, args.Unset, "committing")
	if err != nil {
		status := http.StatusBadRequest
		summary := "the commit agent could not be persisted"
		if errors.Is(err, agent.ErrUnknownAgent) {
			status = http.StatusNotFound
			summary = "the commit agent is not a registered agent"
		}
		reportOutcome(ctx, command.FailedOutcome(status, summary))
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ %v", err),
			Consumed: true,
		}, nil
	}
	if writeFailed {
		// The commit still runs, but persisting the agent did not
		// happen — half of what was asked.
		reportOutcome(ctx, outcome.Outcome{
			Success:    false,
			Status:     http.StatusInternalServerError,
			StatusText: "the commit agent could not be persisted",
		})
	}
	if len(notices) > 0 {
		reply(ctx, cs.Emitter(), input.ChatID, input.MessageID, strings.Join(notices, "\n"))
	}
	if done {
		// --unset: the persisted default is gone and no commit runs.
		reportOutcome(ctx, outcome.Outcome{
			Success:    true,
			Status:     http.StatusOK,
			StatusText: "the global commit agent was removed",
			Data:       map[string]string{KeyScope: "global"},
		})
		return &command.SlashOutput{Consumed: true}, nil
	}

	cfg, loadNotes := Load()

	hc := f.hookContext(ctx, cs, input.ChatID, "commit", args.Continue)
	hcFn := func() HookContext { return hc }
	err = f.withHooks(ctx, cs, input.ChatID, input.MessageID,
		loadNotes, hcFn, cfg.Commit.Hooks.Before, cfg.Commit.Hooks.After,
		func() error {
			res, e := dispatchCommit(ctx, cs, f.deps, input.ChatID, input.MessageID, args, cfg)
			_ = res // dispatchCommit already sent the reply via cs.Emitter()
			// A continuation works in the recorded workspace, so its
			// after-hook keeps that workspace — re-deriving from the
			// chat's cwd would point the hook at the wrong tree.
			if e == nil && !args.Continue {
				hc = f.deriveHookContext(ctx, cs, "commit")
			}
			return e
		})
	if err != nil {
		reportOutcome(ctx, command.FailedOutcome(http.StatusInternalServerError, "the work could not be committed"))
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ /gtw commit failed: %v", err),
			Consumed: true,
		}, nil
	}
	return &command.SlashOutput{Consumed: true}, nil
}

// runSync handles `/gtw sync`: checkout the default branch and
// pull --rebase from origin. The reply is an IM-friendly
// compact summary (✅ branch @ sha + commit list, or ✅
// already up to date) — not git's raw pull stdout. Errors are
// surfaced verbatim (RefreshDefaultBranch already includes the
// dirty-worktree refusal and rebase-conflict guidance the
// user needs).
//
// A branch that was already up to date is a sync that did what it
// was asked to do: the outcome is `ok`, and the card says so.
//
// Wrapped in withHooks so sync.before / sync.after fire.
func (f *Factory) runSync(ctx context.Context, _ command.RuntimeServices, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {
	// Issue #291: no flags, no positional args — reject the tail
	// instead of silently ignoring it (`/gtw sync --rebase` used
	// to look like it selected a strategy).
	if _, err := command.ParseCmdArgs(input.Args[2:], command.CmdSpec{
		Name:    "/gtw sync",
		Usage:   "/gtw sync",
		MinArgs: 0,
		MaxArgs: 0,
	}); err != nil {
		reportOutcome(ctx, command.FailedOutcome(http.StatusBadRequest, "the command line is not valid"))
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ %v", err),
			Consumed: true,
		}, nil
	}

	cwd, failOut := requireCwd(ctx, cs)
	if failOut != nil {
		return failOut, nil
	}

	cfg, loadNotes := Load()

	hc := f.deriveHookContext(ctx, cs, "sync")
	hcFn := func() HookContext { return hc }

	var replyText string
	var syncFacts *syncOutcome
	err := f.withHooks(ctx, cs, input.ChatID, input.MessageID,
		loadNotes, hcFn, cfg.Sync.Hooks.Before, cfg.Sync.Hooks.After,
		func() error {
			repoRoot, e := RepoRoot(ctx, cwd, f.deps.Git)
			if e != nil {
				return e
			}
			// /gtw sync is a user-initiated refresh, so it
			// ignores the SkipRefreshDefaultBranch test seam
			// (buildSyncReply honours it as a short-circuit —
			// but a real user calling /gtw sync should never
			// see that flag set).
			body, facts, e := buildSyncReply(ctx, repoRoot, f.deps)
			if e != nil {
				return e
			}
			replyText = body
			syncFacts = facts
			// Re-derive so post-hook env is always fresh,
			// matching the runPush/runClose/runCommit
			// pattern. Today sync doesn't mutate cs.SelectedCwd
			// so this is a no-op; we keep it so future sync
			// changes (e.g. an auto-rebase scratch dir) flow
			// through to the post-hook env without each
			// handler drifting apart.
			hc = f.deriveHookContext(ctx, cs, "sync")
			return nil
		})
	if err != nil {
		reportOutcome(ctx, command.FailedOutcome(http.StatusInternalServerError, "the branch could not be synced"))
		return &command.SlashOutput{Reply: fmt.Sprintf("❌ %v", err), Consumed: true}, nil
	}
	switch {
	case syncFacts == nil:
		reportOutcome(ctx, command.SuccessOutcome("the sync was skipped", nil))
	case !syncFacts.pulled:
		// A sync is an unconditional refresh: a correct checkout and
		// a pull with nothing to pull means the goal state holds, so
		// the sync did what it was asked to do.
		reportOutcome(ctx, outcome.Outcome{
			Success:    true,
			Status:     http.StatusOK,
			StatusText: "the branch was already up to date",
			Data:       syncFacts.data(),
		})
	default:
		reportOutcome(ctx, outcome.Outcome{
			Success:    true,
			Status:     http.StatusOK,
			StatusText: "the branch was synced",
			Data:       syncFacts.data(),
		})
	}
	return &command.SlashOutput{Reply: replyText, Consumed: true}, nil
}

// buildSyncReply lives in render.go alongside renderSyncReply —
// both shape the /gtw sync card. Kept together so the render
// surface stays cohesive.

// pushArgs bundles the parsed argv tail of `/gtw push <...>`.
// Same rationale as fixArgs: a struct is more future-proof
// than a growing positional return tuple.
//
// F-XX (commit/push split): /gtw push no longer auto-commits,
// so pushArgs is intentionally sparse — push takes no agent
// override of its own. The Agent field is kept here so a
// future `--remote` / `--force`-like flag can land without
// reshaping the parser, but it's currently unused.
type pushArgs struct {
	// Agent, when non-empty, was historically the one-shot
	// commit+push agent override. After the F-XX split /gtw
	// push no longer runs an agent — the field is parsed for
	// back-compat with users typing `-a claude` from muscle
	// memory but ignored by dispatchPush. (The legacy error
	// "no agent selected" still surfaces from /gtw commit,
	// which is where it now belongs.)
	Agent string
}

// parsePushArgs strips `-a <name>` / `--agent <name>` from
// the push argv tail. No positional arg today — /gtw push
// always operates on the current chat's worktree, like
// /gtw close.
// parsePushArgs implements the CLI lexer for /gtw push.
// Recognised flags:
//
//	-a / --agent <name>   override one-shot agent (legacy;
//	                      F-XX /gtw push no longer auto-commits
//	                      but the flag is preserved for
//	                      back-compat with muscle memory)
//
// Unknown tokens are hard-rejected with "unknown flag" —
// matches the F-XX /gtw fix contract. /gtw push takes no
// positional arg, so extra positional tokens are also
// rejected.
func parsePushArgs(argv []string) (pushArgs, error) {
	agent, err := parseAgentOnlyFlag("/gtw push", argv)
	if err != nil {
		return pushArgs{}, err
	}
	return pushArgs{Agent: agent}, nil
}

// commitArgs bundles the parsed argv tail of `/gtw commit
// <...>`. Shape mirrors pushArgs — an Agent override plus the
// three flags /review established for the same job.
type commitArgs struct {
	// Agent, when non-empty, overrides the agent that runs the
	// one-shot commit. Empty means `cs.SelectedAgent()` /
	// `yml.Commit.Agent` apply.
	Agent string

	// Continue picks the last failed /gtw commit of this chat back
	// up: the session it ran under is resumed, or the prompt it
	// was given is replayed.
	Continue bool

	// Global persists the agent of this invocation as the
	// subcommand's default (`commit.agent` in ~/.nightme/gtw.yml).
	Global bool

	// Unset removes that persisted default. It starts no commit.
	Unset bool
}

// parseAgentOnlyFlag implements the CLI lexer for
// /gtw push — a subcommand that accepts exactly one flag
// (`-a <name>` / `--agent <name>`) and no positional arg.
//
// Issue #291 moved the lexer core itself into
// command.ParseCmdArgs so the whole /<cmd> surface shares one
// implementation of the contract (docs/feat/F-gtw-fix.md §1.2 +
// §10) — this function is now just the spec declaration plus the
// "no positional args" arity, and the "unknown flag" /
// "positional" / "missing value" wording comes from the shared
// lexer.
//
// name is the user-facing subcommand ("/gtw push") echoed in
// error messages.
//
// Returns the agent string (empty if `-a` not provided).
func parseAgentOnlyFlag(name string, argv []string) (string, error) {
	parsed, err := command.ParseCmdArgs(argv, command.CmdSpec{
		Name:  name,
		Usage: name + " [-a <agent>]",
		Flags: map[string]command.FlagSpec{
			"-a":      {Name: "agent", TakesValue: true},
			"--agent": {Name: "agent", TakesValue: true},
		},
		MinArgs: 0,
		MaxArgs: 0,
	})
	if err != nil {
		return "", err
	}
	return parsed.Value("agent"), nil
}

// commitCmdSpec is the argv grammar for /gtw commit.
//
// The flags are the ones /review established for the same job:
// -a/--agent names the agent that runs the one-shot, -c/--continue
// picks the last failed run of this chat back up, -g/--global
// persists that agent as the subcommand's default, and --unset
// removes it again.
//
// The lexer (command.ParseCmdArgs) enforces the "unknown flag" /
// arity contract; the combinations the lexer cannot express are
// checked by validateGTWFlags.
var commitCmdSpec = command.CmdSpec{
	Name:  "/gtw commit",
	Usage: "/gtw commit [-a|--agent <name>] [-c|--continue] [-g|--global] [--unset]",
	Flags: map[string]command.FlagSpec{
		"-a":         {Name: "agent", TakesValue: true},
		"--agent":    {Name: "agent", TakesValue: true},
		"-c":         {Name: "continue"},
		"--continue": {Name: "continue"},
		"-g":         {Name: "global"},
		"--global":   {Name: "global"},
		"--unset":    {Name: "unset"},
	},
	MinArgs: 0,
	MaxArgs: 0,
}

// parseCommitArgs implements the CLI lexer for /gtw commit.
func parseCommitArgs(argv []string) (commitArgs, error) {
	parsed, err := command.ParseCmdArgs(argv, commitCmdSpec)
	if err != nil {
		return commitArgs{}, err
	}
	return commitArgs{
		Agent:    strings.TrimSpace(parsed.Value("agent")),
		Continue: parsed.Bool("continue"),
		Global:   parsed.Bool("global"),
		Unset:    parsed.Bool("unset"),
	}, nil
}

// validateGTWFlags rejects the flag combinations /gtw commit and
// /gtw pr cannot act on. Each combination gets its own message so
// the user knows which flag to fix.
//
// It is /review's rule set (validateReviewFlags) without the two
// flags gtw has no concept of — --abort and --timeout.
//
// --continue and --agent are not in the way: naming another agent
// takes the failed run over. A session belongs to the agent that
// ran it, so the takeover replays the stored prompt instead of
// reopening the session.
func validateGTWFlags(agentName string, global, unset, cont bool) error {
	switch {
	case cont && global:
		return errors.New("--continue and --global are mutually exclusive")
	case cont && unset:
		return errors.New("--continue and --unset are mutually exclusive")
	case unset && agentName != "":
		return errors.New("--unset and --agent are mutually exclusive")
	case unset && !global:
		return errors.New("--unset requires --global")
	case global && !unset && agentName == "":
		return errors.New("--global requires --agent <name>")
	}
	return nil
}

// applyGlobalScope performs the global-scope write of one
// /gtw commit / /gtw pr invocation: `-g` persists the agent as the
// subcommand's default, `--unset` removes it.
//
// It returns the notices for the reply and whether the command is
// done — the unset path starts no command. A failed write is a
// notice, not an error: the command itself is the primary action
// and must still run.
//
// verb is the wording of the warning a failed write produces
// ("committing with codex anyway").
func applyGlobalScope(sub, agentName string, global, unset bool, verb string) (notices []string, done bool, writeFailed bool, err error) {
	if !global && !unset {
		return nil, false, false, nil
	}
	switch {
	case unset:
		if err := unsetGlobalAgent(sub); err != nil {
			return nil, false, false, fmt.Errorf("failed to unset %s.agent: %w", sub, err)
		}
		return []string{
			fmt.Sprintf("✅ %s.agent unset (global)", sub),
			"→ file: " + userConfigRelPath,
		}, true, false, nil
	case global:
		// The name must be a registered agent — a persisted name
		// that no agent answers to would only fail later, on
		// every run of the subcommand.
		if _, err := agent.Builtins.Get(agentName); err != nil {
			return nil, false, false, unknownAgentError{name: agentName}
		}
		if err := setGlobalAgent(sub, agentName); err != nil {
			return []string{fmt.Sprintf(
				"⚠️ failed to save %s.agent: %v — %s with %s anyway",
				sub, err, verb, agentName,
			)}, false, true, nil
		}
		return []string{
			fmt.Sprintf("✅ %s.agent = %s (global)", sub, agentName),
			"→ file: " + userConfigRelPath,
		}, false, false, nil
	}
	return nil, false, false, nil
}

// runPR handles `/gtw pr`. Reads the yml at the current CWD
// to find the worktree, generates a Conventional Commits
// title + body via a one-shot agent, then asks the provider
// (GitHub or GitLab) to open the PR.
//
// The flow lives entirely in dispatchPR (pr.go); this wrapper
// is a thin mirror of runPush — parse the agent override, hand
// off, surface any dispatch error to the chat.
func (f *Factory) runPR(ctx context.Context, _ command.RuntimeServices, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {
	args, err := parsePRArgs(input.Args[2:])
	if err != nil {
		reportOutcome(ctx, command.FailedOutcome(http.StatusBadRequest, "the command line is not valid"))
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ %v", err),
			Consumed: true,
		}, nil
	}
	if err := validateGTWFlags(args.Agent, args.Global, args.Unset, args.Continue); err != nil {
		reportOutcome(ctx, command.FailedOutcome(http.StatusBadRequest, "the flag combination is not valid"))
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ %v%s", err, prCmdSpec.UsageTail()),
			Consumed: true,
		}, nil
	}

	// The global scope: `-g` persists the agent, `--unset` removes
	// it. The write happens before the command runs — the persisted
	// setting is machine-wide and does not depend on the chat's
	// workspace.
	notices, done, writeFailed, err := applyGlobalScope(subPR, args.Agent, args.Global, args.Unset, "writing the PR")
	if err != nil {
		status := http.StatusBadRequest
		summary := "the PR agent could not be persisted"
		if errors.Is(err, agent.ErrUnknownAgent) {
			status = http.StatusNotFound
			summary = "the PR agent is not a registered agent"
		}
		reportOutcome(ctx, command.FailedOutcome(status, summary))
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ %v", err),
			Consumed: true,
		}, nil
	}
	if writeFailed {
		// The PR still runs, but persisting the agent did not
		// happen — half of what was asked.
		reportOutcome(ctx, outcome.Outcome{
			Success:    false,
			Status:     http.StatusInternalServerError,
			StatusText: "the PR agent could not be persisted",
		})
	}
	if len(notices) > 0 {
		reply(ctx, cs.Emitter(), input.ChatID, input.MessageID, strings.Join(notices, "\n"))
	}
	if done {
		// --unset: the persisted default is gone and no PR runs.
		reportOutcome(ctx, outcome.Outcome{
			Success:    true,
			Status:     http.StatusOK,
			StatusText: "the global PR agent was removed",
			Data:       map[string]string{KeyScope: "global"},
		})
		return &command.SlashOutput{Consumed: true}, nil
	}

	cfg, loadNotes := Load()

	hc := f.hookContext(ctx, cs, input.ChatID, "pr", args.Continue)
	hcFn := func() HookContext { return hc }
	err = f.withHooks(ctx, cs, input.ChatID, input.MessageID,
		loadNotes, hcFn, cfg.PR.Hooks.Before, cfg.PR.Hooks.After,
		func() error {
			_, e := dispatchPR(ctx, cs, f.deps, input.ChatID, input.MessageID, args, cfg)
			// dispatchPR loads the dispatch context internally
			// (worktree/branch from yml). Re-derive so any
			// future change that mutates cwd during PR
			// dispatch flows to the post-hook env. A continuation
			// works in the recorded workspace, so its after-hook
			// keeps that one.
			if e == nil && !args.Continue {
				hc = f.deriveHookContext(ctx, cs, "pr")
			}
			return e
		})
	if err != nil {
		reportOutcome(ctx, command.FailedOutcome(http.StatusInternalServerError, "the pull request could not be opened"))
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ /gtw pr failed: %v", err),
			Consumed: true,
		}, nil
	}
	return &command.SlashOutput{Consumed: true}, nil
}

// isPRView reports whether the argv names `/gtw pr view`.
//
// The subcommand is the token right after "pr", matched the way the
// shared lexer matches it. Anything else — including a bare
// `/gtw pr` — is the path that opens a pull request, and that path
// is untouched by the read-only view.
func isPRView(args []string) bool {
	return len(args) > 2 && strings.EqualFold(args[2], prViewSubcommand)
}

// runPRView handles `/gtw pr view [number]`. The command reads the
// merge state of a pull request and reports it; it opens nothing,
// changes no label and runs no hook.
func (f *Factory) runPRView(ctx context.Context, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {
	number, err := parsePRViewArgs(input.Args[2:])
	if err != nil {
		reportOutcome(ctx, command.FailedOutcome(http.StatusBadRequest, "the command line is not valid"))
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ %v", err),
			Consumed: true,
		}, nil
	}
	_, _ = dispatchPRView(ctx, cs, f.deps, input.ChatID, input.MessageID, number)
	return &command.SlashOutput{Consumed: true}, nil
}

// runConfig handles `/gtw config [-g|--global] [--unset] lang
// [value]` — the git-style reader / writer for the gtw output
// language. It only touches ~/.nightme/gtw.yml: no worktree, no
// chat cwd, no agent. The reply goes out via cs.Emitter() like
// the other gtw dispatchers.
func (f *Factory) runConfig(ctx context.Context, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {
	_, _ = RunConfig(ctx, cs, f.deps, input.ChatID, input.MessageID, input.Args[2:])
	return &command.SlashOutput{Consumed: true}, nil
}

// prCmdSpec is the argv grammar for /gtw pr.
//
// /gtw pr gets its own spec because it takes two flags beyond the
// ones /gtw commit has: `--ready` moves the linked issue from
// "claimed" to "waiting for review" once the pull request exists, and
// labels the pull request itself `nightme/ready`.
//
// The flag is boolean — it takes no value — so `--ready` alone is the
// whole argument and `/gtw pr --ready -a codex` reads both.
var prCmdSpec = command.CmdSpec{
	Name:  "/gtw pr",
	Usage: "/gtw pr [-a|--agent <name>] [-c|--continue] [-g|--global] [--unset] [--ready]",
	Flags: map[string]command.FlagSpec{
		"-a":         {Name: "agent", TakesValue: true},
		"--agent":    {Name: "agent", TakesValue: true},
		"-c":         {Name: "continue"},
		"--continue": {Name: "continue"},
		"-g":         {Name: "global"},
		"--global":   {Name: "global"},
		"--unset":    {Name: "unset"},
		"--ready":    {Name: "ready"},
	},
	MinArgs: 0,
	MaxArgs: 0,
}

// parsePRArgs implements the CLI lexer for /gtw pr.
// Recognised flags:
//
//	-a / --agent <name>   override one-shot agent
//	-c / --continue       continue the last failed /gtw pr of this chat
//	-g / --global         persist the agent as `pr.agent`
//	--unset               remove the persisted `pr.agent`
//	--ready               move the linked issue to `nightme/ready`
//	                      and label the pull request `nightme/ready`
//
// Unknown tokens are hard-rejected with "unknown flag";
// positional args are rejected too (/gtw pr takes none).
// See parseFixArgs / parsePushArgs for the rationale.
func parsePRArgs(argv []string) (prArgs, error) {
	parsed, err := command.ParseCmdArgs(argv, prCmdSpec)
	if err != nil {
		return prArgs{}, err
	}
	return prArgs{
		Agent:    strings.TrimSpace(parsed.Value("agent")),
		Continue: parsed.Bool("continue"),
		Global:   parsed.Bool("global"),
		Unset:    parsed.Bool("unset"),
		Ready:    parsed.Bool("ready"),
	}, nil
}
