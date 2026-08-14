package inbound

import (
	"context"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/gateway/inbound/teststubs"
	"github.com/cnlangzi/nightme/internal/messages"
)

// TestDispatch_CommanderFallThrough covers the
// 2026-08-06 fall-through semantics end-to-end:
//
//  1. /known — commander handles, MessageHandler NOT invoked,
//     result.Consumed=true with the command's reply.
//  2. /unknown — commander reports handled=true + Consumed=false
//     (slash command attempt but no factory matched), router
//     falls through to MessageHandler. The original text
//     reaches the agent loop unchanged.
//  3. plain text (no "/" prefix) — commander not even consulted,
//     falls straight through to MessageHandler.
//
// All three share the same Dispatch entry point; only the
// commander branch (text starts with "/" + commander result)
// gates whether the fallback path is taken.
func TestDispatch_CommanderFallThrough(t *testing.T) {
	const chatID = "oc_fallthrough"

	cases := []struct {
		name         string
		text         string
		wantHits     int32
		wantHitsText string
		wantReply    string
		wantConsumed bool
	}{
		{
			name:         "known slash command — commander handles, MD not invoked",
			text:         "/known",
			wantHits:     0,
			wantReply:    "handled",
			wantConsumed: true,
		},
		{
			name:         "unknown slash command — falls through to MD with original text",
			text:         "/xyz",
			wantHits:     1,
			wantHitsText: "/xyz",
			wantReply:    "",
			wantConsumed: false,
		},
		{
			name:         "path-like slash command — falls through, MD sees the path",
			text:         "/etc/passwd",
			wantHits:     1,
			wantHitsText: "/etc/passwd",
			wantReply:    "",
			wantConsumed: false,
		},
		{
			name:         "plain text — MD invoked directly, no commander call matters",
			text:         "hello world",
			wantHits:     1,
			wantHitsText: "hello world",
			wantReply:    "",
			wantConsumed: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Fresh router + stubs per subtest so the hit
			// counter starts at zero each time.
			commander := teststubs.NewCommander()
			commander.Recognize("/known", teststubs.Result{Consumed: true, Reply: "handled"})

			msg := teststubs.NewMessage(chatsession.NewManager())
			r := New(msg, commander, teststubs.AlwaysFallThroughShell{}, teststubs.NewReaction(true), nil, "primary")

			res, err := r.Dispatch(context.Background(), &messages.InboundMessage{
				ChatID:     chatID,
				Text:       tc.text,
				HasMention: true, // skip per-chat WatchMode gate (lives in chatsession)
				MessageID:  "om_test",
			})
			// F-59: async dispatch — wait before asserting.
			r.WaitExec()
			if err != nil {
				t.Fatalf("Dispatch returned error: %v", err)
			}
			if got := msg.Hits(); got != tc.wantHits {
				t.Errorf("MD hits = %d, want %d", got, tc.wantHits)
			}
			if tc.wantHits > 0 {
				if got := msg.Text(); got != tc.wantHitsText {
					t.Errorf("MD received text = %q, want %q", got, tc.wantHitsText)
				}
			}
			if res == nil {
				t.Fatalf("Dispatch returned nil result")
			}
			if res.Consumed != tc.wantConsumed {
				t.Errorf("Consumed = %v, want %v", res.Consumed, tc.wantConsumed)
			}
			// F-59: res.Reply is always empty now — replies are
			// emitted asynchronously via the wired Emitter, not
			// carried in CommandResult. tc.wantReply still drives
			// the consumed-flag assertion (a commander that
			// returned Reply must have Consumed=true; a fall-
			// through case has wantReply=""), but we no longer
			// pin the Reply field itself.
			_ = tc.wantReply // see comment above — Reply intentionally not asserted
		})
	}
}

// TestDispatch_FallThrough_HitsMessageHandler verifies that
// a /-prefixed input with no matching commander factory falls
// through to tryMessageDispatch (and thus to the MessageHandler).
// HasMention=true is set so the per-chat WatchMode gate
// (chatsession.Manager.HandleInbound) is not the thing under
// test here — this file tests routing only.
func TestDispatch_FallThrough_HitsMessageHandler(t *testing.T) {
	const chatID = "oc_fallthrough"

	commander := teststubs.NewCommander() // no commands registered
	msg := teststubs.NewMessage(chatsession.NewManager())
	r := New(msg, commander, teststubs.AlwaysFallThroughShell{}, teststubs.NewReaction(true), nil, "primary")

	res, err := r.Dispatch(context.Background(), &messages.InboundMessage{
		ChatID:     chatID,
		Text:       "/xyz",
		HasMention: true,
		MessageID:  "om_test",
	})
	// F-59: async dispatch — wait before asserting.
	r.WaitExec()
	if err != nil {
		t.Fatalf("Dispatch(/xyz, mention): %v", err)
	}
	if res.Dropped {
		t.Errorf("/xyz with mention should fall through to MessageHandler, got %+v", res)
	}
	if got := msg.Hits(); got != 1 {
		t.Errorf("MessageHandler should be called once, got %d", got)
	}
}

// TestDispatch_RecognisedSlash_BypassesMessageHandler guards
// the F-watch §3.1.1 escape hatch end-to-end: a recognised
// slash command (one the commander returns Consumed=true for)
// MUST short-circuit before MessageHandler is invoked, so the
// per-chat WatchMode gate inside it never sees the message.
//
// Concretely: a group chat in WatchModeMention receives a
// `/watch on` from a non-mentioned user. Without bypass the
// gate would drop the message and the user could never
// re-enable listening. With bypass the commander returns
// first, the slash handler mutates WatchMode, and the agent
// loop is never woken for the slash itself.
func TestDispatch_RecognisedSlash_BypassesMessageHandler(t *testing.T) {
	const chatID = "oc_bypass"

	commander := teststubs.NewCommander()
	commander.Recognize("/watch on", teststubs.Result{Consumed: true, Reply: "watch on"})

	msg := teststubs.NewMessage(chatsession.NewManager())
	r := New(msg, commander, teststubs.AlwaysFallThroughShell{}, teststubs.NewReaction(true), nil, "primary")

	res, err := r.Dispatch(context.Background(), &messages.InboundMessage{
		ChatID:     chatID,
		Text:       "/watch on",
		HasMention: false, // the dangerous case: non-mention in a Mention-mode chat
		MessageID:  "om_test",
	})
	// F-59: async dispatch — wait before asserting.
	r.WaitExec()
	if err != nil {
		t.Fatalf("Dispatch(/watch on, no mention): %v", err)
	}
	if !res.Consumed {
		t.Errorf("result.Consumed = false, want true (slash command must claim)")
	}
	if got := msg.Hits(); got != 0 {
		t.Errorf("MessageHandler should NOT be called, got %d hits", got)
	}
}
