package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// This file covers the shared session one-shot helper
// (internal/agent/run_session.go): the turn-end contract, the
// permission auto-allow, the fresh-session rule, and the four failure
// paths. The bridge-specific session paths are covered by each
// bridge's own tests.

// ─── test doubles ──────────────────────────────────────────────────

// sessionFakeDriver replays a scripted event sequence onto the
// agent's event channel once the prompt has been sent.
type sessionFakeDriver struct {
	events chan AgentEvent
	perm   chan string

	// script returns the events to emit for the prompt that was
	// sent. Nil means "no events".
	script func(blocks []ContentBlock) []AgentEvent

	sendErr error

	mu     sync.Mutex
	sent   []ContentBlock
	closed atomic.Bool
}

func newSessionFakeDriver(script func(blocks []ContentBlock) []AgentEvent) *sessionFakeDriver {
	return &sessionFakeDriver{
		events: make(chan AgentEvent, 16),
		perm:   make(chan string, 4),
		script: script,
	}
}

func (d *sessionFakeDriver) SendBlocks(_ context.Context, blocks []ContentBlock) error {
	if d.sendErr != nil {
		return d.sendErr
	}
	d.mu.Lock()
	d.sent = append(d.sent, blocks...)
	d.mu.Unlock()
	go func() {
		for _, ev := range d.script(blocks) {
			d.events <- ev
		}
		close(d.events)
	}()
	return nil
}

func (d *sessionFakeDriver) SendPermission(resp string) error {
	select {
	case d.perm <- resp:
	default:
	}
	return nil
}

func (d *sessionFakeDriver) Reset(context.Context) error { return nil }
func (d *sessionFakeDriver) Close() error                { d.closed.Store(true); return nil }
func (d *sessionFakeDriver) Stop(context.Context) error  { return ErrNotSupported }
func (d *sessionFakeDriver) Keepalive(context.Context, func(context.Context) error) error {
	return nil
}

// sentBlocks returns the blocks the driver received.
func (d *sessionFakeDriver) sentBlocks() []ContentBlock {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]ContentBlock(nil), d.sent...)
}

// sessionFakeStarter is a Starter whose Start returns a live *Agent
// backed by a sessionFakeDriver. It counts Start / RunOnce calls so
// tests can assert which path a caller took.
type sessionFakeStarter struct {
	name string

	// newDriver builds the driver for one Start call.
	newDriver func(cfg StartConfig) *sessionFakeDriver

	startErr error

	starts     atomic.Int32
	runOnce    atomic.Int32
	mu         sync.Mutex
	startedCfg []StartConfig
}

func (s *sessionFakeStarter) Info() Info    { return NewInfo(s.name, ModeJSONIO, "fake-cmd", nil, nil) }
func (s *sessionFakeStarter) Detect() error { return nil }
func (s *sessionFakeStarter) Init(string)   {}

func (s *sessionFakeStarter) Start(_ context.Context, cfg StartConfig) (*Agent, error) {
	s.starts.Add(1)
	s.mu.Lock()
	s.startedCfg = append(s.startedCfg, cfg)
	s.mu.Unlock()
	if s.startErr != nil {
		return nil, s.startErr
	}
	d := s.newDriver(cfg)
	return NewAgent(s.Info(), 0, d.events, d), nil
}

func (s *sessionFakeStarter) RunOnce(context.Context, StartConfig, []ContentBlock, ...RunOnceOption) (RunResult, error) {
	s.runOnce.Add(1)
	return RunResult{}, errors.New("RunOnce must not be used on the session path")
}

func (s *sessionFakeStarter) Review(ctx context.Context, cfg StartConfig, opts ...RunOnceOption) (RunResult, error) {
	return RunSession(ctx, s, cfg, []ContentBlock{{Type: ContentText, Text: BuiltinPrompt}}, opts...)
}

// startedConfigs returns the StartConfig each Start call received.
func (s *sessionFakeStarter) startedConfigs() []StartConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]StartConfig(nil), s.startedCfg...)
}

// sessionReadyEvent is the startup EventAgentReady every bridge emits.
func sessionReadyEvent(sessionID, model string) AgentEvent {
	return AgentEvent{
		Kind:      EventAgentReady,
		SessionID: sessionID,
		Model:     model,
	}
}

// ─── turn-end contract ────────────────────────────────────────────

