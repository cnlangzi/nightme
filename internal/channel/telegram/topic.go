package telegram

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/cnlangzi/nightme/internal/messages"
)

const maxTelegramTextLength = 3900

func (a *Adapter) createTopic(ctx context.Context, chatID, name string) (int, error) {
	var result ForumTopic
	if err := a.apiCall(ctx, "createForumTopic", map[string]any{
		"chat_id":    chatID,
		"name":       strings.TrimSpace(name),
		"icon_color": 0x6FB9F0,
	}, &result); err != nil {
		return 0, err
	}
	if result.MessageThreadID == 0 {
		return 0, errors.New("telegram: createForumTopic returned empty message_thread_id")
	}
	return result.MessageThreadID, nil
}

// sendTelegramMessage is the single Telegram sendMessage egress.
//
// Parameters:
//   - topicID > 0: route into a real Telegram Forum topic
//     (message_thread_id set on the API call).
//   - replyToMessageID > 0: pass as reply_to_message_id so Telegram
//     visually chains the new bubble to the given message. Used in
//     DM / main-window flows to tie OutThinking / OutTool /
//     OutReply / OutResult to the per-turn placeholder message
//     (see docs/channel/telegram.md §11.11).
//   - text == "": omit text field — used by ForceReply prompts
//     where only reply_markup carries semantic content. parse_mode
//     is skipped when text is empty to avoid API rejection.
func (a *Adapter) sendTelegramMessage(ctx context.Context, chatID string, topicID int, replyToMessageID int, text string, keyboard map[string]any) (SendMessageResult, error) {
	params := map[string]any{
		"chat_id": chatID,
		"text":    text,
	}
	if topicID > 0 {
		params["message_thread_id"] = topicID
	}
	if replyToMessageID > 0 {
		params["reply_to_message_id"] = replyToMessageID
	}
	if text != "" {
		params["parse_mode"] = "HTML"
	}
	if keyboard != nil {
		params["reply_markup"] = keyboard
	}
	var result SendMessageResult
	if err := a.apiCall(ctx, "sendMessage", params, &result); err != nil {
		return SendMessageResult{}, err
	}
	return result, nil
}

func (a *Adapter) editTelegramMessage(ctx context.Context, chatID string, messageID int, text string, keyboard map[string]any) error {
	params := map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
	}
	if text != "" {
		params["text"] = text
		params["parse_mode"] = "HTML"
	}
	if keyboard != nil {
		params["reply_markup"] = keyboard
	}
	return a.apiCall(ctx, "editMessageText", params, nil)
}

func (a *Adapter) editTelegramKeyboard(ctx context.Context, chatID string, messageID int, keyboard map[string]any) error {
	return a.apiCall(ctx, "editMessageReplyMarkup", map[string]any{
		"chat_id":      chatID,
		"message_id":   messageID,
		"reply_markup": keyboard,
	}, nil)
}

// setMessageReactions replaces the reaction list on a message
// with the supplied list. Telegram's setMessageReaction is a SET
// semantic (each call REPLACES the entire list).
//
// v6.1+ contract: callers always pass a single-emoji list.
// Telegram bots are limited to ONE reaction per message; the
// adapter reserves the user-message reaction slot for
// MessageSubmitted ("AI thinking") and the placeholder-message
// reaction slot for OnPromptEnded ("done / ✅"). Queued and
// Done are silent drops on the user message — they don't burn
// the reaction slot.
//
// Pass an empty (non-nil) slice to clear all reactions on the
// message — the Telegram API distinguishes [] (clear) from
// missing/null (no change).
func (a *Adapter) setMessageReactions(ctx context.Context, chatID string, messageID int, reactions []map[string]any) error {
	if reactions == nil {
		reactions = []map[string]any{}
	}
	// Convert to []any so the wire format is consistent with
	// the rest of the outbound params (every Channel call site
	// builds `[]any{...}` not `[]map[string]any{...}`).
	asAny := make([]any, len(reactions))
	for i, r := range reactions {
		asAny[i] = r
	}
	return a.apiCall(ctx, "setMessageReaction", map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"reaction":   asAny,
	}, nil)
}

func (a *Adapter) downloadTelegramFile(ctx context.Context, fileID string) (string, error) {
	var result File
	if err := a.apiCall(ctx, "getFile", map[string]any{"file_id": fileID}, &result); err != nil {
		return "", err
	}
	return result.FilePath, nil
}

// sendText sends already-rendered text, splitting at the 4096-char
// Telegram ceiling. replyToMessageID is threaded through every
// chunk so DM reply chains stay intact even when the rendered
// output overflows one message (see docs/channel/telegram.md
// §11.11).
func (a *Adapter) sendText(ctx context.Context, chatID string, topicID int, replyToMessageID int, text string) error {
	parts, err := splitTelegramText(text, maxTelegramTextLength)
	if err != nil {
		return err
	}
	for _, part := range parts {
		if _, err := a.sendTelegramMessage(ctx, chatID, topicID, replyToMessageID, part, nil); err != nil {
			return err
		}
	}
	return nil
}

// sendRenderedText was the v8 per-bubble path: take raw text,
// run RenderMarkdown, split at 3900 chars, send each piece.
// v9 chain rolled all text-emitting Kinds into the chain
// segment appendSegmentForKind path; no callers remain in
// production code. Removed 2026-08-23 (codex review).

// chatIDPrefix is the Telegram channel namespace tag attached to
// every InboundMessage.ChatID by the adapter. It exists so that
// Telegram chatIDs (which are raw integers) cannot collide with
// chatIDs from other channels (Feishu uses "oc_<hex>" natively;
// Slack and future channels will pick their own prefix).
const chatIDPrefix = "tg_"

