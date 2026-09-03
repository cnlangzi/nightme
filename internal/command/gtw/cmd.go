package gtw

import (
	"context"
	"fmt"
	"strings"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
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
	return &Factory{mgr: mgr, deps: deps}
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
func (f *Factory) SetHandlerDeps(deps HandlerDeps) {
	f.deps = deps
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
func (f *Factory) Spec() command.Spec {
	return command.Spec{
		Name:    "gtw",
		Aliases: []string{"team"},
		Summary: "GTW: Git-driven team workflow (claim, label, worktree).",
		Usage: "/gtw fix <issue-id>              claim, label, and create a worktree (plan-first)\n" +
			"/gtw fix <issue-id> -y           direct execute (skip plan-first)\n" +
			"/gtw fix <issue-id> --yes        long form of -y\n" +
			"/gtw fix --name <branch>         create a local worktree (no issue)\n" +
			"/gtw fix -n <branch>             short form of --name\n" +
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
	if mu := f.mgr.runLockFor(input.ChatID); mu != nil {
		mu.Lock()
		defer mu.Unlock()
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
// F-XX adds `-y` / `--yes` to bypass plan-first dispatch and
// go straight to the Execute prompt. F-XX also removes the
// previous `--force` / `-f` flag.
//
// F-51: command.Commander prefixes Args with the command name
// ("gtw"), then the subcommand ("fix"), then the subcommand's
// args. So Args[2] is the first user-supplied token.
func (f *Factory) runFix(ctx context.Context, _ command.RuntimeServices, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {
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

	// cs is supplied by the dispatcher — the same ChatSession
	// that /cwd, /use, /close and other slash commands see in
	// the same chat. No second lookup, no cache that could go
	// stale.
	_, failOut := command.RequireActiveCwd(cs)
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
				[]string{args.RawArg}, args.Yes)
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

// parseFixMode kept for callers that pre-stripped boolean
// fixArgs bundles the parsed argv tail of `/gtw fix <...>`.
// Splitting it into a struct (rather than separate return
// values) keeps the parser functions readable as we add more
// flags — `--yes` / `-y` is the first, future flags
// (`--no-dispatch`, `--base <ref>`) can land here without
// breaking signatures again.
type fixArgs struct {
	Mode   Mode
	RawArg string // issue id (ModeRemote) or branch name (ModeLocal)
	Yes    bool   // --yes / -y: dispatch Execute Prompt instead of Plan
}

// Recognised flags for /gtw fix. The schema is intentionally
// narrow — only flags that exist in F-XX today. Anything else
// is an "unknown flag" error.
//
// Boolean flags (no value, position-independent):
//
//	--yes / -y     dispatch Execute Prompt (Remote mode only)
//
// Value-taking flags (one positional argument follows):
//
//	--name / -n    local mode branch name
//
// Explicitly rejected:
//
//	--force / -f   removed in F-XX
//
// parseFixArgs implements the standard CLI lexer for /gtw fix:
//
//	cmd -options args
//
// Tokens are split into two disjoint slices:
//
//   - options: every "-xxx" or "--xxx" flag (boolean or
//     value-taking) recognised by the inline switch below. Unknown
//     flags → error.
//   - args: positional tokens (no "-" prefix). One per
//     value-taking option (consumed as the option's value)
//     or additional free-standing tokens.
//
// Lexical rules (matching git / kubectl / docker conventions):
//
//  1. Every token starting with "-" is an option. Unknown
//     option → "unknown flag" error.
//  2. Boolean options take no value; the next token is
//     either another option or a positional arg.
//  3. Value-taking options consume the immediately following
//     token as their value, regardless of whether it starts
//     with "-" (matching git's `--name -foo` behaviour).
//     If the flag has no following token, it's a
//     missing-value error.
//  4. Positional tokens after options are collected as args.
//
// `/gtw fix` accepts exactly two positional patterns:
//
//	<issue-id>              Remote mode (1 arg)
//	--name <branch>          Local mode (option + 1 arg)
//	-n <branch>              Local mode (short form)
//
// All other shapes (zero args, too many args, mixed mode
// markers, unknown options) are hard-rejected.
func parseFixArgs(argv []string) (fixArgs, error) {
	// Recognised flags. Inline because /gtw fix has exactly
	// four flags today; a map + struct + enum was premature
	// parameterisation for that surface area.
	const (
		boolYes     = "--yes"
		shortYes    = "-y"
		boolName    = "--name"
		shortName   = "-n"
		removedForce = "--force"
		removedForceShort = "-f"
	)

	yes, usedName := false, false
	args := make([]string, 0, len(argv))

	i := 0
	for i < len(argv) {
		tok := argv[i]

		// First classify: is this a flag-shaped token (starts
		// with "-" or "--") or a positional argument? Tokens
		// like "42" are positional, never flags — even if
		// they happen to look like short forms.
		isFlag := strings.HasPrefix(tok, "-") && tok != "-"
		if !isFlag {
			// Positional argument — collect as-is.
			args = append(args, tok)
			i++
			continue
		}

		// Look up the flag.
		switch tok {
		case boolYes, shortYes:
			yes = true
			i++
		case boolName, shortName:
			usedName = true
			i++
			if i >= len(argv) {
				return fixArgs{}, fmt.Errorf(
					"flag %q requires a value", tok)
			}
			// The next token is the flag's value (consumed),
			// even if it starts with "-" — matching git's
			// `--name -foo` semantics. We do NOT recursively
			// re-classify it as a flag.
			args = append(args, argv[i])
			i++
		case removedForce, removedForceShort:
			// F-XX: --force / -f are explicitly rejected
			// rather than silently dropped. Without this
			// gate, "/gtw fix 42 --force" would parse as a
			// legitimate ModeRemote fix with --force treated
			// as a no-op, leaving users who relied on the
			// old flag in an inconsistent state.
			return fixArgs{}, fmt.Errorf(
				"unknown flag %q (the /gtw fix --force / -f flag was removed in F-XX; "+
					"see docs/feat/F-gtw-fix.md). For a stale worktree path, "+
					"run `git worktree remove --force <path>` or `/gtw close` manually",
				tok)
		default:
			// Unknown flag-shaped token. Could be a real typo
			// (--dry-run, --foo, etc.); surface a generic
			// "unknown flag" listing the recognised set.
			return fixArgs{}, fmt.Errorf(
				"unknown flag %q (recognised flags: --yes/-y; mode: --name/-n <branch>; "+
					"see docs/feat/F-gtw-fix.md)",
				tok)
		}
	}

	// Decide Mode + RawArg from the arg list. Exactly two
	// valid shapes (see doc comment above).
	switch {
	case !usedName && len(args) == 1:
		// Remote mode: bare issue id.
		return fixArgs{
			Mode:   ModeRemote,
			RawArg: strings.TrimSpace(args[0]),
			Yes:    yes,
		}, nil
	case usedName && len(args) == 1:
		// Local mode: --name/-n consumed its branch.
		return fixArgs{
			Mode:   ModeLocal,
			RawArg: strings.TrimSpace(args[0]),
			Yes:    yes,
		}, nil
	case !usedName && len(args) == 0:
		return fixArgs{}, fmt.Errorf(
			"missing argument (need <issue-id> or --name <branch>)")
	default:
		// Everything else: too many args, or mixed-mode
		// marker with multiple bare positional args, etc.
		return fixArgs{}, fmt.Errorf(
			"expected exactly one argument (<issue-id> or --name/-n <branch>); got %d: %v",
			len(args), args)
	}
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
// Neither /gtw close nor /gtw fix exposes a --force: stale
// worktree paths are cleaned manually via
// `git worktree remove --force <path>` or by re-running
// `/gtw close` after the user has unblocked the worktree.
//
// No shim is needed; RunClose reads its state from the
// cwd-scoped yml directly. Deps are forwarded verbatim; the
// reply path is RunClose's own cs.Emitter() (no extra wiring).
// Wrapped in withHooks so close.before / close.after fire.
func (f *Factory) runClose(ctx context.Context, _ command.RuntimeServices, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {
	// Issue #291: /gtw close takes no flags and no positional
	// args, so anything in the tail is a user mistake — most
	// importantly `/gtw close --force`, which the F-XX removal
	// note explicitly tells users not to expect. Before this
	// gate the token was silently swallowed and the close ran
	// anyway, so the user got no signal that --force did nothing.
	if _, err := command.ParseCmdArgs(input.Args[2:], command.CmdSpec{
		Name:    "/gtw close",
		Usage:   "/gtw close",
		MinArgs: 0,
		MaxArgs: 0,
	}); err != nil {
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ %v", err),
			Consumed: true,
		}, nil
	}

	cfg, loadNotes := Load()

	hc := f.deriveHookContext(ctx, cs, "close")
	hcFn := func() HookContext { return hc }
	err := f.withHooks(ctx, cs, input.ChatID, input.MessageID,
		loadNotes, hcFn, cfg.Close.Hooks.Before, cfg.Close.Hooks.After,
		func() error {
			res, e := RunClose(ctx, cs, f.deps, input.ChatID, input.MessageID)
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
// No draft shim needed — push doesn't touch reaction cards.
// Just the same HandlerDeps as everywhere else. Wrapped in
// withHooks so push.before / push.after from ~/.nightme/gtw.yml
// fire.
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
// draft shim is needed.
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
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ %v", err),
			Consumed: true,
		}, nil
	}

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
// <...>`. Shape mirrors pushArgs — a single Agent override
// from `-a <name>` / `--agent <name>`. Empty means
// `cs.SelectedAgent()` / `yml.Commit.Agent` apply.
type commitArgs struct {
	Agent string
}

// parseAgentOnlyFlag implements the CLI lexer for
// /gtw commit / /gtw push / /gtw pr — three subcommands that
// accept exactly one flag (`-a <name>` / `--agent <name>`)
// and no positional arg.
//
// Extracted as a helper because all three parsers were
// byte-identical except for the surrounding struct type
// (pushArgs / commitArgs / prArgs). Issue #291 then moved the
// lexer core itself into command.ParseCmdArgs so the whole
// /<cmd> surface shares one implementation of the contract
// (docs/feat/F-gtw-fix.md §1.2 + §10) — this function is now
// just the spec declaration plus the "no positional args"
// arity, and the "unknown flag" / "positional" / "missing
// value" wording comes from the shared lexer.
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

// parseCommitArgs implements the CLI lexer for /gtw commit.
// See parseAgentOnlyFlag for the shared lexer core; this
// wrapper just packs the agent string into the commitArgs
// struct.
func parseCommitArgs(argv []string) (commitArgs, error) {
	agent, err := parseAgentOnlyFlag("/gtw commit", argv)
	if err != nil {
		return commitArgs{}, err
	}
	return commitArgs{Agent: agent}, nil
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
			_, e := dispatchPR(ctx, cs, f.deps, input.ChatID, input.MessageID, args, cfg.PR.Agent)
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

// parsePRArgs implements the CLI lexer for /gtw pr.
// Recognised flags:
//
//	-a / --agent <name>   override one-shot agent
//
// Unknown tokens are hard-rejected with "unknown flag";
// positional args are rejected too (/gtw pr takes none).
// See parseFixArgs / parsePushArgs for the rationale.
func parsePRArgs(argv []string) (prArgs, error) {
	agent, err := parseAgentOnlyFlag("/gtw pr", argv)
	if err != nil {
		return prArgs{}, err
	}
	return prArgs{Agent: agent}, nil
}