// TestRunSession_ResultTerminator: a bridge that ends the turn with
// EventAgentResult hands back the turn's reply plus the session
// identity from the startup EventAgentReady. A bridge that streamed
// part of the reply before the result event (pi / omp / dsh carry the
// tail on the result) gets both pieces.
func TestRunSession_ResultTerminator(t *testing.T) {
	d := newSessionFakeDriver(func([]ContentBlock) []AgentEvent {
		return []AgentEvent{
			sessionReadyEvent("sess-1", "model-x"),
			{Kind: EventAgentText, Text: "Let me check the diff."},
			{
				Kind: EventAgentResult,
				Result: &AgentResultEvent{
					Text:       "the answer",
					Subtype:    "success",
					DurationMs: 42,
					Usage:      &UsageInfo{InputTokens: 7, OutputTokens: 9},
				},
			},
		}
	})
	s := &sessionFakeStarter{name: "fake", newDriver: func(StartConfig) *sessionFakeDriver { return d }}

	res, err := RunSession(context.Background(), s, StartConfig{Workspace: "/ws"},
		[]ContentBlock{{Type: ContentText, Text: "question"}})
	if err != nil {
		t.Fatalf("RunSession: %v", err)
	}
	want := "Let me check the diff.\nthe answer"
	if res.Text != want {
		t.Errorf("Text = %q, want %q", res.Text, want)
	}
	if res.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want %q", res.SessionID, "sess-1")
	}
	if res.Model != "model-x" {
		t.Errorf("Model = %q, want %q", res.Model, "model-x")
	}
	if res.Subtype != "success" {
		t.Errorf("Subtype = %q, want %q", res.Subtype, "success")
	}
	if res.DurationMs != 42 {
		t.Errorf("DurationMs = %d, want 42", res.DurationMs)
	}
	if res.Usage == nil || res.Usage.OutputTokens != 9 {
		t.Errorf("Usage = %+v, want OutputTokens 9", res.Usage)
	}
	if !d.closed.Load() {
		t.Error("session was not closed")
	}
}

// TestRunSession_DoneSettledTerminator: a bridge that ends the turn
// with EventAgentDone{Reason: "settled"} and streams the reply as
// EventAgentText (the codex / ACP shape) hands back the streamed
// text.
func TestRunSession_DoneSettledTerminator(t *testing.T) {
	d := newSessionFakeDriver(func([]ContentBlock) []AgentEvent {
		return []AgentEvent{
			sessionReadyEvent("sess-2", "model-y"),
			{Kind: EventAgentText, Text: "the answer"},
			{
				Kind: EventAgentDone,
				Done: &AgentDoneEvent{ExitCode: 0, Reason: "settled"},
			},
		}
	})
	s := &sessionFakeStarter{name: "fake", newDriver: func(StartConfig) *sessionFakeDriver { return d }}

	res, err := RunSession(context.Background(), s, StartConfig{Workspace: "/ws"},
		[]ContentBlock{{Type: ContentText, Text: "question"}})
	if err != nil {
		t.Fatalf("RunSession: %v", err)
	}
	if res.Text != "the answer" {
		t.Errorf("Text = %q, want %q", res.Text, "the answer")
	}
	if res.SessionID != "sess-2" {
		t.Errorf("SessionID = %q, want %q", res.SessionID, "sess-2")
	}
}

// TestRunSession_ResultEmptyTextFallsBackToStreamed: a bridge that
// carries the reply only as EventAgentText and emits a metadata-only
// EventAgentResult (codex does exactly this) still hands back the
// streamed text.
func TestRunSession_ResultEmptyTextFallsBackToStreamed(t *testing.T) {
	d := newSessionFakeDriver(func([]ContentBlock) []AgentEvent {
		return []AgentEvent{
			sessionReadyEvent("sess-3", "model-z"),
			{Kind: EventAgentText, Text: "streamed reply"},
			{Kind: EventAgentResult, Result: &AgentResultEvent{}},
		}
	})
	s := &sessionFakeStarter{name: "fake", newDriver: func(StartConfig) *sessionFakeDriver { return d }}

	res, err := RunSession(context.Background(), s, StartConfig{Workspace: "/ws"},
		[]ContentBlock{{Type: ContentText, Text: "question"}})
	if err != nil {
		t.Fatalf("RunSession: %v", err)
	}
	if res.Text != "streamed reply" {
		t.Errorf("Text = %q, want %q", res.Text, "streamed reply")
	}
}

