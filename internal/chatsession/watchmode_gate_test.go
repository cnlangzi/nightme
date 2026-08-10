package chatsession

import (
	"testing"
)

// TestManager_AcceptInbound covers the F-watch §3.1.1 per-chat
// gate, now owned by chatsession (relocated from
// gateway.WithWatchModeResolver / applyWatchModeGate). The
// decision matrix:
//
//	HasMention=true                                → accept
//	HasMention=false + no ChatSession              → accept
//	HasMention=false + ChatSession + WatchModeAll  → accept
//	HasMention=false + ChatSession + WatchModeMention → drop
func TestManager_AcceptInbound(t *testing.T) {
	const chatID = "oc_watch"

	cases := []struct {
		name       string
		hasMention bool
		setup      func(m *Manager) // optional: create + configure ChatSession
		wantAccept bool
	}{
		{
			name:       "mention passes regardless of mode (DM invariant)",
			hasMention: true,
			setup: func(m *Manager) {
				cs, _ := m.GetOrCreate(chatID, "claude")
				_ = cs.SetWatchMode(WatchModeMention)
			},
			wantAccept: true,
		},
		{
			name:       "mention passes in WatchModeAll (still)",
			hasMention: true,
			setup: func(m *Manager) {
				cs, _ := m.GetOrCreate(chatID, "claude")
				_ = cs.SetWatchMode(WatchModeAll)
			},
			wantAccept: true,
		},
		{
			name:       "non-mention dropped in WatchModeMention (default safe mode)",
			hasMention: false,
			setup: func(m *Manager) {
				cs, _ := m.GetOrCreate(chatID, "claude")
				_ = cs.SetWatchMode(WatchModeMention)
			},
			wantAccept: false,
		},
		{
			name:       "non-mention passes in WatchModeAll",
			hasMention: false,
			setup: func(m *Manager) {
				cs, _ := m.GetOrCreate(chatID, "claude")
				_ = cs.SetWatchMode(WatchModeAll)
			},
			wantAccept: true,
		},
		{
			name:       "non-mention passes for unknown chat (no ChatSession yet) — downstream replies with /cwd hint",
			hasMention: false,
			setup:      func(m *Manager) {}, // intentionally no GetOrCreate
			wantAccept: true,
		},
		{
			name:       "non-mention passes for unknown chat when HasMention=true",
			hasMention: true,
			setup:      func(m *Manager) {},
			wantAccept: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr := NewManager()
			tc.setup(mgr)
			got := mgr.AcceptInbound(chatID, tc.hasMention)
			if got != tc.wantAccept {
				t.Errorf("AcceptInbound(%q, hasMention=%v) = %v, want %v",
					chatID, tc.hasMention, got, tc.wantAccept)
			}
		})
	}
}

// TestManager_AcceptInbound_DefaultModeIsMention pins the safe
// default: a freshly created ChatSession drops non-mention group
// messages until the user opts in via `/watch on`.
func TestManager_AcceptInbound_DefaultModeIsMention(t *testing.T) {
	const chatID = "oc_default"

	mgr := NewManager()
	cs, _ := mgr.GetOrCreate(chatID, "claude")
	if got := cs.WatchMode(); got != WatchModeMention {
		t.Fatalf("WatchMode default = %q, want %q", got, WatchModeMention)
	}
	if mgr.AcceptInbound(chatID, false) {
		t.Errorf("default WatchModeMention should drop non-mention message")
	}
	if !mgr.AcceptInbound(chatID, true) {
		t.Errorf("mention should pass regardless of mode")
	}
}

// TestManager_AcceptInbound_ReflectsRuntimeMutation confirms the
// gate reads the ChatSession state live, so a `/watch on` issued
// mid-session flips the next decision without any re-wiring.
func TestManager_AcceptInbound_ReflectsRuntimeMutation(t *testing.T) {
	const chatID = "oc_flip"

	mgr := NewManager()
	cs, _ := mgr.GetOrCreate(chatID, "claude")

	if mgr.AcceptInbound(chatID, false) {
		t.Fatalf("baseline: non-mention should be dropped under default WatchModeMention")
	}

	if err := cs.SetWatchMode(WatchModeAll); err != nil {
		t.Fatalf("SetWatchMode(All): %v", err)
	}
	if !mgr.AcceptInbound(chatID, false) {
		t.Errorf("after /watch on: non-mention should pass under WatchModeAll")
	}

	if err := cs.SetWatchMode(WatchModeMention); err != nil {
		t.Fatalf("SetWatchMode(Mention): %v", err)
	}
	if mgr.AcceptInbound(chatID, false) {
		t.Errorf("after /watch off: non-mention should drop again under WatchModeMention")
	}
}
