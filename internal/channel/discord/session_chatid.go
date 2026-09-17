package discord

import "strings"

// chatIDPrefix is the discord channel namespace tag attached to
// every ChatID the adapter produces. Matches the two-letter
// convention used by the other channels (telegram "tg_",
// feishu "oc_", slack "sl_", bot "bt_") so chatstore's
// ChatIDPrefixes-based validation picks it up automatically.
//
// Discord channel ids are 64-bit snowflakes encoded as decimal
// strings; the raw id is valid UTF-8 digits so we don't need any
// escaping.
const chatIDPrefix = "dc_"

// sessionChatID is the canonical ChatID for a Discord channel. A
// thread channel has its own snowflake in Discord, so no thread
// suffix is needed (per F-33 D4: nightme does not introduce the
// thread concept into its data model).
func sessionChatID(channelID string) string {
	return chatIDPrefix + channelID
}

// splitSessionID parses "dc_<channelID>" and returns the raw
// channel id. Returns ok=false when the session id is missing the
// prefix or carries an empty body — both forms are treated as
// "not a discord chat id" by the adapter's Send dispatch.
func splitSessionID(sessionID string) (channelID string, ok bool) {
	if !strings.HasPrefix(sessionID, chatIDPrefix) {
		return "", false
	}
	body := sessionID[len(chatIDPrefix):]
	if body == "" {
		return "", false
	}
	return body, true
}

// rawChannelIDFromSession strips the "dc_" prefix for use in
// Discord REST API calls. Falls back to the input string when the
// prefix is absent so a misrouted chat id produces a 404 from
// Discord rather than a panic — Discord's own error is the more
// useful diagnostic for the user.
func rawChannelIDFromSession(chatID string) string {
	raw, ok := splitSessionID(chatID)
	if !ok {
		return chatID
	}
	return raw
}
