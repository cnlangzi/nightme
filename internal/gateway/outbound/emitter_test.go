package outbound

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	
	"github.com/cnlangzi/nightme/internal/messages"
)

// fakeChannel is a minimal Channel implementation that records every
// outbound message and returns programmable results.
type fakeChannel struct {
	name      string
	sendCalls int32
	cardCalls int32
	sendErr   error
	cardErr   error
	cardMsgID string
	lastSent  messages.OutboundMessage
	lastCard  messages.OutboundMessage
}

func (f *fakeChannel) Name() string { return f.name }
func (f *fakeChannel) Start(_ context.Context) error { return nil }
func (f *fakeChannel) Stop(_ context.Context) error { return nil }
func (f *fakeChannel) Incoming() <-chan messages.InboundMessage { return nil }

func (f *fakeChannel) Send(_ context.Context, msg messages.OutboundMessage) error {
	atomic.AddInt32(&f.sendCalls, 1)
	f.lastSent = msg
	return f.sendErr
}

func (f *fakeChannel) SendCard(_ context.Context, msg messages.OutboundMessage) (string, error) {
	atomic.AddInt32(&f.cardCalls, 1)
	f.lastCard = msg
	return f.cardMsgID, f.cardErr
}

func TestEmitter_NoSource_PassesThrough(t *testing.T) {
	// Zero-value Options (no Source) must produce a pure
	// passthrough — the caller-built OutboundMessage goes
	// straight to Channel.Send, no StatusBar added.
	fc := &fakeChannel{name: "test"}
	em := New(fc, Options{})

	in := messages.OutboundMessage{ChatID: "c1", Kind: messages.OutReply, Text: "hi"}
	if err := em.Send(context.Background(), in); err != nil {
		t.Fatalf("Send returned err = %v", err)
	}
	if atomic.LoadInt32(&fc.sendCalls) != 1 {
		t.Fatalf("sendCalls = %d, want 1", fc.sendCalls)
	}
	if fc.lastSent.Text != "hi" {
		t.Errorf("lastSent.Text = %q, want hi", fc.lastSent.Text)
	}
	if fc.lastSent.StatusBar != nil {
		t.Errorf("lastSent.StatusBar = %+v, want nil (no source)", fc.lastSent.StatusBar)
	}
}

func TestEmitter_SourceAttachesStatusBar(t *testing.T) {
	// Caller did NOT set StatusBar. Source returns a value.
	// Emitter must attach it before forwarding.
	want := &messages.StatusBar{
		AgentBar: &messages.AgentStatusBar{Agent: "claude", Model: "opus"},
	}
	fc := &fakeChannel{name: "test"}
	em := New(fc, Options{Source: func(_ string) *messages.StatusBar { return want }})

	in := messages.OutboundMessage{ChatID: "c1", Kind: messages.OutReply, Text: "hi"}
	if err := em.Send(context.Background(), in); err != nil {
		t.Fatalf("Send err: %v", err)
	}
	if fc.lastSent.StatusBar != want {
		t.Errorf("lastSent.StatusBar = %p, want %p", fc.lastSent.StatusBar, want)
	}
}

func TestEmitter_SourceNil_DoesNotAttach(t *testing.T) {
	// Source returns nil → emitter must NOT manufacture an empty
	// StatusBar. The footer render path expects nil when the
	// source decides "no chat / no workspace".
	fc := &fakeChannel{name: "test"}
	em := New(fc, Options{Source: func(_ string) *messages.StatusBar { return nil }})

	in := messages.OutboundMessage{ChatID: "c1", Kind: messages.OutReply, Text: "hi"}
	_ = em.Send(context.Background(), in)
	if fc.lastSent.StatusBar != nil {
		t.Errorf("lastSent.StatusBar = %+v, want nil", fc.lastSent.StatusBar)
	}
}

func TestEmitter_CallerStampedWins(t *testing.T) {
	// Caller already set StatusBar; source must NOT be invoked
	// (otherwise the caller's value gets silently overwritten).
	callerSB := &messages.StatusBar{
		AgentBar: &messages.AgentStatusBar{Agent: "from-caller"},
	}
	sourceCalled := false
	source := func(_ string) *messages.StatusBar {
		sourceCalled = true
		return &messages.StatusBar{
			AgentBar: &messages.AgentStatusBar{Agent: "from-source"},
		}
	}
	fc := &fakeChannel{name: "test"}
	em := New(fc, Options{Source: source})

	in := messages.OutboundMessage{
		ChatID:    "c1",
		Kind:      messages.OutReply,
		Text:      "hi",
		StatusBar: callerSB,
	}
	_ = em.Send(context.Background(), in)
	if sourceCalled {
		t.Error("source should not be called when caller pre-set StatusBar")
	}
	if fc.lastSent.StatusBar != callerSB {
		t.Error("caller's StatusBar was overwritten")
	}
}

