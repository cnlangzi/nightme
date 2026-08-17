// ctor_test.go — direct tests for DefaultDeps, WithChannel,
// NewMessageDispatcher, and the WireRuntimeCallbacksAndRestore
// PromptEndBus subscriber (Phase 2.5: the MarkPromptDone
// wrapper was deleted — the runtime now calls ch.OnPromptEnded
// directly inside the subscriber closure).
//
// These tests close coverage gaps that cmd/nightme/run_test.go
// left implicit: every exported constructor / dispatcher
// path now has at least one regression guard at the runtime
// layer, decoupled from the full Runner.Run lifecycle.

package runtime

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"reflect"
	"runtime"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentsession"
	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/channel/echo"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/gateway/outbound"
	"github.com/cnlangzi/nightme/internal/messages"
)

// TestDefaultDeps_HasProductionHooks verifies the production
// Deps wire every hook the orchestrator depends on. A test
// that drives the full Runner.Run will silently misbehave if
// any of these are nil — better to catch the regression here.
func TestDefaultDeps_HasProductionHooks(t *testing.T) {
	d := DefaultDeps()
	if d.LoadConfig == nil {
		t.Error("LoadConfig is nil")
	}
	if d.OpenChatSessions == nil {
		t.Error("OpenChatSessions is nil")
	}
	if d.OpenAgentSessions == nil {
		t.Error("OpenAgentSessions is nil")
	}
	if d.BuildAgents == nil {
		t.Error("BuildAgents is nil")
	}
	if d.NewChannel == nil {
		t.Error("NewChannel is nil")
	}
}

// TestWithChannel_Echo_SkipsFeishuLogin verifies the echo
// channel path doesn't error out on missing Feishu
// credentials (echo is a smoke-test channel with no network).
// The daemon expects SkipFeishuLogin to be true so the
// orchestrator skips the cfg.Feishu.AppID guard.
func TestWithChannel_Echo_SkipsFeishuLogin(t *testing.T) {
	d := DefaultDeps()
	d, err := WithChannel(d, "echo")
	if err != nil {
		t.Fatalf("WithChannel echo: %v", err)
	}
	if !d.SkipFeishuLogin {
		t.Error("SkipFeishuLogin = false, want true (echo path skips Feishu creds check)")
	}
	// NewChannel should now produce a working echo.Channel.
	ch, err := d.NewChannel(nil)
	if err != nil {
		t.Fatalf("NewChannel: %v", err)
	}
	if _, ok := ch.(*echo.Channel); !ok {
		t.Errorf("NewChannel returned %T, want *echo.Channel", ch)
	}
}

// TestWithChannel_FeishuDefault verifies the feishu branch
// leaves SkipFeishuLogin at zero (production requires
// Feishu.AppID + AppSecret to be set).
func TestWithChannel_FeishuDefault(t *testing.T) {
	d := DefaultDeps()
	d, err := WithChannel(d, "feishu")
	if err != nil {
		t.Fatalf("WithChannel feishu: %v", err)
	}
	if d.SkipFeishuLogin {
		t.Error("SkipFeishuLogin = true, want false (feishu path requires credentials)")
	}
}

// TestWithChannel_EmptyIsFeishu documents the legacy behaviour:
// an empty channelName is treated as "feishu" (the default).
func TestWithChannel_EmptyIsFeishu(t *testing.T) {
	d := DefaultDeps()
	d, err := WithChannel(d, "")
	if err != nil {
		t.Fatalf("WithChannel \"\": %v", err)
	}
	if d.SkipFeishuLogin {
		t.Error("SkipFeishuLogin = true, want false (empty defaults to feishu)")
	}
}

// TestWithChannel_UnknownErrors verifies the unknown-name
// branch surfaces a friendly error so the CLI shell can exit
// non-zero instead of silently defaulting to feishu.
func TestWithChannel_UnknownErrors(t *testing.T) {
	d := DefaultDeps()
	_, err := WithChannel(d, "slack")
	if err == nil {
		t.Fatal("WithChannel slack: want error, got nil")
	}
	if !contains(err.Error(), "slack") {
		t.Errorf("error %q should mention the bad channel name", err)
	}
}

