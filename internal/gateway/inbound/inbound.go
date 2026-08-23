// Package inbound — the priority dispatch chain for every
// messages.InboundMessage that arrives at the daemon.
//
// Layered architecture (top-down):
//
//	gateway                — pump (channel → channelCh) + binding table
//	  └─ inbound (here)    — dispatch chain (the only inbound behaviour)
//	       ├─ chatsession  — message routing (WatchMode gate, GetOrCreate, …)
//	       ├─ command      — slash command dispatcher
//	       ├─ shell        — !cmd shell dispatcher
//	       └─ services     — reaction router
//
// All four dispatch targets are imported directly here (no
// callback indirection) because the layering is enforced by
// gateway NOT depending on inbound (gateway holds an *inbound.Router
// and pumps messages into it), and by chatsession / command /
// shell / services NOT importing gateway.
//
// v0.x history: dispatch used to be a flat method set on
// gateway (tryActionDispatch / tryCommandDispatch / …). F-57
// extracted the call-target wiring into per-package dispatchers
// (shell, command) and F-58 lifted the chain out of gateway
// entirely into this package. The result: gateway is now a
// pump + routing table; this package is the only place that
// knows "given an inbound message, who handles it".
//
// Dispatch chain (priority order, first match wins):
//
//  1. tryActionDispatch   — msg.Reaction / msg.Action events
//  2. tryCommandDispatch  — /-prefixed text via command.Commander
//  3. tryShellDispatch    — !-prefixed text via shell.Dispatcher
//  4. tryMessageDispatch  — universal fallback (WatchMode + agent loop)
package inbound

import (
	"context"
	"sync"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	commandServices "github.com/cnlangzi/nightme/internal/command/services"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/shell"
)

// CommandResult is the per-dispatch outcome. Returned by all
// four try methods.
// (kept the field names; the only thing that changed is the
// package that owns the type definition).
type CommandResult struct {
	Reply    string
	Consumed bool
	// Dropped indicates the dispatcher intentionally did not
	// forward the inbound to the message dispatcher. Used by
	// the action branch (no action handler wired, or handler
	// declined the event). Distinct from Consumed=true so log
	// lines can distinguish "the slash command replied" vs
	// "the message was silently dropped".
	Dropped bool
}

// Minimal interface contracts the Router needs from each
// dispatch target. Defined here (the consumer) so:
//
//   - Production types (chatsession.Manager, command.Commander,
//     *shell.Dispatcher, services.ReactionRouter) satisfy
//     them implicitly via Go's structural typing — no
//     upstream interface declaration is required.
//   - Tests can pass small stubs (a `var _ MessageHandler = (*stub)(nil)`)
//     without spinning up real chatsession / command / shell
//     instances. Keeps the dispatch-chain tests unit-scoped.
//
// All four interfaces are "minimum useful surface" — only the
// methods Router actually calls are listed. Adding a method
// call here is a deliberate "inbound needs this from the
// dispatch target" decision; downstream can grow without
// forcing an interface bump.
type (
	// MessageHandler is the chatsession.Manager surface the
	// inbound.Router depends on. Two methods are used:
	//
	//   - HandleInbound: the default branch (tryMessageDispatch)
	//     calls this for every message no other tryDispatch
	//     claimed.
	//   - GetOrCreate: the slash-command and shell branches
	//     resolve the ChatSession up front (commander.Dispatch
	//     and shell.Handle both need cs; the shell handler
	//     specifically needs cs.SelectedCwd).
	MessageHandler interface {
		HandleInbound(ctx context.Context, msg *messages.InboundMessage) error
		GetOrCreate(chatID, primaryAgent string) (*chatsession.ChatSession, error)
	}

	// CommandDispatcher is command.Commander: the inbound.Router's
	// slash-command branch (tryCommandDispatch) calls Match for
	// the synchronous routing decision (chain's handled signal)
	// and Dispatch for the actual async command execution
	// (running inside the runCommand goroutine). See F-59.
	CommandDispatcher interface {
		Match(text string) (cmdName string, matched bool)
		// v1.3+ multi-channel: mgr is the per-channel chatsession.Manager
		// that produced the inbound. Dispatch forwards mgr to
		// cmd.Handle so commands can resolve cross-channel state
		// (mgr.Get / mgr.SendPermission) against the channel that
		// owns this chatID. cs is the ChatSession GetOrCreate'd
		// from mgr by the runtime before Dispatch is called.
		Dispatch(ctx context.Context, rt command.RuntimeServices, mgr *chatsession.Manager, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, bool, error)
	}

	// ShellDispatcher is shell.Dispatcher: the inbound.Router's
	// shell branch (tryShellDispatch) resolves the chat session,
	// builds an InboundRequest, and calls Handle. The signature
	// is symmetric with command.Commander.Dispatch — both take
	// the per-channel chatsession.Manager + per-chat ChatSession,
	// both return a (*Output, bool) pair (bool = handled).
	// v1.3+ multi-channel: mgr is the per-channel chatsession.Manager
	// that produced the inbound. shell.Handle does NOT take a
	// context.Context because the spawned goroutine intentionally
	// outlives any inbound ctx (see internal/shell/dispatch.go
	// Dispatcher.Handle doc for the full rationale).
	ShellDispatcher interface {
		Handle(mgr *chatsession.Manager, cs *chatsession.ChatSession, ir shell.InboundRequest) (*shell.ShellOutput, bool)
	}

	// ReactionRouter is services.ReactionRouter: the inbound.Router's
	// action branch (tryActionDispatch) calls Handle for each
	// reaction / action event.
	ReactionRouter interface {
		Handle(ctx context.Context, chatID string, ev commandServices.ReactionEvent) bool
	}
)

