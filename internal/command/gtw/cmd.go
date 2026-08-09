package gtw

import (
	"context"
	"fmt"
	"strings"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
)

// Factory implements command.SlashCommandFactory for /gtw.
//
// It holds the per-process *Manager (gtw state) and the
// runtime's HandlerDeps (Send / SendCard / Git / Prober /
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

// SetHandlerDeps primes the factory with runtime deps. Also
// pushes the same deps into the Manager so reaction handlers
// see them.
func (f *Factory) SetHandlerDeps(deps HandlerDeps) {
	f.deps = deps
	if f.mgr != nil {
		f.mgr.SetHandlerDeps(deps)
	}
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
			"/gtw close --force               force-close even when the worktree is dirty\n" +
			"/gtw push                       commit + push the worktree's branch to origin\n" +
			"/gtw push --pr                   also open a PR (gh/glab) against the default branch\n" +
			"/gtw push --no-commit            refuse if there are uncommitted changes\n" +
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
func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {
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
	case "push":
		return f.runPush(ctx, rt, cs, input)
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
	// the SlashOutput path simple when no channel is wired in tests.
	if args.Mode == ModeLocal {
		if _, err := DeriveBranchFromName(args.RawArg); err != nil {
			return &command.SlashOutput{
				Reply:    fmt.Sprintf("❌ %v", err),
				Consumed: true,
			}, nil
		}
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
			Reply:    "No active workspace. Send /cwd <path> first.",
			Consumed: true,
		}, nil
	}

	// Build slot / drafts shims that route to the Manager.
	slot := &managerContextSlot{mgr: f.mgr, chatID: input.ChatID}
	drafts := &managerDraftsMap{mgr: f.mgr, chatID: input.ChatID}

	// RunFix signature: (ctx, mode, cs, slot, drafts, deps,
	// chatID, messageID, args, force). Reply is sent inline via
	// cs.Channel(); *Result only carries Consumed / Dropped for
	// the runtime.
	_, err = RunFix(ctx, args.Mode, cs, slot, drafts, f.deps, input.ChatID, input.MessageID, []string{args.RawArg}, args.Force)
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
// --force / -f skips the dirty-worktree refusal and force-
// removes via `git worktree remove --force`. Use this when
// the user genuinely wants to throw away their in-progress
// changes (e.g. they're abandoning the branch). --force does
// NOT bypass the sync step's own dirty-main check on the
// primary repo.
//
// Construction mirrors runFix: the slot / drafts shims route to
// the per-chat Manager state, deps are forwarded verbatim, and
// the reply path is RunClose's own cs.Channel() (no extra wiring).
func (f *Factory) runClose(ctx context.Context, _ command.RuntimeServices, _ *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {
	cs := f.mgr.GetChatSession(input.ChatID)
	if cs == nil {
		return &command.SlashOutput{
			Reply:    "No active chat session.",
			Consumed: true,
		}, nil
	}
	slot := &managerContextSlot{mgr: f.mgr, chatID: input.ChatID}

	force, err := parseCloseForce(input.Args[2:])
	if err != nil {
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ %v", err),
			Consumed: true,
		}, nil
	}

	res, err := RunClose(ctx, cs, slot, f.deps, input.ChatID, input.MessageID, force)
	if err != nil {
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ /gtw close failed: %v", err),
			Consumed: true,
		}, nil
	}
	_ = res // RunClose already sent the reply via cs.Channel()
	return &command.SlashOutput{Consumed: true}, nil
}

// runPush handles `/gtw push`. Reads the yml at the current
// CWD to find the worktree, commits uncommitted changes
// (unless --no-commit), pushes the branch to origin, and
// optionally opens a PR (--pr).
//
// No slot/draft shim needed — push doesn't touch gtw state
// or reaction cards. Just the same HandlerDeps as everywhere
// else, plus the parsed push flags.
func (f *Factory) runPush(ctx context.Context, _ command.RuntimeServices, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {
	args, err := parsePushArgs(input.Args[2:])
	if err != nil {
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ %v", err),
			Consumed: true,
		}, nil
	}

	res, err := RunPush(ctx, cs, f.deps, input.ChatID, input.MessageID, args)
	if err != nil {
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ /gtw push failed: %v", err),
			Consumed: true,
		}, nil
	}
	_ = res // RunPush already sent the reply via cs.Channel()
	return &command.SlashOutput{Consumed: true}, nil
}