// TestRunSession_NonSettledDoneFails: a Done that is not a turn
// terminator (no reason, or "interrupted") means the turn ended
// without a result. The session stays alive after a turn, so waiting
// for another terminator would stall until the caller's deadline —
// the drain reports the failure instead.
func TestRunSession_NonSettledDoneFails(t *testing.T) {
	d := newSessionFakeDriver(func([]ContentBlock) []AgentEvent {
		return []AgentEvent{
			sessionReadyEvent("sess-4", "model-w"),
			{Kind: EventAgentDone, Done: &AgentDoneEvent{ExitCode: 0, Reason: "interrupted"}},
		}
	})
	s := &sessionFakeStarter{name: "fake", newDriver: func(StartConfig) *sessionFakeDriver { return d }}

	_, err := RunSession(context.Background(), s, StartConfig{Workspace: "/ws"},
		[]ContentBlock{{Type: ContentText, Text: "question"}})
	if err == nil {
		t.Fatal("RunSession should fail on a turn that ended without a result")
	}
	if !strings.Contains(err.Error(), "turn ended without a result event") {
		t.Errorf("err = %v, want 'turn ended without a result event'", err)
	}
}

// TestRunSession_EmptyReplyFails: a turn that produced no reply is a
// failure, not an empty success. The print-mode bridges reject an
// empty answer the same way.
func TestRunSession_EmptyReplyFails(t *testing.T) {
	d := newSessionFakeDriver(func([]ContentBlock) []AgentEvent {
		return []AgentEvent{
			sessionReadyEvent("sess-8", "model-q"),
			{
				Kind: EventAgentDone,
				Done: &AgentDoneEvent{ExitCode: 0, Reason: "settled"},
			},
		}
	})
	s := &sessionFakeStarter{name: "fake", newDriver: func(StartConfig) *sessionFakeDriver { return d }}

	_, err := RunSession(context.Background(), s, StartConfig{Workspace: "/ws"},
		[]ContentBlock{{Type: ContentText, Text: "question"}})
	if err == nil {
		t.Fatal("RunSession should fail on a turn that produced no reply")
	}
	if !strings.Contains(err.Error(), "turn ended without a reply") {
		t.Errorf("err = %v, want 'turn ended without a reply'", err)
	}
}

// TestRunSession_FailedTurnKeepsSessionID: a turn that fails after
// the startup Ready still reports the session id. The process is
// gone, but the agent-side conversation survives — the session id is
// what lets the caller reopen it (ResumeSession) and let the agent
// pick the work up where it stopped.
func TestRunSession_FailedTurnKeepsSessionID(t *testing.T) {
	d := newSessionFakeDriver(func([]ContentBlock) []AgentEvent {
		return []AgentEvent{
			sessionReadyEvent("sess-failed", "model-x"),
			{Kind: EventAgentError, Err: errBoom},
		}
	})
	s := &sessionFakeStarter{name: "fake", newDriver: func(StartConfig) *sessionFakeDriver { return d }}

	res, err := RunSession(context.Background(), s, StartConfig{Workspace: "/ws"},
		[]ContentBlock{{Type: ContentText, Text: "question"}})
	if err == nil {
		t.Fatal("RunSession should fail when the turn errors")
	}
	if res.SessionID != "sess-failed" {
		t.Errorf("SessionID = %q, want %q", res.SessionID, "sess-failed")
	}
	if res.Model != "model-x" {
		t.Errorf("Model = %q, want %q", res.Model, "model-x")
	}
}

// TestResumeSession_KeepsSessionID: the resume counterpart of
// RunSession — it reopens the session named by cfg.SessionID instead
// of dropping it for a fresh one.
func TestResumeSession_KeepsSessionID(t *testing.T) {
	d := newSessionFakeDriver(func([]ContentBlock) []AgentEvent {
		return []AgentEvent{
			sessionReadyEvent("sess-resumed", "model-x"),
			{Kind: EventAgentResult, Result: &AgentResultEvent{Text: "picked up where it stopped"}},
		}
	})
	s := &sessionFakeStarter{name: "fake", newDriver: func(StartConfig) *sessionFakeDriver { return d }}

	res, err := ResumeSession(context.Background(), s, StartConfig{
		Workspace: "/ws",
		SessionID: "sess-resumed",
	}, []ContentBlock{{Type: ContentText, Text: "continue"}})
	if err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	cfgs := s.startedConfigs()
	if len(cfgs) != 1 {
		t.Fatalf("Start called %d times, want 1", len(cfgs))
	}
	if cfgs[0].SessionID != "sess-resumed" {
		t.Errorf("Start received SessionID %q, want %q", cfgs[0].SessionID, "sess-resumed")
	}
	if res.Text != "picked up where it stopped" {
		t.Errorf("Text = %q, want the resumed reply", res.Text)
	}
}