// Router owns the priority dispatch chain and the four
// direct dependencies needed to run it. The runtime constructs
// one of these in cmd/nightme/run.go and hands it to
// gateway.New — gateway itself does not import this package
// (and in fact cannot, because gateway sits above it in the
// layering).
type Router struct {
	mu sync.RWMutex

	csMgr     *chatsession.Manager
	commander CommandDispatcher
	shell     ShellDispatcher
	action    ReactionRouter
	primary   string

	// emitter is the F-59 reply sink. F-59 made the
	// try*Dispatch methods asynchronous (they spawn a
	// goroutine that runs the actual command / action /
	// message work), so the reply-emit that previously lived
	// in gateway.dispatchLoop moved down into the inbound
	// package — specifically into the per-branch run*
	// goroutines. The wired Emitter is held here so each run*
	// goroutine can write its reply without having to
	// recover the gateway.Router from context.
	emitter messages.Emitter

	// execWg tracks the F-59 async-dispatch goroutines
	// (runCommand / runAction / runMessage). Production
	// shutdown does NOT wait on this WaitGroup — the goroutines
	// observe ctx cancellation via the inbound ctx, and the
	// shell dispatcher pattern ("goroutine intentionally NOT
	// tracked by the gateway's wg, so dispatchLoop isn't
	// blocked and the daemon can shut down cleanly without
	// deadlocking on its own restart") is mirrored here.
	// execWg exists for **tests only**: tests assert
	// post-dispatch side effects (router hit counts, message
	// handler invocations, reply strings) that only land
	// after the run* goroutine completes. Tests call
	// r.WaitExec() between Dispatch and assertion.
	execWg sync.WaitGroup
}

// New constructs a Router. All five dependencies are required —
// New panics if any is nil (the daemon is broken without each
// of them; explicit fail-fast at construction beats a nil-deref
// at first dispatch). em may be nil in test contexts where the
// reply-emit path is intentionally disabled; the run* helpers
// no-op when emitter is nil.
func New(csMgr *chatsession.Manager, commander CommandDispatcher, sh ShellDispatcher, action ReactionRouter, em messages.Emitter, primaryAgent string) *Router {
	if csMgr == nil {
		panic("inbound.New: chatsession.Manager must not be nil")
	}
	if commander == nil {
		panic("inbound.New: command.Commander must not be nil")
	}
	if sh == nil {
		panic("inbound.New: shell.Dispatcher must not be nil")
	}
	if action == nil {
		panic("inbound.New: services.ReactionRouter must not be nil")
	}
	return &Router{
		csMgr:     csMgr,
		commander: commander,
		shell:     sh,
		action:    action,
		emitter:   em,
		primary:   primaryAgent,
	}
}

