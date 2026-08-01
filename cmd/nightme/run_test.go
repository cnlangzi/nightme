package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/pty"
	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/gateway"
	gatewaycmd "github.com/cnlangzi/nightme/internal/gateway/cmd"
	"github.com/cnlangzi/nightme/internal/registry"
	"github.com/cnlangzi/nightme/internal/session"
)

type fakeRunChannel struct {
	incoming chan channel.Message
	started  chan struct{}

	mu      sync.Mutex
	startN  int
	stopN   int
	closeIn sync.Once
}

func newFakeRunChannel() *fakeRunChannel {
	return &fakeRunChannel{
		incoming: make(chan channel.Message, 8),
		started:  make(chan struct{}),
	}
}

func (f *fakeRunChannel) Start(context.Context) error {
	f.mu.Lock()
	f.startN++
	f.mu.Unlock()
	select {
	case <-f.started:
	default:
		close(f.started)
	}
	return nil
}

func (f *fakeRunChannel) Stop(context.Context) error {
	f.mu.Lock()
	f.stopN++
	f.mu.Unlock()
	f.closeIn.Do(func() { close(f.incoming) })
	return nil
}

func (f *fakeRunChannel) SendMessage(context.Context, string, string) error           { return nil }
func (f *fakeRunChannel) SendLongMessage(context.Context, string, string) error       { return nil }
func (f *fakeRunChannel) Incoming() <-chan channel.Message                            { return f.incoming }
func (f *fakeRunChannel) Name() string                                                { return "fake" }
func (f *fakeRunChannel) Send(ctx context.Context, msg gateway.OutboundMessage) error { return nil }

func (f *fakeRunChannel) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.startN, f.stopN
}

type fakeRunManager struct {
	mu sync.Mutex

	sessions  []*session.Session
	restored  bool
	persisted bool
	detached  []string
	killed    []string
}

func (f *fakeRunManager) Create(context.Context, session.CreateRequest) (*session.Session, error) {
	return nil, errors.New("fake run manager: Create not used")
}
func (f *fakeRunManager) CreateOrUpdate(string, string, string, string, []string) (*session.Session, error) {
	return nil, errors.New("fake run manager: CreateOrUpdate not used")
}
func (f *fakeRunManager) Run(context.Context, string, string, []string) (*session.Session, error) {
	return nil, errors.New("fake run manager: Run not used")
}
func (f *fakeRunManager) KillByChat(string) error {
	return errors.New("fake run manager: KillByChat not used")
}
func (f *fakeRunManager) GetByChat(string) (*session.Session, error) {
	return nil, session.ErrSessionNotFound
}
func (f *fakeRunManager) Get(string) (*session.Session, error) {
	return nil, session.ErrSessionNotFound
}
func (f *fakeRunManager) List() []*session.Session {
	return append([]*session.Session(nil), f.sessions...)
}
func (f *fakeRunManager) Kill(id string) error {
	f.mu.Lock()
	f.killed = append(f.killed, id)
	f.mu.Unlock()
	return nil
}
func (f *fakeRunManager) Restore(context.Context) error {
	f.mu.Lock()
	f.restored = true
	f.mu.Unlock()
	return nil
}
func (f *fakeRunManager) Persist() error {
	f.mu.Lock()
	f.persisted = true
	f.mu.Unlock()
	return nil
}
func (f *fakeRunManager) MarkDetached(id string) error {
	f.mu.Lock()
	f.detached = append(f.detached, id)
	f.mu.Unlock()
	return nil
}

func runTestConfig() *config.Config {
	return &config.Config{Feishu: config.FeishuConfig{
		AppID:     "cli_test",
		AppSecret: "secret_test",
	}}
}

func runTestDeps(cfg *config.Config, ch *fakeRunChannel, mgr *fakeRunManager, signals <-chan os.Signal) runDeps {
	return runDeps{
		loadConfig: func() (*config.Config, error) { return cfg, nil },
		openRegistry: func(*config.Config) (*registry.File, error) {
			return nil, nil
		},
		buildAgents: func(*config.Config) *agent.Registry { return agent.New() },
		newChannel:  func(*config.Config) (channel.Channel, error) { return ch, nil },
		newManager:  func(*agent.Registry, *registry.File, session.EventCallback) session.Manager { return mgr },
		newGateway: func(mgr session.Manager, reg *agent.Registry, resp gatewaycmd.Responder) gateway.Gateway {
			gw := gateway.New(nil)
			gatewaycmd.RegisterDefaultCommands(gw, mgr, reg, resp)
			return gw
		},
		signals: signals,
	}
}