func TestEmitter_SendCard_AttachesAndForwards(t *testing.T) {
	want := &messages.StatusBar{
		AgentBar: &messages.AgentStatusBar{Agent: "claude"},
	}
	fc := &fakeChannel{name: "test", cardMsgID: "msg-123"}
	em := New(fc, Options{Source: func(_ string) *messages.StatusBar { return want }})

	in := messages.OutboundMessage{ChatID: "c1", Kind: messages.OutCard}
	id, err := em.SendCard(context.Background(), in)
	if err != nil {
		t.Fatalf("SendCard err: %v", err)
	}
	if id != "msg-123" {
		t.Errorf("msgID = %q, want msg-123", id)
	}
	if fc.lastCard.StatusBar != want {
		t.Error("SendCard path did not attach StatusBar")
	}
}

func TestEmitter_SendErrorPropagates(t *testing.T) {
	// Channel.Send fails → the error is returned to the caller.
	// Emitter is a passthrough: callers handle logging/metrics
	// (no OnError hook; that lived through one review cycle and
	// was deleted for YAGNI — see Commit 10).
	sendErr := errors.New("channel broken")
	fc := &fakeChannel{name: "test", sendErr: sendErr}
	em := New(fc, Options{})

	err := em.Send(context.Background(), messages.OutboundMessage{ChatID: "c1", Text: "hi"})
	if !errors.Is(err, sendErr) {
		t.Errorf("returned err = %v, want %v", err, sendErr)
	}
}

func TestEmitter_SendCardErrorPropagates(t *testing.T) {
	// SendCard is the same passthrough contract as Send: errors
	// surface to the caller.
	cardErr := errors.New("card channel broken")
	fc := &fakeChannel{name: "test", cardErr: cardErr}
	em := New(fc, Options{})

	_, err := em.SendCard(context.Background(), messages.OutboundMessage{ChatID: "c1"})
	if !errors.Is(err, cardErr) {
		t.Errorf("returned err = %v, want %v", err, cardErr)
	}
}

// TestEmitter_SourceCoLocatesUsageFromMsg covers the F-55 fix:
// when the source's StatusBar.UsageBar is nil but the message
// carries Usage (typical on OutResult after gateway.Translate),
// the emitter must copy msg.Usage across so the footer render
// path can pick it up via sb.UsageBar. Without this, Line 2 of
// the footer silently drops for usage-bearing events.
func TestEmitter_SourceCoLocatesUsageFromMsg(t *testing.T) {
	want := &messages.UsageInfo{InputTokens: 100, OutputTokens: 200}
	fc := &fakeChannel{name: "test"}
	em := New(fc, Options{
		Source: func(_ string) *messages.StatusBar {
			// Source returns StatusBar without UsageBar —
			// simulates a source that doesn't see msg.Usage.
			return &messages.StatusBar{
				AgentBar: &messages.AgentStatusBar{Agent: "claude", Model: "opus"},
			}
		},
	})

	in := messages.OutboundMessage{
		ChatID: "c1",
		Kind:   messages.OutResult,
		Text:   "done",
		Usage:  want,
	}
	if err := em.Send(context.Background(), in); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if fc.lastSent.StatusBar == nil {
		t.Fatal("StatusBar was not attached")
	}
	if fc.lastSent.StatusBar.UsageBar == nil || fc.lastSent.StatusBar.UsageBar.UsageInfo != want {
		t.Errorf("StatusBar.UsageBar.UsageInfo = %+v, want %+v (F-55 co-location)", fc.lastSent.StatusBar.UsageBar.UsageInfo, want)
	}
}

// TestEmitter_SourceCoLocateDoesNotOverwrite verifies the
// source-set UsageBar is preserved if non-nil (caller or source
// already had it; emitter must not clobber).
func TestEmitter_SourceCoLocateDoesNotOverwrite(t *testing.T) {
	sourceUsage := &messages.UsageInfo{InputTokens: 1, OutputTokens: 1}
	sourceSB := &messages.StatusBar{
		AgentBar: &messages.AgentStatusBar{Agent: "claude"},
		UsageBar: &messages.UsageStatusBar{UsageInfo: sourceUsage},
	}
	msgUsage := &messages.UsageInfo{InputTokens: 999, OutputTokens: 999}

	fc := &fakeChannel{name: "test"}
	em := New(fc, Options{
		Source: func(_ string) *messages.StatusBar { return sourceSB },
	})

	_ = em.Send(context.Background(), messages.OutboundMessage{
		ChatID: "c1",
		Kind:   messages.OutResult,
		Usage:  msgUsage,
	})
	if fc.lastSent.StatusBar.UsageBar.UsageInfo != sourceUsage {
		t.Errorf("StatusBar.UsageBar.UsageInfo = %+v, want source-set %+v (no overwrite)", fc.lastSent.StatusBar.UsageBar.UsageInfo, sourceUsage)
	}
	if fc.lastSent.StatusBar.UsageBar.UsageInfo == msgUsage {
		t.Error("emitter should not overwrite source-set UsageInfo with msg.Usage")
	}
}
