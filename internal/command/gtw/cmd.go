package gtw

import (
	"context"
	"fmt"
	"strings"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/gateway/outbound"
)

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
//	mgr.SetHandlerDeps(deps)
//	factory := gtw.NewFactory(mgr)
//	reg := command.NewRegistry()
//	reg.Register(factory)
type Factory struct {
	mgr  *Manager
	deps HandlerDeps
}

// NewFactory constructs a Factory backed by mgr. SetHandlerDeps
// on the Manager separately (or pass deps to NewFactoryWithDeps
// if you prefer — both work).
func NewFactory(mgr *Manager) *Factory {
	return &Factory{mgr: mgr}
}

// NewFactoryWithDeps constructs a Factory and primes it with
// the runtime's HandlerDeps.
func NewFactoryWithDeps(mgr *Manager, deps HandlerDeps) *Factory {
	mgr.SetHandlerDeps(deps)
	return &Factory{mgr: mgr, deps: deps}
}

// init self-registers the gtw command. Phase 2.3: each
// command package's init() calls RegisterBuilder; the runtime
// orchestrator calls SetDeps once at startup to finalize
// every registered builder. GTWExt carries gtw.HandlerDeps
// (typed as `any` in command.Deps to avoid command ↔ gtw
// import cycle).
func init() {
	command.RegisterBuilder(func(d command.Deps) command.SlashCommandFactory {
		handlerDeps, _ := d.GTWExt.(HandlerDeps)
		mgr := NewManager()
		mgr.SetHandlerDeps(handlerDeps)
		// v1.3+ multi-channel: prefer ChatSessionLookup (set by
		// the runtime to runtime.findChatSession) so /gtw replies
		// land on the originating channel. Fall back to the
		// single-mgr path for legacy deployments that don't
		// wire the lookup — d.Manager is the primary (first)
		// per-channel mgr in multi-channel mode, which would
		// otherwise route every chat through the first channel's
		// Emitter.
		lookup := d.ChatSessionLookup
		if lookup == nil {
			lookup = func(chatID string) *chatsession.ChatSession {
				cs, _ := d.Manager.GetOrCreate(chatID, d.Primary)
				return cs
			}
		}
		mgr.SetGetChatSession(lookup)
		return NewFactoryWithDeps(mgr, handlerDeps)
	})
}

// SetHandlerDeps primes the factory with runtime deps. Also
// pushes the same deps into the Manager so reaction handlers
// see them.
func (f *Factory) SetHandlerDeps(deps HandlerDeps) {
	f.deps = deps
	if f.mgr != nil {
		f.mgr.SetHandlerDeps(deps)
	}
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
			em := outbound.Emitter(nil)
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

	var em outbound.Emitter
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
func (f *Factory) Spec() command.Spec {
	return command.Spec{
		Name:    "gtw",
		Aliases: []string{"team"},
		Summary: "GTW: Git-driven team workflow (claim, label, worktree).",
		Usage: "/gtw fix <issue-id>              claim, label, and create a worktree\n" +
			"/gtw fix --name <branch>         create a local worktree (no issue)\n" +
			"/gtw fix -n <branch>             short form of --name\n" +
			"/gtw fix <id> --force            nuke any leftover at the target path, then re-create\n" +
			"/gtw close                       tear down the worktree, delete the branch, and sync main\n" +
			"/gtw commit [-a <agent>]         commit uncommitted work via the configured agent (no push)\n" +
			"/gtw push                        push the worktree branch (clean only — refuses dirty)\n" +
			"/gtw pr                          generate PR title+body, then open the PR\n" +
			"/gtw pr -a claude                override which agent runs the one-shot\n" +
			"/gtw sync                        checkout the default branch and pull --rebase from origin",
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
		return &command.SlashOutput{
			Reply:    f.Spec().Usage,
			Consumed: true,
		}, nil
	}
	switch input.Args[1] {
	case "fix":
		return f.runFix(ctx, rt, cs, input)
	case "close":
		return f.runClose(ctx, rt, cs, input)
	case "commit":
		return f.runCommit(ctx, rt, cs, input)
	case "push":
		return f.runPush(ctx, rt, cs, input)
	case "pr":
		return f.runPR(ctx, rt, cs, input)
	case "sync":
		return f.runSync(ctx, rt, cs, input)
	}
	return &command.SlashOutput{
		Reply:    "Unknown subcommand: " + input.Args[1] + "\n" + f.Spec().Usage,
		Consumed: true,
	}, nil
}

// runFix handles `/gtw fix <issue-id>` and `/gtw fix --name <branch>`.
// F-XX splits the entry into two modes at the factory boundary
// so RunFix sees a single Mode constant.
//
// F-51: command.Commander prefixes Args with the command name
// ("gtw"), then the subcommand ("fix"), then the subcommand's
// args. So Args[2] is the first user-supplied token.
func (f *Factory) runFix(ctx context.Context, _ command.RuntimeServices, _ *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {
	if len(input.Args) < 3 {
		return &command.SlashOutput{
			Reply:    "Usage: /gtw fix <issue-id>  |  /gtw fix --name <branch>",
			Consumed: true,
		}, nil
	}

	args, err := parseFixArgs(input.Args[2:])
	if err != nil {
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
	if args.Mode == ModeRemote {
		if _, err := parseIssueID(args.RawArg); err != nil {
			return &command.SlashOutput{
				Reply:    fmt.Sprintf("Invalid issue id: %q (%v)", args.RawArg, err),
				Consumed: true,
			}, nil
		}
	}

	// Resolve per-chat ChatSession. The runtime wires a lookup
	// at startup that lazy-creates a *chatsession.ChatSession on
	// first GetChatSession miss.
	cs := f.mgr.GetChatSession(input.ChatID)
	if cs == nil || cs.SelectedCwd() == "" {
		return &command.SlashOutput{
			Reply:    command.NoActiveCwdReply,
			Consumed: true,
		}, nil
	}

	// Load user-level hook config (silent if missing). The
	// before/after lists wrap the actual RunFix call below; any
	// load-time warnings ride along in the consolidated reply.
	cfg, loadNotes := Load()

	// Build slot / drafts shims that route to the Manager.
	slot := &managerContextSlot{mgr: f.mgr, chatID: input.ChatID}
	drafts := &managerDraftsMap{mgr: f.mgr, chatID: input.ChatID}

	// RunFix signature: (ctx, mode, cs, slot, drafts, deps,
	// chatID, messageID, args, force). Reply is sent inline via
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
			_, e := RunFix(ctx, args.Mode, cs, slot, drafts, f.deps,
				input.ChatID, input.MessageID,
				[]string{args.RawArg}, args.Force)
			if e == nil {
				// Re-derive so post-hook sees the worktree
				// that RunFix just created + yml-wrote.
				hc = f.deriveHookContext(ctx, cs, "fix")
			}
			return e
		})
	if err != nil {
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ /gtw fix failed: %v", err),
			Consumed: true,
		}, nil
	}
	return &command.SlashOutput{Consumed: true}, nil
}