func TestRun_RequiresConfig(t *testing.T) {
	cmd := newRunCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetContext(context.Background())
	deps := runDeps{loadConfig: func() (*config.Config, error) {
		return nil, errors.New("malformed yaml")
	}}

	err := runRunWith(cmd, deps)
	if err == nil || !strings.Contains(err.Error(), "run: load config") {
		t.Fatalf("runRunWith error = %v, want load-config error", err)
	}
}

func TestRun_RequiresFeishuCredentials(t *testing.T) {
	cmd := newRunCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetContext(context.Background())
	deps := runDeps{
		loadConfig: func() (*config.Config, error) { return &config.Config{}, nil },
		openRegistry: func(*config.Config) (*registry.File, error) {
			return nil, errors.New("must not open registry")
		},
	}

	err := runRunWith(cmd, deps)
	if err == nil || !strings.Contains(err.Error(), "nightme auth login feishu") {
		t.Fatalf("runRunWith error = %v, want auth hint", err)
	}
}

func TestRun_StartsChannelAndManager(t *testing.T) {
	cfg := runTestConfig()
	ch := newFakeRunChannel()
	ch.incoming <- channel.Message{ChatID: "oc_chat", Text: "hello"}
	mgr := &fakeRunManager{sessions: []*session.Session{
		{ID: "s_detached", ChatID: "oc_chat"},
	}}
	signals := make(chan os.Signal, 1)
	go func() {
		<-ch.started
		time.Sleep(10 * time.Millisecond)
		signals <- syscall.SIGTERM
	}()

	var out bytes.Buffer
	cmd := newRunCmd()
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())
	if err := runRunWith(cmd, runTestDeps(cfg, ch, mgr, signals)); err != nil {
		t.Fatalf("runRunWith: %v", err)
	}

	startN, stopN := ch.counts()
	if startN != 1 || stopN != 1 {
		t.Fatalf("channel lifecycle = start %d/stop %d, want 1/1", startN, stopN)
	}
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if !mgr.restored || !mgr.persisted {
		t.Errorf("manager lifecycle restored=%v persisted=%v", mgr.restored, mgr.persisted)
	}
	if len(mgr.detached) != 1 || mgr.detached[0] != "s_detached" {
		t.Errorf("detached sessions = %v, want [s_detached]", mgr.detached)
	}
	if len(mgr.killed) != 0 {
		t.Errorf("Kill called during graceful shutdown: %v", mgr.killed)
	}
	if !strings.Contains(out.String(), "Feishu WebSocket connected") {
		t.Errorf("daemon output = %q", out.String())
	}
}

func TestRun_GracefulShutdown(t *testing.T) {
	cfg := runTestConfig()
	ch := newFakeRunChannel()
	mgr := &fakeRunManager{sessions: []*session.Session{
		{ID: "s_one"},
		{ID: "s_two"},
	}}
	signals := make(chan os.Signal, 1)
	go func() {
		<-ch.started
		signals <- syscall.SIGINT
	}()

	cmd := newRunCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetContext(context.Background())
	if err := runRunWith(cmd, runTestDeps(cfg, ch, mgr, signals)); err != nil {
		t.Fatalf("runRunWith: %v", err)
	}

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.detached) != 2 {
		t.Fatalf("detached %v, want two sessions", mgr.detached)
	}
	if len(mgr.killed) != 0 {
		t.Fatalf("Kill called = %v, want none", mgr.killed)
	}
}

