// F-CLAUDE-PRINT-002: the Emitter is now a pure transport — no
// StatusBar Source, no AttachIfMissing. Every layer that builds
// an OutboundMessage (chatsession event hook, slash-command
// dispatchers, one-shot dispatchers) is responsible for
// filling out.GitStatus themselves.
//
// These tests verify the passthrough: every field on the input
// OutboundMessage reaches the channel unchanged.
package outbound

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
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

// Channel-interface extensions (Phase 2.1 + 2.2). fakeChannel
// has no live state — all three are trivial fallbacks.
func (f *fakeChannel) OnPromptEnded(_ context.Context, _, _ string)        {}
func (f *fakeChannel) HealthSnapshot() (string, json.RawMessage, error) {
	return f.name, json.RawMessage("{}"), nil
}
func (f *fakeChannel) SetLogger(_ *slog.Logger) {}
func (f *fakeChannel) BuildBlocks(text string, _ []messages.Attachment) []agent.ContentBlock {
	if text == "" {
		return nil
	}
	return []agent.ContentBlock{{Type: agent.ContentText, Text: text}}
}

func TestEmitter_Passthrough_Send(t *testing.T) {
	// Zero-value Options (no Source, no anything) — Emitter
	// is a pure passthrough. The caller-built OutboundMessage
	// (with whatever fields it has) goes straight to
	// Channel.Send, untouched.
	fc := &fakeChannel{name: "test"}
	em := New(fc, Options{})

	in := messages.OutboundMessage{
		ChatID:    "c1",
		Kind:      messages.OutReply,
		Text:      "hi",
		Model:     "claude-opus-4-5",
		SessionID: "sess-123",
		Usage:     &agent.UsageInfo{InputTokens: 10, OutputTokens: 20},
		GitStatus: &messages.GitStatus{Workspace: "/repo"},
	}
	if err := em.Send(context.Background(), in); err != nil {
		t.Fatalf("Send returned err = %v", err)
	}
	if atomic.LoadInt32(&fc.sendCalls) != 1 {
		t.Fatalf("sendCalls = %d, want 1", fc.sendCalls)
	}

	// Every field passes through unchanged — the Emitter doesn't
	// synthesize / overwrite / strip anything.
	if fc.lastSent.Text != "hi" {
		t.Errorf("Text = %q, want hi", fc.lastSent.Text)
	}
	if fc.lastSent.Model != "claude-opus-4-5" {
		t.Errorf("Model = %q, want claude-opus-4-5", fc.lastSent.Model)
	}
	if fc.lastSent.SessionID != "sess-123" {
		t.Errorf("SessionID = %q, want sess-123", fc.lastSent.SessionID)
	}
	if fc.lastSent.Usage == nil || fc.lastSent.Usage.InputTokens != 10 {
		t.Errorf("Usage = %+v, want InputTokens=10", fc.lastSent.Usage)
	}
	if fc.lastSent.GitStatus == nil || fc.lastSent.GitStatus.Workspace != "/repo" {
		t.Errorf("GitStatus = %+v, want Workspace=/repo", fc.lastSent.GitStatus)
	}
}

func TestEmitter_Passthrough_SendCard(t *testing.T) {
	// SendCard also pure passthrough — returns the channel's
	// messageID and forwards the message.
	fc := &fakeChannel{name: "test", cardMsgID: "msg-123"}
	em := New(fc, Options{})

	in := messages.OutboundMessage{
		ChatID:  "c1",
		Kind:    messages.OutCard,
		Text:    "card-body",
		ReplyTo: "user-msg-1",
	}
	id, err := em.SendCard(context.Background(), in)
	if err != nil {
		t.Fatalf("SendCard err: %v", err)
	}
	if id != "msg-123" {
		t.Errorf("msgID = %q, want msg-123", id)
	}
	if atomic.LoadInt32(&fc.cardCalls) != 1 {
		t.Errorf("cardCalls = %d, want 1", fc.cardCalls)
	}
	if fc.lastCard.Text != "card-body" {
		t.Errorf("Card text = %q, want card-body", fc.lastCard.Text)
	}
}

func TestEmitter_NoMysteryMutation(t *testing.T) {
	// The Emitter must NOT inject any field the caller didn't set.
	// This is the F-CLAUDE-PRINT-002 invariant: no runtime
	// "stamp" step, no Source lookup, no AttachIfMissing.
	// StatusBar / Usage / GitStatus are all nil (caller didn't
	// set them), and the Emitter must not fill them in.
	fc := &fakeChannel{name: "test"}
	em := New(fc, Options{})

	in := messages.OutboundMessage{
		ChatID: "c1",
		Kind:   messages.OutReply,
		Text:   "hi",
	}
	_ = em.Send(context.Background(), in)

	// All optional fields stay nil — Emitter didn't fill them.
	if fc.lastSent.GitStatus != nil {
		t.Errorf("Emitted GitStatus = %+v, want nil (no source)", fc.lastSent.GitStatus)
	}
	if fc.lastSent.Usage != nil {
		t.Errorf("Emitted Usage = %+v, want nil (no source)", fc.lastSent.Usage)
	}
}

func TestEmitter_ChannelErrorPropagates(t *testing.T) {
	// Channel.Send error propagates up unchanged — Emitter doesn't
	// swallow it.
	fc := &fakeChannel{name: "test", sendErr: errors.New("channel dead")}
	em := New(fc, Options{})

	in := messages.OutboundMessage{ChatID: "c1", Kind: messages.OutReply}
	if err := em.Send(context.Background(), in); err == nil {
		t.Fatal("Send returned nil, want error")
	}
}
