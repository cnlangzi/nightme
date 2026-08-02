package gateway

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/receipt"
)

// fakeChannel records every Send call; safe for concurrent use.
type fakeChannel struct {
	mu      sync.Mutex
	sends   []OutboundMessage
	receipts []string
}

func (c *fakeChannel) Name() string { return "fake" }
func (c *fakeChannel) Incoming() <-chan InboundMessage {
	return make(<-chan InboundMessage) // never produces; tests feed via direct Handle calls
}

func (c *fakeChannel) Send(_ context.Context, m OutboundMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sends = append(c.sends, m)
	return nil
}

func (c *fakeChannel) CreateReceipt(_ context.Context, _ string, userMsgID string, _ []agent.ContentBlock) (receipt.Receipt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.receipts = append(c.receipts, userMsgID)
	return receipt.Receipt("rcpt_" + userMsgID), nil
}

func (c *fakeChannel) UpdateReceipt(_ context.Context, _ receipt.Receipt, _ receipt.ReceiptState) error {
	return nil
}

func (c *fakeChannel) DisposeReceipt(_ context.Context, _ receipt.Receipt) error {
	return nil
}

func (c *fakeChannel) LastText() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.sends) == 0 {
		return ""
	}
	return c.sends[len(c.sends)-1].Text
}

func (c *fakeChannel) AllTexts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.sends))
	for i, m := range c.sends {
		out[i] = m.Text
	}
	return out
}

// fakeAgentSession is a minimal agent.AgentSession for handler tests.
// (Same shape as chatsession/spawn_test.go's fake; duplicated here
// to keep the gateway tests independent of the chatsession test
// internals.)
type fakeAgentSession struct {
	mu     sync.Mutex
	pid    int
	events chan agent.AgentEvent
}

func newFakeAgentSession(pid int) *fakeAgentSession {
	return &fakeAgentSession{pid: pid, events: make(chan agent.AgentEvent, 16)}
}

func (f *fakeAgentSession) Events() <-chan agent.AgentEvent { return f.events }
func (f *fakeAgentSession) PID() int                      { return f.pid }
func (f *fakeAgentSession) SendText(_ string) error       { return nil }
func (f *fakeAgentSession) SendBlocks(_ context.Context, _ []agent.ContentBlock) error {
	return nil
}
func (f *fakeAgentSession) SendPermission(_ string) error  { return nil }
func (f *fakeAgentSession) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	select {
	case <-f.events:
	default:
	}
	close(f.events)
	return nil
}

// fakeSpawner wraps chatsession.Spawner; implementation lives in
// chatsession package. We reuse it via a thin wrapper.
type fakeSpawnerAdapter struct {
	f *fakeSpawner
}

func (s *fakeSpawnerAdapter) Spawn(_ context.Context, name, cwd string, args []string) (agent.AgentSession, error) {
	return s.f.SpawnChatsession(name, cwd, args)
}

// fakeSpawner mirrors chatsession.fakeSpawner but is defined here
// to avoid an import cycle (chatsession tests would need to import
// gateway). Same logic, separate type.
type fakeSpawner struct {
	mu sync.Mutex
	n  int
}

func (s *fakeSpawner) SpawnChatsession(name, cwd string, args []string) (agent.AgentSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	return newFakeAgentSession(20000 + s.n), nil
}

func newTestManager(t *testing.T, withSpawner bool) (*chatsession.Manager, *fakeChannel) {
	t.Helper()
	mgr := chatsession.NewManager()
	if withSpawner {
		// Register fakeSpawner with chatsession.Manager.
		// We need a chatsession.Spawner, not gateway's adapter.
		// chatsession.fakeSpawner is unexported; use a thin inline.
		mgr.WithSpawner(testSpawner{})
	}
	ch := &fakeChannel{}
	return mgr, ch
}

// testSpawner wraps fakeAgentSession spawning into the chatsession.Spawner interface.
type testSpawner struct{}

func (testSpawner) Spawn(_ context.Context, name, cwd string, args []string) (agent.AgentSession, error) {
	static := newFakeAgentSession(30000 + len(name))
	return static, nil
}

// --- Tests ---

func TestHandleCwd_SetsActiveCwd(t *testing.T) {
	mgr, ch := newTestManager(t, false)
	msg := &InboundMessage{ChatID: "oc_xxx", ChatType: ChatTypeP2P, Text: "/cwd /code/bailing"}

	res, err := handleCwd(context.Background(), mgr, ch, msg, []string{"/code/bailing"}, "claude")
	if err != nil {
		t.Fatalf("handleCwd: %v", err)
	}
	if !res.Consumed {
		t.Fatalf("expected Consumed=true")
	}
	if !strings.Contains(ch.LastText(), "Workspace set to /code/bailing") {
		t.Fatalf("reply: %q", ch.LastText())
	}

	cs := mgr.Get("oc_xxx")
	if cs == nil {
		t.Fatalf("ChatSession should exist")
	}
	if cs.ActiveCwd() != "/code/bailing" {
		t.Fatalf("ActiveCwd: %q", cs.ActiveCwd())
	}
}

