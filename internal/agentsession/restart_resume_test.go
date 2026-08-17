// restart_resume_test.go — R1.5: verify RestartFromDeath re-submits
// in-flight blocks to the freshly-spawned bridge.
//
// Why this matters: dsh's session.fork only copies server-side
// history; it does NOT replay any in-flight turn. If the user's
// prompt was being processed when the old bridge died, the new
// bridge would otherwise sit idle (the forked session has the
// transcript but no follow-up prompt), the 5-min watchdog would
// SIGKILL it, and the user's work would silently vanish.
//
// This test pins the resubmit behavior so the bug stays fixed.
package agentsession

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/registry"
)

// recordingSendBlocksAS wraps a fakeAgentSession with a
// SendBlocks-recording driver. Pattern mirrors callRecordingAS
// from test_helpers_test.go so the test setup matches the rest
// of the package's fakes.
type recordingSendBlocksAS struct {
	*fakeAgentSession
	mu    sync.Mutex
	sent  [][]agent.ContentBlock
	inner *recordingSendBlocksDriver
}

type recordingSendBlocksDriver struct {
	inner *recordingSendBlocksAS
}

func (d *recordingSendBlocksDriver) SendBlocks(_ context.Context, b []agent.ContentBlock) error {
	d.inner.mu.Lock()
	cp := make([]agent.ContentBlock, len(b))
	copy(cp, b)
	d.inner.sent = append(d.inner.sent, cp)
	d.inner.mu.Unlock()
	return d.inner.fakeAgentSession.SendBlocks(context.Background(), b)
}
func (d *recordingSendBlocksDriver) SendPermission(string) error  { return nil }
func (d *recordingSendBlocksDriver) Reset(context.Context) error   { return d.inner.New(context.Background()) }
func (d *recordingSendBlocksDriver) Close() error                  { return d.inner.Close() }
func (d *recordingSendBlocksDriver) Stop(context.Context) error   { return nil }
func (d *recordingSendBlocksDriver) Keepalive(context.Context, func(context.Context) error) error {
	return nil
}

func (r *recordingSendBlocksAS) buildLive() *agent.Agent {
	r.inner = &recordingSendBlocksDriver{inner: r}
	return agent.NewAgent(
		agent.NewInfo("fake", agent.ModePTY, "fake", nil, nil),
		r.pid, r.events,
		r.inner,
	)
}

func (r *recordingSendBlocksAS) Sent() [][]agent.ContentBlock {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]agent.ContentBlock, len(r.sent))
	copy(out, r.sent)
	return out
}

// failingSendBlocksAS is a fakeAgentSession whose SendBlocks
// always returns an error — simulates the new bridge rejecting
// the resubmitted prompt.
type failingSendBlocksAS struct {
	*fakeAgentSession
}

func (f *failingSendBlocksAS) buildLive() *agent.Agent {
	return agent.NewAgent(
		agent.NewInfo("fake", agent.ModePTY, "fake", nil, nil),
		f.pid, f.events,
		&failingSendBlocksDriver{inner: f},
	)
}

type failingSendBlocksDriver struct{ inner *failingSendBlocksAS }

func (d *failingSendBlocksDriver) SendBlocks(_ context.Context, _ []agent.ContentBlock) error {
	return errResubmit
}
func (d *failingSendBlocksDriver) SendPermission(string) error  { return nil }
func (d *failingSendBlocksDriver) Reset(context.Context) error   { return nil }
func (d *failingSendBlocksDriver) Close() error                  { return nil }
func (d *failingSendBlocksDriver) Stop(context.Context) error   { return nil }
func (d *failingSendBlocksDriver) Keepalive(context.Context, func(context.Context) error) error {
	return nil
}

var errResubmit = &resubmitError{}

type resubmitError struct{}

func (*resubmitError) Error() string { return "resubmit: simulated bridge rejection" }

