package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
)

// TestHandleThink_TogglesMode covers the three accepted arg
// forms and verifies that ChatSession.ThinkMode flips correctly.
// Mirrors TestHandleWatch_TogglesMode exactly so future drift
// between /watch and /think handler patterns is obvious.
//
// Each sub-test uses a fresh chatID so prior state (left over by
// the previous sub-test) doesn't bleed into the "unknown arg
// doesn't mutate" assertion below.
func TestHandleThink_TogglesMode(t *testing.T) {
	mgr, ch := newTestManager(t, false)
	ctx := context.Background()

	t.Run("on sets ThinkModeShow", func(t *testing.T) {
		ch.sends = nil
		chatID := "oc_on"
		msg := &InboundMessage{ChatID: chatID, Text: "/think on"}
		res, err := handleThink(ctx, mgr, ch, msg, []string{"on"}, "claude")
		if err != nil {
			t.Fatalf("handleThink returned error: %v", err)
		}
		if !res.Consumed {
			t.Errorf("expected Consumed=true")
		}
		cs := mgr.Get(chatID)
		if cs == nil {
			t.Fatal("expected ChatSession to exist after /think on")
		}
		if cs.ThinkMode() != chatsession.ThinkModeShow {
			t.Errorf("ThinkMode = %q, want ThinkModeShow", cs.ThinkMode())
		}
		if !strings.Contains(ch.LastText(), "show") {
			t.Errorf("reply should mention 'show', got %q", ch.LastText())
		}
	})

	t.Run("off sets ThinkModeHide", func(t *testing.T) {
		ch.sends = nil
		chatID := "oc_off"
		msg := &InboundMessage{ChatID: chatID, Text: "/think off"}
		res, err := handleThink(ctx, mgr, ch, msg, []string{"off"}, "claude")
		if err != nil {
			t.Fatalf("handleThink returned error: %v", err)
		}
		if !res.Consumed {
			t.Errorf("expected Consumed=true")
		}
		cs := mgr.Get(chatID)
		if cs == nil {
			t.Fatal("expected ChatSession to exist after /think off")
		}
		if cs.ThinkMode() != chatsession.ThinkModeHide {
			t.Errorf("ThinkMode = %q, want ThinkModeHide", cs.ThinkMode())
		}
		if !strings.Contains(ch.LastText(), "hide") {
			t.Errorf("reply should mention 'hide', got %q", ch.LastText())
		}
	})

	t.Run("no-arg reports current mode", func(t *testing.T) {
		ch.sends = nil
		chatID := "oc_noarg"
		msg := &InboundMessage{ChatID: chatID, Text: "/think"}
		res, err := handleThink(ctx, mgr, ch, msg, nil, "claude")
		if err != nil {
			t.Fatalf("handleThink returned error: %v", err)
		}
		if !res.Consumed {
			t.Errorf("expected Consumed=true")
		}
		if !strings.Contains(ch.LastText(), "Current think mode") {
			t.Errorf("reply should report current mode, got %q", ch.LastText())
		}
	})

	t.Run("unknown arg replies with usage", func(t *testing.T) {
		ch.sends = nil
		chatID := "oc_unknown"
		// Fresh chat: prior ThinkMode is the safe default
		// (ThinkModeShow) since no other sub-test has touched it.
		msg := &InboundMessage{ChatID: chatID, Text: "/think maybe"}
		res, err := handleThink(ctx, mgr, ch, msg, []string{"maybe"}, "claude")
		if err != nil {
			t.Fatalf("handleThink returned error: %v", err)
		}
		if !res.Consumed {
			t.Errorf("expected Consumed=true")
		}
		if !strings.Contains(ch.LastText(), "Unknown think mode") {
			t.Errorf("reply should warn about unknown mode, got %q", ch.LastText())
		}
		// Negative case: an unknown arg must NOT commit a state
		// mutation. The chat's ThinkMode stays at the default
		// (ThinkModeShow) since this is a fresh chat.
		cs := mgr.Get(chatID)
		if cs == nil {
			t.Fatal("expected ChatSession to exist")
		}
		if cs.ThinkMode() != chatsession.ThinkModeShow {
			t.Errorf("unknown arg mutated ThinkMode to %q, want ThinkModeShow (unchanged)",
				cs.ThinkMode())
		}
	})
}