// TestBuildRunAgentRegistry_HasBuiltinsOnly is the architectural
// guard: an empty config (or no config) must yield exactly the
// agents registered via init() in their respective packages. v0.2.x
// ships only `claude`; adding a new built-in is a new package +
// blank import in cmd/nightme/main.go, never a name in a switch.
func TestBuildRunAgentRegistry_HasBuiltinsOnly(t *testing.T) {
	cases := []struct {
		name    string
		entries map[string]config.AgentEntry
	}{
		{"nil map", nil},
		{"empty map", map[string]config.AgentEntry{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &config.Config{Agent: config.AgentConfig{Agents: c.entries}}
			reg := buildRunAgentRegistry(cfg)

			names := make(map[string]bool)
			for _, a := range reg.List() {
				names[a.Name()] = true
			}
			if !names["claude"] {
				t.Errorf("claude missing from registry: %v", names)
			}
			for _, unwanted := range []string{"codex", "opencode"} {
				if names[unwanted] {
					t.Errorf("%s should not be auto-registered (no Builtins entry, no user config)", unwanted)
				}
			}
		})
	}
}

// TestBuildRunAgentRegistry_UserConfigRegistered verifies that any
// agent named in cfg.Agent.Agents becomes available, regardless of
// whether it has a Builtins entry. User-configured agents always
// land in pty (bridge/pty.NewAgent) — the safe default for arbitrary CLIs.
func TestBuildRunAgentRegistry_UserConfigRegistered(t *testing.T) {
	cfg := &config.Config{
		Agent: config.AgentConfig{Agents: map[string]config.AgentEntry{
			"custom": {Command: "/bin/echo", Args: []string{"--custom"}, Env: map[string]string{"Z": "last", "A": "first"}},
		}},
		Session: config.SessionConfig{DefaultPtyCols: 100, DefaultPtyRows: 40},
	}
	reg := buildRunAgentRegistry(cfg)

	custom, ok := mustAgent(reg, "custom").(*pty.Agent)
	if !ok {
		t.Fatalf("custom agent type = %T, want *pty.Agent", mustAgent(reg, "custom"))
	}
	if got := custom.Mode(); got != agent.ModePTY {
		t.Errorf("custom mode = %s, want pty", got)
	}
	args := custom.Args()
	if len(args) != 1 || args[0] != "--custom" {
		t.Errorf("custom args = %v", args)
	}
	if custom.Cols != 100 || custom.Rows != 40 {
		t.Errorf("custom PTY size = %dx%d, want 100x40", custom.Cols, custom.Rows)
	}
}

// TestBuildRunAgentRegistry_UserConfigOverridesBuiltin covers the
// override path: a user entry whose name matches a built-in (e.g.
// `claude`) replaces the built-in. The user's command path wins
// but the new instance is always a pty (PTY bridge) — overriding loses the
// dedicated bridge features (documented in the init() comment).
func TestBuildRunAgentRegistry_UserConfigOverridesBuiltin(t *testing.T) {
	cfg := &config.Config{
		Agent: config.AgentConfig{Agents: map[string]config.AgentEntry{
			"claude": {Command: "/custom/path/claude"},
		}},
	}
	reg := buildRunAgentRegistry(cfg)

	a := mustAgent(reg, "claude")
	if a.Command() != "/custom/path/claude" {
		t.Errorf("claude command = %q, want /custom/path/claude", a.Command())
	}
	if got := a.Mode(); got != agent.ModePTY {
		t.Errorf("claude mode after override = %s, want pty", got)
	}
}

// TestBuildRunAgentRegistry_UnknownByDefault guards the no-fallback
// invariant: a name neither in Builtins nor in cfg.Agent.Agents must
// produce ErrUnknownAgent. This is the architectural difference from
// the v0.2.0 config-driven approach — "I haven't configured it" no
// longer means "give me claude / codex / opencode".
func TestBuildRunAgentRegistry_UnknownByDefault(t *testing.T) {
	reg := buildRunAgentRegistry(&config.Config{})
	for _, name := range []string{"codex", "opencode", "my-agent"} {
		if _, err := reg.Get(name); !errors.Is(err, agent.ErrUnknownAgent) {
			t.Errorf("Get(%s) = %v, want ErrUnknownAgent", name, err)
		}
	}
}

func mustAgent(reg *agent.Registry, name string) agent.Agent {
	a, err := reg.Get(name)
	if err != nil {
		panic(err)
	}
	return a
}

