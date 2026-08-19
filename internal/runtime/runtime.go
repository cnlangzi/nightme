// runtime.go — Runner struct + the multi-channel wiring
// orchestrator (v1.3+).
//
// `Runner.Run` is the daemon entrypoint that the cmd/nightme CLI
// shell calls after filling Deps. It runs the 4-phase wire:
//
//  1. shared resources — cfg / csFile / asFile / agents /
//     prCache / gtwDeps (read-only once per daemon)
//  2. shared wiring    — gtwMgr (with findChatSession-aware
//     SetGetChatSession) / reactionRouter / commander /
//     shellDispatcher / inbound.Router; these all use
//     runtime.findChatSession to resolve the per-chat mgr
//     (the per-channel mgr is the "owner" of each chatID
//     because it's the one that bound the channel's Emitter)
//  3. buildStack (per channel) — ch.Start + chatsession.Manager
//     + outbound.Emitter + WireRuntimeCallbacksAndRestore;
//     stash mgr in runtime.allMgrs; build gateway.Pump
//  4. start — gateway.AttachPumps + gateway.Start
//
// Per-chat restore is lazy (Manager.GetOrCreate on first inbound),
// so the daemon reaches ready without a synchronous chat_sessions
// scan. The per-channel Manager's GetOrCreate reads csFile and
// hydrates the entry on first hit.
//
// The CLI shell owns cobra plumbing, signal handling, and the
// logger installation. Runner.Run takes the logger explicitly
// so it doesn't depend on cobra's context-with-logger trick.
//
// v1.3+ invariants enforced here:
//   - runtime.allMgrs lists every per-channel chatsession.Manager.
//   - runtime.findChatSession(chatID) walks allMgrs (in-memory
//     first, then GetOrCreate per mgr) and returns the owning
//     ChatSession. The first mgr to successfully GetOrCreate a
//     chatID becomes its owner (chatID → mgr is implicit via
//     the chatID-namespaced-per-channel fact; feishu oc_* vs
//     telegram numeric IDs don't collide in practice).
//   - Each mgr has its own per-channel Emitter bound; outbound
//     from that mgr's ChatSessions goes to the right channel.
//   - No shared "default channel" or chatID → channel routing
//     table exists anywhere in the daemon.

package runtime

import (
	"github.com/cnlangzi/nightme/internal/chatstore"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/registry"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/command/gtw"
	commandServices "github.com/cnlangzi/nightme/internal/command/services"

	// Phase 2.3: each command package's init() self-registers
	// via command.RegisterBuilder. The blank imports below
	// ensure those init() functions actually run when the
	// daemon starts — without them, SetDeps would finalize
	// an empty registry.
	_ "github.com/cnlangzi/nightme/internal/command/close"
	_ "github.com/cnlangzi/nightme/internal/command/cwd"
	_ "github.com/cnlangzi/nightme/internal/command/gtw"
	_ "github.com/cnlangzi/nightme/internal/command/newcmd"
	_ "github.com/cnlangzi/nightme/internal/command/queue"
	_ "github.com/cnlangzi/nightme/internal/command/review"
	_ "github.com/cnlangzi/nightme/internal/command/steer"
	_ "github.com/cnlangzi/nightme/internal/command/stop"
	_ "github.com/cnlangzi/nightme/internal/command/think"
	_ "github.com/cnlangzi/nightme/internal/command/tools"
	_ "github.com/cnlangzi/nightme/internal/command/use"
	_ "github.com/cnlangzi/nightme/internal/command/watch"

	"github.com/cnlangzi/nightme/internal/gateway"
	"github.com/cnlangzi/nightme/internal/gateway/inbound"
	"github.com/cnlangzi/nightme/internal/gateway/outbound"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/prcache"
	"github.com/cnlangzi/nightme/internal/shell"
	// dsh is intentionally NOT imported here: the runtime no
	// longer touches the shared dsh host. The dsh bridge handles
	// lazy-start on first use via host.EnsureSharedHost in
	// internal/bridge/dsh/host/ensure.go. Bringing the host
	// package in here would just re-couple the runtime to dsh
	// lifecycle, which is exactly what the lazy-start refactor
	// removes.
)

