package slack

import (
	"strings"
	"testing"
)

func TestSessionChatID_RoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		team     string
		channel  string
		threadTS string
		want     string
	}{
		{"channel top level", "T1", "C1", "", "sl_T1:C1"},
		{"inside thread", "T1", "C1", "1712345678.9001", "sl_T1:C1:1712345678.9001"},
		{"dm", "T1", "D9", "", "sl_T1:D9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sessionChatID(tc.team, tc.channel, tc.threadTS)
			if got != tc.want {
				t.Fatalf("sessionChatID = %q, want %q", got, tc.want)
			}
			team, channel, thread, ok := splitSessionID(got)
			if !ok {
				t.Fatalf("splitSessionID(%q) reported not-ok", got)
			}
			if team != tc.team || channel != tc.channel || thread != tc.threadTS {
				t.Fatalf("round trip = (%q,%q,%q), want (%q,%q,%q)",
					team, channel, thread, tc.team, tc.channel, tc.threadTS)
			}
		})
	}
}

// A thread_ts contains a dot; the ":" separator must not be confused
// by it.
func TestSplitSessionID_ThreadTSKeepsItsDot(t *testing.T) {
	_, _, thread, ok := splitSessionID("sl_T1:C1:1712345678.9001")
	if !ok {
		t.Fatal("expected ok")
	}
	if thread != "1712345678.9001" {
		t.Fatalf("thread = %q, want 1712345678.9001", thread)
	}
}

func TestSessionChatID_EmptyInputsProduceNoID(t *testing.T) {
	if got := sessionChatID("", "C1", ""); got != "" {
		t.Fatalf("missing team should yield empty chat id, got %q", got)
	}
	if got := sessionChatID("T1", "", ""); got != "" {
		t.Fatalf("missing channel should yield empty chat id, got %q", got)
	}
}

// Chat ids from other channels must be rejected, since the runtime
// hands every adapter every chat id.
func TestSplitSessionID_RejectsForeignIDs(t *testing.T) {
	for _, id := range []string{
		"tg_-100123",             // telegram
		"oc_7cc94a3ed15afb8ac60", // feishu
		"",
		"sl_",
		"sl_T1",        // missing channel
		"sl_T1:C1:a:b", // too many segments
		"sl_:C1",       // empty team
		"sl_T1:",       // empty channel
		"sl_T1:C1:",    // empty thread
		"slack_T1:C1",  // wrong prefix
	} {
		if _, _, _, ok := splitSessionID(id); ok {
			t.Fatalf("splitSessionID(%q) should be rejected", id)
		}
	}
}

func TestComputeHasMention_DMInvariant(t *testing.T) {
	// The WatchMode gate in chatsession drops non-mention group
	// traffic. It can only do that safely because a DM is ALWAYS
	// flagged as a mention — otherwise "watch mentions only" would
	// silently swallow every direct message.
	for _, text := range []string{"hello", "", "no mention here", "/cwd /tmp"} {
		if !computeHasMention(channelTypeIM, text, "UBOT", "") {
			t.Fatalf("DM text %q must always be treated as a mention", text)
		}
	}
}