func TestRun_CleanupFlagKillsSessions(t *testing.T) {
	cfg := runTestConfig()
	ch := newFakeRunChannel()
	mgr := &fakeRunManager{sessions: []*session.Session{
		{ID: "s_one"},
		{ID: "s_two"},
	}}
	signals := make(chan os.Signal, 1)
	go func() {
		<-ch.started
		signals <- syscall.SIGINT
	}()

	cmd := newRunCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetContext(context.Background())

	deps := withCleanup(runTestDeps(cfg, ch, mgr, signals), true)
	if err := runRunWith(cmd, deps); err != nil {
		t.Fatalf("runRunWith: %v", err)
	}

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.detached) != 0 {
		t.Errorf("detached = %v, want none under --cleanup", mgr.detached)
	}
	if len(mgr.killed) != 2 {
		t.Errorf("killed = %v, want both sessions", mgr.killed)
	}
}

// TestRun_DefaultDetachesSessions pins the v0.1 behavior so a
// future refactor cannot silently switch shutdown to kill.
func TestRun_DefaultDetachesSessions(t *testing.T) {
	cfg := runTestConfig()
	ch := newFakeRunChannel()
	mgr := &fakeRunManager{sessions: []*session.Session{
		{ID: "s_one"},
	}}
	signals := make(chan os.Signal, 1)
	go func() {
		<-ch.started
		signals <- syscall.SIGINT
	}()

	cmd := newRunCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetContext(context.Background())

	deps := runTestDeps(cfg, ch, mgr, signals)
	if err := runRunWith(cmd, deps); err != nil {
		t.Fatalf("runRunWith: %v", err)
	}

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.killed) != 0 {
		t.Errorf("killed = %v, want none (default is detach)", mgr.killed)
	}
	if len(mgr.detached) != 1 {
		t.Errorf("detached = %v, want one", mgr.detached)
	}
}

// Keep the compile-time contract visible to this package's tests without
// depending on an implementation-specific manager method set.
var _ session.Manager = (*fakeRunManager)(nil)

// realRunManager embeds MemoryManager so the daemon can exercise
// the full gateway integration. It overrides the few methods the
// production daemon actually calls (Create / CreateOrUpdate / Run /
// KillByChat) directly, falling through to the embedded Manager
// for everything else.
type realRunManager struct {
	*session.MemoryManager
}

func (r *realRunManager) CreateOrUpdate(chatID, chatType, workspace, agentName string, args []string) (*session.Session, error) {
	return r.MemoryManager.CreateOrUpdate(chatID, chatType, workspace, agentName, args)
}

func (r *realRunManager) Run(ctx context.Context, chatID, agentName string, extraArgs []string) (*session.Session, error) {
	return r.MemoryManager.Run(ctx, chatID, agentName, extraArgs)
}

func (r *realRunManager) KillByChat(chatID string) error {
	return r.MemoryManager.KillByChat(chatID)
}

// recordingChannel is a fakeRunChannel that also captures every
// reply the gateway sends back, so integration tests can assert
// on the user-visible trail.
type recordingChannel struct {
	*fakeRunChannel
	mu       sync.Mutex
	replies  []string
	routedIn []string
}

func newRecordingChannel() *recordingChannel {
	return &recordingChannel{fakeRunChannel: newFakeRunChannel()}
}

func (r *recordingChannel) SendMessage(_ context.Context, _, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.replies = append(r.replies, text)
	return nil
}

func (r *recordingChannel) SendLongMessage(ctx context.Context, chatID, text string) error {
	return r.SendMessage(ctx, chatID, text)
}

func (r *recordingChannel) Send(ctx context.Context, msg gateway.OutboundMessage) error {
	return r.SendMessage(ctx, msg.ChatID, msg.Text)
}

func (r *recordingChannel) lastReply() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.replies) == 0 {
		return ""
	}
	return r.replies[len(r.replies)-1]
}

