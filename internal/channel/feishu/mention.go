// F-watch: mention prefix strip + HasMention detection.
//
// See docs/channel/feishu.md §6.10 and docs/SPEC.md §3.1.1 for
// the design rationale. Quick recap of the contract:
//
//   - Feishu renders @-mentions inside text content as placeholders
//     like "@_user_1" or "@_all" with metadata in
//     message.Mentions[].Key. When the user @-s the bot, the
//     message text typically starts with "@_user_1 " (placeholder
//
//   - space), which breaks gateway.ParseCommand (which requires
//     HasPrefix "/").
//
//   - We strip leading @bot_key / @_all placeholders from text so
//     slash commands parse correctly. Mid-text mentions are
//     preserved (the user's actual content).
//
//   - We also compute HasMention, which captures the ORIGINAL
//     message semantic ("did this message address the bot or
//     @_all") independent of the stripped text. Gateway dispatcher
//     combines HasMention with ChatSession.WatchMode to drop
//     non-mention group messages when WatchMode == WatchModeMention.
//
//   - Chat type detection: DM (chat_type="p2p") is always
//     HasMention=true (every DM message is implicitly addressed to
//     the bot). Group/topic_group check the Mentions array.
//     Unknown chat_type falls back to HasMention=true (safe
//     over-process rather than drop).
package feishu