// RunOptions bundles the per-run parameters that aren't part of
// Deps (signals, output writer, logger). The CLI shell passes
// these once at startup; tests can pass io.Discard / a discard
// logger / a closed signal channel.
type RunOptions struct {
	Out    io.Writer
	Logger *slog.Logger
	SigCh  <-chan os.Signal
}

// Runner is the per-daemon container: holds Deps + the run-time
// overrides. Constructed by the CLI shell, never reused across
// processes.
type Runner struct {
	Deps Deps
	Out  RunOptions
}

// New builds a Runner from Deps with default RunOptions
// (io.Discard / slog.Default() / nil signals). Use RunWith when
// you need to override those (the CLI shell always does).
func New(deps Deps) *Runner {
	return &Runner{Deps: deps}
}

// RunWith builds a Runner with explicit RunOptions. Nil fields
// in opts are still filled by Runner.Run — no early fill here
// to avoid two passes (Run already calls withDefaults).
func RunWith(deps Deps, opts RunOptions) *Runner {
	return &Runner{Deps: deps, Out: opts}
}

// Run starts the long-running daemon and blocks until ctx is
// cancelled or SigCh fires. Returns the shutdown error (channel
// stop failure, if any).
//
// Pass opts=nil to use the default per-Deps RunOptions (signals
// installed, Out=io.Discard, Logger=slog.Default()).
func (r *Runner) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	deps := r.Deps
	opts := r.Out
	opts = opts.withDefaults()
	deps = deps.fillDefaults()

	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	sigCh := opts.SigCh
	if sigCh == nil {
		owned := make(chan os.Signal, 2)
		signal.Notify(owned, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(owned)
		sigCh = owned
	}
	return runDaemon(ctx, opts.Out, deps, sigCh, opts.Logger)
}

// withDefaults / fillDefaults — local helpers to keep Runner
// construction noise-free.

func (o RunOptions) withDefaults() RunOptions {
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return o
}

func (d Deps) fillDefaults() Deps {
	def := DefaultDeps()
	if d.LoadConfig == nil {
		d.LoadConfig = def.LoadConfig
	}
	if d.OpenChatSessions == nil {
		d.OpenChatSessions = def.OpenChatSessions
	}
	if d.OpenAgentSessions == nil {
		d.OpenAgentSessions = def.OpenAgentSessions
	}
	if d.BuildAgents == nil {
		d.BuildAgents = def.BuildAgents
	}
	if d.NewChannels == nil {
		d.NewChannels = def.NewChannels
	}
	return d
}