// integrationDeps builds a runDeps that wires the real
// MemoryManager + the gateway so the daemon can run a full
// /cwd → /run → /kill round-trip on the supplied channel.
func integrationDeps(t *testing.T, ch *recordingChannel, signals <-chan os.Signal) runDeps {
	t.Helper()
	agents := agent.New()
	// /bin/cat blocks on stdin so the spawned agent stays alive
	// without producing output, which keeps the reply ordering
	// deterministic.
	a := pty.NewAgent("claude", "/bin/cat", nil, nil)
	a.Cols = 80
	a.Rows = 24
	agents.Register(a)
	mgr := &realRunManager{MemoryManager: session.NewMemoryManager(agents, nil, nil)}
	return runDeps{
		loadConfig: func() (*config.Config, error) {
			return &config.Config{Feishu: config.FeishuConfig{
				AppID: "cli_test", AppSecret: "secret_test",
			}}, nil
		},
		openRegistry: func(*config.Config) (*registry.File, error) { return nil, nil },
		buildAgents:  func(*config.Config) *agent.Registry { return agents },
		newChannel:   func(*config.Config) (channel.Channel, error) { return ch, nil },
		newManager: func(agents *agent.Registry, reg *registry.File, cb session.EventCallback) session.Manager {
			return mgr
		},
		newGateway: func(mgr session.Manager, reg *agent.Registry, resp gatewaycmd.Responder) gateway.Gateway {
			gw := gateway.New(nil)
			gatewaycmd.RegisterDefaultCommands(gw, mgr, reg, resp)
			return gw
		},
		signals: signals,
	}
}

// TestRun_IntegratesGateway_Cwd verifies that a /cwd message
// arriving on the channel is dispatched to the gateway and the
// user sees the workspace-set reply.
func TestRun_IntegratesGateway_Cwd(t *testing.T) {
	ch := newRecordingChannel()
	signals := make(chan os.Signal, 1)
	go func() {
		<-ch.started
		ch.incoming <- channel.Message{ChatID: "oc_chat", Text: "/cwd /tmp"}
		time.Sleep(20 * time.Millisecond)
		signals <- syscall.SIGTERM
	}()

	var out bytes.Buffer
	cmd := newRunCmd()
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())
	if err := runRunWith(cmd, integrationDeps(t, ch, signals)); err != nil {
		t.Fatalf("runRunWith: %v", err)
	}

	got := ch.lastReply()
	if !strings.Contains(got, "Workspace set") {
		t.Errorf("last reply = %q, want workspace-set", got)
	}
}

// TestRun_GatewayRoundTrip simulates the full spec flow:
// /cwd → /run → /kill. Each reply is captured.
func TestRun_GatewayRoundTrip(t *testing.T) {
	ch := newRecordingChannel()
	signals := make(chan os.Signal, 1)
	go func() {
		<-ch.started
		ch.incoming <- channel.Message{ChatID: "oc_chat", Text: "/cwd /tmp"}
		time.Sleep(20 * time.Millisecond)
		ch.incoming <- channel.Message{ChatID: "oc_chat", Text: "/run claude"}
		time.Sleep(20 * time.Millisecond)
		ch.incoming <- channel.Message{ChatID: "oc_chat", Text: "/kill"}
		time.Sleep(20 * time.Millisecond)
		signals <- syscall.SIGTERM
	}()

	var out bytes.Buffer
	cmd := newRunCmd()
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())
	if err := runRunWith(cmd, integrationDeps(t, ch, signals)); err != nil {
		t.Fatalf("runRunWith: %v", err)
	}

	ch.mu.Lock()
	defer ch.mu.Unlock()
	if len(ch.replies) < 3 {
		t.Fatalf("got %d replies, want at least 3 (cwd + run + kill): %q", len(ch.replies), ch.replies)
	}
	// The first reply should be the workspace-set confirmation.
	if !strings.Contains(ch.replies[0], "Workspace set") {
		t.Errorf("reply[0] = %q, want workspace-set", ch.replies[0])
	}
	// The /run reply should report "Already running" (PID is alive).
	if !strings.Contains(ch.replies[1], "running") {
		t.Errorf("reply[1] = %q, want running feedback", ch.replies[1])
	}
	// The /kill reply is the kill confirmation.
	if !strings.Contains(ch.replies[2], "killed") {
		t.Errorf("reply[2] = %q, want 'killed' feedback", ch.replies[2])
	}
}