// Dispatch is the priority chain entry point. Walks the four
// tryDispatch methods in order; the first one that claims the
// input (returns handled=true) wins. The chain itself does
// NOT inspect msg.Reaction / msg.Action / msg.Text at the top
// level — each tryDispatch owns its own pattern matching. To
// add a new dispatch mode, add a tryDispatch method + one
// entry in the chain slice below.
//
// Returns (nil, nil) only if the chain fails to claim (which
// can't happen today — tryMessageDispatch always claims —
// but the empty-chain behaviour is preserved as a safety net).
// Dispatch is the priority chain entry point. Walks the four
// tryDispatch methods in order; the first one that claims the
// input (returns handled=true) wins. The chain itself does
// NOT inspect msg.Reaction / msg.Action / msg.Text at the top
// level — each tryDispatch owns its own pattern matching. To
// add a new dispatch mode, add a tryDispatch method + one
// entry in the chain slice below.
//
// v1.3+ multi-channel: mgr is the per-channel chatsession.Manager
// that produced the inbound. Each try method uses mgr to
// resolve the right ChatSession (mgr.GetOrCreate) and pass it
// to the underlying command / shell handler, so commands and
// shell results route back to the right channel's Emitter.
//
// Returns (nil, nil) only if the chain fails to claim (which
// can't happen today — tryMessageDispatch always claims —
// but the empty-chain behaviour is preserved as a safety net).
func (r *Router) Dispatch(ctx context.Context, mgr *chatsession.Manager, msg *messages.InboundMessage) (*CommandResult, error) {
	if msg == nil {
		return nil, nil
	}
	for _, try := range []func(context.Context, *chatsession.Manager, *messages.InboundMessage) (bool, *CommandResult, error){
		r.tryActionDispatch,
		r.tryCommandDispatch,
		r.tryShellDispatch,
		r.tryMessageDispatch,
	} {
		handled, result, err := try(ctx, mgr, msg)
		if err != nil {
			return nil, err
		}
		if handled {
			return result, nil
		}
	}
	return nil, nil
}

// --- type-assertion helpers ----------------------------------------

// requireCommander returns the wired CommandDispatcher, or
// false if not wired. Kept as a method (vs. a field) so the
// nil check is a single source of truth in New.
func (r *Router) requireCommander() (CommandDispatcher, bool) {
	return r.commander, r.commander != nil
}

// requireShell returns the wired ShellDispatcher, or false
// if not wired.
func (r *Router) requireShell() (ShellDispatcher, bool) {
	return r.shell, r.shell != nil
}

// requireAction returns the wired ReactionRouter, or false
// if not wired.
func (r *Router) requireAction() (ReactionRouter, bool) {
	return r.action, r.action != nil
}

// WaitExec blocks until every async-dispatch goroutine
// spawned by Dispatch (F-59: runCommand / runAction /
// runMessage) has returned. Tests-only — production code MUST
// NOT call this; daemon shutdown observes ctx cancellation
// in the goroutines themselves (mirroring the shell.Dispatcher
// pattern documented in internal/shell/dispatch.go).

// CsMgr returns the chatsession.Manager that backs this router
// (the constructor's csMgr arg, which tests use to wire Emitter
// in the v1.3+ multi-channel path before calling gateway.New).
// Production paths go through AttachPumps + the per-channel
// mgr; this getter is only useful for legacy single-mgr callers
// that drive DispatchInbound without AttachPumps (e.g. the v0.x
// e2e_slash_test regression guard).
func (r *Router) CsMgr() *chatsession.Manager {
	return r.csMgr
}
//
// Safe to call multiple times — WaitGroup.Wait on a drained
// group is a fast no-op. Safe to call concurrently with
// Dispatch — the WaitGroup correctly tracks increments that
// happen during the wait.
func (r *Router) WaitExec() {
	r.execWg.Wait()
}
