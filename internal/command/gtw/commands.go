package gtw

import (
	"context"
	"fmt"
	"strconv"
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
		Usage: "/gtw fix <issue-id>     claim, label, and create a worktree\n" +
			"/gtw list                 list pending gtw drafts in this chat\n" +
			"/gtw reset                clear gtw state for this chat (debug)",
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

// runFix handles `/gtw fix <issue-id>`. The main fix flow:
//  1. Resolve activeCwd (need an active chat session with
//     workspace).
//  2. Build a Sender / slot / drafts shim that adapts the
//     *Manager back to the legacy ContextSlot / DraftsMap
//     interfaces (RunFix still uses those — refactoring RunFix
//     itself is out of scope for B2.4).
//  3. Call RunFix with the same arguments the gateway used to
//     pass.
func (f *Factory) runFix(ctx context.Context, rt command.RuntimeServices, input command.SlashInput) (*command.SlashOutput, error) {
	if len(input.Args) < 3 {
		return &command.SlashOutput{
			Reply:    "Usage: /gtw fix <issue-id>",
			Consumed: true,
		}, nil
	}
	// F-51: command.Commander prefixes Args with the command
	// name ("gtw"), then the subcommand ("fix"), then the
	// subcommand's args. So Args[2:] is the trailing argv for
	// /gtw fix (e.g. ["42"] for /gtw fix 42).
	issueArg := strings.TrimSpace(input.Args[2])
	issueID, err := strconv.Atoi(issueArg)
	if err != nil || issueID <= 0 {
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("Invalid issue id: %q", issueArg),
			Consumed: true,
		}, nil
	}

	// Resolve per-chat Sender. The runtime should have set it
	// via Manager.SetSender at chat creation; if missing,
	// reply with a hint.
	sender := f.mgr.GetSender(input.ChatID)
	if sender == nil {
		return &command.SlashOutput{
			Reply:    "No active chat session. Send /cwd <path> first.",
			Consumed: true,
		}, nil
	}
	if sender.ActiveCwd() == "" {
		return &command.SlashOutput{
			Reply:    "No active workspace. Send /cwd <path> first.",
			Consumed: true,
		}, nil
	}

	// Build slot / drafts shims that route to the Manager
	// (so the legacy RunFix code keeps working without
	// refactor).
	slot := &managerContextSlot{mgr: f.mgr, chatID: input.ChatID}
	drafts := &managerDraftsMap{mgr: f.mgr, chatID: input.ChatID}

	// RunFix signature: (ctx, cs, slot, drafts, deps, chatID,
	// messageID, args). Reply is sent inline via deps.Send;
	// *Result only carries Consumed / Dropped for the runtime.
	_, err = RunFix(ctx, sender, slot, drafts, f.deps, input.ChatID, input.MessageID, input.Args[2:])
	if err != nil {
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ /gtw fix failed: %v", err),
			Consumed: true,
		}, nil
	}
	// RunFix sent its reply via deps.Send — return an empty
	// SlashOutput marked Consumed so the gateway doesn't
	// double-send.
	return &command.SlashOutput{Consumed: true}, nil
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
		fmt.Fprintf(&b, "  [%d] kind=%s  issueID=%d  branch=%q  repo=%q  createdAt=%s\n",
			i, d.Kind, d.Payload.IssueID, d.Payload.Branch, d.Payload.Repo,
			d.CreatedAt.Format("2006-01-02T15:04:05Z07:00"))
	}
	return &command.SlashOutput{Reply: b.String(), Consumed: true}, nil
}

// runReset handles `/gtw reset`. Clears gtw state for the
// current chat (debug-only).
func (f *Factory) runReset(_ command.RuntimeServices, input command.SlashInput) (*command.SlashOutput, error) {
	before := f.mgr.DraftCount(input.ChatID)
	f.mgr.Reset(input.ChatID)
	hadCtx := f.mgr.HasContext(input.ChatID) // already cleared by Reset
	_ = hadCtx
	return &command.SlashOutput{
		Reply:    fmt.Sprintf("✅ /gtw reset — cleared gtwContext + %d draft(s) for this chat", before),
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
