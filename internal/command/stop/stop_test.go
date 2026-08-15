// Tests for the stop package's public API.
//
// These tests exercise stop.StopSelectedAgent / FormatStopResult
// through the ChatSession + AgentSession public surface only, with
// an in-memory bridge stub that records Stop calls. Mirrors the
// pattern in internal/command/close/close_test.go.
package stop_test

import (
	"github.com/cnlangzi/nightme/internal/messages"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentsession"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	stoppkg "github.com/cnlangzi/nightme/internal/command/stop"
)

// stubStoppable implements the agent.driver contract with the
// minimum surface required by AgentSession: SendBlocks /
// SendPermission / Reset / Close / Stop. Tests inject
// their own return values for Stop to drive the success / failure
// matrix.
type stubStoppable struct {
	stopErr error
	stopped int
}

func (s *stubStoppable) SendBlocks(_ context.Context, _ []agent.ContentBlock) error {
	return nil
}
func (s *stubStoppable) SendPermission(_ string) error { return nil }
func (s *stubStoppable) Reset(_ context.Context) error  { return nil }
func (s *stubStoppable) Close() error                   { return nil }
func (s *stubStoppable) Stop(_ context.Context) error {
	s.stopped++
	return s.stopErr
}

// nopCh satisfies outbound.Emitter for tests that need a
// non-nil channel to construct a ChatSession but don't exercise
// the channel surface.
type nopCh struct{}

func (nopCh) Send(_ context.Context, _ messages.OutboundMessage) error { return nil }
func (nopCh) SendCard(_ context.Context, _ messages.OutboundMessage) (string, error) {
	return "", nil
}
func (nopCh) Patch(_ context.Context, _ messages.OutboundMessage) error { return nil }

// setupSelectedAS wires up a ChatSession + selected AgentSession +
// stub driver, returning the stub so the test can drive Stop's
// return value and observe call counts.
func setupSelectedAS(t *testing.T, agentName, cwd string, isReady bool) (*chatsession.ChatSession, *agentsession.AgentSession, *stubStoppable) {
	t.Helper()
	mgr := chatsession.NewManager()
	cs, _ := mgr.GetOrCreate("c1", agentName)
	cs.WithPersistence(nil, nil) //nolint:revive // test setup
	if err := cs.SetSelectedCwd(cwd); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	if err := cs.SetSelectedAgent(agentName); err != nil {
		t.Fatalf("SetSelectedAgent: %v", err)
	}
	// Without WithSpawner, LookupSelectedAgentSession returns a
	// Detached AgentSession with no process — perfect for tests
	// that want to inject a stub handle via SetHandleForTest.
	as, err := cs.LookupSelectedAgentSession()
	if err != nil {
		t.Fatalf("LookupSelectedAgentSession: %v", err)
	}
	stub := &stubStoppable{}
	a := agent.NewAgent(agent.Info{
		Name:    agentName,
		Mode:    agent.ModeJSONIO,
		Command: "stub",
	}, 12345, make(chan agent.AgentEvent, 1), stub)
	as.SetHandleForTest(a)
	as.SetStatusForTest(agentsession.StatusRunning)
	as.SetIsReadyForTest(isReady)
	return cs, as, stub
}

// TestStopSelectedAgent_NilCS — stop.StopSelectedAgent with nil CS
// returns stop.ErrNoContext (defensive).
func TestStopSelectedAgent_NilCS(t *testing.T) {
	_, err := stoppkg.StopSelectedAgent(&stoppkg.Cmd{CS: nil, Ctx: context.Background()})
	if err == nil {
		t.Fatal("want error when CS is nil")
	}
	if !strings.Contains(err.Error(), "ChatSession") {
		t.Errorf("want error mentioning ChatSession, got %v", err)
	}
}

