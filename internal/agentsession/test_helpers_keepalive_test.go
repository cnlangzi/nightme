// Test helpers for keepalive_test.go.
package agentsession

import (
	"context"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// testKeepaliveDriver is a minimal driver that records
// SendBlocks calls AND implements Keepalive with the semantics
// the test wants. Keeps the agent package's driver interface
// minimal — no production code path uses this driver.
type testKeepaliveDriver struct {
	mu          sync.Mutex
	sent        int
	keepalivePID int             // PID that Keepalive reports alive for
	recoverErr  error           // if non-nil, Keepalive's onRecover returns this
	skipRecover bool            // if true, onRecover is nil (caller forgot to wire)
}

func (d *testKeepaliveDriver) SendBlocks(_ context.Context, _ []agent.ContentBlock) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sent++
	return nil
}
func (d *testKeepaliveDriver) SendPermission(_ string) error { return nil }
func (d *testKeepaliveDriver) Reset(_ context.Context) error { return nil }
func (d *testKeepaliveDriver) Close() error                  { return nil }
func (d *testKeepaliveDriver) Stop(_ context.Context) error  { return nil }

func (d *testKeepaliveDriver) Keepalive(ctx context.Context, onRecover func(context.Context) error) error {
	d.mu.Lock()
	keepalivePID := d.keepalivePID
	recoverErr := d.recoverErr
	skipRecover := d.skipRecover
	d.mu.Unlock()

	if keepalivePID <= 0 {
		// Simulate "dead" — invoke onRecover or surface error.
		if skipRecover || onRecover == nil {
			return errKeepaliveFailedForTest
		}
		if err := onRecover(ctx); err != nil {
			return err
		}
		if recoverErr != nil {
			return recoverErr
		}
		// Recovery "succeeded" — swap in a fresh live driver.
		// Caller will retry SendBlocks on the new driver.
	}
	return nil
}

// makeKeepaliveTestAS constructs an AS whose driver's Keepalive
// reports pid as alive. recovery is invoked if pid is dead and
// returns nil (so the second call sees a fresh, "live" driver
// via driver-swap below).
func makeKeepaliveTestAS(t *testing.T, pid int) (*AgentSession, *agent.Agent, *testKeepaliveDriver) {
	t.Helper()
	d := &testKeepaliveDriver{keepalivePID: pid}
	ag := agent.NewAgent(
		agent.NewInfo("test", agent.ModePTY, "test", nil, nil),
		pid,
		make(chan agent.AgentEvent, 8),
		d,
	)
	as := NewAgentSession(
		newAgentSessionID(),
		"cs_test",
		"claude",
		"/tmp",
		nil,
	)
	as.handle = ag
	as.pid = pid
	as.stat = StatusRunning
	as.isReady.Store(true)
	// Wire a spawner so Submit's recovery callback can run.
	// We use a spawner that returns the same Agent (so the test
	// harness's driver is reachable again after recovery).
	as.SetSpawner(testSpawnerReturning(ag))
	return as, ag, d
}

// makeKeepaliveTestASNoCallback constructs an AS whose driver
// reports dead but receives a nil onRecover callback. Submit
// must surface a wrapped error rather than silently writing
// to the dead handle.
func makeKeepaliveTestASNoCallback(t *testing.T, pid int) (*AgentSession, *agent.Agent, *testKeepaliveDriver) {
	t.Helper()
	d := &testKeepaliveDriver{keepalivePID: 0, skipRecover: true}
	ag := agent.NewAgent(
		agent.NewInfo("test", agent.ModePTY, "test", nil, nil),
		pid,
		make(chan agent.AgentEvent, 8),
		d,
	)
	as := NewAgentSession(
		newAgentSessionID(),
		"cs_test",
		"claude",
		"/tmp",
		nil,
	)
	as.handle = ag
	as.pid = pid
	as.stat = StatusRunning
	as.isReady.Store(true)
	as.SetSpawner(testSpawnerReturning(ag))
	return as, ag, d
}

// makeKeepaliveTestASFailingRecover constructs an AS whose
// driver's recovery callback returns an error. Submit must
// surface it.
func makeKeepaliveTestASFailingRecover(t *testing.T, pid int) (*AgentSession, *agent.Agent, *testKeepaliveDriver) {
	t.Helper()
	d := &testKeepaliveDriver{keepalivePID: 0, recoverErr: errRecoverFailedForTest}
	ag := agent.NewAgent(
		agent.NewInfo("test", agent.ModePTY, "test", nil, nil),
		pid,
		make(chan agent.AgentEvent, 8),
		d,
	)
	as := NewAgentSession(
		newAgentSessionID(),
		"cs_test",
		"claude",
		"/tmp",
		nil,
	)
	as.handle = ag
	as.pid = pid
	as.stat = StatusRunning
	as.isReady.Store(true)
	as.SetSpawner(testSpawnerReturning(ag))
	return as, ag, d
}

// testSpawnerReturning is a Spawner that returns the given
// pre-built *agent.Agent regardless of inputs. Used by the
// keepalive tests to short-circuit real bridge spawning.
type testSpawnerFixed struct {
	ag *agent.Agent
}

func (s testSpawnerFixed) Spawn(_ context.Context, _, _ string, _ []string, _ string) (*agent.Agent, error) {
	return s.ag, nil
}

func testSpawnerReturning(ag *agent.Agent) Spawner {
	return testSpawnerFixed{ag: ag}
}