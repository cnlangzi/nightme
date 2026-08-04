package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
)

// TestHandleWatch_TogglesMode covers the three accepted arg
// forms and verifies that ChatSession.WatchMode flips correctly.
func TestHandleWatch_TogglesMode(t *testing.T) {
	mgr, ch := newTestManager(t, false)
	ctx := context.Background()

	t.Run("on sets WatchModeAll", func(t *testing.T) {
		ch.sends = nil // reset
		msg := &InboundMessage{ChatID: "oc_test", Text: "/watch on"}
		res, err := handleWatch(ctx, mgr, ch, msg, []string{"on"}, "claude")
		if err != nil {
			t.Fatalf("handleWatch returned error: %v", err)
		}
		if !res.Consumed {
			t.Errorf("expected Consumed=true")
		}
		cs := mgr.Get("oc_test")
		if cs == nil {
			t.Fatal("expected ChatSession to exist after /watch on")
		}
		if cs.WatchMode() != chatsession.WatchModeAll {
			t.Errorf("WatchMode = %q, want WatchModeAll", cs.WatchMode())
		}
		if !strings.Contains(ch.LastText(), "all") {
			t.Errorf("reply should mention 'all', got %q", ch.LastText())
		}
	})

	t.Run("off sets WatchModeMention", func(t *testing.T) {
		ch.sends = nil
		msg := &InboundMessage{ChatID: "oc_test", Text: "/watch off"}
		res, err := handleWatch(ctx, mgr, ch, msg, []string{"off"}, "claude")
		if err != nil {
			t.Fatalf("handleWatch returned error: %v", err)
		}
		if !res.Consumed {
			t.Errorf("expected Consumed=true")
		}
		cs := mgr.Get("oc_test")
		if cs == nil {
			t.Fatal("expected ChatSession to exist after /watch off")
		}
		if cs.WatchMode() != chatsession.WatchModeMention {
			t.Errorf("WatchMode = %q, want WatchModeMention", cs.WatchMode())
		}
		if !strings.Contains(ch.LastText(), "mention") {
			t.Errorf("reply should mention 'mention', got %q", ch.LastText())
		}
	})

	t.Run("no-arg reports current mode", func(t *testing.T) {
		ch.sends = nil
		msg := &InboundMessage{ChatID: "oc_test", Text: "/watch"}
		res, err := handleWatch(ctx, mgr, ch, msg, nil, "claude")
		if err != nil {
			t.Fatalf("handleWatch returned error: %v", err)
		}
		if !res.Consumed {
			t.Errorf("expected Consumed=true")
		}
		if !strings.Contains(ch.LastText(), "Current watch mode") {
			t.Errorf("reply should report current mode, got %q", ch.LastText())
		}
	})

	t.Run("unknown arg replies with usage", func(t *testing.T) {
		ch.sends = nil
		msg := &InboundMessage{ChatID: "oc_test", Text: "/watch maybe"}
		res, err := handleWatch(ctx, mgr, ch, msg, []string{"maybe"}, "claude")
		if err != nil {
			t.Fatalf("handleWatch returned error: %v", err)
		}
		if !res.Consumed {
			t.Errorf("expected Consumed=true")
		}
		if !strings.Contains(ch.LastText(), "Unknown watch mode") {
			t.Errorf("reply should warn about unknown mode, got %q", ch.LastText())
		}
	})
}

// TestHandleWatch_CreatesChatSession covers that /watch works
// even without prior /cwd. The handler must create a ChatSession
// on demand (mgr.GetOrCreate) so users can opt-in to watching
// before they've set up a workspace. globalPrimary must be
// propagated so the new ChatSession has a non-empty primaryAgent
// (otherwise /use later would inherit "" → spawn failure).
func TestHandleWatch_CreatesChatSession(t *testing.T) {
	mgr, ch := newTestManager(t, false)
	ctx := context.Background()

	msg := &InboundMessage{ChatID: "oc_brand_new", Text: "/watch on"}
	if _, err := handleWatch(ctx, mgr, ch, msg, []string{"on"}, "claude"); err != nil {
		t.Fatalf("handleWatch returned error: %v", err)
	}
	cs := mgr.Get("oc_brand_new")
	if cs == nil {
		t.Fatalf("expected ChatSession to be created by /watch")
	}
	if cs.PrimaryAgent() != "claude" {
		t.Errorf("PrimaryAgent after lazy create via /watch = %q, want %q",
			cs.PrimaryAgent(), "claude")
	}
}

// TestRegisterChatSessionCommands_RegistersWatch covers that
// the runtime registration includes the new /watch command
// alongside /cwd /use /kill.
func TestRegisterChatSessionCommands_RegistersWatch(t *testing.T) {
	mgr, ch := newTestManager(t, false)
	gw := New(nil).(*Router)
	RegisterChatSessionCommands(gw, mgr, ch, "claude")

	cmds := gw.ListCommands()
	var found bool
	for _, c := range cmds {
		if c.Name == "watch" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("/watch not in registered commands; got %v", cmds)
	}
}