// rawChatIDFromSession strips the channel prefix from a session
// ChatID for use in Telegram Bot API calls. The Bot API rejects
// non-numeric chat_id values, so any adapter code that talks to
// the API (sendMessage / editMessageText / setMessageReaction /
// etc.) must convert the namespaced session form back to the raw
// Telegram chat.id first.
//
// Falls back to the input on parse failure (e.g., a chatID
// passed in raw form by a unit test) so the API call still
// goes through and the API can reject it with a clear error.
func rawChatIDFromSession(chatID string) string {
	raw, _, ok := splitSessionID(chatID)
	if !ok {
		return chatID
	}
	return raw
}

// sessionChatID maps a Telegram update to a stable, channel-
// namespaced chatID used by the inbound pipeline.
//
// Format:
//   "tg_" + chat.id                    (private DM / group main window)
//   "tg_" + chat.id + ":" + thread_id  (user is in a real Telegram topic)
//
// The result is a pure function of (chat.id, thread_id). It does
// NOT depend on:
//   - The daemon's internal state (auto-created sentinel topics)
//   - Whether the chat has Forum Topics enabled
//
// The same chat therefore always produces the same chatID, across
// daemon restarts, config changes, and state file loss. This is
// the contract that lets /cwd in DM persist and find the same
// ChatSession on the next message.
func (a *Adapter) sessionChatID(rawChatID string, threadID int) string {
	if threadID > 0 {
		return chatIDPrefix + rawChatID + ":" + strconv.Itoa(threadID)
	}
	return chatIDPrefix + rawChatID
}

// sessionTopicID resolves the thread_id to send into given a
// session ChatID. Pure function over the chatID string: strips
// the "tg_" prefix, parses the optional ":thread_id" suffix,
// and returns 0 when the caller is in a non-topic message
// (private DM / group main window / channel).
func (a *Adapter) sessionTopicID(sessionID string) int {
	if a == nil {
		return 0
	}
	_, threadID, ok := splitSessionID(sessionID)
	if !ok {
		return 0
	}
	return threadID
}

// splitSessionID parses "tg_<chat.id>[:thread_id]" into parts.
// Returns ok=false when the input is missing the "tg_" prefix
// (caller treats it as a non-telegram chatID; feishu's "oc_<hex>"
// stays untouched by the telegram adapter) or when the body
// after the prefix is empty.
func splitSessionID(sessionID string) (rawChatID string, threadID int, ok bool) {
	if !strings.HasPrefix(sessionID, chatIDPrefix) {
		return "", 0, false
	}
	body := sessionID[len(chatIDPrefix):]
	if body == "" {
		return "", 0, false
	}
	// body is either "chatid" or "chatid:thread_id"
	idx := strings.Index(body, ":")
	if idx < 0 {
		return body, 0, true
	}
	tid, err := strconv.Atoi(body[idx+1:])
	if err != nil {
		return body, 0, true
	}
	return body[:idx], tid, true
}

func (a *Adapter) choiceKeyboard(state *ChoiceState) map[string]any {
	if state == nil || state.Choice == nil {
		return map[string]any{"inline_keyboard": []any{}}
	}
	options := state.Choice.Options
	if len(state.Choice.Questions) > state.Step {
		options = state.Choice.Questions[state.Step].Options
	}
	if len(options) == 0 && len(state.Choice.Questions) == 0 {
		return map[string]any{"inline_keyboard": []any{}}
	}
	rows := make([]any, 0, len(options)+1)
	for index, option := range options {
		label := option.Label
		if label == "" {
			label = option.ID
		}
		button := map[string]any{
			"text":          escapeHTML(label),
			"callback_data": choiceCallbackData(state, index),
		}
		if len(state.Choice.Questions) > 0 {
			rows = append(rows, []any{button})
		} else {
			if index%2 == 0 {
				rows = append(rows, []any{})
			}
			row := rows[len(rows)-1].([]any)
			row = append(row, button)
			rows[len(rows)-1] = row
		}
	}
	if len(state.Choice.Questions) > 0 {
		rows = append(rows, []any{map[string]any{
			"text":          "Type your answer",
			"callback_data": inputCallbackData(state),
		}})
	}
	return map[string]any{"inline_keyboard": rows}
}

func choiceCallbackData(state *ChoiceState, optionIndex int) string {
	return "c:" + shortID(state.RequestID) + ":" + strconv.Itoa(optionIndex)
}

func inputCallbackData(state *ChoiceState) string {
	return "i:" + shortID(state.RequestID)
}

func shortID(value string) string {
	if len(value) <= 16 {
		return value
	}
	return value[:8] + "-" + value[len(value)-8:]
}

func renderChoice(state *ChoiceState) string {
	if state == nil || state.Choice == nil {
		return ""
	}
	choice := state.Choice
	title := choice.Title
	if title == "" {
		title = "Action Needed"
	}
	if choice.Kind == messages.ChoiceKindPermission {
		if title == "Action Needed" {
			title = "Waiting for approval"
		}
	} else if title == "Waiting for approval" {
		title = "Action Needed"
	}
	var body strings.Builder
	body.WriteString("<b>")
	body.WriteString(escapeHTML(title))
	body.WriteString("</b>")
	if len(choice.Questions) > 0 {
		if state.Step < len(choice.Questions) {
			body.WriteString("\n\n")
			body.WriteString(escapeHTML(choice.Questions[state.Step].Question))
		}
	} else if choice.Body != "" {
		body.WriteString("\n\n")
		body.WriteString(escapeHTML(choice.Body))
	}
	return body.String()
}
