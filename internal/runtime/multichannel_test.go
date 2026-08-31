// multichannel_test.go — v1.3+ multi-channel wire coverage.
//
// Exercises the 4-phase wire contract:
//  1. runtime.allMgrs lists every per-channel Manager
//  2. runtime.findChatSession routes chatIDs to the right mgr
//  3. buildStack creates a per-channel Manager + Emitter + Gateway
//     Pump with the per-channel Emitter bound
//
// The tests use a minimal stubChannel (channel.Channel
// implementation) so the wire tests don't depend on the
// real feishu / telegram adapters — that decoupling is what
// makes the OCP registry work for v1.3+ multi-channel.
package runtime

import (
	"github.com/cnlangzi/nightme/internal/chatstore"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentregistry"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/registry"
)

// stubChannel is a minimal channel.Channel used by the wire
// tests. It captures every Send call so tests can assert on
// the routing path; the Incoming chan is buffered so tests can
// Inject messages synchronously.
type stubChannel struct {
	name   string
	prefix string
	in     chan messages.InboundMessage
	sends  *capturedSends
}

func newStubChannel(name string, sends *capturedSends) *stubChannel {
	return &stubChannel{
		name:  name,
		in:    make(chan messages.InboundMessage, 8),
		sends: sends,
	}
}

func (s *stubChannel) Name() string                              { return s.name }
func (s *stubChannel) Start(_ context.Context) error             { return nil }
func (s *stubChannel) Stop(_ context.Context) error              { return nil }
func (s *stubChannel) Incoming() <-chan messages.InboundMessage { return s.in }
func (s *stubChannel) Inject(m messages.InboundMessage)        { s.in <- m }
func (s *stubChannel) Send(_ context.Context, _ messages.OutboundMessage) error {
	if s.sends != nil {
		s.sends.add()
	}
	return nil
}
func (s *stubChannel) Patch(_ context.Context, _ messages.OutboundMessage) error { return nil }
func (s *stubChannel) OnPromptEnded(_ context.Context, _, _ string)             {}
func (s *stubChannel) HealthSnapshot() (string, json.RawMessage, error) {
	return s.name, json.RawMessage("{}"), nil
}
func (s *stubChannel) SetLogger(_ *slog.Logger) {}
func (s *stubChannel) ChatIDPrefix() string         { return s.prefix }
func (s *stubChannel) BuildBlocks(_ string, _ []messages.Attachment) []agent.ContentBlock {
	return nil
}

// capturedSends is a thread-safe counter of channel.Send calls.
type capturedSends struct {
	mu sync.Mutex
	n  int
}

func (c *capturedSends) add()                   { c.mu.Lock(); c.n++; c.mu.Unlock() }
func (c *capturedSends) count() int            { c.mu.Lock(); defer c.mu.Unlock(); return c.n }
func (c *capturedSends) reset()                { c.mu.Lock(); c.n = 0; c.mu.Unlock() }