// runDaemon is the daemon core. v1.3+ multi-channel 4-phase
// wire:
//
//	1. shared resources  — cfg / csFile / asFile / agents / prCache / gtwDeps
//	2. shared wiring     — gtwMgr / reactionRouter / commander /
//	                        shellDispatcher / inbound.Router
//	                        (all use runtime.findChatSession for the
//	                        per-chat mgr lookup)
//	3. buildStack        — for each registered channel with valid
//	                        credentials: ch.Start + chatsession.Manager
//	                        + outbound.Emitter + WireCallbacks; stash
//	                        mgr in runtime.allMgrs; build gateway.Pump
//	4. start              — gateway.AttachPumps + gateway.Start
//
// Per-chat restore is lazy (Manager.GetOrCreate on first inbound),
// so the daemon reaches ready without a synchronous chat_sessions
// scan. See CHANNEL.md §3 for the full flow diagram.
func runDaemon(ctx context.Context, out io.Writer, deps Deps, sigCh <-chan os.Signal, logger *slog.Logger) error {
	cfg, err := deps.LoadConfig()
	if err != nil {
		return fmt.Errorf("run: load config: %w", err)
	}
	if cfg == nil {
		return errors.New("run: load config: returned nil config")
	}

	csFile, err := deps.OpenChatSessions(cfg)
	if err != nil {
		return fmt.Errorf("run: open chat_sessions: %w", err)
	}
	asFile, err := deps.OpenAgentSessions(cfg)
	if err != nil {
		return fmt.Errorf("run: open agent_sessions: %w", err)
	}

	// Tidy up the obsolete v0.1 registry.json (the v1.2 daemon
	// no longer reads it). Best-effort.
	if err := RemoveLegacyRegistryFile(cfg); err != nil {
		logger.Warn("remove legacy registry.json", "err", err)
	}

	agents := deps.BuildAgents(cfg)
	if agents == nil {
		return errors.New("run: agent registry is nil")
	}

	// dsh is no longer started at boot. The dsh bridge lazy-starts
	// it on first use via host.EnsureSharedHost (see
	// internal/bridge/dsh/host/ensure.go). A user who never picks
	// the dsh agent — or doesn't have dsh installed — pays nothing
	// at startup; the daemon reaches ready without dsh in scope.
	//
	// Per-session re-attachment after a daemon restart is handled
	// by dsh's own session.fork at first use, not by a boot-time
	// RecoverAll pass. See dsh.newDriver → handshakeSession.

	// Shared spawner — every per-channel mgr uses this so the
	// agent registry is the single source of truth for which
	// agents can be spawned.
	spawner := chatsession.NewRegistrySpawner(agents)

	// v1.3+ multi-channel: scan the registry for every channel
	// with valid credentials. A channel whose builder returns
	// an error (missing creds) is skipped — runtime keeps going
	// with whatever subset of channels did start.
	chs, err := deps.NewChannels(cfg)
	if err != nil {
		return fmt.Errorf("run: new channels: %w", err)
	}
	if len(chs) == 0 {
		return fmt.Errorf("run: no channels configured; run `nightme login <channel>` first")
	}

	// Build the router wiring (slashCommandDispatcher +
	// messageDispatcher).
	// messageDispatcher is constructed later (after `em` is
	// built) so its error-reply paths — queue full / no
	// workspace / spawn failed — go through the same Emitter as
	// the runtime pump and pick up the StatusBar footer. The
	// old channel_wrap prepended its own Emitter wrap, so the
	// bug class "error replies miss the footer" is now caught at
	// compile time: the dispatcher is typed as outbound.Emitter
	// from the start.

	// F-31 + F-think + F-38: install gw.OnMessageState AND the
	// runtime's EventHandler into every ChatSession via the
	// Manager.onCreate hook. Both callbacks MUST be installed
	// together — separating them is a silent-failure landmine
	// because AgentEventBus / MessageStateBus fire only when
	// they have subscribers. The actual wiring + ordering
	// (WithOnCreate BEFORE RestoreFromRegistry) is enforced
	// inside WireRuntimeCallbacksAndRestore — see its doc.
	//
	// Per-AgentSession PR / MR cache. Built before Emitter
	// construction (the GitStatusLookup closure reads it) and
	// before gateway.New (the Emitter flows into Gateway's
	// outbound chokepoint). See prcache.Registry comment for
	// why this is owned at runtime scope, not on AgentSession
	// itself.
	prCacheReg := &prcache.Registry{}

	gtwDeps := gtw.HandlerDeps{
		Git:     gtw.ExecGitRunner{},
		Prober:  &gtw.ExecHTTPProber{},
		PRCache: prCacheReg,
	}

	// prResolver is the prcache ↔ gtw bridge: the cache
	// stores (pr, branch, expiresAt) but doesn't know how to
	// git symbolic-ref or `gh pr list`. We compose those
	// here, in the runtime, so prcache stays a leaf package
	// (no `gtw` import). Called by Cache.MaybeRefresh inside
	// the per-stamp trigger; runs in a goroutine on the
	// background-refresh path.
	prResolver := func(ctx context.Context, dir string) (*messages.PR, string, error) {
		branch, err := gtw.CurrentBranch(ctx, dir, gtwDeps.Git)
		if err != nil || branch == "" {
			return nil, branch, err
		}
		pr, err := gtw.CollectPR(ctx, dir, branch, gtwDeps)
		return pr, branch, err
	}

	// GitStatus deps — used by ChatSession.GitStatus on every
	// outbound stamp. The closures capture gtwDeps + prCacheReg;
	// chatsession itself stays decoupled from gtw (which would
	// create an import cycle: gtw → chatsession → outbound).
	gitStatusDeps := chatsession.GitStatusDeps{
		CollectGit: func(ctx context.Context, cwd string) (*messages.GitStatusSnapshot, error) {
			return gtw.CollectReadiness(ctx, cwd, gtw.ExecGitRunner{})
		},
		LookupPR: func(asID, cwd string) *messages.PR {
			// Per-read update attempt: every stamp's PR query
			// pessimistically nudges the cache. MaybeRefresh is
			// sync, no I/O, conditional: it only spawns a
			// goroutine when the 60s TTL has elapsed AND no
			// refresh is currently in flight. The common case
			// (cache fresh, no recent refresh) is one mutex
			// acquire + one time.Now() compare + one unlock.
			// PR() returns the last-known value either way —
			// a fresh refresh is visible on the NEXT read,
			// not this one. /gtw {pr, close} bypass the lazy
			// trigger by writing directly via
			// deps.PRCache.WritePR when they already know the
			// answer (the new PR number, or "branch deleted,
			// clear").
			if prCache := prCacheReg.GetOrCreate(asID); prCache != nil {
				prCache.MaybeRefresh(cwd, prResolver)
				return prCache.PR()
			}
			return nil
		},
	}

	// Per-channel mgr construction happens in buildStack
	// (Phase 3). The shared singletons below (gtwMgr, reactionRouter,
	// commander, shellDispatcher, inbound.Router) use
	// findChatSession to resolve per-chat sessions; they do not
	// hold a reference to any single mgr.

	gtwMgr := gtw.NewManager()
	gtwMgr.SetHandlerDeps(gtwDeps)

	// Phase 2.3: command/* packages self-register via init()
	// — see internal/command/runtime.go. The orchestrator just
	// calls SetDeps once with the manager + primary + gtw
	// extension deps, then fetches the populated registry.

	// Wire gtw's per-process state. gtw's init() builder does
	// Per-channel mgr construction happens in buildStack
	// (Phase 3). The shared singletons below (gtwMgr, reactionRouter,
	// commander, shellDispatcher, inbound.Router) use
	// findChatSession to resolve per-chat sessions; they do not
	// hold a reference to any single mgr.

	// Reaction router (services) — gtw's ReactionRouter
	// dispatches msg.Reaction / msg.Action events.
	//
	// gtw.Manager.HandleReaction is intentionally cs-blind (the
	// gtw package owns NO chat-session read logic — see
	// internal/command/gtw/manager.go doc). We resolve cs here
	// via findChatSession so the reaction path sees the same
	// per-channel session a slash command would see, and pass
	// the result through.
	router := commandServices.NewReactionRouter()
	router.Register("*", func(ctx context.Context, ev commandServices.ReactionEvent) bool {
		cs := findChatSession(ev.ChatID, cfg.Primary)
		if cs == nil {
			return false
		}
		return gtwMgr.HandleReaction(ctx, ev, cs)
	})

	// Slash command registry + commander. After SetDeps
	// every command/* package's init() has produced a factory;
	// Default() returns the populated registry.
	//
	// command/* factories do not take a *chatsession.Manager.
	// ChatSession references are supplied passively: slash
	// commands receive cs from the dispatcher parameter;
	// reactions receive cs from the runtime-layer wrapper that
	// resolves cs before calling gtwMgr.HandleReaction.
	command.SetDeps(command.Deps{
		Primary: cfg.Primary,
		// GTWExt carries gtw's HandlerDeps. Chat-session lookup
		// for the gtw reaction path is wired inline at the
		// ReactionRouter.Register call below (see the wrapper
		// closure that resolves cs via findChatSession before
		// calling gtwMgr.HandleReaction).
		GTWExt: gtwDeps,
	})
	reg := command.Default()
	commander := command.NewCommander(reg)

	// shellDispatcher owns the full shell-dispatch flow:
	// prefix detection, framework ⏳→✅ MessageState emission,
	// async exec, result posting.
	shellDispatcher := shell.NewDispatcher()

	// Build the dispatch chain (shared, used by all channels).
	// v1.3+ multi-channel: each pump's mgr closure is the
	// "current" mgr for that channel; the dispatcher takes
	// mgr per call. The Emitter argument is the per-call
	// fallback used by emitReply for command error paths —
	// the real outbound goes through cs.Emitter() inside
	// cmd.Handle. Pass a no-op Emitter; the error path
	// logs the failure and returns a SlashOutput to the
	// dispatch chain.
	ir := inbound.New(
		newNoOpMgr(), // csMgr: not used at the dispatcher level; the dispatch chain takes mgr per call. See inbound.Dispatch.
		commander,
		shellDispatcher,
		router,
		&noOpEmitter{},
		cfg.Primary,
	)

	// ── Phase 3: buildStack per channel ────────────────────────
	stackOpts := buildStackOpts{
		spawner:       spawner,
		csFile:        csFile,
		asFile:        asFile,
		primaryAgent:  cfg.Primary,
		gitStatusDeps: gitStatusDeps,
		logger:        logger,
	}

	var pumps []gateway.Pump
	startedChannels := []channel.Channel{}
	for _, ch := range chs {
		if err := ch.Start(ctx); err != nil {
			logger.Error("channel start failed",
				"name", ch.Name(), "err", err)
			continue
		}
		ch.SetLogger(logger)
		fmt.Fprintf(out, "Channel %s connected\n", ch.Name())

		pump, err := buildStack(ch, stackOpts, registerMgrInAllMgrs)
		if err != nil {
			return fmt.Errorf("run: build channel %s: %w", ch.Name(), err)
		}
		pumps = append(pumps, pump)
		startedChannels = append(startedChannels, ch)

		if deps.RegisterHealth != nil {
			deps.RegisterHealth(ch.HealthSnapshot)
		}
	}
	if len(pumps) == 0 {
		return errors.New("run: all channels failed to start; check creds and logs")
	}

	// ── Phase 4: start gateway ────────────────────────────────
	gwImpl := gateway.New(ir)
	gwImpl.AttachPumps(pumps...)
	if err := gwImpl.Start(ctx); err != nil {
		return fmt.Errorf("run: start gateway: %w", err)
	}
	defer func() { _ = gwImpl.Stop(context.Background()) }()

	if deps.OnReady != nil {
		deps.OnReady()
	}

	logger.Info("daemon running",
		"channels", len(pumps),
		"primary", cfg.Primary)

	// Block on signal or context cancellation.
	select {
	case <-ctx.Done():
	case sig, ok := <-sigCh:
		if ok && sig != nil {
			fmt.Fprintf(out, "[nightme] received %s\n", sig)
		}
	}
	return ShutdownRunMulti(out, startedChannels, csFile, asFile, prCacheReg, logger)
}