// TestRun_NonSlashMessagePassthrough verifies that plain text
// messages (not matching any slash command) flow through the
// gateway fallback. The fallback is wired to forward to the live
// session via SendText; since we have no session here, the
// fallback's "no workspace set" hint should be the reply.
func TestRun_NonSlashMessagePassthrough(t *testing.T) {
	ch := newRecordingChannel()
	signals := make(chan os.Signal, 1)
	go func() {
		<-ch.started
		ch.incoming <- channel.Message{ChatID: "oc_chat", Text: "hello world"}
		time.Sleep(20 * time.Millisecond)
		signals <- syscall.SIGTERM
	}()

	var out bytes.Buffer
	cmd := newRunCmd()
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())
	if err := runRunWith(cmd, integrationDeps(t, ch, signals)); err != nil {
		t.Fatalf("runRunWith: %v", err)
	}

	got := ch.lastReply()
	if got != "no workspace set, send /cwd <path> first" {
		t.Errorf("reply = %q, want workspace-set hint", got)
	}
}

// TestRun_UnrecognizedSlashRoutesFallback verifies that an
// unknown /-command transparently falls through to the fallback
// handler (matching the spec's "agent's namespace" rule).
func TestRun_UnrecognizedSlashRoutesFallback(t *testing.T) {
	ch := newRecordingChannel()
	signals := make(chan os.Signal, 1)
	go func() {
		<-ch.started
		ch.incoming <- channel.Message{ChatID: "oc_chat", Text: "/clear"}
		time.Sleep(20 * time.Millisecond)
		signals <- syscall.SIGTERM
	}()

	var out bytes.Buffer
	cmd := newRunCmd()
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())
	if err := runRunWith(cmd, integrationDeps(t, ch, signals)); err != nil {
		t.Fatalf("runRunWith: %v", err)
	}

	got := ch.lastReply()
	if got != "no workspace set, send /cwd <path> first" {
		t.Errorf("reply = %q, want fallback hint", got)
	}
}

// TestRun_HelpCommandWorks verifies /help flows through the
// gateway and the body lists the default commands.
func TestRun_HelpCommandWorks(t *testing.T) {
	ch := newRecordingChannel()
	signals := make(chan os.Signal, 1)
	go func() {
		<-ch.started
		ch.incoming <- channel.Message{ChatID: "oc_chat", Text: "/help"}
		time.Sleep(20 * time.Millisecond)
		signals <- syscall.SIGTERM
	}()

	var out bytes.Buffer
	cmd := newRunCmd()
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())
	if err := runRunWith(cmd, integrationDeps(t, ch, signals)); err != nil {
		t.Fatalf("runRunWith: %v", err)
	}

	got := ch.lastReply()
	for _, want := range []string{"/cwd", "/run", "/kill", "/help"} {
		if !strings.Contains(got, want) {
			t.Errorf("help reply missing %q: %q", want, got)
		}
	}
}

// TestRun_DefaultDepsIncludesGateway ensures the production
// defaultRunDeps wires a non-nil newGateway so the daemon
// actually creates one.
func TestRun_DefaultDepsIncludesGateway(t *testing.T) {
	deps := defaultRunDeps()
	if deps.newGateway == nil {
		t.Fatal("defaultRunDeps.newGateway is nil; PR #5 wiring missing")
	}
}

// echoIntegrationDeps mirrors integrationDeps but with a /bin/cat
// agent so we can exercise the multi-message path without a
// Claude Code dependency. /bin/cat echoes its stdin to stdout,
// so each EventText emitted by the PTY bridge matches the text
// the user message carried — perfect for asserting per-message
// ordering and deadlock-freeness without involving an LLM.
func echoIntegrationDeps(t *testing.T, ch *recordingChannel, signals <-chan os.Signal) runDeps {
	t.Helper()
	agents := agent.New()
	a := pty.NewAgent("echo", "/bin/cat", nil, nil)
	a.Cols = 80
	a.Rows = 24
	agents.Register(a)
	mgr := &realRunManager{MemoryManager: session.NewMemoryManager(agents, nil, nil)}
	return runDeps{
		loadConfig: func() (*config.Config, error) {
			return &config.Config{Feishu: config.FeishuConfig{
				AppID: "cli_test", AppSecret: "secret_test",
			}}, nil
		},
		openRegistry: func(*config.Config) (*registry.File, error) { return nil, nil },
		buildAgents:  func(*config.Config) *agent.Registry { return agents },
		newChannel:   func(*config.Config) (channel.Channel, error) { return ch, nil },
		newManager: func(agents *agent.Registry, reg *registry.File, cb session.EventCallback) session.Manager {
			return mgr
		},
		newGateway: func(mgr session.Manager, reg *agent.Registry, resp gatewaycmd.Responder) gateway.Gateway {
			gw := gateway.New(nil)
			gatewaycmd.RegisterDefaultCommands(gw, mgr, reg, resp)
			return gw
		},
		signals: signals,
	}
}

