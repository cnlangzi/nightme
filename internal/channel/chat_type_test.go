package channel

import (
	"testing"

	"github.com/cnlangzi/nightme/internal/gateway"
)

// TestMessage_IsDM covers the chat-type discriminator. The Session
// Manager uses this to decide whether a chat should host a workspace
// (DMs are a single auxiliary plane — group chats each have their
// own session keyed by chat_id).
func TestMessage_IsDM(t *testing.T) {
	cases := []struct {
		name     string
		chatType string
		want     bool
	}{
		{"p2p is DM", "p2p", true},
		{"group is not DM", "group", false},
		{"topic_group is not DM", "topic_group", false},
		{"empty is unknown, not DM", "", false},
		{"future unknown value not DM", "dm-priority", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := Message{ChatType: gateway.ChatType(tc.chatType)}
			if got := IsDM(m); got != tc.want {
				t.Errorf("IsDM() with ChatType=%q = %v, want %v", tc.chatType, got, tc.want)
			}
		})
	}
}