func TestHandleCwd_NoArg_RepliesUsage(t *testing.T) {
	mgr, ch := newTestManager(t, false)
	res, err := handleCwd(context.Background(), mgr, ch, &InboundMessage{ChatID: "oc_xxx"}, nil, "claude")
	if err != nil {
		t.Fatalf("handleCwd: %v", err)
	}
	if !res.Consumed {
		t.Fatalf("expected Consumed=true")
	}
	if !strings.Contains(ch.LastText(), "Usage") {
		t.Fatalf("reply: %q", ch.LastText())
	}
}

func TestHandleUse_NoActiveCwd_RepliesError(t *testing.T) {
	mgr, ch := newTestManager(t, true)
	res, err := handleUse(context.Background(), mgr, ch,
		&InboundMessage{ChatID: "oc_xxx", ChatType: ChatTypeP2P},
		[]string{"claude"}, "claude")
	if err != nil {
		t.Fatalf("handleUse: %v", err)
	}
	if !res.Consumed {
		t.Fatalf("expected Consumed=true")
	}
	if !strings.Contains(ch.LastText(), "/cwd") {
		t.Fatalf("reply should mention /cwd: %q", ch.LastText())
	}
}

func TestHandleUse_LazySpawn(t *testing.T) {
	mgr, ch := newTestManager(t, true)
	cs := mgr.GetOrCreate("oc_xxx", "p2p", "claude")
	cs.SetActiveCwd("/code/bailing")

	res, err := handleUse(context.Background(), mgr, ch,
		&InboundMessage{ChatID: "oc_xxx", ChatType: ChatTypeP2P},
		[]string{"claude"}, "claude")
	if err != nil {
		t.Fatalf("handleUse: %v", err)
	}
	if !res.Consumed {
		t.Fatalf("expected Consumed=true")
	}
	if !strings.Contains(ch.LastText(), "Now using claude") {
		t.Fatalf("reply: %q", ch.LastText())
	}
	if as := cs.ActiveAgentSession(); as == nil {
		t.Fatalf("expected active AgentSession")
	} else if as.Status() != chatsession.StatusRunning {
		t.Fatalf("expected Running after /use, got %q", as.Status())
	}
}

func TestHandleUse_NoArg_RepliesUsage(t *testing.T) {
	mgr, ch := newTestManager(t, false)
	res, err := handleUse(context.Background(), mgr, ch,
		&InboundMessage{ChatID: "oc_xxx", ChatType: ChatTypeP2P},
		nil, "claude")
	if err != nil {
		t.Fatalf("handleUse: %v", err)
	}
	if !res.Consumed {
		t.Fatalf("expected Consumed=true")
	}
	if !strings.Contains(ch.LastText(), "Usage") {
		t.Fatalf("reply: %q", ch.LastText())
	}
}

func TestHandleKill_NoChatSession_RepliesCalmly(t *testing.T) {
	mgr, ch := newTestManager(t, false)
	res, err := handleKill(context.Background(), mgr, ch,
		&InboundMessage{ChatID: "oc_unknown", ChatType: ChatTypeP2P})
	if err != nil {
		t.Fatalf("handleKill: %v", err)
	}
	if !res.Consumed {
		t.Fatalf("expected Consumed=true")
	}
	if !strings.Contains(ch.LastText(), "No active") {
		t.Fatalf("reply: %q", ch.LastText())
	}
}

func TestHandleKill_ClearsPool(t *testing.T) {
	mgr, ch := newTestManager(t, true)
	cs := mgr.GetOrCreate("oc_xxx", "p2p", "claude")
	cs.SetActiveCwd("/code/bailing")
	cs.SetActiveAgent("claude")
	cs.LookupActiveAgentSession() // spawn
	cs.LookupActiveAgentSession() // second lookup same agent

	if len(cs.Pool()) == 0 {
		t.Fatalf("precondition: pool should have entries")
	}

	res, err := handleKill(context.Background(), mgr, ch,
		&InboundMessage{ChatID: "oc_xxx", ChatType: ChatTypeP2P})
	if err != nil {
		t.Fatalf("handleKill: %v", err)
	}
	if !res.Consumed {
		t.Fatalf("expected Consumed=true")
	}
	if !strings.Contains(ch.LastText(), "Killed") {
		t.Fatalf("reply: %q", ch.LastText())
	}
	if len(cs.Pool()) != 0 {
		t.Fatalf("pool should be empty after kill, got %d", len(cs.Pool()))
	}
	// activeCwd and activeAgent survive /kill.
	if cs.ActiveCwd() != "/code/bailing" {
		t.Fatalf("ActiveCwd should survive /kill, got %q", cs.ActiveCwd())
	}
}

func TestRegisterChatSessionCommands_RegistersAllThree(t *testing.T) {
	mgr, ch := newTestManager(t, false)
	gw := New(nil, nil)
	RegisterChatSessionCommands(gw, mgr, ch, "claude")

	for _, name := range []string{"/cwd", "/use", "/kill"} {
		found := false
		for _, c := range gw.ListCommands() {
			if c.Name == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("command %s not registered", name)
		}
	}
}

// Compile-time guard: handleUse / handleCwd / handleKill are
// consumed by RegisterChatSessionCommands. Pinning the references
// here so future refactors don't accidentally drop them.
var (
	_ = handleCwd
	_ = handleUse
	_ = handleKill
)