// TestRun_ConsecutiveMessagesDoNotDeadlock exercises the bug that
// motivated the SendUserMessage eviction fix: a follow-up user
// message arriving while the previous turn is still in-flight
// triggers an eviction path that, before the fix, self-deadlocked
// the dispatchLoop goroutine.
//
// With /bin/cat as the agent and a recordingChannel capturing
// replies, we send three messages back-to-back without sleeping
// between them. Each message becomes a real receipt on the
// recordingChannel via Channel.Send; the eviction path runs
// because each new receipt displaces the still-active previous
// receipt. With the pre-fix code the third message hangs; with
// the post-fix code all three messages produce replies within
// the deadline.
//
// This test deliberately uses the recordingChannel (a fake
// channel.Channel) rather than the Feishu adapter so the failure
// mode is unambiguous: if it deadlocks here, the deadlock lives
// in the gateway/session/bridge abstraction — not in the
// Feishu-specific SendUserMessage eviction.
func TestRun_ConsecutiveMessagesDoNotDeadlock(t *testing.T) {
	ch := newRecordingChannel()
	signals := make(chan os.Signal, 1)

	// Three messages fired in quick succession. No sleep between
	// them: the receipt for msg1 is still StateWaiting or
	// StateExecuting when msg2 lands, and the receipt for msg2
	// is in the same state when msg3 lands — the exact eviction
	// path the deadlock fix targets.
	const messages = 3
	go func() {
		<-ch.started
		ch.incoming <- channel.Message{ChatID: "oc_chat", Text: "/cwd /tmp"}
		time.Sleep(20 * time.Millisecond)
		ch.incoming <- channel.Message{ChatID: "oc_chat", Text: "/run echo"}
		time.Sleep(20 * time.Millisecond)
		for i := 0; i < messages; i++ {
			ch.incoming <- channel.Message{
				ChatID: "oc_chat",
				Text:   "msg-" + string(rune('a'+i)),
			}
		}
		// Give the events time to flow through the bridge and
		// reach channel.Send, but with a generous deadline so
		// the test fails loud and clear if anything deadlocks.
		time.Sleep(500 * time.Millisecond)
		signals <- syscall.SIGTERM
	}()

	var out bytes.Buffer
	cmd := newRunCmd()
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())

	done := make(chan error, 1)
	go func() { done <- runRunWith(cmd, echoIntegrationDeps(t, ch, signals)) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runRunWith: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runRunWith deadlocked: consecutive user messages should not block the dispatch loop")
	}

	// Verify the gateway replied to the slash commands AND each
	// of the three user messages produced at least one outbound
	// Send on the channel — the receipt-rendering path ran for
	// every message without deadlocking.
	ch.mu.Lock()
	replies := append([]string(nil), ch.replies...)
	ch.mu.Unlock()

	wantSlashReplies := []string{"Workspace set", "running"}
	for _, want := range wantSlashReplies {
		found := false
		for _, r := range replies {
			if strings.Contains(r, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing slash-command reply containing %q in %v", want, replies)
		}
	}

	// We don't assert a fixed count here (the bridge may produce
	// multiple outbound messages per user turn depending on the
	// receipt strategy); we assert at least `messages` outbound
	// Send calls happened AFTER /run echo, which is the
	// fingerprint of the user messages reaching the channel.
	if len(replies) < 2+messages {
		t.Errorf("replies = %d, want at least %d (slash replies + per-message outbound Sends): %v",
			len(replies), 2+messages, replies)
	}
}
