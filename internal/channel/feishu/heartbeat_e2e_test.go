// Package feishu — F-63 end-to-end heartbeat test.
//
// Exercises the full path: bridge AgentEvent → handler (built
// inline, see buildHandler below) → outbound.Emitter →
// *Adapter.Send → receiptFor → ApplyHeartbeat → renderLocked →
// PATCH body containing the heartbeat header.
//
// Why we build the handler inline instead of importing
// internal/runtime.NewEventHandler: importing runtime here
// creates a cycle (runtime/deps.go imports feishu.NewAdapter
// for its default channel wiring). The handler inline is
// intentionally minimal — it reproduces only the F-63-relevant
// behaviour (Translate + Identity stamp + Heartbeat observe +
// em.Send + default policies). The point of this test is to
// pin the WIRE: the adapter's OutHeartbeat case + the receipt
// PATCH body shape. The handler's policy chain is exhaustively
// tested in internal/runtime/heartbeat_handler_test.go.
//
// Without this test, a future refactor could silently break
// the wire (e.g. drop the OutHeartbeat case in Adapter.Send,
// or forget to call receiptFor in the new case) and the
// per-package tests would still pass.

package feishu

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/gateway/outbound"
	"github.com/cnlangzi/nightme/internal/messages"
)

// newAdapterWithBot builds an *Adapter bypassing NewAdapter's
// credential requirement. Production paths always go through
// NewAdapter (which talks to lark); tests bypass with a
// minimal struct literal so we can inject a mock receiptBot.
// The Adapter's other fields (lark client, WS, etc.) are
// nil — the receipt card render path we exercise doesn't
// touch them.
func newAdapterWithBot(bot receiptBot) *Adapter {
	a := &Adapter{
		logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		receiptsByUserMsgID: map[string]*MessageReceipt{},
		pendingHeartbeats:   map[string]messages.HeartbeatSnapshot{},
		threadReplyLimiter:  newThreadReplyLimiter(0, 0),
	}
	return a
}

// installReceipt directly inserts a pre-built receipt into the
// adapter's map, bypassing ensureReceiptForTyping (which needs
// a live lark REST client to send the cold-start card). For
// e2e tests the lark send is irrelevant — we only care about
// the ApplyHeartbeat + PATCH path on the existing receipt.
func installReceipt(a *Adapter, bot receiptBot, chatID, userMsgID, msgID string) *MessageReceipt {
	r := NewMessageReceiptForReply(chatID, userMsgID, msgID, bot)
	r.cardMsgID = msgID // pretend a card was already sent
	a.mu.Lock()
	a.receiptsByUserMsgID[userMsgID] = r
	a.mu.Unlock()
	return r
}

// buildHandler is a minimal reproduction of the production
// NewEventHandler — only the F-63-relevant slices. Intentionally
// not exported; only this test uses it. Mirrors the order:
//
//	1. Translate
//	2. Stamp ReplyTo + identity
//	3. ⭐ Heartbeat observe (BEFORE policies — core invariant)
//	4. Default policies (think / tools gate)
//	5. em.Send
func buildHandler(em outbound.Emitter, cs *chatsession.ChatSession, thinkMode chatsession.ThinkMode, toolsMode chatsession.ToolsMode) func(chatsession.AgentEventEnvelope) {
	_ = cs.SetThinkMode(thinkMode)
	_ = cs.SetToolsMode(toolsMode)
	return func(env chatsession.AgentEventEnvelope) {
		out, ok := outbound.Translate(env.ChatID, *env.Event)
		if !ok {
			return
		}
		out.ReplyTo = env.UserMsgID
		if out.AgentName == "" {
			out.AgentName = env.AgentSession.Agent
		}
		if out.Workspace == "" {
			out.Workspace = env.AgentSession.Cwd
		}

		// ⭐ F-63 observe BEFORE policies.
		if env.UserMsgID != "" && cs.Heartbeat() != nil {
			if cs.Heartbeat().Observe(env.UserMsgID, out.Kind) {
				snap := cs.Heartbeat().Snapshot(env.UserMsgID)
				if !snap.Empty() {
					_ = em.Send(context.Background(), messages.OutboundMessage{
						ChatID:    env.ChatID,
						Kind:      messages.OutHeartbeat,
						ReplyTo:   env.UserMsgID,
						Heartbeat: &snap,
					})
				}
			}
		}

		// Default policies (think / tools gates).
		if cs.ThinkMode() == chatsession.ThinkModeHide && out.Kind == messages.OutThinking {
			return
		}
		if cs.ToolsMode() == chatsession.ToolsModeHide &&
			(out.Kind == messages.OutToolStart || out.Kind == messages.OutToolEnd) {
			return
		}

		_ = em.Send(context.Background(), out)
	}
}

