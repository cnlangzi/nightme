package chatsession

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentsession/agentstest"
	"github.com/cnlangzi/nightme/internal/registry"
)

// discardLogger keeps the reaper's records out of the test output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// deadPID is a PID that cannot exist on the host: the kernel's PID
// space is far smaller, so kill(pid, 0) reports ESRCH and the
// reaper sees the CLI as gone.
const deadPID = 999_999_999

// reaperAS builds an AgentSession that looks like a live CLI to the
// reaper: status running, a PID, and a bridge handle.
//
// pid decides the liveness the reaper observes: pass os.Getpid()
// for a live CLI, deadPID for one that is already gone.
func reaperAS(t *testing.T, pool *AgentSessionPool, chatID, cwd string, pid int) *AgentSession {
	t.Helper()
	as := NewAgentSession(newAgentSessionID(), chatID, "claude", cwd, nil)
	handle := newFakeAgentSession(pid).buildLive()
	agentstest.SetHandle(as, handle)
	as.SetRunning(pid)
	pool.Put(chatID, as)
	t.Cleanup(func() { as.Shutdown() })
	return as
}

// reaperAt builds a reaper whose clock is offset into the future by
// the given duration. The offset makes an AS that has been silent
// since its spawn look idle, without waiting an hour for it.
func reaperAt(pool *AgentSessionPool, offset time.Duration) *AgentReaper {
	r := NewAgentReaper(pool, discardLogger())
	r.now = func() time.Time { return time.Now().Add(offset) }
	return r
}

func TestReaper_SkipsSessionsThatAreNotRunning(t *testing.T) {
	pool := NewAgentSessionPool()

	notRunning := NewAgentSession(newAgentSessionID(), "chat_a", "claude", "/tmp/a", nil)
	agentstest.SetHandle(notRunning, newFakeAgentSession(os.Getpid()).buildLive())
	agentstest.SetStatus(notRunning, StatusExited)
	agentstest.SetPID(notRunning, os.Getpid())
	pool.Put("chat_a", notRunning)

	noPID := NewAgentSession(newAgentSessionID(), "chat_b", "claude", "/tmp/b", nil)
	agentstest.SetHandle(noPID, newFakeAgentSession(os.Getpid()).buildLive())
	agentstest.SetStatus(noPID, StatusRunning)
	agentstest.SetPID(noPID, 0)
	pool.Put("chat_b", noPID)

	noHandle := NewAgentSession(newAgentSessionID(), "chat_c", "claude", "/tmp/c", nil)
	agentstest.SetStatus(noHandle, StatusRunning)
	agentstest.SetPID(noHandle, os.Getpid())
	pool.Put("chat_c", noHandle)

	r := reaperAt(pool, 2*time.Hour)
	r.tick()

	if got := r.Snapshot().RespawnsHit; got != 0 {
		t.Errorf("RespawnsHit = %d, want 0 (nothing qualified for reclamation)", got)
	}
	if got := notRunning.Status(); got != StatusExited {
		t.Errorf("not-running session status = %s, want %s", got, StatusExited)
	}
	if got := noPID.Status(); got != StatusRunning {
		t.Errorf("pid-less session status = %s, want %s (must be left alone)", got, StatusRunning)
	}
	if got := noHandle.Status(); got != StatusRunning {
		t.Errorf("handle-less session status = %s, want %s (must be left alone)", got, StatusRunning)
	}
}

func TestReaper_SkipsFreshSession(t *testing.T) {
	pool := NewAgentSessionPool()
	as := reaperAS(t, pool, "chat_a", "/tmp/a", os.Getpid())

	r := reaperAt(pool, time.Minute)
	r.tick()

	if got := as.Status(); got != StatusRunning {
		t.Errorf("status = %s, want %s (a CLI that just talked is not idle)", got, StatusRunning)
	}
	if got := r.Snapshot().RespawnsHit; got != 0 {
		t.Errorf("RespawnsHit = %d, want 0", got)
	}
}

func TestReaper_SkipsSessionWithTurnInFlight(t *testing.T) {
	pool := NewAgentSessionPool()
	as := reaperAS(t, pool, "chat_a", "/tmp/a", os.Getpid())
	agentstest.SetIsReady(as, false)

	r := reaperAt(pool, 2*time.Hour)
	r.tick()

	if got := as.Status(); got != StatusRunning {
		t.Errorf("status = %s, want %s (a CLI working on a turn must not be killed)", got, StatusRunning)
	}
	if got := r.Snapshot().RespawnsHit; got != 0 {
		t.Errorf("RespawnsHit = %d, want 0", got)
	}
}