// TestRestartFromDeath_ResubmitsInFlightBlocks verifies the fix:
// when RestartFromDeath successfully spawns a fresh bridge, any
// blocks that were in flight when the old bridge died are sent
// to the new bridge via SendBlocks so the user's prompt continues
// being processed.
func TestRestartFromDeath_ResubmitsInFlightBlocks(t *testing.T) {
	// AS in the post-death shape: Exited, inFlightMessages holds
	// the user's prompt, sessionID preserved for --resume.
	as := NewAgentSession(newAgentSessionID(), "cs_test", "dsh", "/code", nil)
	as.SetSessionID("old-session-id")
	as.SetPersist(func(_ *registry.AgentSessionEntry) error { return nil })

	blocks := []agent.ContentBlock{
		{Type: agent.ContentText, Text: "继续"},
	}
	as.asMu.Lock()
	as.inFlightMessages = []registry.InFlightMessageRef{
		{
			ID:         "msg-1",
			Blocks:     blocks,
			ReceivedAt: time.Now(),
		},
	}
	as.stat = StatusExited
	as.pid = 9999 // pretend the old bridge died with this pid
	as.asMu.Unlock()

	// The new bridge: a fresh fakeAgentSession wrapped with a
	// SendBlocks-recording driver. fakeRestartSpawner hands it
	// back when respawn calls Spawn.
	newBridge := &recordingSendBlocksAS{fakeAgentSession: newFakeAgentSession(4242)}
	spawner := &fakeRestartSpawner{handle: newBridge.buildLive()}

	if err := as.RestartFromDeath(context.Background(), spawner); err != nil {
		t.Fatalf("RestartFromDeath: %v", err)
	}

	// The new bridge must be wired in.
	if got := as.Handle(); got == nil {
		t.Fatalf("Handle should be non-nil after RestartFromDeath")
	}
	if got := as.PID(); got != 4242 {
		t.Fatalf("PID = %d, want 4242 (new bridge)", got)
	}
	if got := as.Status(); got != StatusRunning {
		t.Fatalf("Status = %s, want StatusRunning", got)
	}

	// The fix: SendBlocks must have been called on the new bridge
	// with the same blocks that were in flight.
	sent := newBridge.Sent()
	if len(sent) != 1 {
		t.Fatalf("SendBlocks call count = %d, want 1 (the in-flight resubmit)", len(sent))
	}
	if len(sent[0]) != 1 || sent[0][0].Text != "继续" {
		t.Fatalf("resubmitted blocks = %+v, want [{text:继续}]", sent[0])
	}

	// isReady should be false after resubmit — TryFlush should
	// back off until the bridge emits KindPromptEnded.
	if as.IsReady() {
		t.Fatalf("IsReady = true after resubmit, want false (bridge busy)")
	}
}

// TestRestartFromDeath_NoInFlightNoResubmit pins the negative case:
// when nothing was in flight when the bridge died (clean crash
// mid-idle, or post-endPrompt), RestartFromDeath must NOT send a
// bogus empty SendBlocks.
func TestRestartFromDeath_NoInFlightNoResubmit(t *testing.T) {
	as := NewAgentSession(newAgentSessionID(), "cs_test", "dsh", "/code", nil)
	as.SetSessionID("old-session-id")
	as.SetPersist(func(_ *registry.AgentSessionEntry) error { return nil })
	// inFlightMessages intentionally left empty.

	newBridge := &recordingSendBlocksAS{fakeAgentSession: newFakeAgentSession(4242)}
	spawner := &fakeRestartSpawner{handle: newBridge.buildLive()}

	if err := as.RestartFromDeath(context.Background(), spawner); err != nil {
		t.Fatalf("RestartFromDeath: %v", err)
	}

	if sent := newBridge.Sent(); len(sent) != 0 {
		t.Fatalf("SendBlocks called %d times on empty inFlightMessages, want 0", len(sent))
	}
	if !as.IsReady() {
		t.Fatalf("IsReady = false with no resubmit, want true (idle bridge is ready for next user message)")
	}
}

