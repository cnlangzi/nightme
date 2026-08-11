package gateway

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/cnlangzi/nightme/internal/gatewaytest"
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

	gw := New(md, &gatewaytest.NoopEmitter{}).(*Router)
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
				ChatID:     chatID,
				Text:       tc.text,
				HasMention: true, // skip per-chat WatchMode gate (now in chatsession.Manager.AcceptInbound)
				MessageID:  "om_test",
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

// TestDispatchInbound_FallThrough_HitsMessageDispatcher verifies
// that a /-prefixed input with no matching commander factory
// falls through to dispatchMessage (and thus to the runtime
// messageDispatcher). HasMention=true is set so the per-chat
// WatchMode gate (now in chatsession.Manager.AcceptInbound) is
// not the thing under test here — this file tests gateway
// routing only.
//
// The corresponding WatchMode gate coverage lives in
// internal/chatsession/watchmode_gate_test.go.
func TestDispatchInbound_FallThrough_HitsMessageDispatcher(t *testing.T) {
	const chatID = "oc_fallthrough"

	var dispatched int32
	md := MessageDispatcher(func(_ context.Context, _ *InboundMessage) error {
		atomic.AddInt32(&dispatched, 1)
		return nil
	})

	gw := New(md, &gatewaytest.NoopEmitter{}).(*Router)
	gw.WithCommander(func(_ context.Context, msg *InboundMessage) (*CommandResult, error) {
		// No slash commands "registered" — every /-input falls
		// through with handled=true + Consumed=false.
		if len(msg.Text) > 0 && msg.Text[0] == '/' {
			return &CommandResult{Consumed: false}, nil
		}
		return nil, nil
	})

	res, err := gw.DispatchInbound(context.Background(), &InboundMessage{
		ChatID:     chatID,
		Text:       "/xyz",
		HasMention: true, // skip WatchMode gate; routing is the subject
		MessageID:  "om_test",
	})
	if err != nil {
		t.Fatalf("DispatchInbound(/xyz, mention): %v", err)
	}
	if res.Dropped {
		t.Errorf("/xyz with mention should fall through to messageDispatcher, got %+v", res)
	}
	if got := atomic.LoadInt32(&dispatched); got != 1 {
		t.Errorf("messageDispatcher should be called once, got %d", got)
	}
}

// TestDispatchInbound_RecognisedSlash_BypassesMessageDispatcher
// guards the F-watch §3.1.1 escape hatch end-to-end: a recognised
// slash command (one the commander shim returns Consumed=true for)
// MUST short-circuit before the runtime messageDispatcher is
// invoked, so the per-chat WatchMode gate inside it never sees
// the message.
//
// Concretely: a group chat in WatchModeMention receives a
// `/watch on` from a non-mentioned user. Without bypass the gate
// would drop the message and the user could never re-enable
// listening. With bypass the commander returns first, the slash
// handler mutates WatchMode, and the agent loop is never woken
// for the slash itself.
//
// Pre-refactor this property was an emergent consequence of the
// gateway holding the gate. Post-refactor the gate lives in
// chatsession.Manager.AcceptInbound, which the runtime
// messageDispatcher closure calls — so the bypass now has to be
// exercised via the routing boundary (recognised slash returns
// before dispatchMessage runs). This test pins both halves:
// routing (no messageDispatcher call) and bypass contract (the
// gate is never even consulted).
func TestDispatchInbound_RecognisedSlash_BypassesMessageDispatcher(t *testing.T) {
	const chatID = "oc_bypass"

	var dispatched int32
	commanderCalled := false
	md := MessageDispatcher(func(_ context.Context, _ *InboundMessage) error {
		atomic.AddInt32(&dispatched, 1)
		return nil
	})

	gw := New(md, &gatewaytest.NoopEmitter{}).(*Router)
	gw.WithCommander(func(_ context.Context, msg *InboundMessage) (*CommandResult, error) {
		if msg.Text == "/watch on" {
			commanderCalled = true
			return &CommandResult{Consumed: true, Reply: "watch on"}, nil
		}
		return nil, nil
	})

	res, err := gw.DispatchInbound(context.Background(), &InboundMessage{
		ChatID:     chatID,
		Text:       "/watch on",
		HasMention: false, // the dangerous case: non-mention in a Mention-mode chat
		MessageID:  "om_test",
	})
	if err != nil {
		t.Fatalf("DispatchInbound(/watch on, no mention): %v", err)
	}
	if !commanderCalled {
		t.Fatal("commander shim was not invoked; routing should reach it before messageDispatcher")
	}
	if got := atomic.LoadInt32(&dispatched); got != 0 {
		t.Errorf("messageDispatcher must NOT run for a recognised slash command (would call AcceptInbound); got %d calls", got)
	}
	if res == nil || !res.Consumed {
		t.Errorf("expected Consumed=true from commander branch; got %+v", res)
	}
	if res.Reply != "watch on" {
		t.Errorf("Reply = %q, want %q", res.Reply, "watch on")
	}
}