// TestEndToEnd_Heartbeat_HeaderInPatchedCard runs the full
// pipeline and asserts the heartbeat header lands in a PATCH
// body.
func TestEndToEnd_Heartbeat_HeaderInPatchedCard(t *testing.T) {
	mgr := chatsession.NewManager()
	cs, _ := mgr.GetOrCreate("oc_e2e_chat", "claude")

	bot := &mockReceiptBot{}
	adapter := newAdapterWithBot(bot)
	em := outbound.New(adapter, outbound.Options{})
	handler := buildHandler(em, cs, chatsession.ThinkModeShow, chatsession.ToolsModeShow)

	// Install a receipt so the heartbeat PATCH has somewhere
	// to land.
	installReceipt(adapter, bot, "oc_e2e_chat", "om_e2e_user", "om_e2e_card_1")

	as := chatsession.NewAgentSession("as_e2e", "cs_oc_e2e_chat", "claude", "/tmp", nil)

	// Drive two countable events. The second falls inside the
	// 2s throttle window of the first heartbeat PATCH.
	handler(chatsession.AgentEventEnvelope{
		ChatID: "oc_e2e_chat", AgentSession: as, UserMsgID: "om_e2e_user",
		Event: &agent.AgentEvent{Kind: agent.EventAgentText, Text: "[思考] pondering"},
	})
	handler(chatsession.AgentEventEnvelope{
		ChatID: "oc_e2e_chat", AgentSession: as, UserMsgID: "om_e2e_user",
		Event: &agent.AgentEvent{Kind: agent.EventAgentToolStart, ToolStart: &agent.AgentToolStartEvent{ID: "t1", Name: "Read"}},
	})

	// Tracker state.
	snap := cs.Heartbeat().Snapshot("om_e2e_user")
	if snap.ThinkCount != 1 {
		t.Fatalf("ThinkCount = %d, want 1", snap.ThinkCount)
	}
	if snap.ToolCount != 1 {
		t.Fatalf("ToolCount = %d, want 1", snap.ToolCount)
	}

	// At least one PATCH must contain the heartbeat header.
	if len(bot.patches) == 0 {
		t.Fatalf("expected at least 1 receipt PATCH; got 0")
	}
	body := findBodyWithHeader(bot.patches)
	if body == "" {
		t.Fatalf("no PATCH body contained heartbeat header; got %d:\n%s",
			len(bot.patches), formatAllBodies(bot.patches))
	}

	// Inspect the first element of the PATCHed body.
	var parsed struct {
		Body struct {
			Elements []map[string]any `json:"elements"`
		}
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("PATCH body not valid JSON: %v", err)
	}
	if len(parsed.Body.Elements) == 0 {
		t.Fatal("PATCH body has no elements")
	}
	first, _ := parsed.Body.Elements[0]["content"].(string)

	// The PATCH was triggered by the first heartbeat event with
	// ThinkCount=1, ToolCount=0 — so the header reflects that
	// state at PATCH time.
	if !strings.Contains(first, "🤖 Working") {
		t.Fatalf("missing 🤖 Working: %q", first)
	}
	if !strings.Contains(first, "💭 1") {
		t.Fatalf("missing 💭 1: %q", first)
	}
	if !strings.Contains(first, "⏱ ") {
		t.Fatalf("missing ⏱ time prefix: %q", first)
	}
}

