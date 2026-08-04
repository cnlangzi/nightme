package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
)

// TestHandleTools_TogglesMode covers the three accepted arg
// forms and verifies that ChatSession.ToolsMode flips correctly.
// Mirrors TestHandleThink_TogglesMode so future drift between
// /think and /tools handler patterns is obvious. Note the
// direction is OPPOSITE: /think defaults to Show; /tools defaults
// to Hide.
//
// Each sub-test uses a fresh chatID so prior state (left over by
// the previous sub-test) doesn't bleed into the "unknown arg
// doesn't mutate" assertion below.
func TestHandleTools_TogglesMode(t *testing.T) {
	mgr, ch := newTestManager(t, false)
	ctx := context.Background()

	t.Run("on sets ToolsModeShow", func(t *testing.T) {
		ch.sends = nil
		chatID := "oc_tools_on"
		msg := &InboundMessage{ChatID: chatID, Text: "/tools on"}
		res, err := handleTools(ctx, mgr, ch, msg, []string{"on"}, "claude")
		if err != nil {
			t.Fatalf("handleTools returned error: %v", err)
		}
		if !res.Consumed {
			t.Errorf("expected Consumed=true")
		}
		cs := mgr.Get(chatID)
		if cs == nil {
			t.Fatal("expected ChatSession to exist after /tools on")
		}
		if cs.ToolsMode() != chatsession.ToolsModeShow {
			t.Errorf("ToolsMode = %q, want ToolsModeShow", cs.ToolsMode())
		}
		if !strings.Contains(ch.LastText(), "show") {
			t.Errorf("reply should mention 'show', got %q", ch.LastText())
		}
	})

	t.Run("off sets ToolsModeHide", func(t *testing.T) {
		ch.sends = nil
		chatID := "oc_tools_off"
		msg := &InboundMessage{ChatID: chatID, Text: "/tools off"}
		res, err := handleTools(ctx, mgr, ch, msg, []string{"off"}, "claude")
		if err != nil {
			t.Fatalf("handleTools returned error: %v", err)
		}
		if !res.Consumed {
			t.Errorf("expected Consumed=true")
		}
		cs := mgr.Get(chatID)
		if cs == nil {
			t.Fatal("expected ChatSession to exist after /tools off")
		}
		if cs.ToolsMode() != chatsession.ToolsModeHide {
			t.Errorf("ToolsMode = %q, want ToolsModeHide", cs.ToolsMode())
		}
		if !strings.Contains(ch.LastText(), "hide") {
			t.Errorf("reply should mention 'hide', got %q", ch.LastText())
		}
	})

	t.Run("no-arg reports current mode", func(t *testing.T) {
		ch.sends = nil
		chatID := "oc_tools_noarg"
		msg := &InboundMessage{ChatID: chatID, Text: "/tools"}
		res, err := handleTools(ctx, mgr, ch, msg, nil, "claude")
		if err != nil {
			t.Fatalf("handleTools returned error: %v", err)
		}
		if !res.Consumed {
			t.Errorf("expected Consumed=true")
		}
		if !strings.Contains(ch.LastText(), "Current tools mode") {
			t.Errorf("reply should report current mode, got %q", ch.LastText())
		}
	})

	t.Run("unknown arg replies with usage", func(t *testing.T) {
		ch.sends = nil
		chatID := "oc_tools_unknown"
		// Fresh chat: prior ToolsMode is the safe default
		// (ToolsModeHide) since no other sub-test has touched it.
		msg := &InboundMessage{ChatID: chatID, Text: "/tools maybe"}
		res, err := handleTools(ctx, mgr, ch, msg, []string{"maybe"}, "claude")
		if err != nil {
			t.Fatalf("handleTools returned error: %v", err)
		}
		if !res.Consumed {
			t.Errorf("expected Consumed=true")
		}
		if !strings.Contains(ch.LastText(), "Unknown tools mode") {
			t.Errorf("reply should warn about unknown mode, got %q", ch.LastText())
		}
		// Negative case: an unknown arg must NOT commit a state
		// mutation. The chat's ToolsMode stays at the default
		// (ToolsModeHide) since this is a fresh chat.
		cs := mgr.Get(chatID)
		if cs == nil {
			t.Fatal("expected ChatSession to exist")
		}
		if cs.ToolsMode() != chatsession.ToolsModeHide {
			t.Errorf("unknown arg mutated ToolsMode to %q, want ToolsModeHide (unchanged)",
				cs.ToolsMode())
		}
	})
}

// TestHandleTools_AcceptsShowHideAliases confirms the semantic
// aliases (`show`/`hide`) parse equivalently to the slash-command
// aliases (`on`/`off`). Lets users pick whichever phrasing they
// remember. Mirrors TestHandleThink_AcceptsShowHideAliases.
func TestHandleTools_AcceptsShowHideAliases(t *testing.T) {
	mgr, ch := newTestManager(t, false)
	ctx := context.Background()

	for _, arg := range []string{"show", "hide"} {
		ch.sends = nil
		msg := &InboundMessage{ChatID: "oc_tools_alias", Text: "/tools " + arg}
		if _, err := handleTools(ctx, mgr, ch, msg, []string{arg}, "claude"); err != nil {
			t.Fatalf("handleTools(%q) returned error: %v", arg, err)
		}
		cs := mgr.Get("oc_tools_alias")
		if cs == nil {
			t.Fatalf("ChatSession missing after /tools %s", arg)
		}
		var want chatsession.ToolsMode
		switch arg {
		case "show":
			want = chatsession.ToolsModeShow
		case "hide":
			want = chatsession.ToolsModeHide
		}
		if cs.ToolsMode() != want {
			t.Errorf("after /tools %s: ToolsMode = %q, want %q", arg, cs.ToolsMode(), want)
		}
	}
}

