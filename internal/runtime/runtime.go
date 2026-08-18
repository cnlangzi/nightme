// runtime.go — Runner struct + the wiring orchestrator.
//
// `Runner.Run` is the daemon entrypoint that the cmd/nightme CLI
// shell calls after parsing --channel and filling Deps. It runs
// the 7-step wiring sequence:
//
//  1. Load config (cfg) and registry stores (csFile, asFile)
//  2. Build agent registry (agents) + IM channel (ch); ch.Start
//  3. Build chatsession.Manager (mgr) with spawner + persistence
//  4. Build shared outbound infra:
//     - prcache.Registry (per-AS PR cache)
//     - gtw.HandlerDeps (git runner, HTTP prober)
//     - outbound.Emitter (the single outbound chokepoint;
//       holds ch and the GitStatusLookup closure that reads
//       mgr.Get(chatID) → cs.GitStatus(ctx))
//  5. Build gtw.Manager, ReactionRouter, command.Commander,
//     shell.Dispatcher (the command-adapter layer)
//  6. Build gateway.Router (messageDispatcher + em); wire
//     gwImpl.WithCommander / WithShellDispatch / WithActionHandler
//  7. mgr.WithEmitter(em) + WireRuntimeCallbacksAndRestore (must
//     precede gwImpl.Start; the latter depends on chat sessions
//     having their per-bus subscribers installed)
//
// The CLI shell owns cobra plumbing, signal handling, and the
// logger installation. Runner.Run takes the logger explicitly
// so it doesn't depend on cobra's context-with-logger trick.

package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/cnlangzi/nightme/internal/chatsession"
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
	if d.NewChannel == nil {
		d.NewChannel = def.NewChannel
	}
	return d
}