func TestReaper_ReclaimsIdleCLI(t *testing.T) {
	pool := NewAgentSessionPool()
	as := reaperAS(t, pool, "chat_a", "/tmp/a", os.Getpid())
	as.SetSessionID("session-abc")

	r := reaperAt(pool, 2*time.Hour)
	r.tick()

	if got := as.Status(); got != StatusExited {
		t.Errorf("status = %s, want %s", got, StatusExited)
	}
	if got := as.PID(); got != 0 {
		t.Errorf("pid = %d, want 0", got)
	}
	code := as.ExitCode()
	if code == nil {
		t.Fatal("exit code is nil, want the reaper's termination code")
	}
	if *code != registry.ExitCodeTerminatedByNightme {
		t.Errorf("exit code = %d, want %d", *code, registry.ExitCodeTerminatedByNightme)
	}
	if got := r.Snapshot().RespawnsHit; got != 1 {
		t.Errorf("RespawnsHit = %d, want 1", got)
	}

	// The session identity survives: the next user message resumes
	// the CLI with the same id.
	if got := as.SessionID(); got != "session-abc" {
		t.Errorf("SessionID = %q, want %q", got, "session-abc")
	}
	if pool.FindByID(as.ID) == nil {
		t.Error("the reclaimed session left the pool")
	}
}

func TestReaper_ReclaimsDeadProcess(t *testing.T) {
	pool := NewAgentSessionPool()
	as := reaperAS(t, pool, "chat_a", "/tmp/a", deadPID)

	r := reaperAt(pool, time.Minute)
	r.tick()

	if got := as.Status(); got != StatusExited {
		t.Errorf("status = %s, want %s", got, StatusExited)
	}
	code := as.ExitCode()
	if code == nil {
		t.Fatal("exit code is nil, want 0")
	}
	if *code != 0 {
		t.Errorf("exit code = %d, want 0 (the CLI exited on its own)", *code)
	}
	if got := r.Snapshot().RespawnsHit; got != 1 {
		t.Errorf("RespawnsHit = %d, want 1", got)
	}
}

// closeBarrier releases a fixed number of waiters only once all of
// them have arrived.
type closeBarrier struct {
	mu      sync.Mutex
	count   int
	want    int
	arrived chan struct{}
	release chan struct{}
}

func newCloseBarrier(want int) *closeBarrier {
	return &closeBarrier{
		want:    want,
		arrived: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (b *closeBarrier) enter() {
	b.mu.Lock()
	b.count++
	if b.count == b.want {
		close(b.arrived)
	}
	b.mu.Unlock()
	<-b.release
}

// barrierDriver is a fake bridge driver whose Close blocks on a
// shared barrier — the reaper can only get every Close past the
// barrier if it closes the batch concurrently.
type barrierDriver struct {
	barrier *closeBarrier
}

func (d *barrierDriver) SendBlocks(context.Context, []agent.ContentBlock) error { return nil }
func (d *barrierDriver) SendPermission(string) error                            { return nil }
func (d *barrierDriver) Reset(context.Context) error                            { return nil }
func (d *barrierDriver) Stop(context.Context) error                             { return agent.ErrNotSupported }
func (d *barrierDriver) Keepalive(context.Context, func(context.Context) error) error {
	return nil
}

func (d *barrierDriver) Close() error {
	d.barrier.enter()
	return nil
}

func TestReaper_ClosesCandidatesConcurrently(t *testing.T) {
	const n = 3

	pool := NewAgentSessionPool()
	barrier := newCloseBarrier(n)
	for i := range n {
		as := NewAgentSession(newAgentSessionID(), "chat_a", "claude", fmt.Sprintf("/tmp/a%d", i), nil)
		handle := agent.NewAgent(
			agent.NewInfo("fake", agent.ModePTY, "fake", nil, nil),
			deadPID,
			make(chan agent.AgentEvent, 1),
			&barrierDriver{barrier: barrier},
		)
		agentstest.SetHandle(as, handle)
		as.SetRunning(deadPID)
		pool.Put("chat_a", as)
		t.Cleanup(func() { as.Shutdown() })
	}

	r := reaperAt(pool, time.Minute)
	done := make(chan struct{})
	go func() {
		r.tick()
		close(done)
	}()

	select {
	case <-barrier.arrived:
		// Every candidate entered Close before any of them
		// returned — the batch ran concurrently.
	case <-time.After(5 * time.Second):
		close(barrier.release)
		t.Fatal("the closes did not overlap: only some candidates entered Close")
	}
	close(barrier.release)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("tick did not return")
	}
	if got := r.Snapshot().RespawnsHit; got != n {
		t.Errorf("RespawnsHit = %d, want %d", got, n)
	}
}

func TestReaper_StartStopAreIdempotent(t *testing.T) {
	pool := NewAgentSessionPool()
	r := NewAgentReaper(pool, discardLogger())

	if r.Start() != true {
		t.Fatal("Start returned false on the first call")
	}
	if r.Start() != false {
		t.Error("Start returned true on the second call")
	}
	if !r.Snapshot().Active {
		t.Error("Active = false while the reaper is running")
	}

	r.Stop()
	r.Stop()

	if r.Snapshot().Active {
		t.Error("Active = true after Stop")
	}
}