// TestHandleTools_CreatesChatSession covers that /tools works
// even without prior /cwd. The handler must create a ChatSession
// on demand (mgr.GetOrCreate) so users can opt-in to tool display
// before they've set up a workspace. globalPrimary must be
// propagated so the new ChatSession has a non-empty primaryAgent
// (otherwise /use later would inherit "" → spawn failure).
// Mirrors TestHandleThink_CreatesChatSession.
func TestHandleTools_CreatesChatSession(t *testing.T) {
	mgr, ch := newTestManager(t, false)
	ctx := context.Background()

	msg := &InboundMessage{ChatID: "oc_tools_brand_new", Text: "/tools on"}
	if _, err := handleTools(ctx, mgr, ch, msg, []string{"on"}, "claude"); err != nil {
		t.Fatalf("handleTools returned error: %v", err)
	}
	cs := mgr.Get("oc_tools_brand_new")
	if cs == nil {
		t.Fatalf("expected ChatSession to be created by /tools")
	}
	if cs.PrimaryAgent() != "claude" {
		t.Errorf("PrimaryAgent after lazy create via /tools = %q, want %q",
			cs.PrimaryAgent(), "claude")
	}
	if cs.ToolsMode() != chatsession.ToolsModeShow {
		t.Errorf("ToolsMode after lazy create via /tools on = %q, want ToolsModeShow",
			cs.ToolsMode())
	}
}

// TestRegisterChatSessionCommands_RegistersTools covers that the
// runtime registration includes the new /tools command alongside
// /cwd /use /kill /watch /think /new.
func TestRegisterChatSessionCommands_RegistersTools(t *testing.T) {
	mgr, ch := newTestManager(t, false)
	gw := New(nil).(*Router)
	RegisterChatSessionCommands(gw, mgr, ch, "claude")

	cmds := gw.ListCommands()
	var found bool
	for _, c := range cmds {
		if c.Name == "tools" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("/tools not in registered commands; got %v", cmds)
	}
}

// TestHandleTools_DoesNotAffectWatchOrThink verifies the three
// per-chat toggles are pairwise independent: setting ToolsMode
// must not flip WatchMode or ThinkMode. Otherwise a /tools typo
// could silently change message-watching or thinking behaviour.
func TestHandleTools_DoesNotAffectWatchOrThink(t *testing.T) {
	mgr, ch := newTestManager(t, false)
	ctx := context.Background()

	// Set WatchMode=All + ThinkMode=Hide via /watch on / /think off,
	// then flip ToolsMode on.
	msg1 := &InboundMessage{ChatID: "oc_indep", Text: "/watch on"}
	if _, err := handleWatch(ctx, mgr, ch, msg1, []string{"on"}, "claude"); err != nil {
		t.Fatalf("/watch on failed: %v", err)
	}
	msg2 := &InboundMessage{ChatID: "oc_indep", Text: "/think off"}
	if _, err := handleThink(ctx, mgr, ch, msg2, []string{"off"}, "claude"); err != nil {
		t.Fatalf("/think off failed: %v", err)
	}
	msg3 := &InboundMessage{ChatID: "oc_indep", Text: "/tools on"}
	if _, err := handleTools(ctx, mgr, ch, msg3, []string{"on"}, "claude"); err != nil {
		t.Fatalf("/tools on failed: %v", err)
	}

	cs := mgr.Get("oc_indep")
	if cs == nil {
		t.Fatal("ChatSession missing")
	}
	if cs.WatchMode() != chatsession.WatchModeAll {
		t.Errorf("/tools on flipped WatchMode to %q, want WatchModeAll (independent)",
			cs.WatchMode())
	}
	if cs.ThinkMode() != chatsession.ThinkModeHide {
		t.Errorf("/tools on flipped ThinkMode to %q, want ThinkModeHide (independent)",
			cs.ThinkMode())
	}
	if cs.ToolsMode() != chatsession.ToolsModeShow {
		t.Errorf("ToolsMode after /tools on = %q, want ToolsModeShow",
			cs.ToolsMode())
	}
}

// TestHandleTools_DefaultIsHide locks the default-direction
// invariant: a fresh ChatSession (no /tools ever fired) must
// report ToolsMode == ToolsModeHide. Mirrors
// TestChatSession_New_DefaultToolsModeIsHide at the handler
// integration level.
func TestHandleTools_DefaultIsHide(t *testing.T) {
	mgr, ch := newTestManager(t, false)
	ctx := context.Background()

	msg := &InboundMessage{ChatID: "oc_default_hide", Text: "/tools"}
	if _, err := handleTools(ctx, mgr, ch, msg, nil, "claude"); err != nil {
		t.Fatalf("handleTools returned error: %v", err)
	}
	cs := mgr.Get("oc_default_hide")
	if cs == nil {
		t.Fatal("ChatSession missing")
	}
	if cs.ToolsMode() != chatsession.ToolsModeHide {
		t.Errorf("fresh ChatSession ToolsMode = %q, want ToolsModeHide (default)",
			cs.ToolsMode())
	}
	if !strings.Contains(ch.LastText(), "hide") {
		t.Errorf("no-arg /tools reply should report 'hide' default, got %q",
			ch.LastText())
	}
}