// TestPromptEndBus_DelegatesToChannelOnPromptEnded pins the
// Phase 2.5 contract: WireRuntimeCallbacksAndRestore's
// PromptEndBus subscriber invokes ch.OnPromptEnded for each
// PromptEndedEvent. The previous MarkPromptDone wrapper
// was deleted because it added an indirection (return
// ch.OnPromptEnded) with no type-assertion benefit — the
// channel interface already exposes OnPromptEnded.
//
// To test: capture the OnPromptEnded calls via a stub
// Channel, create a ChatSession AFTER the wiring so
// WithOnCreate fires the subscriber, publish a
// PromptEndedEvent, assert the call landed.
func TestPromptEndBus_DelegatesToChannelOnPromptEnded(t *testing.T) {
	mgr := chatsession.NewManager().WithPrimaryAgent("claude")
	stub := &capturingChannel{name: "capture", record: []string{}}

	if err := WireRuntimeCallbacksAndRestore(
		mgr,
		outbound.New(echo.New("test", io.Discard), outbound.Options{}),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		chatsession.GitStatusDeps{},
		stub,
	); err != nil {
		t.Fatalf("WireRuntimeCallbacksAndRestore: %v", err)
	}

	// Create the ChatSession AFTER wiring so WithOnCreate
	// fires and installs the PromptEndBus subscriber.
	cs, _ := mgr.GetOrCreate("oc_prompt_end", "claude")

	cs.PromptEndBus.Publish(agentsession.PromptEndedEvent{
		ChatID:    cs.ChatID,
		UserMsgID: "om_pe_test",
	})

	if len(stub.record) != 1 {
		t.Fatalf("expected 1 OnPromptEnded call, got %d", len(stub.record))
	}
	if stub.record[0] != "oc_prompt_end|om_pe_test" {
		t.Errorf("OnPromptEnded call args = %q, want %q", stub.record[0], "oc_prompt_end|om_pe_test")
	}
}

// TestPromptEndBus_NonFeishuChannelIsNoOp pins the contract
// that echo's OnPromptEnded is a no-op — a future regression
// that adds logging/side-effects to echo's no-op would
// change behavior under WireRuntimeCallbacksAndRestore.
func TestPromptEndBus_NonFeishuChannelIsNoOp(t *testing.T) {
	mgr := chatsession.NewManager().WithPrimaryAgent("claude")
	ch := echo.New("test", io.Discard) // echo.OnPromptEnded is no-op

	if err := WireRuntimeCallbacksAndRestore(
		mgr,
		outbound.New(ch, outbound.Options{}),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		chatsession.GitStatusDeps{},
		ch,
	); err != nil {
		t.Fatalf("WireRuntimeCallbacksAndRestore: %v", err)
	}

	// Create the ChatSession AFTER wiring so WithOnCreate
	// fires and installs the subscriber.
	cs, _ := mgr.GetOrCreate("oc_echo_pe", "claude")

	// Must not panic. echo.OnPromptEnded is a no-op, so this
	// returns cleanly.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("non-Feishu PromptEnd panicked: %v", r)
		}
	}()
	cs.PromptEndBus.Publish(agentsession.PromptEndedEvent{
		ChatID:    cs.ChatID,
		UserMsgID: "om_echo_pe",
	})
}

// TestNewMessageDispatcher_WatchModeDrop pins F-watch §3.1.1:
// when the per-chat WatchMode rejects a message (group chat
// without mention + ChatSession already exists with default
// WatchModeMention), the dispatcher must return nil WITHOUT
// proceeding to GetOrCreate / spawn / queue. We assert this
// by checking the channel saw no outbound events.
//
// The drop branch only fires when a ChatSession already
// exists — AcceptInbound lets the first non-mentioned
// message through so the dispatcher can reply "send /cwd
// first". So this test pre-creates the chat.
func TestNewMessageDispatcher_WatchModeDrop(t *testing.T) {
	mgr := chatsession.NewManager().WithPrimaryAgent("claude")
	ch := echo.New("test", io.Discard)
	em := outbound.New(ch, outbound.Options{})

	// Pre-create the ChatSession so AcceptInbound can read its
	// WatchMode. Default is WatchModeMention, which means a
	// non-mentioned group message is dropped on the floor.
	if _, err := mgr.GetOrCreate("oc_drop", "claude"); err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	d := NewMessageDispatcher(mgr, em, ch, "claude", slog.New(slog.NewTextHandler(io.Discard, nil)))

	err := d(context.Background(), &messages.InboundMessage{
		ChatID:     "oc_drop",
		UserID:     "u_1",
		MessageID:  "om_1",
		Text:       "plain message without mention",
		HasMention: false,
	})
	if err != nil {
		t.Fatalf("dispatcher returned error: %v", err)
	}
	if got := len(ch.Record()); got != 0 {
		t.Errorf("dropped message produced %d outbound events; want 0", got)
	}
}

// TestNewMessageDispatcher_NoSelectedCwdErr verifies the
// ErrNoSelectedCwd branch: when the chat has no workspace
// selected, the dispatcher must surface a friendly
// "send /cwd first" message via the emitter — NOT silently
// drop, NOT panic.
func TestNewMessageDispatcher_NoSelectedCwdErr(t *testing.T) {
	mgr := chatsession.NewManager().WithPrimaryAgent("claude")
	ch := echo.New("test", io.Discard)
	em := outbound.New(ch, outbound.Options{})

	d := NewMessageDispatcher(mgr, em, ch, "claude", slog.New(slog.NewTextHandler(io.Discard, nil)))

	err := d(context.Background(), &messages.InboundMessage{
		ChatID:     "oc_nocwd",
		UserID:     "u_1",
		MessageID:  "om_1",
		Text:       "hello",
		HasMention: true,
	})
	if err != nil {
		t.Fatalf("dispatcher returned error: %v", err)
	}
	got := ch.Record()
	if len(got) != 1 {
		t.Fatalf("want 1 OutReply (no-workspace guidance), got %d", len(got))
	}
	if !contains(got[0].Text, "/cwd") {
		t.Errorf("error reply %q should mention /cwd", got[0].Text)
	}
}