// parseFixMode inspects the argv tail after "fix" and decides
// whether the call is the issue-id (ModeRemote) or
// --name/-n (ModeLocal) flow. Returns the mode plus the raw
// value (issue id or branch name).
//
// Format:
//
//	[issue-id]                    → ModeRemote, raw = issue-id
//	--name <branch>               → ModeLocal,  raw = branch
//	-n <branch>                   → ModeLocal,  raw = branch
//
// Errors on missing value after the flag, or on
// unrecognised leading tokens.
func parseFixMode(argv []string) (Mode, string, error) {
	if len(argv) < 1 {
		return "", "", fmt.Errorf("missing argument")
	}
	switch argv[0] {
	case "--name", "-n":
		if len(argv) < 2 || strings.TrimSpace(argv[1]) == "" {
			return "", "", fmt.Errorf("--name requires a branch name argument")
		}
		return ModeLocal, strings.TrimSpace(argv[1]), nil
	default:
		// Bare argument → treat as issue id. Validation lives
		// in parseIssueID at the caller.
		return ModeRemote, strings.TrimSpace(argv[0]), nil
	}
}

// fixArgs bundles the parsed argv tail of `/gtw fix <...>`.
// Splitting it into a struct (rather than separate return
// values) keeps the parser functions readable as we add more
// flags — `--force` / `-f` is the first, future flags
// (`--no-dispatch`, `--base <ref>`) can land here without
// breaking signatures again.
type fixArgs struct {
	Mode   Mode
	RawArg string // issue id (ModeRemote) or branch name (ModeLocal)
	Force  bool   // --force / -f: skip path-occupied preflight + nuke any leftover at the target path
}

// parseFixArgs is the argv tail → fixArgs entry point. It
// strips --force / -f tokens from anywhere in the tail (so
// both `/gtw fix --force 42` and `/gtw fix 42 --force`
// parse), then dispatches the remaining tokens to parseFixMode.
//
// --force is intentionally permissive — it doesn't take an
// argument and silently accepts duplicates. The semantic is
// "any --force wins"; this matches git's own CLI conventions
// for boolean flags.
func parseFixArgs(argv []string) (fixArgs, error) {
	force := false
	filtered := make([]string, 0, len(argv))
	for _, a := range argv {
		if a == "--force" || a == "-f" {
			force = true
			continue
		}
		filtered = append(filtered, a)
	}
	if len(filtered) == 0 {
		return fixArgs{}, fmt.Errorf("missing argument")
	}
	mode, rawArg, err := parseFixMode(filtered)
	if err != nil {
		return fixArgs{}, err
	}
	return fixArgs{Mode: mode, RawArg: rawArg, Force: force}, nil
}

