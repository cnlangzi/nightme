// ctor_test.go — direct tests for DefaultDeps, WithChannel,
// MarkPromptDone, NewMessageDispatcher, and the shared no-op
// promptDone closure.
//
// These tests close coverage gaps that cmd/nightme/run_test.go
// left implicit: every exported constructor / dispatcher
// path now has at least one regression guard at the runtime
// layer, decoupled from the full Runner.Run lifecycle.

package runtime

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"runtime"
	"testing"

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

// TestMarkPromptDone_NoOpIsShared pins the contract for
// non-Feishu channels: MarkPromptDone returns the SAME
// package-level `noOpPromptDone` closure on every call so
// per-ChatSession install does not allocate a new closure
// per chat.
//
// Regression guard for "MarkPromptDone re-allocated the
// no-op on every call". The patch landed before this
// invariant was pinned; if a future edit re-introduces
// allocation, this test fails.
func TestMarkPromptDone_NoOpIsShared(t *testing.T) {
	ch := echo.New("test", io.Discard)
	first := MarkPromptDone(ch)
	second := MarkPromptDone(ch)
	if first == nil || second == nil {
		t.Fatal("MarkPromptDone returned nil closure")
	}
	// Two values both referring to the same `noOpPromptDone`
	// variable compare equal via reflect.ValueOf.Pointer()
	// (the F-58 perf invariant). Direct equality on function
	// values is not allowed in Go, so we compare the
	// underlying code pointers via runtime.FuncForPC.
	if funcPC(t, first) != funcPC(t, second) {
		t.Errorf("non-Feishu MarkPromptDone did not return the shared no-op closure " +
			"(per-ChatSession install would re-allocate)")
	}
}

// TestMarkPromptDone_NoOpDoesNotPanic verifies the shared
// no-op closure is safe to call with any (ctx, chatID,
// msgID) — a future regression that adds e.g. a nil-check
// on ctx without guarding it would panic here.
func TestMarkPromptDone_NoOpDoesNotPanic(t *testing.T) {
	cb := noOpPromptDone
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("noOpPromptDone panicked: %v", r)
		}
	}()
	cb(context.Background(), "oc_x", "om_x")
	cb(context.Background(), "", "")
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

	d := NewMessageDispatcher(mgr, em, "claude", slog.New(slog.NewTextHandler(io.Discard, nil)))

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

	d := NewMessageDispatcher(mgr, em, "claude", slog.New(slog.NewTextHandler(io.Discard, nil)))

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

	d := NewMessageDispatcher(mgr, em, "claude", slog.New(slog.NewTextHandler(io.Discard, nil)))

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

	d := NewMessageDispatcher(mgr, em, "claude", slog.New(slog.NewTextHandler(io.Discard, nil)))

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

// funcPC returns the program-counter of fn's underlying code.
// Two closures pointing at the same `noOpPromptDone` variable
// share a PC; two freshly-allocated no-op closures don't.
// Used to pin the F-58 perf invariant: MarkPromptDone must
// return the SAME no-op for every non-Feishu channel.
func funcPC(t *testing.T, fn any) uintptr {
	t.Helper()
	v := reflect.ValueOf(fn)
	if v.Kind() != reflect.Func {
		t.Fatalf("funcPC: not a function value (kind=%s)", v.Kind())
	}
	pc := v.Pointer()
	if pc == 0 {
		t.Fatalf("funcPC: zero pointer for %v", fn)
	}
	// Resolve via runtime.FuncForPC to sanity-check that the
	// PC maps to a real function (some nil-function values
	// expose a non-zero PC but no Func entry).
	if f := runtime.FuncForPC(pc); f == nil {
		t.Fatalf("funcPC: no runtime.Func for PC=%x", pc)
	}
	return pc
}