// TestNewMessageDispatcher_AcceptInboundGateAccepts verifies
// the positive path of the same gate — a mentioned group
// message passes through to GetOrCreate.
func TestNewMessageDispatcher_AcceptInboundGateAccepts(t *testing.T) {
	mgr := chatsession.NewManager().WithPrimaryAgent("claude")
	ch := echo.New("test", io.Discard)
	em := outbound.New(ch, outbound.Options{})

	d := NewMessageDispatcher(mgr, em, ch, "claude", slog.New(slog.NewTextHandler(io.Discard, nil)))

	err := d(context.Background(), &messages.InboundMessage{
		ChatID:     "oc_mention",
		UserID:     "u_1",
		MessageID:  "om_1",
		Text:       "hello",
		HasMention: true,
	})
	if err != nil {
		t.Fatalf("dispatcher returned error: %v", err)
	}
	if cs := mgr.Get("oc_mention"); cs == nil {
		t.Error("mentioned message should allocate ChatSession; got nil")
	}
	// One outbound event: the "no workspace" reply.
	if got := len(ch.Record()); got != 1 {
		t.Errorf("want 1 OutReply (no-workspace), got %d", got)
	}
}

// TestNewMessageDispatcher_NilMsg pins a corner-case contract:
// a nil InboundMessage must be treated as a no-op, not a
// nil-deref panic. Production never sends nil here, but a
// future transport mis-wiring might, and the dispatcher is
// the wrong place to surface that error.
func TestNewMessageDispatcher_NilMsg(t *testing.T) {
	mgr := chatsession.NewManager().WithPrimaryAgent("claude")
	ch := echo.New("test", io.Discard)
	em := outbound.New(ch, outbound.Options{})

	d := NewMessageDispatcher(mgr, em, ch, "claude", slog.New(slog.NewTextHandler(io.Discard, nil)))

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil msg panicked: %v", r)
		}
	}()
	if err := d(context.Background(), nil); err != nil {
		t.Errorf("nil msg returned %v, want nil", err)
	}
}

// contains is a tiny stdlib-free substring check so this test
// file doesn't pull in strings just for two assertions.
func contains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// funcPC was used by the deleted TestMarkPromptDone_NoOpIsShared
// — kept as a stub for future tests that need to compare
// method-value code pointers (rare but cheap to retain).
var _ = func(t *testing.T, fn any) uintptr {
	t.Helper()
	v := reflect.ValueOf(fn)
	if v.Kind() != reflect.Func {
		t.Fatalf("funcPC: not a function value (kind=%s)", v.Kind())
	}
	pc := v.Pointer()
	if pc == 0 {
		t.Fatalf("funcPC: zero pointer for %v", fn)
	}
	if f := runtime.FuncForPC(pc); f == nil {
		t.Fatalf("funcPC: no runtime.Func for PC=%x", pc)
	}
	return pc
}

// capturingChannel is a minimal channel.Channel stub that
// records every OnPromptEnded call so TestPromptEndBus_*
// tests can assert the subscriber routed the event through
// ch.OnPromptEnded (Phase 2.5 contract). Every other method
// is a trivial fallback — the tests don't exercise Send.
type capturingChannel struct {
	name   string
	mu     sync.Mutex
	record []string
}

func (c *capturingChannel) Name() string                  { return c.name }
func (c *capturingChannel) Start(_ context.Context) error { return nil }
func (c *capturingChannel) Stop(_ context.Context) error  { return nil }
func (c *capturingChannel) Incoming() <-chan messages.InboundMessage {
	return make(<-chan messages.InboundMessage)
}
func (c *capturingChannel) Send(_ context.Context, _ messages.OutboundMessage) error {
	return nil
}
func (c *capturingChannel) OnPromptEnded(_ context.Context, chatID, msgID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record = append(c.record, chatID+"|"+msgID)
}
func (c *capturingChannel) HealthSnapshot() (string, json.RawMessage, error) {
	return c.name, json.RawMessage("{}"), nil
}
func (c *capturingChannel) SetLogger(_ *slog.Logger) {}
func (c *capturingChannel) BuildBlocks(text string, _ []messages.Attachment) []agent.ContentBlock {
	if text == "" {
		return nil
	}
	return []agent.ContentBlock{{Type: agent.ContentText, Text: text}}
}

// channel.Channel contract compliance — compiler-checked at
// build time. (The interface is satisfied implicitly; this
// declaration is documentation.)
var _ channel.Channel = (*capturingChannel)(nil)