// Ensure unused imports stay declared (some are referenced in
// ─────────────────────────────────────────────────────────────────
// v1.3+ multi-channel plumbing
// ─────────────────────────────────────────────────────────────────
//
// allMgrs lists every per-channel chatsession.Manager owned by
// the daemon. Each channel adapter gets its own Manager at
// startup; chatIDs are channel-namespaced so a chat lookup
// naturally resolves to exactly one Manager.
//
// Concurrency: populated during the 3rd wire phase
// (buildStack, single-threaded) and read by:
//   - the gateway pumpOne goroutine (per channel)
//   - the gtw reaction handler (event-driven)
//   - findChatSession (event-driven, called by gtw / command)
//
// We use a sync.RWMutex so reads (lookup) don't block each other.
var (
	allMgrsMu sync.RWMutex
	allMgrs   []*chatsession.Manager
)

// findChatSession returns the ChatSession that owns chatID.
//
// Lookup order:
//  1. in-memory hit (mgr.Get) — covers chats hydrated on a
//     previous GetOrCreate call in this daemon's lifetime
//  2. lazy hydrate via mgr.GetOrCreate — fires Manager's
//     hydrateFromEntry path (reads csFile, restores AgentSession
//     pool, fires onCreate for handler installation)
//
// We walk every per-channel mgr. The first one that returns
// the chat wins. chatID namespacing across channels (feishu
// oc_* vs telegram numeric IDs) is what makes this unambiguous
// in practice — a feishu chatID never collides with a telegram
// chatID, so the iteration always finds exactly one mgr.
//
// Returns nil only if every mgr.GetOrCreate failed (which
// shouldn't happen — the mgr's own persistence layer is the
// source of truth; failing means csFile is corrupt).
func findChatSession(chatID, primaryAgent string) *chatsession.ChatSession {
	allMgrsMu.RLock()
	mgrs := append([]*chatsession.Manager(nil), allMgrs...)
	allMgrsMu.RUnlock()

	for _, mgr := range mgrs {
		if cs := mgr.Get(chatID); cs != nil {
			return cs
		}
	}
	for _, mgr := range mgrs {
		cs, err := mgr.GetOrCreate(chatID, primaryAgent)
		if err == nil && cs != nil {
			return cs
		}
	}
	return nil
}

