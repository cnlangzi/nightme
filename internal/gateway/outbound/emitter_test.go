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
	name        string
	sendCalls   int32
	cardCalls   int32
	sendErr     error
	cardErr     error
	cardMsgID   string
	lastSent    messages.OutboundMessage
	lastCard    messages.OutboundMessage
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

func TestEmitter_NoStamper_PassesThrough(t *testing.T) {
	// Zero-value Options (no Stamper) must produce a
	// pure passthrough — the caller-built OutboundMessage goes
	// straight to Channel.Send, no SessionContext added.
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
	if fc.lastSent.SessionContext != nil {
		t.Errorf("lastSent.SessionContext = %+v, want nil (no stamper)", fc.lastSent.SessionContext)
	}
}

func TestEmitter_StamperAttachesSessionContext(t *testing.T) {
	// Caller did NOT set SessionContext. Stamper returns a value.
	// Emitter must attach it before forwarding.
	want := &messages.SessionContext{Agent: "claude", Model: "opus"}
	fc := &fakeChannel{name: "test"}
	em := New(fc, Options{Stamper: func(_ string) *messages.SessionContext { return want }})

	in := messages.OutboundMessage{ChatID: "c1", Kind: messages.OutReply, Text: "hi"}
	if err := em.Send(context.Background(), in); err != nil {
		t.Fatalf("Send err: %v", err)
	}
	if fc.lastSent.SessionContext != want {
		t.Errorf("lastSent.SessionContext = %p, want %p", fc.lastSent.SessionContext, want)
	}
}

func TestEmitter_StamperNil_DoesNotAttach(t *testing.T) {
	// Stamper returns nil → emitter must NOT manufacture an empty
	// SessionContext. The footer render path expects nil when the
	// stamper decides "no active session".
	fc := &fakeChannel{name: "test"}
	em := New(fc, Options{Stamper: func(_ string) *messages.SessionContext { return nil }})

	in := messages.OutboundMessage{ChatID: "c1", Kind: messages.OutReply, Text: "hi"}
	_ = em.Send(context.Background(), in)
	if fc.lastSent.SessionContext != nil {
		t.Errorf("lastSent.SessionContext = %+v, want nil", fc.lastSent.SessionContext)
	}
}

func TestEmitter_CallerStampedWins(t *testing.T) {
	// Caller already set SessionContext; stamper must NOT be invoked
	// (otherwise the caller's value gets silently overwritten).
	callerSC := &messages.SessionContext{Agent: "from-caller"}
	stamperCalled := false
	stamper := func(_ string) *messages.SessionContext {
		stamperCalled = true
		return &messages.SessionContext{Agent: "from-stamper"}
	}
	fc := &fakeChannel{name: "test"}
	em := New(fc, Options{Stamper: stamper})

	in := messages.OutboundMessage{
		ChatID:         "c1",
		Kind:           messages.OutReply,
		Text:           "hi",
		SessionContext: callerSC,
	}
	_ = em.Send(context.Background(), in)
	if stamperCalled {
		t.Error("stamper should not be called when caller pre-set SessionContext")
	}
	if fc.lastSent.SessionContext != callerSC {
		t.Error("caller's SessionContext was overwritten")
	}
}

func TestEmitter_SendCard_StampsAndForwards(t *testing.T) {
	want := &messages.SessionContext{Agent: "claude"}
	fc := &fakeChannel{name: "test", cardMsgID: "msg-123"}
	em := New(fc, Options{Stamper: func(_ string) *messages.SessionContext { return want }})

	in := messages.OutboundMessage{ChatID: "c1", Kind: messages.OutCard}
	id, err := em.SendCard(context.Background(), in)
	if err != nil {
		t.Fatalf("SendCard err: %v", err)
	}
	if id != "msg-123" {
		t.Errorf("msgID = %q, want msg-123", id)
	}
	if fc.lastCard.SessionContext != want {
		t.Error("SendCard path did not stamp SessionContext")
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

// TestEmitter_StamperCoLocatesUsageFromMsg covers the F-55 fix:
// when the stamper's SessionContext.Usage is nil but the message
// carries Usage (typical on OutResult after gateway.Translate),
// the emitter must copy msg.Usage across so the footer render
// path can pick it up via ctx.Usage. Without this, Line 2 of the
// footer silently drops for usage-bearing events.
func TestEmitter_StamperCoLocatesUsageFromMsg(t *testing.T) {
	want := &messages.UsageInfo{InputTokens: 100, OutputTokens: 200}
	fc := &fakeChannel{name: "test"}
	em := New(fc, Options{
		Stamper: func(_ string) *messages.SessionContext {
			// Stamper returns SC without Usage — simulates a
			// stamper that doesn't see msg.Usage.
			return &messages.SessionContext{Agent: "claude", Model: "opus"}
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
	if fc.lastSent.SessionContext == nil {
		t.Fatal("SessionContext was not stamped")
	}
	if fc.lastSent.SessionContext.Usage != want {
		t.Errorf("SessionContext.Usage = %+v, want %+v (F-55 co-location)", fc.lastSent.SessionContext.Usage, want)
	}
}

// TestEmitter_StamperCoLocateDoesNotOverwrite verifies the
// stamper-set Usage is preserved if non-nil (caller or stamper
// already had it; emitter must not clobber).
func TestEmitter_StamperCoLocateDoesNotOverwrite(t *testing.T) {
	stamperSC := &messages.SessionContext{
		Agent: "claude",
		Usage: &messages.UsageInfo{InputTokens: 1, OutputTokens: 1},
	}
	msgUsage := &messages.UsageInfo{InputTokens: 999, OutputTokens: 999}

	fc := &fakeChannel{name: "test"}
	em := New(fc, Options{
		Stamper: func(_ string) *messages.SessionContext { return stamperSC },
	})

	_ = em.Send(context.Background(), messages.OutboundMessage{
		ChatID: "c1",
		Kind:   messages.OutResult,
		Usage:  msgUsage,
	})
	if fc.lastSent.SessionContext.Usage != stamperSC.Usage {
		t.Errorf("SessionContext.Usage = %+v, want stamper-set %+v (no overwrite)", fc.lastSent.SessionContext.Usage, stamperSC.Usage)
	}
	if fc.lastSent.SessionContext.Usage == msgUsage {
		t.Error("emitter should not overwrite stamper-set Usage with msg.Usage")
	}
}