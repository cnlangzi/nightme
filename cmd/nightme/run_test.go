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
func (f *fakeRunManager) CreateOrUpdate(string, string, string, []string) (*session.Session, error) {
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
		signals:     signals,
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

func TestBuildRunAgentRegistry_UsesArgsAndEnv(t *testing.T) {
	cfg := &config.Config{
		Agent: config.AgentConfig{Agents: map[string]config.AgentEntry{
			"claude": {Command: "/bin/echo", Args: []string{"--flag"}, Env: map[string]string{"Z": "last", "A": "first"}},
		}},
		Session: config.SessionConfig{DefaultPtyCols: 100, DefaultPtyRows: 40},
	}
	reg := buildRunAgentRegistry(cfg)
	a, err := reg.Get("claude")
	if err != nil {
		t.Fatalf("Get(claude): %v", err)
	}
	pty, ok := a.(*ptyagent.Agent)
	if !ok {
		t.Fatalf("agent type = %T, want *ptyagent.Agent", a)
	}
	if pty.Name() != "claude" || pty.Command != "/bin/echo" {
		t.Fatalf("agent = name %q command %q", pty.Name(), pty.Command)
	}
	if len(pty.Args) != 1 || pty.Args[0] != "--flag" {
		t.Errorf("args = %v, want [--flag]", pty.Args)
	}
	if len(pty.Env) != 2 || pty.Env[0] != "A=first" || pty.Env[1] != "Z=last" {
		t.Errorf("env = %v, want sorted values", pty.Env)
	}
	if pty.Cols != 100 || pty.Rows != 40 {
		t.Errorf("pty size = %dx%d, want 100x40", pty.Cols, pty.Rows)
	}
}

// Keep the compile-time contract visible to this package's tests without
// depending on an implementation-specific manager method set.
var _ session.Manager = (*fakeRunManager)(nil)
