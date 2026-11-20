package runtime

import (
	"io"
	"log/slog"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
)

// TestAgentReaper_ScansTheManagersPool pins the daemon wiring the
// idle-CLI reaper depends on: the reaper scans the daemon-wide
// AgentSessionPool, and the chat managers put their sessions into
// that same pool.
//
// A session parked by /cwd lives only in the daemon-wide pool —
// the chat's own working set no longer holds it — so a reaper
// pointed at any other source would never see the sessions worth
// reclaiming.
func TestAgentReaper_ScansTheManagersPool(t *testing.T) {
	clearAllMgrs(t)
	opts, sends := setupWire(t)
	opts.asPool = chatsession.NewAgentSessionPool()

	ch := newStubChannel("alpha", sends)
	pump, err := buildStack(ch, opts, registerMgrInAllMgrs)
	if err != nil {
		t.Fatalf("buildStack: %v", err)
	}
	mgr := pump.Manager
	if mgr == nil {
		t.Fatal("pump.Manager is nil")
	}

	if got := mgr.AgentSessionPool(); got != opts.asPool {
		t.Fatalf("manager pool = %p, want the daemon-wide pool %p", got, opts.asPool)
	}

	cs, err := mgr.GetOrCreate("chat-reaper", "claude")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	cs.SetSelectedCwd(t.TempDir())

	// The session lands in the pool even when the agent CLI is not
	// installed on this host — the spawn attempt is the bridge's
	// business, the pool membership is the chat layer's.
	_, _ = cs.LookupSelectedAgentSession()

	if len(opts.asPool.List()) == 0 {
		t.Fatal("the chat session did not land in the daemon-wide pool")
	}
}

// TestAgentReaper_StartStopDuringDaemonTeardown covers the
// reaper's lifecycle contract: Start launches it, Stop blocks until
// its goroutine is gone, and both are idempotent.
func TestAgentReaper_StartStopDuringDaemonTeardown(t *testing.T) {
	opts, _ := setupWire(t)
	opts.asPool = chatsession.NewAgentSessionPool()

	reaper := chatsession.NewAgentReaper(opts.asPool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !reaper.Start() {
		t.Fatal("Start returned false on the first call")
	}
	if reaper.Start() {
		t.Error("Start returned true on the second call")
	}
	if !reaper.Snapshot().Active {
		t.Error("Active = false while the reaper is running")
	}

	reaper.Stop()
	reaper.Stop()

	if reaper.Snapshot().Active {
		t.Error("Active = true after Stop")
	}
}