// buildStackOpts bundles the per-channel dependencies that
// buildStack needs. spawner / csFile / asFile / primaryAgent
// are shared across channels (per-daemon); the rest are
// per-channel.
type buildStackOpts struct {
	spawner       chatsession.Spawner
	csFile        *chatstore.Store
	asFile        *registry.AgentSessionFile
	primaryAgent  string
	gitStatusDeps chatsession.GitStatusDeps
	logger        *slog.Logger
}

// buildStack wires one channel: starts the channel, constructs
// a per-channel chatsession.Manager + outbound.Emitter, installs
// the runtime handlers, registers the Manager in runtime.allMgrs,
// and returns a gateway.Pump for the gateway to attach.
//
// registerMgr is called to stash the freshly-built mgr in
// runtime.allMgrs. Passing the callback as a parameter (rather
// than reaching for the package var directly) keeps buildStack
// testable in isolation.
func buildStack(
	ch channel.Channel,
	opts buildStackOpts,
	registerMgr func(*chatsession.Manager),
) (gateway.Pump, error) {
	if ch == nil {
		return gateway.Pump{}, errors.New("runtime: buildStack: nil channel")
	}

	// Per-channel Manager. Declared FIRST so the Emitter's
	// GitStatusLookup closure (created below) can capture mgr
	// by reference — Go doesn't allow forward references in
	// closure bodies even though the variable is in scope.
	mgr := chatsession.NewManager().
		WithSpawner(opts.spawner).
		WithPersistence(opts.csFile, opts.asFile).
		WithPrimaryAgent(opts.primaryAgent).
		WithGitStatusDeps(opts.gitStatusDeps)

	// Per-channel Emitter: wraps the channel adapter + a
	// per-mgr GitStatusLookup closure. The closure reads from
	// THIS mgr (not allMgrs) so the snapshot reflects the
	// per-channel chat session, not some other channel's.
	em := outbound.New(ch, outbound.Options{
		GitStatusLookup: func(ctx context.Context, chatID string) *messages.GitStatus {
			cs := mgr.Get(chatID)
			if cs == nil {
				return nil
			}
			return cs.GitStatus(ctx)
		},
	})

	// Bind the per-channel Emitter to this mgr so new
	// ChatSessions inherit it. MUST happen BEFORE
	// WireRuntimeCallbacksAndRestore: that call may trigger
	// onCreate for hydrated chats (v0.x RestoreFromRegistry path),
	// and onCreate sets cs.emitter from m.emitter — which must
	// already be wired.
	mgr.WithEmitter(em)

	// Register the mgr BEFORE WireRuntimeCallbacksAndRestore:
	// the latter calls mgr.WithOnCreate, which fires for both
	// hydrated chats (legacy v0.x RestoreFromRegistry) and
	// new GetOrCreate calls. Registering first means any
	// onCreate can be observed by other goroutines.
	if registerMgr != nil {
		registerMgr(mgr)
	}

	// Install the runtime handlers (EventHandler /
	// MessageStateBus / PromptEndBus). This call must happen
	// BEFORE Start so the channel is in a known state when the
	// first inbound arrives; the per-ChatSession subscriptions
	// are wired via Manager.WithOnCreate, and onCreate fires
	// both for hydrated chats and for new GetOrCreate calls.
	if err := WireRuntimeCallbacksAndRestore(
		mgr, em, opts.logger,
		opts.gitStatusDeps,
		ch,
	); err != nil {
		return gateway.Pump{}, fmt.Errorf("run: wire channel %s: %w", ch.Name(), err)
	}

	return gateway.Pump{Channel: ch, Manager: mgr}, nil
}

