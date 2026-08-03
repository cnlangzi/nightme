package gateway

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
)

// TestDispatchInbound_WatchModeGate covers the F-watch §3.1.1
// dispatcher gate:
//
//   - HasMention=true (bot/@_all) → pass regardless of WatchMode
//   - HasMention=false + WatchModeMention → drop (default behaviour)
//   - HasMention=false + WatchModeAll → pass (after /watch on)
//
// Slash commands always pass the gate (so /watch on works from a
// non-mention group message).
//
// These tests use a no-op messageDispatcher that counts calls
// so we can verify drop / pass exactly.
func TestDispatchInbound_WatchModeGate(t *testing.T) {
	const chatID = "oc_chat"

	var dispatched int32
	md := MessageDispatcher(func(_ context.Context, _ *InboundMessage) error {
		atomic.AddInt32(&dispatched, 1)
		return nil
	})

	gw := New(md).(*Router)
	gw.WithWatchModeResolver(func(c string) (chatsession.WatchMode, bool) {
		if c != chatID {
			return 0, false
		}
		return chatsession.WatchModeMention, true
	})

	cases := []struct {
		name        string
		hasMention  bool
		watchMode   chatsession.WatchMode
		wantDropped bool
	}{
		{
			name:        "mentioned message passes regardless of mode",
			hasMention:  true,
			watchMode:   chatsession.WatchModeMention,
			wantDropped: false,
		},
		{
			name:        "non-mentioned message dropped in WatchModeMention (default)",
			hasMention:  false,
			watchMode:   chatsession.WatchModeMention,
			wantDropped: true,
		},
		{
			name:        "non-mentioned message passes in WatchModeAll",
			hasMention:  false,
			watchMode:   chatsession.WatchModeAll,
			wantDropped: false,
		},
		{
			name:        "mentioned message passes in WatchModeAll (still)",
			hasMention:  true,
			watchMode:   chatsession.WatchModeAll,
			wantDropped: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			atomic.StoreInt32(&dispatched, 0)
			gw.WithWatchModeResolver(func(c string) (chatsession.WatchMode, bool) {
				if c != chatID {
					return 0, false
				}
				return tc.watchMode, true
			})

			res, err := gw.DispatchInbound(context.Background(), &InboundMessage{
				ChatID:     chatID,
				Text:       "hello world", // plain text, not a slash command
				HasMention: tc.hasMention,
				MessageID:  "om_test",
			})
			if err != nil {
				t.Fatalf("DispatchInbound returned error: %v", err)
			}
			if tc.wantDropped {
				if !res.Dropped {
					t.Errorf("expected Dropped=true; got %+v", res)
				}
				if atomic.LoadInt32(&dispatched) != 0 {
					t.Errorf("expected zero dispatches; got %d", dispatched)
				}
			} else {
				if res.Dropped {
					t.Errorf("expected Dropped=false; got %+v", res)
				}
				if atomic.LoadInt32(&dispatched) != 1 {
					t.Errorf("expected 1 dispatch; got %d", dispatched)
				}
			}
		})
	}
}

// TestDispatchInbound_WatchModeGate_SlashBypasses covers the
// "slash commands always pass" rule: a `/watch on` from a
// non-mention group message must reach the slash dispatcher,
// even when WatchMode == WatchModeMention (otherwise users
// can't opt back in once they've opted out).
func TestDispatchInbound_WatchModeGate_SlashBypasses(t *testing.T) {
	const chatID = "oc_chat"

	called := false
	cmd := Command{
		Name: "watch",
		Handler: func(_ context.Context, _ *InboundMessage, _ []string) (*CommandResult, error) {
			called = true
			return &CommandResult{Consumed: true}, nil
		},
	}

	gw := New(nil).(*Router)
	gw.Register(cmd)
	gw.WithWatchModeResolver(func(_ string) (chatsession.WatchMode, bool) {
		return chatsession.WatchModeMention, true // would normally drop
	})

	// Non-mentioned message with /watch on text.
	res, err := gw.DispatchInbound(context.Background(), &InboundMessage{
		ChatID:     chatID,
		Text:       "/watch on",
		HasMention: false,
		MessageID:  "om_test",
	})
	if err != nil {
		t.Fatalf("DispatchInbound returned error: %v", err)
	}
	if res.Dropped {
		t.Errorf("/watch slash command was dropped by WatchMode gate; want pass-through")
	}
	if !called {
		t.Errorf("/watch handler was not invoked; expected it to run regardless of gate")
	}
}

