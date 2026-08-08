package gtw

import (
	"context"
	"fmt"
	"strings"

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
			"/gtw list                        list pending gtw drafts in this chat\n" +
			"/gtw reset                       clear gtw state for this chat (debug)",
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
func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices, input command.SlashInput) (*command.SlashOutput, error) {
	if len(input.Args) < 2 {
		return &command.SlashOutput{
			Reply:    f.Spec().Usage,
			Consumed: true,
		}, nil
	}
	switch input.Args[1] {
	case "fix":
		return f.runFix(ctx, rt, input)
	case "list":
		return f.runList(rt, input)
	case "reset":
		return f.runReset(rt, input)
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
func (f *Factory) runFix(ctx context.Context, rt command.RuntimeServices, input command.SlashInput) (*command.SlashOutput, error) {
	if len(input.Args) < 3 {
		return &command.SlashOutput{
			Reply:    "Usage: /gtw fix <issue-id>  |  /gtw fix --name <branch>",
			Consumed: true,
		}, nil
	}

	mode, rawArg, err := parseFixMode(input.Args[2:])
	if err != nil {
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ %v", err),
			Consumed: true,
		}, nil
	}

	// Local-mode quick validation: the slug must not be empty
	// after normalisation. Doing this here (before RunFix) keeps
	// the SlashOutput path simple when deps.Send is nil in tests.
	if mode == ModeLocal {
		if _, err := DeriveBranchFromName(rawArg); err != nil {
			return &command.SlashOutput{
				Reply:    fmt.Sprintf("❌ %v", err),
				Consumed: true,
			}, nil
		}
	}
	// ID-mode quick validation: pre-validate locally with
	// parseIssueID (not strconv.Atoi) so "#42" is accepted —
	// the GitHub/GitLab convention users have muscle memory for.
	if mode == ModeRemote {
		if _, err := parseIssueID(rawArg); err != nil {
			return &command.SlashOutput{
				Reply:    fmt.Sprintf("Invalid issue id: %q (%v)", rawArg, err),
				Consumed: true,
			}, nil
		}
	}

	// Resolve per-chat ChatSession. The runtime wires a lookup
	// at startup that lazy-creates a *chatsession.ChatSession on
	// first GetChatSession miss.
	cs := f.mgr.GetChatSession(input.ChatID)
	if cs == nil || cs.ActiveCwd() == "" {
		return &command.SlashOutput{
			Reply:    "No active workspace. Send /cwd <path> first.",
			Consumed: true,
		}, nil
	}

	// Build slot / drafts shims that route to the Manager.
	slot := &managerContextSlot{mgr: f.mgr, chatID: input.ChatID}
	drafts := &managerDraftsMap{mgr: f.mgr, chatID: input.ChatID}

	// RunFix signature: (ctx, mode, cs, slot, drafts, deps,
	// chatID, messageID, args). Reply is sent inline via
	// deps.Send; *Result only carries Consumed / Dropped for
	// the runtime.
	_, err = RunFix(ctx, mode, cs, slot, drafts, f.deps, input.ChatID, input.MessageID, []string{rawArg})
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

// runList handles `/gtw list`. Lists pending gtw drafts in
// the current chat.
func (f *Factory) runList(_ command.RuntimeServices, input command.SlashInput) (*command.SlashOutput, error) {
	drafts := f.mgr.ListDrafts(input.ChatID)
	if len(drafts) == 0 {
		return &command.SlashOutput{
			Reply:    "/gtw list: (none in this chat)",
			Consumed: true,
		}, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "/gtw list — %d in this chat:\n", len(drafts))
	for i, d := range drafts {
		// Local-mode drafts have IssueID == -1 (no remote issue).
		// Display "(local)" instead of "-1" so the user doesn't
		// think the system is in a bad state.
		issueLabel := fmt.Sprintf("%d", d.Payload.IssueID)
		if d.Payload.IssueID == -1 {
			issueLabel = "(local)"
		}
		fmt.Fprintf(&b, "  [%d] kind=%s  issueID=%s  branch=%q  repo=%q  createdAt=%s\n",
			i, d.Kind, issueLabel, d.Payload.Branch, d.Payload.Repo,
			d.CreatedAt.Format("2006-01-02T15:04:05Z07:00"))
	}
	return &command.SlashOutput{Reply: b.String(), Consumed: true}, nil
}

// runReset handles `/gtw reset`. Clears gtw state for the
// current chat (debug-only).
func (f *Factory) runReset(_ command.RuntimeServices, input command.SlashInput) (*command.SlashOutput, error) {
	before := f.mgr.DraftCount(input.ChatID)
	f.mgr.Reset(input.ChatID)
	return &command.SlashOutput{
		Reply:    fmt.Sprintf("✅ /gtw reset — cleared Context + %d draft(s) for this chat", before),
		Consumed: true,
	}, nil
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