// TestHandleThink_AcceptsShowHideAliases confirms the semantic
// aliases (`show`/`hide`) parse equivalently to the slash-command
// aliases (`on`/`off`). Lets users pick whichever phrasing they
// remember.
func TestHandleThink_AcceptsShowHideAliases(t *testing.T) {
	mgr, ch := newTestManager(t, false)
	ctx := context.Background()

	for _, arg := range []string{"show", "hide"} {
		ch.sends = nil
		msg := &InboundMessage{ChatID: "oc_alias", Text: "/think " + arg}
		if _, err := handleThink(ctx, mgr, ch, msg, []string{arg}, "claude"); err != nil {
			t.Fatalf("handleThink(%q) returned error: %v", arg, err)
		}
		cs := mgr.Get("oc_alias")
		if cs == nil {
			t.Fatalf("ChatSession missing after /think %s", arg)
		}
		var want chatsession.ThinkMode
		switch arg {
		case "show":
			want = chatsession.ThinkModeShow
		case "hide":
			want = chatsession.ThinkModeHide
		}
		if cs.ThinkMode() != want {
			t.Errorf("after /think %s: ThinkMode = %q, want %q", arg, cs.ThinkMode(), want)
		}
	}
}

// TestHandleThink_CreatesChatSession covers that /think works
// even without prior /cwd. The handler must create a ChatSession
// on demand (mgr.GetOrCreate) so users can opt-in to thinking on
// or off before they've set up a workspace. globalPrimary must
// be propagated so the new ChatSession has a non-empty
// primaryAgent (otherwise /use later would inherit "" → spawn
// failure).
func TestHandleThink_CreatesChatSession(t *testing.T) {
	mgr, ch := newTestManager(t, false)
	ctx := context.Background()

	msg := &InboundMessage{ChatID: "oc_brand_new", Text: "/think on"}
	if _, err := handleThink(ctx, mgr, ch, msg, []string{"on"}, "claude"); err != nil {
		t.Fatalf("handleThink returned error: %v", err)
	}
	cs := mgr.Get("oc_brand_new")
	if cs == nil {
		t.Fatalf("expected ChatSession to be created by /think")
	}
	if cs.PrimaryAgent() != "claude" {
		t.Errorf("PrimaryAgent after lazy create via /think = %q, want %q",
			cs.PrimaryAgent(), "claude")
	}
	if cs.ThinkMode() != chatsession.ThinkModeShow {
		t.Errorf("ThinkMode after lazy create via /think on = %q, want ThinkModeShow",
			cs.ThinkMode())
	}
}

// TestRegisterChatSessionCommands_RegistersThink covers that the
// runtime registration includes the new /think command alongside
// /cwd /use /kill /watch /new.
func TestRegisterChatSessionCommands_RegistersThink(t *testing.T) {
	mgr, ch := newTestManager(t, false)
	gw := New(nil).(*Router)
	RegisterChatSessionCommands(gw, mgr, ch, "claude")

	cmds := gw.ListCommands()
	var found bool
	for _, c := range cmds {
		if c.Name == "think" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("/think not in registered commands; got %v", cmds)
	}
}

// TestHandleThink_DoesNotAffectWatchMode verifies the two per-chat
// toggles are independent: setting ThinkMode must not flip
// WatchMode, and vice versa. Otherwise a /think typo could
// silently change message-watching behaviour.
func TestHandleThink_DoesNotAffectWatchMode(t *testing.T) {
	mgr, ch := newTestManager(t, false)
	ctx := context.Background()

	// Set WatchMode=All via /watch on, then flip ThinkMode off.
	msg1 := &InboundMessage{ChatID: "oc_indep", Text: "/watch on"}
	if _, err := handleWatch(ctx, mgr, ch, msg1, []string{"on"}, "claude"); err != nil {
		t.Fatalf("/watch on failed: %v", err)
	}
	msg2 := &InboundMessage{ChatID: "oc_indep", Text: "/think off"}
	if _, err := handleThink(ctx, mgr, ch, msg2, []string{"off"}, "claude"); err != nil {
		t.Fatalf("/think off failed: %v", err)
	}

	cs := mgr.Get("oc_indep")
	if cs == nil {
		t.Fatal("ChatSession missing")
	}
	if cs.WatchMode() != chatsession.WatchModeAll {
		t.Errorf("/think off flipped WatchMode to %q, want WatchModeAll (independent)",
			cs.WatchMode())
	}
	if cs.ThinkMode() != chatsession.ThinkModeHide {
		t.Errorf("ThinkMode after /think off = %q, want ThinkModeHide",
			cs.ThinkMode())
	}
}