//go:build windows

// Real-pi AS-reuse verification on Windows.
//
// Drives the full chatsession pipeline (Manager → ChatSession →
// HandleInbound → Spawn → bridge.SendBlocks → pi stdin →
// readPump → events) for N messages and asserts:
//
//  1. The Spawner is invoked exactly once across N messages
//     (one CLI process is reused).
//  2. The OS PID of the underlying pi process is identical
//     before message 1 and after message N (the process
//     never died and was not respawned).
//  3. The AS status stays Running throughout.
//
// Skipped when `pi` is not on PATH. Run explicitly:
//
//	go test ./internal/chatsession -run TestAS_ReuseAcrossMessages_RealPi -v
package chatsession

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	pibridge "github.com/cnlangzi/nightme/internal/bridge/pi"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/registry"
)

func TestAS_ReuseAcrossMessages_RealPi(t *testing.T) {
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skipf("pi not on PATH: %v", err)
	}
	// Quiet pi's own debug logging — we only care about the
	// runtime's view here.
	os.Setenv("NIGHTME_PI_DEBUG", "0")

	dir := t.TempDir()
	csFile, err := registry.OpenChatSessionFile(dir + "/chat_sessions.json")
	if err != nil {
		t.Fatalf("OpenChatSessionFile: %v", err)
	}
	asFile, err := registry.OpenAgentSessionFile(dir + "/agent_sessions.json")
	if err != nil {
		t.Fatalf("OpenAgentSessionFile: %v", err)
	}

	// Wire a real spawner (pi registry entry) wrapped in a
	// counter so we can assert "Spawn called once".
	piStarter := pibridge.NewStarter("pi", "pi", nil)
	reg := agent.New()
	reg.Register(piStarter)
	innerSpawner := NewRegistrySpawner(reg)
	spy := &countSpawner{inner: innerSpawner}

	mgr := NewManager()
	mgr.WithSpawner(spy)
	cs, _ := mgr.GetOrCreate("oc_real_reuse", "pi")
	cs = cs.WithPersistence(csFile, asFile)
	cs = cs.WithSpawner(spy) // also wire directly so cs.spawner != nil
	if err := cs.SetSelectedCwd(dir); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	cs.SetWatchMode(WatchModeAll)

	// Subscribe to events so the readpump has a consumer and
	// the bridge process stays healthy across messages.
	var (
		mu        sync.Mutex
		seenPIDs  []int
		readyOnce sync.Once
	)
	cs.AgentEventBus.Subscribe(func(env AgentEventEnvelope) bool {
		if env.Event.Kind == agent.EventAgentReady {
			pid := env.AgentSession.PID()
			mu.Lock()
			seenPIDs = append(seenPIDs, pid)
			mu.Unlock()
			readyOnce.Do(func() {})
		}
		return false
	})

	// PumpEvents drives enriched events from the readpump
	// queue onto the EventHandler. The runtime installs this
	// in production; we do the same here.
	pumpCtx, pumpCancel := context.WithCancel(context.Background())
	defer pumpCancel()
	go cs.PumpEvents(pumpCtx)

	// Drop a few harmless "ping" messages and assert the
	// Spawner count + AS pid stay constant. We don't need
	// the agent to fully process each one — we just need
	// each HandleInbound to traverse LookupSelectedAgentSession
	// and observe that it short-circuits.
	const N = 4
	for i := 0; i < N; i++ {
		mgr.HandleInbound(context.Background(), &messages.InboundMessage{
			ChatID:    "oc_real_reuse",
			MessageID: "om_real_reuse_" + itoa(i),
			UserID:    "u_real_reuse",
			Text:      "ping",
		})
	}

	// Give the readpump a moment to surface the EventAgentReady
	// from the first Spawn so we can record the pid.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := len(seenPIDs)
		mu.Unlock()
		if got >= 1 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	mu.Lock()
	if len(seenPIDs) == 0 {
		mu.Unlock()
		t.Fatalf("never received EventAgentReady from pi within 20s — Spawn may have failed")
	}
	firstPID := seenPIDs[0]
	mu.Unlock()
	t.Logf("first EventAgentReady pid=%d", firstPID)

	// Now read the AS pid from the runtime and compare. Both
	// must agree — and both must equal firstPID.
	as := cs.SelectedAgentSession()
	if as == nil {
		t.Fatalf("no selected AS after HandleInbound")
	}
	if as.PID() != firstPID {
		t.Errorf("AS.PID=%d, EventAgentReady pid=%d (mismatch — readpump is reading a different handle)",
			as.PID(), firstPID)
	}
	if as.Status() != StatusRunning {
		t.Errorf("AS status=%s, want %s after %d messages", as.Status(), StatusRunning, N)
	}

	// Core assertion: across N HandleInbound calls, the
	// Spawner fired ONCE — proving the runtime reuses the
	// existing AS.
	if got := spy.calls.Load(); got != 1 {
		t.Errorf("Spawn called %d times across %d messages; want 1 (AS single-process reuse violated)", got, N)
	}

	// Final pid check: after N messages, the OS pid is still
	// the original one.
	if as.PID() != firstPID {
		t.Errorf("pid drifted across messages: first=%d now=%d", firstPID, as.PID())
	}

	// Cleanup.
	snapshot := cs.AgentSessionsInCwd(dir)
	for _, as := range snapshot {
		_ = as.Close()
		cs.DropAgentSession(as)
	}
}