// TestRunSession_IdentityFromNonReadyEvent: every event carries the
// bridge-side session context, so the session id and model survive a
// startup Ready that was consumed elsewhere.
func TestRunSession_IdentityFromNonReadyEvent(t *testing.T) {
	d := newSessionFakeDriver(func([]ContentBlock) []AgentEvent {
		return []AgentEvent{
			{
				Kind:      EventAgentText,
				Text:      "the answer",
				SessionID: "sess-9",
				Model:     "model-r",
			},
			{
				Kind: EventAgentResult,
				Result: &AgentResultEvent{
					Text: "tail",
				},
			},
		}
	})
	s := &sessionFakeStarter{name: "fake", newDriver: func(StartConfig) *sessionFakeDriver { return d }}

	res, err := RunSession(context.Background(), s, StartConfig{Workspace: "/ws"},
		[]ContentBlock{{Type: ContentText, Text: "question"}})
	if err != nil {
		t.Fatalf("RunSession: %v", err)
	}
	if res.SessionID != "sess-9" {
		t.Errorf("SessionID = %q, want %q", res.SessionID, "sess-9")
	}
	if res.Model != "model-r" {
		t.Errorf("Model = %q, want %q", res.Model, "model-r")
	}
}

// ─── permission auto-allow ────────────────────────────────────────

// TestRunSession_AutoAllowsPermission: there is no interactive user
// on the one-shot path, so a permission request is answered with the
// bridge's first option and the turn continues.
func TestRunSession_AutoAllowsPermission(t *testing.T) {
	d := newSessionFakeDriver(func([]ContentBlock) []AgentEvent {
		return []AgentEvent{
			sessionReadyEvent("sess-5", "model-v"),
			{
				Kind: EventAgentPermission,
				Permission: &AgentPermissionRequest{
					Tool:    "Bash",
					Action:  "run ls",
					Options: []string{"Allow once", "Reject"},
				},
			},
			{
				Kind:   EventAgentResult,
				Result: &AgentResultEvent{Text: "after approval"},
			},
		}
	})
	s := &sessionFakeStarter{name: "fake", newDriver: func(StartConfig) *sessionFakeDriver { return d }}

	res, err := RunSession(context.Background(), s, StartConfig{Workspace: "/ws"},
		[]ContentBlock{{Type: ContentText, Text: "question"}})
	if err != nil {
		t.Fatalf("RunSession: %v", err)
	}
	if res.Text != "after approval" {
		t.Errorf("Text = %q, want %q", res.Text, "after approval")
	}
	select {
	case resp := <-d.perm:
		if resp != "Allow once" {
			t.Errorf("permission response = %q, want %q", resp, "Allow once")
		}
	default:
		t.Error("permission was never answered")
	}
}

// ─── fresh session ────────────────────────────────────────────────

// TestRunSession_ClearsSessionID: a one-shot turn always opens a
// fresh session — resuming the caller's session would splice the
// turn into an unrelated conversation.
func TestRunSession_ClearsSessionID(t *testing.T) {
	d := newSessionFakeDriver(func([]ContentBlock) []AgentEvent {
		return []AgentEvent{{Kind: EventAgentResult, Result: &AgentResultEvent{Text: "ok"}}}
	})
	s := &sessionFakeStarter{name: "fake", newDriver: func(StartConfig) *sessionFakeDriver { return d }}

	if _, err := RunSession(context.Background(), s, StartConfig{
		Workspace: "/ws",
		SessionID: "stale-session-id",
	}, []ContentBlock{{Type: ContentText, Text: "question"}}); err != nil {
		t.Fatalf("RunSession: %v", err)
	}
	cfgs := s.startedConfigs()
	if len(cfgs) != 1 {
		t.Fatalf("Start called %d times, want 1", len(cfgs))
	}
	if cfgs[0].SessionID != "" {
		t.Errorf("Start received SessionID %q, want empty", cfgs[0].SessionID)
	}
	if cfgs[0].Workspace != "/ws" {
		t.Errorf("Start received Workspace %q, want /ws", cfgs[0].Workspace)
	}
}

// ─── failure paths ────────────────────────────────────────────────

// TestRunSession_StartFailure: a failed spawn is reported as an error.
func TestRunSession_StartFailure(t *testing.T) {
	boom := errors.New("binary not on PATH")
	s := &sessionFakeStarter{
		name:     "fake",
		startErr: boom,
		newDriver: func(StartConfig) *sessionFakeDriver {
			return newSessionFakeDriver(nil)
		},
	}
	_, err := RunSession(context.Background(), s, StartConfig{Workspace: "/ws"},
		[]ContentBlock{{Type: ContentText, Text: "question"}})
	if err == nil {
		t.Fatal("RunSession should error when Start fails")
	}
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want wrap of %v", err, boom)
	}
}