import (
	"context"
	"encoding/json"
	"strings"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// fetchBotOpenID lazily fetches and caches the bot's own open_id
// via the SDK's GetBotIdentity call (which has its own 30min TTL).
//
// Behavior:
//   - First call: triggers a single sync fetch; result cached for
//     adapter lifetime.
//   - Subsequent calls: return the cached value, zero network cost.
//   - Fetch failure: returns "" and logs at warn level. The caller
//     (computeHasMention) treats "" as "unknown bot id" — for group
//     messages, falls back to @_all-only detection (if @_all is in
//     mentions, HasMention=true; else false). For DM, HasMention is
//     still true regardless. This is the conservative path: we
//     drop the message rather than mis-attribute to a wrong bot
//     id, but DM is unaffected.
//
// Context: handleMessage passes the per-message ctx; the SDK
// fetch respects ctx cancellation.
func (a *Adapter) fetchBotOpenID(ctx context.Context) string {
	a.botOpenIDOnce.Do(func() {
		openID, err := a.lookupBotOpenID(ctx)
		if err != nil {
			if a.logger != nil {
				a.logger.Warn("feishu: bot identity fetch failed; HasMention will fall back to conservative defaults",
					"err", err)
			}
			return
		}
		a.botOpenID = openID
	})
	return a.botOpenID
}

// lookupBotOpenID does the actual SDK call. Split out from
// fetchBotOpenID so the sync.Once body stays small and testable.
//
// Implementation: GET /open-apis/bot/v3/info with tenant access
// token. This is the same call the SDK's internal
// Channel.GetBotIdentity makes (see
// larksuite/oapi-sdk-go/v3@v3.9.9/channel/channel.go:210). We
// replicate it here because nightme uses lark.NewClient (the
// standard REST client), not lark.NewChatBot (which exposes the
// SDK's Channel type with GetBotIdentity). The raw REST path is
// stable across SDK versions.
//
// Returns the bot's open_id on success, empty string + error
// otherwise. Caller (fetchBotOpenID) treats empty as "unknown".
func (a *Adapter) lookupBotOpenID(ctx context.Context) (string, error) {
	if a.larkClient == nil {
		return "", errBotClientUnavailable
	}
	resp, err := a.larkClient.Get(ctx, "/open-apis/bot/v3/info", nil, larkcore.AccessTokenTypeTenant)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != 200 {
		return "", errBotIdentityUnavailable
	}
	// Response body shape:
	//   { "code": 0, "msg": "ok", "bot": { "open_id": "ou_xxx", ... } }
	var payload struct {
		Bot struct {
			OpenID string `json:"open_id"`
		} `json:"bot"`
	}
	if err := json.Unmarshal(resp.RawBody, &payload); err != nil {
		return "", err
	}
	if payload.Bot.OpenID == "" {
		return "", errBotIdentityUnavailable
	}
	return payload.Bot.OpenID, nil
}

// errBotClientUnavailable and errBotIdentityUnavailable are
// sentinel errors so callers (and tests) can distinguish "we
// couldn't reach the SDK" from "SDK responded but no identity".
// They are not user-visible; logs only.
var (
	errBotClientUnavailable   = botClientUnavailableErr{}
	errBotIdentityUnavailable = botIdentityUnavailableErr{}
)

type botClientUnavailableErr struct{}

func (botClientUnavailableErr) Error() string {
	return "feishu: lark client not initialized"
}

type botIdentityUnavailableErr struct{}

func (botIdentityUnavailableErr) Error() string {
	return "feishu: GetBotIdentity returned nil"
}

// computeHasMention returns true if the original message
// addresses the bot (@bot open_id) or @_all, or if the chat type
// is DM (every DM message is implicitly addressed).
//
//   - chat_type == "p2p" → true (DM always)
//   - chat_type == "group" or "topic_group" → true if mentions
//     contain botOpenID or "@_all"; otherwise false
//   - unknown / empty chat_type → true (safe fallback: prefer
//     over-process to drop)
//
// botOpenID == "" (identity not yet fetched or fetch failed) is
// handled gracefully: DM still returns true; group falls back to
// @_all-only detection (any @_all → true).
func computeHasMention(message *larkim.EventMessage, botOpenID string) bool {
	chatType := ""
	if message.ChatType != nil {
		chatType = *message.ChatType
	}
	switch chatType {
	case "p2p":
		// DM: every message implicitly addressed to bot
		return true
	case "group", "topic_group":
		// group: check mentions for bot or @_all
		for _, m := range message.Mentions {
			if m == nil {
				continue
			}
			if m.Key != nil && *m.Key == "@_all" {
				return true
			}
			if botOpenID != "" && m.Id != nil && m.Id.OpenId != nil && *m.Id.OpenId == botOpenID {
				return true
			}
		}
		return false
	default:
		// unknown / empty: safe fallback
		return true
	}
}

// stripMentionPrefix removes consecutive leading @bot_key and/or
// @_all placeholders + following whitespace from text, so the
// remainder can be passed to gateway.ParseCommand without
// breaking slash-command detection.
//
// Rules:
//   - Only strips LEADING mentions (anchored at start of text).
//   - Strips in a loop: "@_all @bot hello" → "hello" (both gone).
//   - Mention must be followed by at least one whitespace char
//     (space, tab, NBSP) — prevents stripping mid-text mentions
//     that happen to land at position 0 by coincidence.
//   - Mid-text mentions are NOT touched.
//   - Strips "@_all" without needing botOpenID.
//
// Returns the stripped text. Empty result is valid (e.g. "@_all"
// alone with no following content).
func stripMentionPrefix(text string, mentions []*larkim.MentionEvent, botOpenID string) string {
	if text == "" || len(mentions) == 0 {
		return text
	}

	// Build set of mention keys we should strip: bot's own key +
	// literal "@_all". Keys are the placeholders Feishu uses in
	// text content (e.g. "@_user_1").
	stripKeys := make(map[string]bool)
	if botOpenID != "" {
		for _, m := range mentions {
			if m == nil || m.Key == nil {
				continue
			}
			if m.Id != nil && m.Id.OpenId != nil && *m.Id.OpenId == botOpenID {
				stripKeys[*m.Key] = true
			}
		}
	}
	stripKeys["@_all"] = true

	// Loop: strip leading mention + whitespace, repeat as long as
	// the new leading token is also a mention. The loop bound
	// is len(mentions) since each iteration consumes at least one
	// mention; this prevents pathological infinite loops if logic
	// is wrong.
	for range mentions {
		consumed := false
		for key := range stripKeys {
			if !strings.HasPrefix(text, key) {
				continue
			}
			after := text[len(key):]
			// Must be followed by at least one whitespace
			// char (U+0020 / U+0009 / U+00A0). An empty
			// remainder is also fine (mention was the entire
			// content).
			if after == "" {
				text = ""
				consumed = true
				break
			}
			if r := []rune(after)[0]; r != ' ' && r != '\t' && r != '\u00A0' {
				continue
			}
			text = strings.TrimLeft(after, " \t\u00A0")
			consumed = true
			break
		}
		if !consumed {
			break
		}
	}
	return text
}