// TestStopSelectedAgent_NoTurnInFlight — when the selectedAS is
// ready (no in-flight prompt), Stop returns Action="noop" without
// invoking bridge.Stop.
func TestStopSelectedAgent_NoTurnInFlight(t *testing.T) {
	cs, _, stub := setupSelectedAS(t, "claude", "/tmp", true)

	result, err := stoppkg.StopSelectedAgent(&stoppkg.Cmd{CS: cs, Ctx: context.Background()})
	if err != nil {
		t.Fatalf("StopSelectedAgent: %v", err)
	}
	if result.Action != "noop" {
		t.Errorf("Action = %q, want noop", result.Action)
	}
	if stub.stopped != 0 {
		t.Errorf("bridge.Stop called %d times, want 0 (no turn in flight)", stub.stopped)
	}
}

// TestStopSelectedAgent_Stopped — when the selectedAS has an in-
// flight prompt (IsReady=false) and bridge.Stop returns nil,
// result.Action="stopped".
func TestStopSelectedAgent_Stopped(t *testing.T) {
	cs, as, stub := setupSelectedAS(t, "claude", "/tmp", false)
	_ = as // already configured by setupSelectedAS

	result, err := stoppkg.StopSelectedAgent(&stoppkg.Cmd{CS: cs, Ctx: context.Background()})
	if err != nil {
		t.Fatalf("StopSelectedAgent: %v", err)
	}
	if result.Action != "stopped" {
		t.Errorf("Action = %q, want stopped", result.Action)
	}
	if stub.stopped != 1 {
		t.Errorf("bridge.Stop called %d times, want 1", stub.stopped)
	}
	if result.Agent != "claude" || result.Cwd != "/tmp" {
		t.Errorf("result = %+v, want (claude, /tmp)", result)
	}
}

// TestStopSelectedAgent_NotSupported — when bridge.Stop returns
// agent.ErrNotSupported (pty bridge), result.Action=
// "not-supported".
func TestStopSelectedAgent_NotSupported(t *testing.T) {
	cs, _, stub := setupSelectedAS(t, "bash", "/tmp", false)
	stub.stopErr = agent.ErrNotSupported

	result, err := stoppkg.StopSelectedAgent(&stoppkg.Cmd{CS: cs, Ctx: context.Background()})
	if err != nil {
		t.Fatalf("StopSelectedAgent: %v", err)
	}
	if result.Action != "not-supported" {
		t.Errorf("Action = %q, want not-supported", result.Action)
	}
	if !errors.Is(result.Error, agent.ErrNotSupported) {
		t.Errorf("Error = %v, want ErrNotSupported", result.Error)
	}
}

// TestStopSelectedAgent_Failed — when bridge.Stop returns a generic
// error, result.Action="failed" and the error is preserved.
func TestStopSelectedAgent_Failed(t *testing.T) {
	cs, _, stub := setupSelectedAS(t, "claude", "/tmp", false)
	stub.stopErr = errors.New("bridge wedged")

	result, err := stoppkg.StopSelectedAgent(&stoppkg.Cmd{CS: cs, Ctx: context.Background()})
	if err != nil {
		t.Fatalf("StopSelectedAgent: %v", err)
	}
	if result.Action != "failed" {
		t.Errorf("Action = %q, want failed", result.Action)
	}
	if result.Error == nil || !strings.Contains(result.Error.Error(), "bridge wedged") {
		t.Errorf("Error = %v, want 'bridge wedged'", result.Error)
	}
}

// TestFormatStopResult_Stopped — FormatStopResult renders a stopped
// entry with the agent name + cwd + next-prompt hint.
func TestFormatStopResult_Stopped(t *testing.T) {
	got := stoppkg.FormatStopResult(stoppkg.Result{
		Agent: "claude", Cwd: "/code/A", Action: "stopped",
	})
	if !strings.Contains(got, "claude") || !strings.Contains(got, "/code/A") {
		t.Errorf("want reply to name stopped entry, got %q", got)
	}
	if !strings.Contains(got, "Next prompt will take over") {
		t.Errorf("want hint mentioning 'Next prompt will take over', got %q", got)
	}
}

