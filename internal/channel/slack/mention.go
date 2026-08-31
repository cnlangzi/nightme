package slack

import (
	"strings"
)

// computeHasMention decides whether an inbound message is addressed
// to the bot. The adapter only stamps the flag; the actual gate lives
// in chatsession, which combines it with the chat's WatchMode (see
// internal/chatsession/watchmode.go).
//
// Rules, mirroring the feishu adapter's contract:
//
//   - DM (channelType "im"): always true. Every DM is implicitly
//     addressed to the bot. This is the invariant the WatchMode gate
//     relies on to drop non-mention group traffic without ever
//     silencing a DM.
//   - group DM / channel: true iff the text mentions the bot's user
//     id, @here / @channel / @everyone, is a slash command, or is a
//     reply to one of the bot's own messages.
//   - unknown channel type: true (over-processing beats dropping).
func computeHasMention(channelType, text, botUserID, parentUserID string) bool {
	if channelType == channelTypeIM {
		return true
	}
	if channelType == "" {
		return true
	}
	if parentUserID != "" && botUserID != "" && parentUserID == botUserID {
		// The user replied to something the bot said.
		return true
	}
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "/") {
		return true
	}
	lower := strings.ToLower(trimmed)
	for _, broadcast := range []string{"<!here", "<!channel", "<!everyone"} {
		if strings.Contains(lower, broadcast) {
			return true
		}
	}
	if botUserID != "" && strings.Contains(text, "<@"+botUserID+">") {
		return true
	}
	return false
}

// stripMentionPrefix removes leading bot mentions and broadcast tags
// so slash commands survive an @-prefixed invocation.
//
// Slack renders mentions as "<@U123>" and broadcasts as "<!here>".
// A group message reading "<@UBOT> /cwd /tmp" must reach the command
// parser as "/cwd /tmp" — parser.ParseCommand requires a literal "/"
// at position 0 (internal/gateway/parser.go).
//
// Only LEADING mentions are stripped, and only when followed by
// whitespace or end-of-string; a mention in the middle of a sentence
// is part of what the user meant to say and is left alone.
func stripMentionPrefix(text, botUserID string) string {
	out := strings.TrimLeft(text, " \t ")
	for {
		token, rest, ok := leadingMentionToken(out, botUserID)
		if !ok {
			break
		}
		_ = token
		out = strings.TrimLeft(rest, " \t ")
	}
	return out
}

// leadingMentionToken splits a leading mention token off s. It
// returns ok == false when s does not start with a strippable
// mention.
func leadingMentionToken(s, botUserID string) (token, rest string, ok bool) {
	if !strings.HasPrefix(s, "<@") && !strings.HasPrefix(s, "<!") {
		return "", "", false
	}
	end := strings.Index(s, ">")
	if end < 0 {
		return "", "", false
	}
	token = s[:end+1]
	rest = s[end+1:]

	// Broadcast tags (<!here>, <!channel>, <!everyone>) always strip.
	if strings.HasPrefix(token, "<!") {
		return token, rest, true
	}
	// User mentions strip only when they name the bot. Slack may
	// append a display name: "<@U123|nightme>".
	inner := strings.TrimSuffix(strings.TrimPrefix(token, "<@"), ">")
	if idx := strings.Index(inner, "|"); idx >= 0 {
		inner = inner[:idx]
	}
	if botUserID != "" && inner == botUserID {
		return token, rest, true
	}
	return "", "", false
}