// TestDispatchInbound_WatchModeGate_NoResolver covers the
// backward-compatibility path: when no WatchMode resolver is
// wired (e.g. tests, or pre-F-watch runtime), the gate is a
// no-op and every message passes through. This guarantees the
// F-watch change does not regress runtimes that haven't been
// updated yet.
func TestDispatchInbound_WatchModeGate_NoResolver(t *testing.T) {
	const chatID = "oc_chat"

	var dispatched int32
	md := MessageDispatcher(func(_ context.Context, _ *InboundMessage) error {
		atomic.AddInt32(&dispatched, 1)
		return nil
	})
	gw := New(md).(*Router)
	// Intentionally no WithWatchModeResolver call.

	res, err := gw.DispatchInbound(context.Background(), &InboundMessage{
		ChatID:     chatID,
		Text:       "hello",
		HasMention: false, // would be dropped if gate were active
		MessageID:  "om_test",
	})
	if err != nil {
		t.Fatalf("DispatchInbound returned error: %v", err)
	}
	if res.Dropped {
		t.Errorf("expected Dropped=false when no resolver is wired; got %+v", res)
	}
	if atomic.LoadInt32(&dispatched) != 1 {
		t.Errorf("expected 1 dispatch; got %d", dispatched)
	}
}

// TestDispatchInbound_WatchModeGate_UnknownChat covers the case
// where the resolver returns (zero, false) for an unknown chat
// ID (no ChatSession yet). The gate must NOT drop these — let
// the downstream dispatcher reply with "send /cwd first" as
// before.
func TestDispatchInbound_WatchModeGate_UnknownChat(t *testing.T) {
	const chatID = "oc_unknown"

	var dispatched int32
	md := MessageDispatcher(func(_ context.Context, _ *InboundMessage) error {
		atomic.AddInt32(&dispatched, 1)
		return nil
	})
	gw := New(md).(*Router)
	gw.WithWatchModeResolver(func(_ string) (chatsession.WatchMode, bool) {
		return 0, false // no ChatSession
	})

	res, err := gw.DispatchInbound(context.Background(), &InboundMessage{
		ChatID:     chatID,
		Text:       "hello",
		HasMention: false,
		MessageID:  "om_test",
	})
	if err != nil {
		t.Fatalf("DispatchInbound returned error: %v", err)
	}
	if res.Dropped {
		t.Errorf("expected Dropped=false for unknown chat; got %+v", res)
	}
	if atomic.LoadInt32(&dispatched) != 1 {
		t.Errorf("expected 1 dispatch; got %d", dispatched)
	}
}

// TestDispatchInbound_WatchModeGate_DMInvariant covers the
// invariant "DM messages are always processed regardless of
// WatchMode". The channel adapter is contractually required to
// set HasMention=true for every DM message (every DM is
// implicitly addressed to the bot). This test pins that
// contract: even with WatchMode=WatchModeMention, the gate
// must NOT drop a message that arrives with HasMention=true.
//
// The corresponding adapter-side invariant test
// (computeHasMention DM branch) lives in
// internal/channel/feishu/mention_test.go — if either test
// regresses, the DM contract is broken.
func TestDispatchInbound_WatchModeGate_DMInvariant(t *testing.T) {
	cases := []struct {
		name      string
		watchMode chatsession.WatchMode
	}{
		{"DM with WatchModeMention (default) — message has HasMention=true so it passes", chatsession.WatchModeMention},
		{"DM with WatchModeAll — message has HasMention=true so it passes", chatsession.WatchModeAll},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var dispatched int32
			md := MessageDispatcher(func(_ context.Context, _ *InboundMessage) error {
				atomic.AddInt32(&dispatched, 1)
				return nil
			})
			gw := New(md).(*Router)
			gw.WithWatchModeResolver(func(_ string) (chatsession.WatchMode, bool) {
				return tc.watchMode, true
			})

			// HasMention=true simulates what the channel
			// adapter must do for every DM message. If this
			// field is ever set to false for a DM, the gate
			// would (correctly) drop it — the test would
			// catch the regression because we set it true.
			res, err := gw.DispatchInbound(context.Background(), &InboundMessage{
				ChatID:     "oc_dm",
				Text:       "hello",
				HasMention: true, // DM invariant: always true
				MessageID:  "om_dm_test",
			})
			if err != nil {
				t.Fatalf("DispatchInbound returned error: %v", err)
			}
			if res.Dropped {
				t.Errorf("DM message was dropped; DM invariant violated. want pass, got Dropped=%v", res.Dropped)
			}
			if atomic.LoadInt32(&dispatched) != 1 {
				t.Errorf("DM message should be dispatched exactly once; got %d", dispatched)
			}
		})
	}
}