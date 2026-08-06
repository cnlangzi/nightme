package gateway

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
)

// TestDispatchInbound_CommanderFallThrough covers the
// 2026-08-06 fall-through semantics end-to-end:
//
//  1. /known — commander handles, messageDispatcher NOT invoked,
//     result.Consumed=true with the command's reply.
//  2. /unknown — commander reports handled=true + Consumed=false
//     (slash command attempt but no factory matched), gateway
//     falls through to messageDispatcher. The original text
//     reaches the agent loop unchanged.
//  3. plain text (no "/" prefix) — commander not even consulted,
//     falls straight through to messageDispatcher.
//
// All three share the same DispatchInbound entry point; only the
// commander-shim branch (HasPrefix + commander result) gates
// whether the legacy path is taken.
func TestDispatchInbound_CommanderFallThrough(t *testing.T) {
	const chatID = "oc_fallthrough"

	var dispatched int32
	var lastDispatchedText atomic.Value // string
	lastDispatchedText.Store("")
	md := MessageDispatcher(func(_ context.Context, msg *InboundMessage) error {
		atomic.AddInt32(&dispatched, 1)
		lastDispatchedText.Store(msg.Text)
		return nil
	})

	gw := New(md).(*Router)
	gw.WithCommander(func(_ context.Context, msg *InboundMessage) (*CommandResult, error) {
		// Stub commander: only /known is "registered". Everything
		// else is treated as either not-a-slash-command (handled=
		// false) or slash-command-but-unknown (handled=true,
		// Consumed=false). Mirrors the real commander contract.
		switch msg.Text {
		case "/known":
			return &CommandResult{Consumed: true, Reply: "handled"}, nil
		default:
			if len(msg.Text) > 0 && msg.Text[0] == '/' {
				// Slash command attempt, no factory — fall through.
				return &CommandResult{Consumed: false}, nil
			}
			// Plain text — commander reports handled=false.
			return nil, nil
		}
	})

	cases := []struct {
		name              string
		text              string
		wantDispatched    bool // should messageDispatcher be invoked?
		wantDispatchedTxt string // what text should reach messageDispatcher
		wantReply         string // what Reply should DispatchInbound return
		wantConsumed      bool
	}{
		{
			name:           "known slash command — commander handles, MD not invoked",
			text:           "/known",
			wantDispatched: false,
			wantReply:      "handled",
			wantConsumed:   true,
		},
		{
			name:              "unknown slash command — falls through to MD with original text",
			text:              "/xyz",
			wantDispatched:    true,
			wantDispatchedTxt: "/xyz",
			wantReply:         "",
			wantConsumed:      false,
		},
		{
			name:              "path-like slash command — falls through, MD sees the path",
			text:              "/etc/passwd",
			wantDispatched:    true,
			wantDispatchedTxt: "/etc/passwd",
			wantReply:         "",
			wantConsumed:      false,
		},
		{
			name:              "plain text — MD invoked directly, no commander call matters",
			text:              "hello world",
			wantDispatched:    true,
			wantDispatchedTxt: "hello world",
			wantReply:         "",
			wantConsumed:      false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			atomic.StoreInt32(&dispatched, 0)
			lastDispatchedText.Store("")

			res, err := gw.DispatchInbound(context.Background(), &InboundMessage{
				ChatID:    chatID,
				Text:      tc.text,
				HasMention: true, // skip WatchMode gate so the test exercises the dispatch path cleanly
				MessageID: "om_test",
			})
			if err != nil {
				t.Fatalf("DispatchInbound returned error: %v", err)
			}

			if got := atomic.LoadInt32(&dispatched); (got > 0) != tc.wantDispatched {
				t.Errorf("dispatched = %d, wantDispatched = %v", got, tc.wantDispatched)
			}
			if tc.wantDispatched {
				if got := lastDispatchedText.Load().(string); got != tc.wantDispatchedTxt {
					t.Errorf("MD received text = %q, want %q", got, tc.wantDispatchedTxt)
				}
			}
			if res == nil {
				t.Fatalf("DispatchInbound returned nil result")
			}
			if res.Consumed != tc.wantConsumed {
				t.Errorf("Consumed = %v, want %v (Reply=%q)", res.Consumed, tc.wantConsumed, res.Reply)
			}
			if res.Reply != tc.wantReply {
				t.Errorf("Reply = %q, want %q", res.Reply, tc.wantReply)
			}
		})
	}
}

// TestDispatchInbound_FallThrough_RespectsWatchModeGate verifies
// that the F-watch §3.1.1 gate applies uniformly to the
// commander fall-through path AND the plain-text path. Both
// routes share the same single decision point (dispatchMessage),
// so a /-prefixed input with no matching factory is gated
// identically to a plain-text input of equivalent intent.
func TestDispatchInbound_FallThrough_RespectsWatchModeGate(t *testing.T) {
	const chatID = "oc_gate"

	var dispatched int32
	md := MessageDispatcher(func(_ context.Context, _ *InboundMessage) error {
		atomic.AddInt32(&dispatched, 1)
		return nil
	})

	gw := New(md).(*Router)
	gw.WithCommander(func(_ context.Context, msg *InboundMessage) (*CommandResult, error) {
		// No slash commands "registered" — every /-input falls
		// through with handled=true + Consumed=false.
		if len(msg.Text) > 0 && msg.Text[0] == '/' {
			return &CommandResult{Consumed: false}, nil
		}
		return nil, nil
	})
	gw.WithWatchModeResolver(func(_ string) (chatsession.WatchMode, bool) {
		// Would drop non-mention group messages.
		return chatsession.WatchModeMention, true
	})

	// Group chat, no mention: gate drops both plain text and
	// /-prefixed fall-through. This is the symmetric behaviour
	// we want — once a message is not a recognised command, the
	// /-prefix is irrelevant for gating decisions.
	res, err := gw.DispatchInbound(context.Background(), &InboundMessage{
		ChatID:     chatID,
		Text:       "/xyz",
		HasMention: false,
		MessageID:  "om_test",
	})
	if err != nil {
		t.Fatalf("DispatchInbound(/xyz, no mention): %v", err)
	}
	if !res.Dropped {
		t.Errorf("/xyz with no mention in WatchModeMention should be dropped by the gate, got %+v", res)
	}
	if got := atomic.LoadInt32(&dispatched); got != 0 {
		t.Errorf("messageDispatcher should NOT be called when the gate drops, got %d", got)
	}

	// Same chat, but mention present: gate passes, fall-through
	// reaches messageDispatcher.
	atomic.StoreInt32(&dispatched, 0)
	res, err = gw.DispatchInbound(context.Background(), &InboundMessage{
		ChatID:     chatID,
		Text:       "/xyz",
		HasMention: true,
		MessageID:  "om_test",
	})
	if err != nil {
		t.Fatalf("DispatchInbound(/xyz, mention): %v", err)
	}
	if res.Dropped {
		t.Errorf("/xyz with mention should pass the gate, got %+v", res)
	}
	if got := atomic.LoadInt32(&dispatched); got != 1 {
		t.Errorf("messageDispatcher should be called once, got %d", got)
	}
}