// runDaemon is the daemon core. Wires chatsession.Manager +
// Spawner + EventCallback; runs the gateway until signal /
// context cancel.
//
// See Runner.Run doc for the full 7-step wiring sequence.
func runDaemon(ctx context.Context, out io.Writer, deps Deps, sigCh <-chan os.Signal, logger *slog.Logger) error {
	cfg, err := deps.LoadConfig()
	if err != nil {
		return fmt.Errorf("run: load config: %w", err)
	}
	if cfg == nil {
		return errors.New("run: load config: returned nil config")
	}
	if !deps.SkipFeishuLogin && (cfg.Feishu.AppID == "" || cfg.Feishu.AppSecret == "") {
		return errors.New("run: Feishu credentials are not configured; run `nightme login feishu`")
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

	ch, err := deps.NewChannel(cfg)
	if err != nil {
		return fmt.Errorf("run: create channel: %w", err)
	}
	if ch == nil {
		return errors.New("run: channel is nil")
	}

	// Build the ChatSession manager.
	spawner := chatsession.NewRegistrySpawner(agents)
	mgr := chatsession.NewManager().
		WithSpawner(spawner).
		WithPersistence(csFile, asFile)

	// startChannel wires the channel and starts its connection.
	if err := ch.Start(ctx); err != nil {
		logger.Error("channel disconnected", "reason", err)
		return fmt.Errorf("run: start channel: %w", err)
	}
	logger.Info("channel connected")
	fmt.Fprintln(out, "Channel connected")

	// Phase 2.1: Channel interface carries its own logger +
	// health snapshot — no feishu-specific type assertion here.
	ch.SetLogger(logger)

	// F-40: register the WS lifecycle snapshot with the daemoncontrol
	// server so `nightme health` can answer. ch.HealthSnapshot
	// is implemented by every Channel (feishu returns its
	// WSHealthSnapshot JSON-encoded; echo returns Name() + an
	// empty payload).
	if deps.RegisterHealth != nil {
		deps.RegisterHealth(ch.HealthSnapshot)
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


	// emitter is the single daemon-wide outbound chokepoint.
	// Constructed here (before gateway.New) so the Gateway can
	// hold the reference at construction time — every
	// downstream send (runtime pump / slash command /
	// MessageState / PATCH) flows through the same Emitter
	// instance. Manager.WithEmitter (further down) binds the
	// same Emitter to every ChatSession.
	//
	// F-CLAUDE-PRINT-002 + fix-status-bar-git: GitStatus stamping
	// is centralized at the Emitter chokepoint via the
	// GitStatusLookup closure below. chatsession owns the
	// snapshot; the Emitter stamps it on every Send / SendCard
	// whose msg.GitStatus is nil. Business code never reads
	// GitStatus directly.
	em := outbound.New(ch, outbound.Options{
		// GitStatusLookup reaches the chatsession via mgr.Get and
		// delegates to cs.GitStatus(ctx). The lookup is invoked for
		// every Send / SendCard whose msg.GitStatus is nil — the
		// SINGLE chokepoint where every outbound message picks up
		// its git snapshot. ChatSession.GitStatus rebuilds the
		// snapshot from scratch on every call (no per-chat cache
		// layer): CollectGit runs `git status --porcelain --branch`
		// synchronously against SelectedCwd with a 3s timeout cap,
		// and LookupPR runs prcache.Cache.MaybeRefresh + PR() so
		// every read also nudges the PR refresh. See
		// chatsession.GitStatus for the contract.
		GitStatusLookup: func(ctx context.Context, chatID string) *messages.GitStatus {
			cs := mgr.Get(chatID)
			if cs == nil {
				return nil
			}
			return cs.GitStatus(ctx)
		},
	})

	// Bind the same Emitter to chatsession.Manager so its
	// HandleInbound error-reply paths inherit the StatusBar
	// footer (no-workspace / spawn-failed / queue-full).
	mgr.WithEmitter(em).WithPrimaryAgent(cfg.Primary).
		WithGitStatusDeps(gitStatusDeps)

	// F-58: gateway is now a thin pump + binding table. The
	// dispatch chain lives in *inbound.Router (constructed
	// below, after commander / shellDispatcher / router are
	// built). The four direct dependencies here match the
	// priority chain in inbound.Dispatch:
	//   1. command.Commander  (/-prefixed text)
	//   2. shell.Dispatcher   (!-prefixed text)
	//   3. chatsession.Manager (default — agent loop)
	//   4. services.ReactionRouter (msg.Reaction / msg.Action)
	// Note: the ReactionRouter is consulted FIRST in the
	// chain (action events carry empty Text and must not fall
	// through to a slash command). Order of the four
	// constructor arguments does NOT match the priority
	// order; see internal/gateway/inbound/inbound.go for the
	// actual chain slice.
	// gwImpl is constructed below, once inbound.Router exists. It
	// is deliberately NOT declared here as a nil *gateway.Router:
	// a nil declaration this far from the assignment is what let
	// `gwImpl.AttachChannels(ch)` drift ABOVE the constructor
	// during the F-58 refactor, which made every daemon start
	// SIGSEGV on a nil receiver. Declare-at-assignment keeps that
	// class of reordering a compile error instead of a crash.

	// All chat-session commands (/cwd /use /kill /new /watch /think
	// /tools) and /gtw are SlashCommandFactory implementations
	// implementations registered with reg.Register below. The legacy
	// gateway.RegisterChatSessionCommands helper is deleted;
	// gateway only sees the slash-command path via WithCommander
	// (the shim below) and the reaction path via WithActionHandler.

	// F-XX: gtw directly uses *chatsession.ChatSession (the
	// Sender interface is gone). Wiring is now:
	//   (a) gtw.Manager owns the state,
	//   (b) services.ReactionRouter dispatches reactions,
	//   (c) command.Commander routes slash commands,
	//   (d) this runtime shim translates *messages.InboundMessage
	//       ↔ command.SlashInput / *CommandResult,
	//   (e) SetGetChatSession hands gtw a per-chat lookup so
	//       RunFix / HandleDraftReaction can call
	//       cs.SelectedCwd / SetSelectedCwd / QueueUserMessage
	//       directly.

	gtwMgr := gtw.NewManager()
	gtwMgr.SetHandlerDeps(gtwDeps)

	// chatID → ChatSession lookup. The runtime owns this closure
	// so gtw's reaction / fix handlers can call into the
	// chatsession API without re-implementing GetOrCreate.
	// (Replaces the per-channel-resolver pattern that was used
	// when chatsession carried a per-chat Channel. Now that
	// chatsession holds a shared Emitter, only a shared CS
	// lookup is needed here.)
	gtwMgr.SetGetChatSession(func(chatID string) *chatsession.ChatSession {
		cs, _ := mgr.GetOrCreate(chatID, cfg.Primary)
		return cs
	})

	// Phase 2.3: command/* packages self-register via init()
	// — see internal/command/runtime.go. The orchestrator just
	// calls SetDeps once with the manager + primary + gtw
	// extension deps, then fetches the populated registry.

	// Wire gtw's per-process state. gtw's init() builder does
	// the heavy lifting (creating its own *Manager, calling
	// SetHandlerDeps + SetGetChatSession), but the runtime
	// owns the gtw.HandleReaction registration with the
	// reaction router (the router itself isn't a factory
	// pattern).
	command.SetDeps(command.Deps{
		Manager: mgr,
		Primary: cfg.Primary,
		GTWExt:  gtwDeps,
	})

	// Reaction router (services) — gtw's ReactionRouter
	// dispatches msg.Reaction / msg.Action events. The
	// handler is built from gtw.NewManager() inside gtw's
	// init closure above, but we still need a reference to
	// it here to register with the router. Easiest: create
	// the same Manager + SetHandlerDeps once more, and reuse
	// the router.Register pattern from the v0.x code.
	router := commandServices.NewReactionRouter()
	router.Register("*", gtwMgr.HandleReaction)

	// Slash command registry + commander. After SetDeps above
	// every command/* package's init() has produced a factory;
	// Default() returns the populated registry.
	reg := command.Default()
	commander := command.NewCommander(reg)

	// shellDispatcher owns the full shell-dispatch flow:
	// prefix detection, framework ⏳→✅ MessageState emission,
	// async exec, result posting. The shim in cmd/nightme/run.go
	// only does type adaptation — see shell.Dispatcher.Handle
	// for the actual logic. shell.Dispatcher takes the shared
	// outbound.Emitter directly (post-F-XX Sender removal), so
	// all outbound messages — including shell replies — flow
	// through the same chokepoint as slash commands, regular
	// messages, and receipt cards.
	shellDispatcher := shell.NewDispatcher(mgr.Emitter())

	// F-watch §3.1.1: the per-chat WatchMode gate used to be wired
	// here via gwImpl.WithWatchModeResolver. It moved into
	// chatsession.Manager.AcceptInbound (called from
	// newMessageDispatcher below) so the policy sits next to its
	// state — no more callback injection across the import
	// boundary. See internal/chatsession/manager.go AcceptInbound.

	// F-51: the action handler now routes through
	// services.ReactionRouter instead of calling
	// chatsession.ChatSession.HandleAction (chatsession no
	// longer dispatches reactions). The shim translates
	// F-58: build the dispatch chain. The *inbound.Router owns
	// the four direct dependencies (chatsession, command, shell,
	// action) and walks them in priority order. Replaces the v0.x
	// shim closures (WithCommander / WithShellDispatch /
	// WithActionHandler) that used to live here.
	ir := inbound.New(mgr, commander, shellDispatcher, router, em, cfg.Primary)
	gwImpl := gateway.New(ir, em)
	// Attach AFTER construction (see the note at the declaration
	// site above): the gateway needs its channel binding before
	// Start pumps inbound messages.
	gwImpl.AttachChannels(ch)

	// WithOnCreate fires for both restored (RestoreFromRegistry)
	// and future (GetOrCreate) ChatSessions. Place BEFORE
	// RestoreFromRegistry so restored chats get their handlers.
	if err := WireRuntimeCallbacksAndRestore(mgr, em, logger, gitStatusDeps, ch); err != nil {
		return fmt.Errorf("run: wire+restore: %w", err)
	}

	if err := gwImpl.Start(ctx); err != nil {
		return fmt.Errorf("run: start gateway: %w", err)
	}
	defer func() { _ = gwImpl.Stop(context.Background()) }()

	if deps.OnReady != nil {
		deps.OnReady()
	}

	logger.Info("daemon running",
		"chat_sessions", len(mgr.List()),
		"primary", cfg.Primary)

	// Block on signal or context cancellation.
	select {
	case <-ctx.Done():
	case sig, ok := <-sigCh:
		if ok && sig != nil {
			fmt.Fprintf(out, "[nightme] received %s\n", sig)
		}
	}
	return ShutdownRun(out, ch, mgr, csFile, asFile, prCacheReg, logger)
}

// Ensure unused imports stay declared (some are referenced in
// deeper runtime files that we'll add next; these locals let
// the file compile before handler.go / dispatcher.go are wired).
var (
	_ = io.Discard
)