// runClose handles `/gtw close`. Tears down the worktree
// created by `/gtw fix`, deletes the local branch, switches CWD
// back to the main repo, clears gtw state, and then runs the
// upstream sync (same card /gtw sync emits). Two cards land in
// the chat: one for the local teardown, one for the sync.
// See wip/gtw.md §14.5 for the full flow and RunClose for the
// implementation.
//
// No flags — close is intentionally all-or-nothing. If the
// worktree is dirty the user must commit / stash / discard
// before re-running; we don't expose a force-escape hatch.
// /gtw fix keeps its own --force (different concern: nuking a
// leftover worktree at the target path) so the close symmetry
// doesn't extend there.
//
// Construction mirrors runFix: the slot / drafts shims route to
// the per-chat Manager state, deps are forwarded verbatim, and
// the reply path is RunClose's own cs.Emitter() (no extra wiring).
// Wrapped in withHooks so close.before / close.after fire.
func (f *Factory) runClose(ctx context.Context, _ command.RuntimeServices, _ *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {
	cs := f.mgr.GetChatSession(input.ChatID)
	if cs == nil {
		return &command.SlashOutput{
			Reply:    "No active chat session.",
			Consumed: true,
		}, nil
	}
	slot := &managerContextSlot{mgr: f.mgr, chatID: input.ChatID}

	cfg, loadNotes := Load()

	hc := f.deriveHookContext(ctx, cs, "close")
	hcFn := func() HookContext { return hc }
	err := f.withHooks(ctx, cs, input.ChatID, input.MessageID,
		loadNotes, hcFn, cfg.Close.Hooks.Before, cfg.Close.Hooks.After,
		func() error {
			res, e := RunClose(ctx, cs, slot, f.deps, input.ChatID, input.MessageID)
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
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ /gtw close failed: %v", err),
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
// No slot/draft shim needed — push doesn't touch gtw state
// or reaction cards. Just the same HandlerDeps as everywhere
// else. Wrapped in withHooks so push.before / push.after from
// ~/.nightme/gtw.yml fire.
func (f *Factory) runPush(ctx context.Context, _ command.RuntimeServices, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {
	args, err := parsePushArgs(input.Args[2:])
	if err != nil {
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
// slot/draft shim is needed.
func (f *Factory) runCommit(ctx context.Context, _ command.RuntimeServices, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {
	args, err := parseCommitArgs(input.Args[2:])
	if err != nil {
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ %v", err),
			Consumed: true,
		}, nil
	}

	cfg, loadNotes := Load()

	hc := f.deriveHookContext(ctx, cs, "commit")
	hcFn := func() HookContext { return hc }
	err = f.withHooks(ctx, cs, input.ChatID, input.MessageID,
		loadNotes, hcFn, cfg.Commit.Hooks.Before, cfg.Commit.Hooks.After,
		func() error {
			res, e := dispatchCommit(ctx, cs, f.deps, input.ChatID, input.MessageID, args, cfg.Commit.Agent)
			_ = res // dispatchCommit already sent the reply via cs.Emitter()
			if e == nil {
				hc = f.deriveHookContext(ctx, cs, "commit")
			}
			return e
		})
	if err != nil {
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ /gtw commit failed: %v", err),
			Consumed: true,
		}, nil
	}
	return &command.SlashOutput{Consumed: true}, nil
}

// runSync handles `/gtw sync`: checkout the default branch and
// pull --rebase from origin. The reply is an IM-friendly
// compact summary (✅ branch @ sha + commit list, or ✨
// already up to date) — not git's raw pull stdout. Errors are
// surfaced verbatim (RefreshDefaultBranch already includes the
// dirty-worktree refusal and rebase-conflict guidance the
// user needs).
//
// Wrapped in withHooks so sync.before / sync.after fire.
func (f *Factory) runSync(ctx context.Context, _ command.RuntimeServices, _ *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {
	cs := f.mgr.GetChatSession(input.ChatID)
	cwd, failOut := command.RequireActiveCwd(cs)
	if failOut != nil {
		return failOut, nil
	}

	cfg, loadNotes := Load()

	hc := f.deriveHookContext(ctx, cs, "sync")
	hcFn := func() HookContext { return hc }

	var replyText string
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
			body, e := buildSyncReply(ctx, repoRoot, f.deps)
			if e != nil {
				return e
			}
			replyText = body
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
		return &command.SlashOutput{Reply: fmt.Sprintf("❌ %v", err), Consumed: true}, nil
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
func parsePushArgs(argv []string) (pushArgs, error) {
	out := pushArgs{}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch a {
		case "-a", "--agent":
			if i+1 >= len(argv) {
				return out, fmt.Errorf("missing value for %s", a)
			}
			out.Agent = argv[i+1]
			i++
		default:
			// Unknown token. Silent accept — future flag
			// additions (e.g. positional branch arg) won't
			// break callers passing them by mistake.
		}
	}
	return out, nil
}

// commitArgs bundles the parsed argv tail of `/gtw commit
// <...>`. Shape mirrors pushArgs — a single Agent override
// from `-a <name>` / `--agent <name>`. Empty means
// `cs.SelectedAgent()` / `yml.Commit.Agent` apply.
type commitArgs struct {
	Agent string
}

// parseCommitArgs strips `-a <name>` / `--agent <name>` from
// the commit argv tail. Same shape as parsePushArgs — v1 has
// no positional arg; /gtw commit always operates on the
// current chat's worktree. Unknown flags tolerated (future
// flags like `--amend` can land without breaking callers).
func parseCommitArgs(argv []string) (commitArgs, error) {
	out := commitArgs{}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch a {
		case "-a", "--agent":
			if i+1 >= len(argv) {
				return out, fmt.Errorf("missing value for %s", a)
			}
			out.Agent = argv[i+1]
			i++
		default:
			// Unknown token. Silent accept.
		}
	}
	return out, nil
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
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ %v", err),
			Consumed: true,
		}, nil
	}

	cfg, loadNotes := Load()

	hc := f.deriveHookContext(ctx, cs, "pr")
	hcFn := func() HookContext { return hc }
	err = f.withHooks(ctx, cs, input.ChatID, input.MessageID,
		loadNotes, hcFn, cfg.PR.Hooks.Before, cfg.PR.Hooks.After,
		func() error {
			_, e := dispatchPR(ctx, cs, f.deps, input.ChatID, input.MessageID, args)
			// dispatchPR loads the dispatch context internally
			// (worktree/branch from yml). Re-derive so any
			// future change that mutates cwd during PR
			// dispatch flows to the post-hook env.
			if e == nil {
				hc = f.deriveHookContext(ctx, cs, "pr")
			}
			return e
		})
	if err != nil {
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ /gtw pr failed: %v", err),
			Consumed: true,
		}, nil
	}
	return &command.SlashOutput{Consumed: true}, nil
}

// parsePRArgs strips `-a <name>` / `--agent <name>` from the
// pr argv tail. Mirrors parsePushArgs — v1 has no positional
// arg; /gtw pr always operates on the current chat's worktree,
// like /gtw push. Unknown flags are tolerated (future flags
// like --draft / --base can land without breaking callers).
func parsePRArgs(argv []string) (prArgs, error) {
	out := prArgs{}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch a {
		case "-a", "--agent":
			if i+1 >= len(argv) {
				return out, fmt.Errorf("missing value for %s", a)
			}
			out.Agent = argv[i+1]
			i++
		default:
			// Unknown token. Silent accept.
		}
	}
	return out, nil
}

// --- shim adapters that let legacy RunFix see Manager state ---

// managerContextSlot adapts Manager to the legacy ContextSlot
// interface (Load / Store) used by RunFix / HandleAction. The
// Manager owns the per-chat context; this shim is the only
// bridge between the legacy code and the new state.
type managerContextSlot struct {
	mgr    *Manager
	chatID string
}

func (s *managerContextSlot) Load() Context { return s.mgr.GetContext(s.chatID) }
func (s *managerContextSlot) Store(c Context) {
	if (c == Context{}) {
		s.mgr.ClearContext(s.chatID)
		return
	}
	s.mgr.SetContext(s.chatID, c)
}

// managerDraftsMap adapts Manager to the DraftsMap
// interface (Store / Take / Lookup / Count) used by RunFix /
// HandleAction. Drafts are keyed by (chatID, requestID) on the
// Manager; the shim pins chatID.
type managerDraftsMap struct {
	mgr    *Manager
	chatID string
}

func (d *managerDraftsMap) Store(requestID string, draft *Draft) {
	d.mgr.StoreDraft(d.chatID, requestID, draft)
}
func (d *managerDraftsMap) Take(requestID string) *Draft {
	return d.mgr.TakeDraft(d.chatID, requestID)
}
func (d *managerDraftsMap) Lookup(requestID string) *Draft {
	return d.mgr.GetDraft(d.chatID, requestID)
}
func (d *managerDraftsMap) Count() int { return d.mgr.DraftCount(d.chatID) }