// TestRestartFromDeath_ResubmitByteForByteEquality is the
// strictest contract check: the blocks the new bridge receives
// via SendBlocks must be the EXACT same blocks (text, image
// fields, order, count) that were in in-flight when the old
// bridge died. Catches regressions where the resubmit might
// drop fields, reorder blocks, or wrap content incorrectly.
func TestRestartFromDeath_ResubmitByteForByteEquality(t *testing.T) {
	as := NewAgentSession(newAgentSessionID(), "cs_test", "dsh", "/code", nil)
	as.SetSessionID("old-session-byte")
	as.SetPersist(func(_ *registry.AgentSessionEntry) error { return nil })

	original := []agent.ContentBlock{
		{Type: agent.ContentText, Text: "请继续 #1"},
		{Type: agent.ContentImage, Path: "/tmp/foo.png", MediaType: "image/png"},
		{Type: agent.ContentText, Text: "请继续 #3 — multi line\ndashed"},
		{Type: agent.ContentFile, Path: "/tmp/data.json", MediaType: "application/json"},
	}
	as.asMu.Lock()
	as.inFlightMessages = []registry.InFlightMessageRef{
		{ID: "msg-byte-1", Blocks: original, ReceivedAt: time.Now()},
	}
	as.stat = StatusExited
	as.pid = 11111
	as.asMu.Unlock()

	newBridge := &recordingSendBlocksAS{fakeAgentSession: newFakeAgentSession(7777)}
	spawner := &fakeRestartSpawner{handle: newBridge.buildLive()}

	if err := as.RestartFromDeath(context.Background(), spawner); err != nil {
		t.Fatalf("RestartFromDeath: %v", err)
	}

	sent := newBridge.Sent()
	if len(sent) != 1 {
		t.Fatalf("SendBlocks called %d times, want 1", len(sent))
	}
	got := sent[0]
	if len(got) != len(original) {
		t.Fatalf("resubmit block count = %d, want %d", len(got), len(original))
	}
	for i := range got {
		if got[i].Type != original[i].Type {
			t.Errorf("block %d: Type = %q, want %q", i, got[i].Type, original[i].Type)
		}
		if got[i].Text != original[i].Text {
			t.Errorf("block %d: Text = %q, want %q", i, got[i].Text, original[i].Text)
		}
		if got[i].Path != original[i].Path {
			t.Errorf("block %d: Path = %q, want %q", i, got[i].Path, original[i].Path)
		}
		if got[i].MediaType != original[i].MediaType {
			t.Errorf("block %d: MediaType = %q, want %q", i, got[i].MediaType, original[i].MediaType)
		}
	}

	// Concatenate text/path for a smoke summary.
	summary := func(bs []agent.ContentBlock) string {
		s := ""
		for _, b := range bs {
			s += "[" + string(b.Type) + ":" + b.Text + b.Path + "]"
		}
		return s
	}
	t.Logf("original: %s", summary(original))
	t.Logf("resubmit: %s", summary(got))
	t.Logf("BYTE-FOR-BYTE EQUAL across %d blocks (text+image+file mixed)", len(got))
}

// TestRestartFromDeath_ResubmitFailureDoesNotStall verifies that a
// failed SendBlocks on the new bridge restores isReady=true so the
// queue can drain the next message, instead of wedging the bridge
// in a not-ready state forever.
func TestRestartFromDeath_ResubmitFailureDoesNotStall(t *testing.T) {
	as := NewAgentSession(newAgentSessionID(), "cs_test", "dsh", "/code", nil)
	as.SetSessionID("old-session-id")
	as.SetPersist(func(_ *registry.AgentSessionEntry) error { return nil })
	as.asMu.Lock()
	as.inFlightMessages = []registry.InFlightMessageRef{
		{ID: "msg-1", Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "继续"}}, ReceivedAt: time.Now()},
	}
	as.stat = StatusExited
	as.asMu.Unlock()

	// Spawner returns a bridge whose SendBlocks always errors.
	failing := &failingSendBlocksAS{fakeAgentSession: newFakeAgentSession(4242)}
	spawner := &fakeRestartSpawner{handle: failing.buildLive()}

	if err := as.RestartFromDeath(context.Background(), spawner); err != nil {
		t.Fatalf("RestartFromDeath: %v (resubmit failure must NOT propagate)", err)
	}

	if !as.IsReady() {
		t.Fatalf("IsReady = false after resubmit failure, want true (queue must still drain)")
	}
}