// TestEndToEnd_Heartbeat_ThinkOff_HeaderStillUpdates is the
// F-63 §3.7 row 2 invariant end-to-end: /think off drops the
// original OutThinking from the chat, but the heartbeat-driven
// PATCH body STILL contains "💭 1". This is the test that
// fails if a future refactor moves the observe AFTER the
// policy chain.
func TestEndToEnd_Heartbeat_ThinkOff_HeaderStillUpdates(t *testing.T) {
	mgr := chatsession.NewManager()
	cs, _ := mgr.GetOrCreate("oc_e2e_thinkoff", "claude")

	bot := &mockReceiptBot{}
	adapter := newAdapterWithBot(bot)
	em := outbound.New(adapter, outbound.Options{})
	handler := buildHandler(em, cs, chatsession.ThinkModeHide, chatsession.ToolsModeShow)

	installReceipt(adapter, bot, "oc_e2e_thinkoff", "om_e2e_user", "om_e2e_card_1")

	as := chatsession.NewAgentSession("as_e2e", "cs_oc_e2e_thinkoff", "claude", "/tmp", nil)

	handler(chatsession.AgentEventEnvelope{
		ChatID: "oc_e2e_thinkoff", AgentSession: as, UserMsgID: "om_e2e_user",
		Event: &agent.AgentEvent{Kind: agent.EventAgentText, Text: "[思考] hidden"},
	})

	// Counter incremented despite /think off (core F-63 invariant).
	if snap := cs.Heartbeat().Snapshot("om_e2e_user"); snap.ThinkCount != 1 {
		t.Fatalf("ThinkCount = %d, want 1 (/think off must not suppress counter)", snap.ThinkCount)
	}
	body := findBodyWithHeader(bot.patches)
	if body == "" {
		t.Fatalf("expected at least one heartbeat PATCH; got %d:\n%s",
			len(bot.patches), formatAllBodies(bot.patches))
	}
	if !strings.Contains(body, "💭 1") {
		t.Fatalf("heartbeat header missing 💭 1 under /think off; body=%s", body)
	}
}

// TestEndToEnd_Heartbeat_ConcurrentApply_NoRace is the -race
// canary for the lock-through-renderLocked contract
// (optimization #1's fix). The semantic guarantee ("no
// counter increment is lost") cannot be asserted purely by
// the final value because ApplyHeartbeat takes an ABSOLUTE
// snapshot — last-write-wins on ThinkCount, with goroutine
// scheduling deciding the final value. What we CAN assert:
//
//   - No data race (the -race detector catches any unlock-
//     before-read regression).
//   - The final ThinkCount is exactly the count from whichever
//     goroutine acquired the lock last (so the value is one of
//     {1..N} but not arbitrary; proves no torn writes).
//   - No panic / nil deref.
//
// The previous test that asserted "final == N" was wrong:
// goroutines race on scheduling, not on the lock. The lock
// serialises writes but doesn't define order. Run with -race
// in CI; the failure mode this test catches is a regression
// in the ApplyHeartbeat lock contract.
func TestEndToEnd_Heartbeat_ConcurrentApply_NoRace(t *testing.T) {
	bot := &mockReceiptBot{}
	adapter := newAdapterWithBot(bot)
	receipt := installReceipt(adapter, bot, "oc_e2e_concurrent", "om_e2e_user", "om_e2e_card_1")
	// Disable throttle so every call exercises renderLocked.
	receipt.heartbeatMinInterval = 0

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(n int) {
			defer wg.Done()
			receipt.ApplyHeartbeat(context.Background(), messages.HeartbeatSnapshot{
				ThinkCount: n + 1, LastBeatAt: time.Now(),
			})
		}(i)
	}
	wg.Wait()

	// Lock-protected read so we observe the post-wg.Wait()
	// state without depending on Go's weak memory model.
	receipt.mu.Lock()
	final := receipt.heartbeat
	receipt.mu.Unlock()

	// Last-write-wins semantics: final.ThinkCount must be one
	// of the values written by any goroutine (1..goroutines).
	// Out-of-range = torn write = lock contract broken.
	if final.ThinkCount < 1 || final.ThinkCount > goroutines {
		t.Fatalf("torn write: final ThinkCount = %d, want 1..%d",
			final.ThinkCount, goroutines)
	}
	if final.LastBeatAt.IsZero() {
		t.Fatal("LastBeatAt must be set after ApplyHeartbeat")
	}
}

// ─── helpers ───

// findBodyWithHeader returns the most-recent PATCH body
// containing the heartbeat header; empty string if none.
func findBodyWithHeader(cards []cardCall) string {
	for i := len(cards) - 1; i >= 0; i-- {
		if strings.Contains(cards[i].Body, "🤖 Working") {
			return cards[i].Body
		}
	}
	return ""
}

func formatAllBodies(cards []cardCall) string {
	if len(cards) == 0 {
		return "(none)"
	}
	var b strings.Builder
	for i, c := range cards {
		b.WriteString("---\n[")
		b.WriteString(string(rune('0' + i)))
		b.WriteString("]:\n")
		b.WriteString(c.Body)
		b.WriteString("\n")
	}
	return b.String()
}