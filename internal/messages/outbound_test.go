package messages

import (
	"testing"
	"time"
)

// TestOutboundKindString_Heartbeat (F-63) covers the OutHeartbeat
// case added to the String() switch. Mirrors the documented
// per-kind -> string map; this test pins it so a future rename
// surfaces here instead of in a random adapter log line.
func TestOutboundKindString_Heartbeat(t *testing.T) {
	if got, want := OutHeartbeat.String(), "heartbeat"; got != want {
		t.Fatalf("OutHeartbeat.String() = %q, want %q", got, want)
	}
}

// TestHeartbeatSnapshot_Empty (F-63) covers the zero-value /
// populated / partially-populated boundary conditions the
// channel adapter uses to decide whether to drop the follow-up
// OutHeartbeat message.
func TestHeartbeatSnapshot_Empty(t *testing.T) {
	cases := []struct {
		name string
		snap HeartbeatSnapshot
		want bool
	}{
		{"zero", HeartbeatSnapshot{}, true},
		{"think_only", HeartbeatSnapshot{ThinkCount: 1}, false},
		{"tool_only", HeartbeatSnapshot{ToolCount: 1}, false},
		{"lastbeat_only", HeartbeatSnapshot{LastBeatAt: time.Now()}, false},
		{"all_populated", HeartbeatSnapshot{
			ThinkCount: 3, ToolCount: 5, LastBeatAt: time.Now(),
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.snap.Empty(); got != c.want {
				t.Fatalf("Empty() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestOutboundMessage_HeartbeatFieldRoundtrip (F-63) covers the
// Heartbeat field on OutboundMessage. Channel adapters read it
// directly; the test pins pointer-vs-value semantics so a future
// "store by value" refactor breaks here, not at a render site.
func TestOutboundMessage_HeartbeatFieldRoundtrip(t *testing.T) {
	now := time.Date(2026, 8, 15, 14, 35, 22, 0, time.UTC)
	snap := &HeartbeatSnapshot{
		ThinkCount: 3,
		ToolCount:  12,
		LastBeatAt: now,
	}
	msg := OutboundMessage{
		Kind:      OutHeartbeat,
		ReplyTo:   "om_x_user_123",
		Heartbeat: snap,
	}
	if msg.Heartbeat == nil {
		t.Fatal("Heartbeat nil after assignment")
	}
	if msg.Heartbeat.ThinkCount != 3 || msg.Heartbeat.ToolCount != 12 {
		t.Fatalf("counts lost: %+v", msg.Heartbeat)
	}
	if !msg.Heartbeat.LastBeatAt.Equal(now) {
		t.Fatalf("LastBeatAt lost: got %v want %v", msg.Heartbeat.LastBeatAt, now)
	}
	if msg.Heartbeat.Empty() {
		t.Fatal("populated snapshot reported Empty()=true")
	}
}