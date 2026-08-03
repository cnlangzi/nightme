package feishu

import (
	"testing"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// makeMention is a small constructor so test cases stay readable.
// key is the placeholder that appears in text (e.g. "@_user_1").
// id is the open_id the mention resolves to; pass "@_all" or
// "ou_bot" as test data.
func makeMention(key, id string) *larkim.MentionEvent {
	return &larkim.MentionEvent{
		Key: &key,
		Id: &larkim.UserId{
			OpenId: &id,
		},
	}
}

// TestStripMentionPrefix covers the F-watch §6.10 strip rules:
//
//   - leading bot mention + space → strip
//   - leading @_all + space → strip (no bot id needed)
//   - multiple leading mentions chained → strip all
//   - mid-text mentions → NOT touched
//   - empty remainder after mention → stripped to ""
//   - non-mention prefix → untouched
//   - missing trailing whitespace → mention NOT stripped (avoid
//     false positives when content itself starts with @_user_N)
//   - bot id mismatch → NOT stripped (only the bot's own mention)
func TestStripMentionPrefix(t *testing.T) {
	botKey := "@_user_1"
	botOpenID := "ou_bot"
	otherKey := "@_user_2"
	otherOpenID := "ou_other"

	mentions := []*larkim.MentionEvent{
		makeMention(botKey, botOpenID),
		makeMention(otherKey, otherOpenID),
	}

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "leading bot mention + space stripped",
			in:   botKey + " /watch on",
			want: "/watch on",
		},
		{
			name: "leading bot mention alone stripped to empty",
			in:   botKey,
			want: "",
		},
		{
			name: "leading @_all stripped without bot id",
			in:   "@_all /cwd /tmp",
			want: "/cwd /tmp",
		},
		{
			name: "multiple leading mentions chained stripped",
			in:   "@_all " + botKey + " hello world",
			want: "hello world",
		},
		{
			name: "mid-text mention untouched",
			in:   "look at this " + botKey + " bug",
			want: "look at this " + botKey + " bug",
		},
		{
			name: "no mention untouched",
			in:   "hello world",
			want: "hello world",
		},
		{
			name: "mention without trailing whitespace NOT stripped",
			in:   botKey + "/watch on", // no space between
			want: botKey + "/watch on",
		},
		{
			name: "non-bot mention not stripped",
			in:   otherKey + " hello",
			want: otherKey + " hello",
		},
		{
			name: "tab counts as whitespace separator",
			in:   botKey + "\t/watch on",
			want: "/watch on",
		},
		{
			name: "NBSP counts as whitespace separator",
			in:   botKey + "\u00A0/watch on",
			want: "/watch on",
		},
		{
			name: "empty input unchanged",
			in:   "",
			want: "",
		},
		{
			name: "only @_all with no content",
			in:   "@_all",
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripMentionPrefix(tc.in, mentions, botOpenID)
			if got != tc.want {
				t.Errorf("stripMentionPrefix(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestStripMentionPrefix_NoMentions covers the nil-mentions fast
// path: when the message has no mention metadata (rare but
// possible for empty mentions arrays), the text is returned
// untouched.
func TestStripMentionPrefix_NoMentions(t *testing.T) {
	got := stripMentionPrefix("hello world", nil, "ou_bot")
	if got != "hello world" {
		t.Errorf("stripMentionPrefix with nil mentions should return text unchanged, got %q", got)
	}
}

// TestComputeHasMention covers the F-watch §6.10 HasMention rules:
//
//   - chat_type == "p2p" → always true (DM)
//   - chat_type == "group" with bot mention → true
//   - chat_type == "group" with @_all → true
//   - chat_type == "group" with non-bot mention → false
//   - chat_type == "topic_group" → same as group
//   - chat_type == "" / unknown → true (safe fallback)
func TestComputeHasMention(t *testing.T) {
	botOpenID := "ou_bot"
	otherOpenID := "ou_other"

	cases := []struct {
		name        string
		chatType    string
		mentions    []*larkim.MentionEvent
		botOpenID   string
		wantMention bool
	}{
		{
			name:        "DM is always has mention",
			chatType:    "p2p",
			mentions:    nil,
			botOpenID:   "", // even without bot id, DM is true
			wantMention: true,
		},
		{
			name:        "DM with no mentions but bot id set",
			chatType:    "p2p",
			mentions:    nil,
			botOpenID:   botOpenID,
			wantMention: true,
		},
		{
			name:        "group with bot mention",
			chatType:    "group",
			mentions:    []*larkim.MentionEvent{makeMention("@_user_1", botOpenID)},
			botOpenID:   botOpenID,
			wantMention: true,
		},
		{
			name:        "group with @_all",
			chatType:    "group",
			mentions:    []*larkim.MentionEvent{makeMention("@_all", "@_all")},
			botOpenID:   "",
			wantMention: true,
		},
		{
			name:        "group with only non-bot mention",
			chatType:    "group",
			mentions:    []*larkim.MentionEvent{makeMention("@_user_2", otherOpenID)},
			botOpenID:   botOpenID,
			wantMention: false,
		},
		{
			name:        "group with no mentions",
			chatType:    "group",
			mentions:    nil,
			botOpenID:   botOpenID,
			wantMention: false,
		},
		{
			name:        "topic_group with bot mention",
			chatType:    "topic_group",
			mentions:    []*larkim.MentionEvent{makeMention("@_user_1", botOpenID)},
			botOpenID:   botOpenID,
			wantMention: true,
		},
		{
			name:        "topic_group with no mention",
			chatType:    "topic_group",
			mentions:    nil,
			botOpenID:   botOpenID,
			wantMention: false,
		},
		{
			name:        "unknown chat_type defaults true (safe)",
			chatType:    "future_chat_type",
			mentions:    nil,
			botOpenID:   "",
			wantMention: true,
		},
		{
			name:        "empty chat_type defaults true",
			chatType:    "",
			mentions:    nil,
			botOpenID:   "",
			wantMention: true,
		},
		{
			name:        "group with bot id unknown but @_all present",
			chatType:    "group",
			mentions:    []*larkim.MentionEvent{makeMention("@_all", "@_all")},
			botOpenID:   "", // identity fetch failed
			wantMention: true, // @_all still detected
		},
		{
			name:        "group with bot id unknown and no @_all",
			chatType:    "group",
			mentions:    []*larkim.MentionEvent{makeMention("@_user_1", "ou_unknown")},
			botOpenID:   "",
			wantMention: false, // can't identify bot, conservative drop
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := &larkim.EventMessage{
				ChatType: &tc.chatType,
				Mentions: tc.mentions,
			}
			got := computeHasMention(msg, tc.botOpenID)
			if got != tc.wantMention {
				t.Errorf("computeHasMention(chat_type=%q, %d mentions, botOpenID=%q) = %v, want %v",
					tc.chatType, len(tc.mentions), tc.botOpenID, got, tc.wantMention)
			}
		})
	}
}

// TestParseWatchMode covers the slash-command argument parser.
// Returns the WatchMode + a bool that is false for unknown
// inputs (caller should reply with a usage hint).
//
// WatchMode lives in package chatsession (see
// internal/chatsession/watchmode.go for the source-of-truth);
// tests for it are in internal/chatsession/watchmode_test.go.

// TestComputeHasMention_DMInvariant pins the F-watch invariant
// that DM messages are ALWAYS processed regardless of WatchMode.
// The contract is:
//
//   For any DM message (chat_type == "p2p"), regardless of
//   mentions / botOpenID / WatchMode setting, HasMention MUST be
//   true. This is what lets the gateway dispatcher gate drop
//   non-mention group messages without ever accidentally dropping
//   a DM.
//
// If this test regresses, F-watch will start dropping DM messages
// in chats that have WatchMode=WatchModeMention (the default).
// That's a hard user-visible bug. The corresponding
// dispatcher-side test lives in
// internal/gateway/dispatch_watch_test.go
// (TestDispatchInbound_WatchModeGate_DMInvariant).
func TestComputeHasMention_DMInvariant(t *testing.T) {
	cases := []struct {
		name      string
		botOpenID string
		mentions  []*larkim.MentionEvent
	}{
		{"DM with no bot id and no mentions", "", nil},
		{"DM with bot id set, no mentions", "ou_bot", nil},
		{"DM with bot id set, other-user mentions only", "ou_bot", []*larkim.MentionEvent{makeMention("@_user_2", "ou_other")}},
		{"DM with empty mentions array", "ou_bot", []*larkim.MentionEvent{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chatType := "p2p"
			msg := &larkim.EventMessage{
				ChatType: &chatType,
				Mentions: tc.mentions,
			}
			if !computeHasMention(msg, tc.botOpenID) {
				t.Errorf("DM invariant broken: computeHasMention returned false for chat_type=p2p with botOpenID=%q and %d mentions",
					tc.botOpenID, len(tc.mentions))
			}
		})
	}
}

// TestWatchMode_String covers the fmt.Stringer implementation.
//
// WatchMode lives in package chatsession; tests are colocated.