// setupWire builds the minimal shared deps (spawner / csFile /
// asFile / buildStackOpts) that every multi-channel test needs.
// It does NOT clear allMgrs (callers must do that, in a
// t.Cleanup, to avoid cross-test bleed).
func setupWire(t *testing.T) (buildStackOpts, *capturedSends) {
	t.Helper()
	dir := t.TempDir()
	csFile, err := chatstore.New(filepath.Join(dir, "chat_sessions.json"))
	if err != nil {
		t.Fatalf("OpenChatSessionFile: %v", err)
	}
	asFile, err := registry.OpenAgentSessionFile(filepath.Join(dir, "agent_sessions.json"))
	if err != nil {
		t.Fatalf("OpenAgentSessionFile: %v", err)
	}
	sends := &capturedSends{}
	cfg := &config.Config{}
	cfg.Feishu.AppID = "test-appid"
	cfg.Feishu.AppSecret = "test-secret"
	agents := buildTestAgents(cfg)
	opts := buildStackOpts{
		spawner:       chatsession.NewRegistrySpawner(agents),
		csFile:        csFile,
		asFile:        asFile,
		primaryAgent:  "claude",
		gitStatusDeps: chatsession.GitStatusDeps{},
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return opts, sends
}

// buildTestAgents is a thin wrapper around agentregistry.Build
// (the public entrypoint the runtime uses for the production
// agent registry). Tests get a fresh registry per call.
func buildTestAgents(cfg *config.Config) *agent.Registry {
	return agentregistry.Build(cfg, "")
}

// clearAllMgrs is a test helper that empties the runtime's
// per-channel manager slice. Tests must call this in their
// setup to avoid bleed.
func clearAllMgrs(t *testing.T) {
	t.Helper()
	allMgrsMu.Lock()
	allMgrs = allMgrs[:0]
	allMgrsMu.Unlock()
	t.Cleanup(func() {
		allMgrsMu.Lock()
		allMgrs = allMgrs[:0]
		allMgrsMu.Unlock()
	})
}

// TestBuildStack_PerChannelMgr_RegisteredInAllMgrs verifies
// that buildStack registers each freshly-built Manager in
// runtime.allMgrs (so findChatSession can resolve cross-channel
// chatIDs).
func TestBuildStack_PerChannelMgr_RegisteredInAllMgrs(t *testing.T) {
	clearAllMgrs(t)
	opts, sends := setupWire(t)

	chA := newStubChannel("alpha", sends)
	chB := newStubChannel("beta", sends)

	pumpA, err := buildStack(chA, opts, registerMgrInAllMgrs)
	if err != nil {
		t.Fatalf("buildStack alpha: %v", err)
	}
	pumpB, err := buildStack(chB, opts, registerMgrInAllMgrs)
	if err != nil {
		t.Fatalf("buildStack beta: %v", err)
	}

	allMgrsMu.RLock()
	n := len(allMgrs)
	allMgrsMu.RUnlock()
	if n != 2 {
		t.Errorf("allMgrs has %d managers, want 2 (alpha + beta)", n)
	}

	if pumpA.Manager == pumpB.Manager {
		t.Error("pumpA.Manager == pumpB.Manager; buildStack must produce distinct per-channel Managers")
	}
	if pumpA.Channel != chA {
		t.Error("pumpA.Channel != chA")
	}
	if pumpB.Channel != chB {
		t.Error("pumpB.Channel != chB")
	}
}

// TestFindChatSession_RoutesChatToFirstMgr verifies the v1.3
// core contract: findChatSession walks every per-channel
// Manager and returns the one that owns the chatID. The first
// mgr to successfully GetOrCreate the chat becomes its owner.
func TestFindChatSession_RoutesChatToFirstMgr(t *testing.T) {
	clearAllMgrs(t)
	opts, _ := setupWire(t)

	chA := newStubChannel("alpha", nil)
	chB := newStubChannel("beta", nil)

	if _, err := buildStack(chA, opts, registerMgrInAllMgrs); err != nil {
		t.Fatalf("alpha: %v", err)
	}
	if _, err := buildStack(chB, opts, registerMgrInAllMgrs); err != nil {
		t.Fatalf("beta: %v", err)
	}

	allMgrsMu.RLock()
	defer allMgrsMu.RUnlock()
	if got := len(allMgrs); got != 2 {
		t.Fatalf("allMgrs has %d managers, want 2", got)
	}
	alpha := allMgrs[0]
	beta := allMgrs[1]

	// findChatSession is Get-only (docs/CHATSTORE.md); create via
	// the owning channel's Manager first.
	if _, err := alpha.GetOrCreate("shared_chat", "claude"); err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	cs := findChatSession("shared_chat", "claude")
	if cs == nil {
		t.Fatal("findChatSession: expected existing chat")
	}
	if alpha.Get("shared_chat") == nil {
		t.Error("alpha should own shared_chat")
	}
	if beta.Get("shared_chat") != nil {
		t.Error("beta should NOT own shared_chat (alpha claimed first)")
	}
}

// TestFindChatSession_NilIfNoMgrsRegistered verifies the trivial
// "no channels" failure mode.
func TestFindChatSession_NilIfNoMgrsRegistered(t *testing.T) {
	clearAllMgrs(t)
	if cs := findChatSession("any_chat", "primary"); cs != nil {
		t.Errorf("findChatSession with no mgrs: got %v, want nil", cs)
	}
}

// TestRegisterMgrInAllMgrs_Threadsafe is a smoke test for the
// concurrent-append path.
func TestRegisterMgrInAllMgrs_Threadsafe(t *testing.T) {
	clearAllMgrs(t)

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			m := chatsession.NewManager()
			registerMgrInAllMgrs(m)
		}()
	}
	wg.Wait()

	allMgrsMu.RLock()
	defer allMgrsMu.RUnlock()
	if got := len(allMgrs); got != n {
		t.Errorf("allMgrs has %d managers, want %d", got, n)
	}
}

// TestBuildStack_BindsPerChannelEmitter verifies that each
// mgr gets its own Emitter (the per-channel Emitter wired
// during buildStack). Without WithEmitter, every cs created
// via the mgr would have a nil emitter and outbound would be
// dropped silently.
func TestBuildStack_BindsPerChannelEmitter(t *testing.T) {
	clearAllMgrs(t)
	opts, _ := setupWire(t)

	chA := newStubChannel("alpha", nil)
	chB := newStubChannel("beta", nil)

	pumpA, err := buildStack(chA, opts, registerMgrInAllMgrs)
	if err != nil {
		t.Fatalf("alpha: %v", err)
	}
	pumpB, err := buildStack(chB, opts, registerMgrInAllMgrs)
	if err != nil {
		t.Fatalf("beta: %v", err)
	}

	// Each mgr must have its Emitter wired.
	if pumpA.Manager.Emitter() == nil {
		t.Error("pumpA.Manager.Emitter() == nil; buildStack must bind the per-channel Emitter")
	}
	if pumpB.Manager.Emitter() == nil {
		t.Error("pumpB.Manager.Emitter() == nil; buildStack must bind the per-channel Emitter")
	}

	// Different channels → different Emitter instances.
	if pumpA.Manager.Emitter() == pumpB.Manager.Emitter() {
		t.Error("pumpA.Manager.Emitter() == pumpB.Manager.Emitter(); each mgr must own its own Emitter")
	}
}

// TestBuildStack_PumpsCarryCorrectChannel verifies that the
// returned Pump entries carry the right (Channel, Manager)
// tuple so gateway.pumpOne closes over the per-channel mgr.
func TestBuildStack_PumpsCarryCorrectChannel(t *testing.T) {
	clearAllMgrs(t)
	opts, _ := setupWire(t)

	chA := newStubChannel("alpha", nil)
	pump, err := buildStack(chA, opts, registerMgrInAllMgrs)
	if err != nil {
		t.Fatalf("buildStack: %v", err)
	}
	if pump.Channel != chA {
		t.Error("Pump.Channel != stub channel")
	}
	if pump.Manager == nil {
		t.Error("Pump.Manager is nil")
	}
}

// suppress "imported and not used" warnings for packages that
// appear in test stubs.
var _ = context.Background