// ─────────────────────────────────────────────────────────────────
// Legacy stubs (kept here to avoid breaking unrelated tooling
// before handler.go / dispatcher.go land; replaced in the
// per-channel wiring pass).
// ─────────────────────────────────────────────────────────────────
var (
	_ = io.Discard
)

// registerMgrInAllMgrs stashes m into the package-level allMgrs
// slice. It is passed as the registerMgr callback to
// buildStack so buildStack doesn't reach for the package var
// directly (improves testability).
func registerMgrInAllMgrs(m *chatsession.Manager) {
	allMgrsMu.Lock()
	allMgrs = append(allMgrs, m)
	allMgrsMu.Unlock()
}

// unregisterMgrFromAllMgrs removes m (used by tests to
// clean up after a buildStack). Production never calls this.
func unregisterMgrFromAllMgrs(m *chatsession.Manager) {
	allMgrsMu.Lock()
	defer allMgrsMu.Unlock()
	out := allMgrs[:0]
	for _, x := range allMgrs {
		if x != m {
			out = append(out, x)
		}
	}
	allMgrs = out
}

// noOpMgr satisfies the csMgr argument of inbound.New without
// exposing any single per-channel Manager. v1.3+ multi-channel:
// the dispatcher takes mgr per call via the per-pump pump
// closure; the constructor-level csMgr field is only used for
// legacy tryMessageDispatch paths and the inbound.Router's
// own emitReply path (which itself routes through cs.Emitter).
// Since the production code path always passes a real mgr via
// Dispatch(ctx, mgr, msg), this stub is never actually invoked
// against real data — it's only there to satisfy the inbound.New
// signature.
func newNoOpMgr() *chatsession.Manager {
	// Build a real (uninitialized) chatsession.Manager. It's
	// only used for inbound.New's signature; the per-call dispatch
	// path supplies the real mgr. We don't wire persistence,
	// spawner, or emitter — the stub's methods are never invoked
	// in production (per-call dispatch replaces them).
	return chatsession.NewManager()
}

// noOpEmitter satisfies outbound.Emitter for the
// dispatch-chain fallback path. The real outbound goes through
// cs.Emitter() inside cmd.Handle; this stub only matters if
// the dispatcher wants to send a reply without a per-channel
// cs, which doesn't happen in v1.3+ (every Dispatch has a real
// cs from GetOrCreate).
type noOpEmitter struct{}

func (noOpEmitter) Send(_ context.Context, _ messages.OutboundMessage) error { return nil }
func (noOpEmitter) Patch(_ context.Context, _ messages.OutboundMessage) error { return nil }