func TestComputeHasMention_Channel(t *testing.T) {
	cases := []struct {
		name         string
		channelType  string
		text         string
		parentUserID string
		want         bool
	}{
		{"explicit mention", "channel", "<@UBOT> hi", "", true},
		{"no mention", "channel", "just chatting", "", false},
		{"here broadcast", "channel", "<!here> heads up", "", true},
		{"channel broadcast", "channel", "<!channel> ship it", "", true},
		{"slash command", "channel", "/cwd /tmp", "", true},
		{"reply to bot", "channel", "yes please", "UBOT", true},
		{"reply to human", "channel", "yes please", "UHUMAN", false},
		{"other user mentioned", "channel", "<@UOTHER> hi", "", false},
		{"private channel no mention", "group", "chatting", "", false},
		{"unknown type defaults open", "", "chatting", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeHasMention(tc.channelType, tc.text, "UBOT", tc.parentUserID)
			if got != tc.want {
				t.Fatalf("computeHasMention = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStripMentionPrefix(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// The reason this exists: ParseCommand requires "/" at
		// index 0, so "<@UBOT> /cwd" would otherwise never parse.
		{"mention then slash command", "<@UBOT> /cwd /tmp", "/cwd /tmp"},
		{"mention with display name", "<@UBOT|nightme> /help", "/help"},
		{"broadcast then command", "<!here> /status", "/status"},
		{"stacked prefixes", "<!channel> <@UBOT> hello", "hello"},
		{"no mention", "plain text", "plain text"},
		{"mid-text mention preserved", "look at <@UBOT> here", "look at <@UBOT> here"},
		{"other user not stripped", "<@UOTHER> hello", "<@UOTHER> hello"},
		{"mention only", "<@UBOT>", ""},
		{"unterminated tag left alone", "<@UBOT hello", "<@UBOT hello"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripMentionPrefix(tc.in, "UBOT"); got != tc.want {
				t.Fatalf("stripMentionPrefix(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSplitRunes_RespectsCeilingAndPrefersBoundaries(t *testing.T) {
	long := strings.Repeat("word ", 40) // 200 runes
	parts := splitRunes(long, 50)
	if len(parts) < 4 {
		t.Fatalf("expected several parts, got %d", len(parts))
	}
	for i, p := range parts {
		if n := len([]rune(p)); n > 50 {
			t.Fatalf("part %d has %d runes, over the 50 ceiling", i, n)
		}
	}
	if joined := strings.Join(parts, ""); joined != long {
		t.Fatal("splitting must not lose or duplicate content")
	}
}

func TestSplitRunes_ShortTextIsUntouched(t *testing.T) {
	parts := splitRunes("hello", maxMarkdownChunkRunes)
	if len(parts) != 1 || parts[0] != "hello" {
		t.Fatalf("short text should pass through, got %#v", parts)
	}
}

func TestSplitRunes_CountsRunesNotBytes(t *testing.T) {
	// A CJK rune is 3 bytes; a byte-based split would cut it into
	// mojibake and undercount the budget by 3x.
	cjk := strings.Repeat("测", 100)
	parts := splitRunes(cjk, 40)
	for _, p := range parts {
		if n := len([]rune(p)); n > 40 {
			t.Fatalf("part has %d runes, over ceiling", n)
		}
	}
	if strings.Join(parts, "") != cjk {
		t.Fatal("CJK content was corrupted by splitting")
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("hello", 10); got != "hello" {
		t.Fatalf("under limit should pass through, got %q", got)
	}
	got := truncateRunes("hello world", 8)
	if n := len([]rune(got)); n != 8 {
		t.Fatalf("truncated to %d runes, want 8", n)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncation should be visible, got %q", got)
	}
}

func TestFooterBlocks(t *testing.T) {
	if blocks := footerBlocks(nil); blocks != nil {
		t.Fatal("no lines should produce no blocks so callers can append unconditionally")
	}
	if blocks := footerBlocks([]string{"", "   "}); blocks != nil {
		t.Fatal("blank lines should produce no blocks")
	}
	blocks := footerBlocks([]string{"🤖: claude", "💰: 1k"})
	if len(blocks) != 2 {
		t.Fatalf("expected divider + context, got %d blocks", len(blocks))
	}
}

func TestHeartbeatText(t *testing.T) {
	// Mirrors the F-63 contract: the placeholder and the counters
	// are mutually exclusive, never concatenated.
	if got := heartbeatText(nil); got != "🤖 Working" {
		t.Fatalf("nil heartbeat = %q, want the bare placeholder", got)
	}
	hb := hbSnapshot(0, 0)
	if got := heartbeatText(&hb); got != "🤖 Working" {
		t.Fatalf("zero counters = %q, want the bare placeholder", got)
	}
	hb = hbSnapshot(3, 0)
	if got := heartbeatText(&hb); got != "💭 3" {
		t.Fatalf("think only = %q", got)
	}
	hb = hbSnapshot(0, 5)
	if got := heartbeatText(&hb); got != "🔧 5" {
		t.Fatalf("tool only = %q", got)
	}
	hb = hbSnapshot(3, 5)
	if got := heartbeatText(&hb); got != "💭 3 · 🔧 5" {
		t.Fatalf("both = %q", got)
	}
	if strings.Contains(heartbeatText(&hb), "Working") {
		t.Fatal("counters must replace the Working placeholder, not prefix it")
	}
}

func TestToolTitle(t *testing.T) {
	if got := toolTitle(nil); got != "tool" {
		t.Fatalf("nil tool = %q", got)
	}
	if got := toolTitle(toolInfo("Bash", "git status")); got != "Bash(git status)" {
		t.Fatalf("got %q", got)
	}
	if got := toolTitle(toolInfo("Read", "")); got != "Read" {
		t.Fatalf("no args should omit the parens, got %q", got)
	}
	// Multi-line args collapse so the card title stays one line.
	if got := toolTitle(toolInfo("Bash", "a\n  b")); got != "Bash(a b)" {
		t.Fatalf("got %q", got)
	}
}

func TestParseActionValue(t *testing.T) {
	if v := encodeActionValue("req-1", "allow"); v != "req-1::allow" {
		t.Fatalf("encode = %q", v)
	}
	req, opt, ok := parseActionValue("req-1::allow")
	if !ok || req != "req-1" || opt != "allow" {
		t.Fatalf("parse = (%q,%q,%v)", req, opt, ok)
	}
	if _, _, ok := parseActionValue("garbage"); ok {
		t.Fatal("a value without the separator must be rejected")
	}
	if _, _, ok := parseActionValue("::allow"); ok {
		t.Fatal("an empty request id must be rejected")
	}
}
