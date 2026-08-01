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
	"github.com/cnlangzi/nightme/internal/agent/ptyagent"
	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/gateway"
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

func (f *fakeRunChannel) SendMessage(context.Context, string, string) error     { return nil }
func (f *fakeRunChannel) SendLongMessage(context.Context, string, string) error { return nil }
func (f *fakeRunChannel) Incoming() <-chan channel.Message                      { return f.incoming }

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
		newGateway: func(mgr session.Manager, reg *agent.Registry, resp gateway.Responder) gateway.Gateway {
			gw := gateway.New(nil)
			gateway.RegisterDefaultCommands(gw, mgr, reg, resp)
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
	if !strings.Contains(out.String(), "Feishu WebSocket connected") || !strings.Contains(out.String(), "received: hello") {
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

func TestBuildRunAgentRegistry_UsesModes(t *testing.T) {
	cfg := &config.Config{
		Agent: config.AgentConfig{Agents: map[string]config.AgentEntry{
			"claude":   {Command: "/bin/echo", Args: []string{"--flag"}},
			"codex":    {Command: "/bin/echo"},
			"opencode": {Command: "/bin/echo"},
			"custom":   {Command: "/bin/echo", Args: []string{"--custom"}, Env: map[string]string{"Z": "last", "A": "first"}},
		}},
		Session: config.SessionConfig{DefaultPtyCols: 100, DefaultPtyRows: 40},
	}
	reg := buildRunAgentRegistry(cfg)
	for name, want := range map[string]agent.Mode{
		"claude":   agent.ModeJSONIO,
		"codex":    agent.ModeACP,
		"opencode": agent.ModeACP,
		"custom":   agent.ModePTY,
	} {
		a, err := reg.Get(name)
		if err != nil {
			t.Fatalf("Get(%s): %v", name, err)
		}
		if got := a.Mode(); got != want {
			t.Errorf("%s mode = %s, want %s", name, got, want)
		}
	}

	custom, ok := mustAgent(reg, "custom").(*ptyagent.Agent)
	if !ok {
		t.Fatalf("custom agent type = %T, want *ptyagent.Agent", mustAgent(reg, "custom"))
	}
	args := custom.Args()
	if len(args) != 1 || args[0] != "--custom" {
		t.Errorf("custom args = %v", args)
	}
	if custom.Cols != 100 || custom.Rows != 40 {
		t.Errorf("custom PTY size = %dx%d, want 100x40", custom.Cols, custom.Rows)
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
	a := ptyagent.New("claude", "/bin/cat", nil, nil)
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
		newGateway: func(mgr session.Manager, reg *agent.Registry, resp gateway.Responder) gateway.Gateway {
			gw := gateway.New(nil)
			gateway.RegisterDefaultCommands(gw, mgr, reg, resp)
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