// TestRunSession_SendFailure: a failed SendBlocks is reported as an
// error.
func TestRunSession_SendFailure(t *testing.T) {
	boom := errors.New("broken pipe")
	d := newSessionFakeDriver(nil)
	d.sendErr = boom
	s := &sessionFakeStarter{name: "fake", newDriver: func(StartConfig) *sessionFakeDriver { return d }}

	_, err := RunSession(context.Background(), s, StartConfig{Workspace: "/ws"},
		[]ContentBlock{{Type: ContentText, Text: "question"}})
	if err == nil {
		t.Fatal("RunSession should error when SendBlocks fails")
	}
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want wrap of %v", err, boom)
	}
}

// TestRunSession_StreamClosedBeforeTerminator: a stream that closes
// without a turn terminator is an error, not an empty success.
func TestRunSession_StreamClosedBeforeTerminator(t *testing.T) {
	d := newSessionFakeDriver(func([]ContentBlock) []AgentEvent {
		return []AgentEvent{
			sessionReadyEvent("sess-6", "model-u"),
			{Kind: EventAgentText, Text: "half an answer"},
		}
	})
	s := &sessionFakeStarter{name: "fake", newDriver: func(StartConfig) *sessionFakeDriver { return d }}

	_, err := RunSession(context.Background(), s, StartConfig{Workspace: "/ws"},
		[]ContentBlock{{Type: ContentText, Text: "question"}})
	if err == nil {
		t.Fatal("RunSession should error when the stream closes without a terminator")
	}
	if !strings.Contains(err.Error(), "event stream closed before the turn ended") {
		t.Errorf("err = %v, want 'event stream closed before the turn ended'", err)
	}
}

// TestRunSession_ContextCancelled: a cancelled context aborts the
// turn.
func TestRunSession_ContextCancelled(t *testing.T) {
	d := newSessionFakeDriver(func([]ContentBlock) []AgentEvent {
		return []AgentEvent{{Kind: EventAgentText, Text: "waiting"}}
	})
	s := &sessionFakeStarter{name: "fake", newDriver: func(StartConfig) *sessionFakeDriver { return d }}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := RunSession(ctx, s, StartConfig{Workspace: "/ws"},
		[]ContentBlock{{Type: ContentText, Text: "question"}})
	if err == nil {
		t.Fatal("RunSession should error when the context is cancelled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want wrap of context.Canceled", err)
	}
}

// TestRunSession_ErrorEvent: an EventAgentError fails the turn.
func TestRunSession_ErrorEvent(t *testing.T) {
	boom := errors.New("upstream blew up")
	d := newSessionFakeDriver(func([]ContentBlock) []AgentEvent {
		return []AgentEvent{
			sessionReadyEvent("sess-7", "model-t"),
			{Kind: EventAgentError, Err: boom},
		}
	})
	s := &sessionFakeStarter{name: "fake", newDriver: func(StartConfig) *sessionFakeDriver { return d }}

	_, err := RunSession(context.Background(), s, StartConfig{Workspace: "/ws"},
		[]ContentBlock{{Type: ContentText, Text: "question"}})
	if err == nil {
		t.Fatal("RunSession should error on an error event")
	}
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want wrap of %v", err, boom)
	}
}

// ─── payload size ─────────────────────────────────────────────────

// TestRunSession_LargePayload: a prompt larger than the platform's
// argv limit (131072 bytes on Linux) reaches the agent intact over
// the session path. The session transport writes to a pipe, which has
// no length limit.
func TestRunSession_LargePayload(t *testing.T) {
	const wantSize = 200 * 1024
	payload := strings.Repeat("x", wantSize)

	d := newSessionFakeDriver(func(blocks []ContentBlock) []AgentEvent {
		if len(blocks) != 1 || len(blocks[0].Text) != wantSize {
			t.Errorf("driver received %d bytes, want %d", len(blocks[0].Text), wantSize)
		}
		return []AgentEvent{{Kind: EventAgentResult, Result: &AgentResultEvent{Text: "ok"}}}
	})
	s := &sessionFakeStarter{name: "fake", newDriver: func(StartConfig) *sessionFakeDriver { return d }}

	if _, err := RunSession(context.Background(), s, StartConfig{Workspace: "/ws"},
		[]ContentBlock{{Type: ContentText, Text: payload}}); err != nil {
		t.Fatalf("RunSession: %v", err)
	}
	sent := d.sentBlocks()
	if len(sent) != 1 || len(sent[0].Text) != wantSize {
		t.Fatalf("driver received %d blocks, want 1 of %d bytes", len(sent), wantSize)
	}
}
