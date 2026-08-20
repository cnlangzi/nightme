package telegram

import "testing"

// TestSessionChatID_Stable is the table-driven contract for the
// stable chatID mapping. Every (rawChatID, threadID) pair must
// produce the same chatID on every call, across daemon restarts,
// config changes, and state file loss — see docs/CHANNEL.md §5.5
// and docs/channel/telegram.md §5.1.
func TestSessionChatID_Stable(t *testing.T) {
	tests := []struct {
		name      string
		rawChatID string
		threadID  int
		want      string
	}{
		{"dm private", "8684538097", 0, "tg_8684538097"},
		{"group main window no forum", "-10012345", 0, "tg_-10012345"},
		{"group main window forum enabled", "-10012345", 0, "tg_-10012345"},
		{"group topic 42", "-10012345", 42, "tg_-10012345:42"},
		{"group topic 88", "-10012345", 88, "tg_-10012345:88"},
		{"group topic 1", "-10012345", 1, "tg_-10012345:1"},
		{"private with large topic id (edge)", "1234567890", 999999, "tg_1234567890:999999"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &Adapter{}
			for i := range 100 {
				got := a.sessionChatID(tt.rawChatID, tt.threadID)
				if got != tt.want {
					t.Fatalf("sessionChatID(%q, %d) = %q, want %q (iter %d)",
						tt.rawChatID, tt.threadID, got, tt.want, i)
				}
			}
		})
	}
}

// TestSplitSessionID_RoundTrip verifies that sessionChatID and
// splitSessionID are mutual inverses for every supported input —
// the contract that keeps the inbound adapter and the outbound
// split reading the same chatID.
func TestSplitSessionID_RoundTrip(t *testing.T) {
	tests := []struct {
		name      string
		rawChatID string
		threadID  int
	}{
		{"dm", "8684538097", 0},
		{"group main window", "-10012345", 0},
		{"group topic 42", "-10012345", 42},
		{"group topic 88", "-10012345", 88},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &Adapter{}
			chatID := a.sessionChatID(tt.rawChatID, tt.threadID)
			gotRaw, gotThread, ok := splitSessionID(chatID)
			if !ok {
				t.Fatalf("splitSessionID(%q) returned ok=false", chatID)
			}
			if gotRaw != tt.rawChatID || gotThread != tt.threadID {
				t.Errorf("splitSessionID(%q) = (%q, %d), want (%q, %d)",
					chatID, gotRaw, gotThread, tt.rawChatID, tt.threadID)
			}
		})
	}
}

// TestSplitSessionID_RejectsNonTelegram verifies that chatIDs
// from other channels (e.g. Feishu's "oc_<hex>") are NOT parsed
// by the telegram adapter — the dispatcher / router relies on
// ok=false to skip telegram-specific routing.
func TestSplitSessionID_RejectsNonTelegram(t *testing.T) {
	tests := []string{
		"oc_abcdef1234567890",     // Feishu
		"slack_T123456",           // hypothetical Slack
		"8684538097",              // bare digit (pre-tg_ legacy)
		"",                        // empty
		"tg_",                     // prefix only
		"tg_abc:notanumber",       // bad thread id → ok=true but thread parses as 0
		"tg_abc:42:extra",         // extra colon → only first ":" used
	}
	for _, in := range tests {
		t.Run(in, func(t *testing.T) {
			if in == "tg_abc:42:extra" {
				// First ":" is at index 3; body is "abc:42:extra".
				// strconv.Atoi("42:extra") fails, so threadID=0
				// and rawChatID = full body. That's the documented
				// "parse failure → treat as main window" behaviour.
				raw, threadID, ok := splitSessionID(in)
				if !ok {
					t.Fatalf("splitSessionID(%q) returned ok=false", in)
				}
				if raw != "abc:42:extra" || threadID != 0 {
					t.Errorf("splitSessionID(%q) = (%q, %d), want (%q, 0)",
						in, raw, threadID, "abc:42:extra")
				}
				return
			}
			if in == "tg_abc:notanumber" {
				raw, threadID, ok := splitSessionID(in)
				if !ok {
					t.Fatalf("splitSessionID(%q) returned ok=false", in)
				}
				if raw != "abc:notanumber" || threadID != 0 {
					t.Errorf("splitSessionID(%q) = (%q, %d), want (%q, 0)",
						in, raw, threadID, "abc:notanumber")
				}
				return
			}
			raw, threadID, ok := splitSessionID(in)
			if ok {
				t.Errorf("splitSessionID(%q) = (%q, %d, %v), want ok=false",
					in, raw, threadID, ok)
			}
		})
	}
}

// TestSessionChatID_DMSession_ReEntry stresses the most
// important property: a DM that receives /cwd and then any
// other message must produce the same chatID across both calls.
// This is the exact bug that motivated the tg_ prefix design.
func TestSessionChatID_DMSession_ReEntry(t *testing.T) {
	a := &Adapter{}
	first := a.sessionChatID("8684538097", 0)
	for i := range 1000 {
		got := a.sessionChatID("8684538097", 0)
		if got != first {
			t.Fatalf("DM re-entry changed chatID: %q vs %q (iter %d)", got, first, i)
		}
	}
}

// TestSessionChatID_GroupTopic_ReEntrySameTopic ensures two
// messages in the same Telegram topic produce the same chatID
// (they both hit the same ChatSession).
func TestSessionChatID_GroupTopic_ReEntrySameTopic(t *testing.T) {
	a := &Adapter{}
	first := a.sessionChatID("-10012345", 42)
	for i := range 1000 {
		got := a.sessionChatID("-10012345", 42)
		if got != first {
			t.Fatalf("Topic re-entry changed chatID: %q vs %q (iter %d)", got, first, i)
		}
	}
}

// TestSessionChatID_GroupTopic_DifferentTopicsAreDifferent
// ensures two topics in the same group produce different chatIDs
// (each gets its own ChatSession).
func TestSessionChatID_GroupTopic_DifferentTopicsAreDifferent(t *testing.T) {
	a := &Adapter{}
	topic42 := a.sessionChatID("-10012345", 42)
	topic88 := a.sessionChatID("-10012345", 88)
	if topic42 == topic88 {
		t.Fatalf("expected different chatIDs for topics 42 and 88, both = %q", topic42)
	}
}

// TestSessionTopicID_Alignment verifies that sessionTopicID
// returns the thread_id that was passed to sessionChatID (round
// trip via the chatID string).
func TestSessionTopicID_Alignment(t *testing.T) {
	a := &Adapter{}
	tests := []struct {
		rawChatID string
		threadID  int
	}{
		{"8684538097", 0},
		{"-10012345", 0},
		{"-10012345", 42},
		{"-10012345", 88},
	}
	for _, tt := range tests {
		chatID := a.sessionChatID(tt.rawChatID, tt.threadID)
		got := a.sessionTopicID(chatID)
		if got != tt.threadID {
			t.Errorf("sessionTopicID(%q) = %d, want %d", chatID, got, tt.threadID)
		}
	}
}
