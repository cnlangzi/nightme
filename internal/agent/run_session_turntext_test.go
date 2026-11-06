package agent

import (
	"context"
	"strings"
	"testing"
)

// This file covers the optional TurnText capability: a bridge driver
// that can report the exact text of the turn in progress. The
// one-shot drain prefers that report over the reply it reassembles
// from the event stream, so a bridge that streams its reply in lossy
// pieces still lands the agent's own bytes in RunResult.Text.

// ─── test doubles ──────────────────────────────────────────────────

// turnTextFakeDriver is a session fake that also reports the exact
// text of the turn — the capability the drain prefers.
type turnTextFakeDriver struct {
	*sessionFakeDriver
	text string
}

func (d *turnTextFakeDriver) TurnText() string { return d.text }

// oneShotFakeStarter mirrors the shape every bridge's RunOnce has:
// the one-shot turn runs over the session transport.
type oneShotFakeStarter struct {
	name   string
	events chan AgentEvent
	d      driver
}

func (s *oneShotFakeStarter) Info() Info {
	return NewInfo(s.name, ModeJSONIO, "fake-cmd", nil, nil)
}
func (s *oneShotFakeStarter) Detect() error { return nil }
func (s *oneShotFakeStarter) Init(string)   {}

func (s *oneShotFakeStarter) Start(context.Context, StartConfig) (*Agent, error) {
	return NewAgent(s.Info(), 0, s.events, s.d), nil
}

func (s *oneShotFakeStarter) RunOnce(ctx context.Context, cfg StartConfig, blocks []ContentBlock, opts ...RunOnceOption) (RunResult, error) {
	return RunSession(ctx, s, cfg, blocks, opts...)
}

func (s *oneShotFakeStarter) Review(ctx context.Context, cfg StartConfig, opts ...RunOnceOption) (RunResult, error) {
	return RunSession(ctx, s, cfg, []ContentBlock{{Type: ContentText, Text: BuiltinPrompt}}, opts...)
}

// newTurnTextStarter builds a starter whose driver streams one text
// event and reports exact as the turn's text.
func newTurnTextStarter(name, streamed, exact string) *oneShotFakeStarter {
	d := newSessionFakeDriver(func([]ContentBlock) []AgentEvent {
		return []AgentEvent{
			sessionReadyEvent("sess-turn-text", "model-x"),
			{Kind: EventAgentText, Text: streamed},
			{Kind: EventAgentDone, Done: &AgentDoneEvent{ExitCode: 0, Reason: "settled"}},
		}
	})
	return &oneShotFakeStarter{
		name:   name,
		events: d.events,
		d:      &turnTextFakeDriver{sessionFakeDriver: d, text: exact},
	}
}

// ─── the capability ────────────────────────────────────────────────

// TestRunSession_PrefersDriverTurnText: the driver's own report of
// the turn's text wins over the streamed join. The bridge holds the
// raw reply; the stream only carries its trimmed pieces. The report's
// outer whitespace is trimmed like every other reply, but the
// paragraph break inside it survives.
func TestRunSession_PrefersDriverTurnText(t *testing.T) {
	s := newTurnTextStarter("fake", "trimmed piece", "exact\n\nreply\n")

	res, err := RunSession(context.Background(), s, StartConfig{Workspace: "/ws"},
		[]ContentBlock{{Type: ContentText, Text: "question"}})
	if err != nil {
		t.Fatalf("RunSession: %v", err)
	}
	if res.Text != "exact\n\nreply" {
		t.Errorf("Text = %q, want the driver's exact text %q", res.Text, "exact\n\nreply")
	}
}

// TestRunSession_TurnTextOptional: a driver without the capability
// still gets the streamed join — the capability is optional.
func TestRunSession_TurnTextOptional(t *testing.T) {
	d := newSessionFakeDriver(func([]ContentBlock) []AgentEvent {
		return []AgentEvent{
			sessionReadyEvent("sess-no-turn-text", "model-x"),
			{Kind: EventAgentText, Text: "streamed reply"},
			{Kind: EventAgentDone, Done: &AgentDoneEvent{ExitCode: 0, Reason: "settled"}},
		}
	})
	s := &sessionFakeStarter{name: "fake", newDriver: func(StartConfig) *sessionFakeDriver { return d }}

	res, err := RunSession(context.Background(), s, StartConfig{Workspace: "/ws"},
		[]ContentBlock{{Type: ContentText, Text: "question"}})
	if err != nil {
		t.Fatalf("RunSession: %v", err)
	}
	if res.Text != "streamed reply" {
		t.Errorf("Text = %q, want the streamed join %q", res.Text, "streamed reply")
	}
}

// TestRunSession_EmptyTurnTextFallsBackToStreamed: a driver that
// implements the capability but has nothing to report leaves the
// streamed join in place.
func TestRunSession_EmptyTurnTextFallsBackToStreamed(t *testing.T) {
	s := newTurnTextStarter("fake", "streamed reply", "")

	res, err := RunSession(context.Background(), s, StartConfig{Workspace: "/ws"},
		[]ContentBlock{{Type: ContentText, Text: "question"}})
	if err != nil {
		t.Fatalf("RunSession: %v", err)
	}
	if res.Text != "streamed reply" {
		t.Errorf("Text = %q, want the streamed join %q", res.Text, "streamed reply")
	}
}

// TestRunSession_BlankTurnTextFallsBackToStreamed: a driver whose
// report holds nothing but whitespace has no reply to offer — the
// streamed join answers instead.
func TestRunSession_BlankTurnTextFallsBackToStreamed(t *testing.T) {
	s := newTurnTextStarter("fake", "streamed reply", "  \n\t ")

	res, err := RunSession(context.Background(), s, StartConfig{Workspace: "/ws"},
		[]ContentBlock{{Type: ContentText, Text: "question"}})
	if err != nil {
		t.Fatalf("RunSession: %v", err)
	}
	if res.Text != "streamed reply" {
		t.Errorf("Text = %q, want the streamed join %q", res.Text, "streamed reply")
	}
}

// ─── payload size ──────────────────────────────────────────────────

// TestRunOnce_OversizedPrompt: a one-shot turn carries a prompt
// larger than the platform's argv limit (32767 characters per
// command line on Windows, 131072 bytes per argv string on Linux).
// The prompt travels over the bridge's protocol pipe, so the turn
// completes instead of failing to spawn.
func TestRunOnce_OversizedPrompt(t *testing.T) {
	const wantSize = 64 * 1024
	payload := strings.Repeat("x", wantSize)

	d := newSessionFakeDriver(func(blocks []ContentBlock) []AgentEvent {
		if len(blocks) != 1 || len(blocks[0].Text) != wantSize {
			t.Errorf("driver received %d bytes, want %d", len(blocks[0].Text), wantSize)
		}
		return []AgentEvent{{Kind: EventAgentResult, Result: &AgentResultEvent{Text: "ok"}}}
	})
	s := &oneShotFakeStarter{name: "fake", events: d.events, d: d}

	res, err := s.RunOnce(context.Background(), StartConfig{Workspace: "/ws"},
		[]ContentBlock{{Type: ContentText, Text: payload}})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Text != "ok" {
		t.Errorf("Text = %q, want ok", res.Text)
	}
	sent := d.sentBlocks()
	if len(sent) != 1 || len(sent[0].Text) != wantSize {
		t.Fatalf("driver received %d blocks, want 1 of %d bytes", len(sent), wantSize)
	}
}