// runSync handles `/gtw sync`: checkout the default branch and
// pull --rebase from origin. The reply is an IM-friendly
// compact summary (✅ branch @ sha + commit list, or ✨
// already up to date) — not git's raw pull stdout. Errors are
// surfaced verbatim (RefreshDefaultBranch already includes the
// dirty-worktree refusal and rebase-conflict guidance the
// user needs).
func (f *Factory) runSync(ctx context.Context, _ command.RuntimeServices, _ *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {
	cwd, failOut := command.RequireActiveCwd(f.mgr.GetChatSession(input.ChatID))
	if failOut != nil {
		return failOut, nil
	}
	repoRoot, err := RepoRoot(ctx, cwd, f.deps.Git)
	if err != nil {
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ %v", err),
			Consumed: true,
		}, nil
	}
	// /gtw sync is a user-initiated refresh, so it ignores the
	// SkipRefreshDefaultBranch test seam (buildSyncReply honours
	// it as a short-circuit — but a real user calling /gtw sync
	// should never see that flag set).
	body, err := buildSyncReply(ctx, repoRoot, f.deps)
	if err != nil {
		return &command.SlashOutput{Reply: err.Error(), Consumed: true}, nil
	}
	return &command.SlashOutput{Reply: body, Consumed: true}, nil
}

// buildSyncReply lives in render.go alongside renderSyncReply —
// both shape the /gtw sync card. Kept together so the render
// surface stays cohesive.

// parseCloseForce extracts --force / -f from the close argv
// tail. Unlike /gtw fix, /gtw close takes no positional arg
// (you can't target a specific worktree — it always operates
// on the current chat's worktree), so the only thing to parse
// is the boolean flag. We still tolerate positional tokens
// silently for forward-compat (future: /gtw close <branch>).
func parseCloseForce(argv []string) (bool, error) {
	force := false
	for _, a := range argv {
		switch a {
		case "--force", "-f":
			force = true
		default:
			// Unknown token. Future: surface this when
			// /gtw close starts accepting positional args.
			// For now: silent accept.
		}
	}
	return force, nil
}

// pushArgs bundles the parsed argv tail of `/gtw push <...>`.
// Same rationale as fixArgs: a struct is more future-proof
// than a growing positional return tuple.
type pushArgs struct {
	// OpenPR, when true, also runs `gh pr create` (or
	// `glab mr create`) after push. Off by default — we
	// don't want to surprise users with auto-created PRs
	// on every /gtw push.
	OpenPR bool

	// NoCommit, when true, refuses any uncommitted changes
	// in the worktree. Off by default — auto-staging +
	// committing is the convenience path; users who want
	// strictness pass --no-commit.
	NoCommit bool
}

// parsePushArgs strips --pr / --no-commit (and their short
// forms once defined) from the push argv tail. No positional
// arg today — /gtw push always operates on the current
// chat's worktree, like /gtw close.
func parsePushArgs(argv []string) (pushArgs, error) {
	out := pushArgs{}
	for _, a := range argv {
		switch a {
		case "--pr":
			out.OpenPR = true
		case "--no-commit":
			out.NoCommit = true
		default:
			// Unknown token. Future: surface when we
			// accept positional args (e.g. --branch foo).
			// For now: silent accept.
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

// managerDraftsMap adapts Manager to the legacy DraftsMap
// interface (Store / Take / Lookup / Count) used by RunFix /
// HandleAction. Drafts are keyed by (chatID, userMsgID) on the
// Manager; the shim pins chatID.
type managerDraftsMap struct {
	mgr    *Manager
	chatID string
}

func (d *managerDraftsMap) Store(userMsgID string, draft *Draft) {
	d.mgr.StoreDraft(d.chatID, userMsgID, draft)
}
func (d *managerDraftsMap) Take(userMsgID string) *Draft {
	return d.mgr.TakeDraft(d.chatID, userMsgID)
}
func (d *managerDraftsMap) Lookup(userMsgID string) *Draft {
	return d.mgr.GetDraft(d.chatID, userMsgID)
}
func (d *managerDraftsMap) Count() int { return d.mgr.DraftCount(d.chatID) }