// TestFormatStopResult_Noop — FormatStopResult on Action="noop"
// renders "No turn in flight on <agent> @ <cwd>".
func TestFormatStopResult_Noop(t *testing.T) {
	got := stoppkg.FormatStopResult(stoppkg.Result{
		Agent: "claude", Cwd: "/code/A", Action: "noop",
	})
	if !strings.Contains(got, "No turn in flight") {
		t.Errorf("want 'No turn in flight' message, got %q", got)
	}
}

// TestFormatStopResult_NotSupported — FormatStopResult on Action=
// "not-supported" tells the user to fall back to /close.
func TestFormatStopResult_NotSupported(t *testing.T) {
	got := stoppkg.FormatStopResult(stoppkg.Result{
		Agent: "bash", Cwd: "/code/A", Action: "not-supported",
	})
	if !strings.Contains(got, "/close") {
		t.Errorf("want reply pointing at /close fallback, got %q", got)
	}
}

// TestHandler_NoSession — the /stop handler with an unknown chat
// ID replies with the canonical "No active chat session." message.
func TestHandler_NoSession(t *testing.T) {
	mgr := chatsession.NewManager()
	f := stoppkg.NewFactory(mgr)

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil,
		command.SlashInput{ChatID: "no-such-chat"})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !out.Consumed {
		t.Fatal("want Consumed=true")
	}
	if !strings.Contains(out.Reply, "No active chat session") {
		t.Errorf("want reply mentioning 'No active chat session', got %q", out.Reply)
	}
}

// TestHandler_UsageError — the /stop handler with trailing args
// rejects with "Usage: /stop".
func TestHandler_UsageError(t *testing.T) {
	mgr := chatsession.NewManager()
	f := stoppkg.NewFactory(mgr)

	// Build a ChatSession so the chat-existence preflight passes.
	cs, _ := mgr.GetOrCreate("c1", "claude")
	cs.WithPersistence(nil, nil) //nolint:revive // test setup
	if err := cs.SetSelectedCwd("/tmp"); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, cs,
		command.SlashInput{ChatID: "c1", Args: []string{"stop", "extra"}})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !out.Consumed {
		t.Fatal("want Consumed=true")
	}
	if !strings.Contains(out.Reply, "Usage: /stop") {
		t.Errorf("want reply with usage hint, got %q", out.Reply)
	}
}

// TestHandler_NoTurnInFlight — the /stop handler on a selectedAS
// with no in-flight prompt replies with the canonical "No turn in
// flight" message.
func TestHandler_NoTurnInFlight(t *testing.T) {
	cs, _, _ := setupSelectedAS(t, "claude", "/tmp", true)
	mgr := chatsession.NewManager()
	f := stoppkg.NewFactory(mgr)

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, cs,
		command.SlashInput{ChatID: cs.ChatID, Args: []string{"stop"}})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !out.Consumed {
		t.Fatal("want Consumed=true")
	}
	if !strings.Contains(out.Reply, "No turn in flight") {
		t.Errorf("want 'No turn in flight' reply, got %q", out.Reply)
	}
}

// TestHandler_Stopped — the /stop handler on a selectedAS with an
// in-flight prompt calls bridge.Stop and replies with the
// "Next prompt will take over" hint.
func TestHandler_Stopped(t *testing.T) {
	cs, _, stub := setupSelectedAS(t, "claude", "/tmp", false)
	mgr := chatsession.NewManager()
	f := stoppkg.NewFactory(mgr)

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, cs,
		command.SlashInput{ChatID: cs.ChatID, Args: []string{"stop"}})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !out.Consumed {
		t.Fatal("want Consumed=true")
	}
	if !strings.Contains(out.Reply, "Next prompt will take over") {
		t.Errorf("want 'Next prompt will take over' reply, got %q", out.Reply)
	}
	if stub.stopped != 1 {
		t.Errorf("bridge.Stop called %d times, want 1", stub.stopped)
	}
}
