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

func (a *Adapter) sendTelegramMessage(ctx context.Context, chatID string, topicID int, text string, keyboard map[string]any) (SendMessageResult, error) {
	params := map[string]any{
		"chat_id": chatID,
		"text":    text,
	}
	if topicID > 0 {
		params["message_thread_id"] = topicID
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

func (a *Adapter) setMessageReaction(ctx context.Context, chatID string, messageID int, emoji string) error {
	reaction := []any{}
	if emoji != "" {
		reaction = append(reaction, map[string]any{"type": "emoji", "emoji": emoji})
	}
	return a.apiCall(ctx, "setMessageReaction", map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"reaction":   reaction,
	}, nil)
}

func (a *Adapter) downloadTelegramFile(ctx context.Context, fileID string) (string, error) {
	var result File
	if err := a.apiCall(ctx, "getFile", map[string]any{"file_id": fileID}, &result); err != nil {
		return "", err
	}
	return result.FilePath, nil
}

func (a *Adapter) sendText(ctx context.Context, chatID string, topicID int, text string) error {
	parts, err := splitTelegramText(text, maxTelegramTextLength)
	if err != nil {
		return err
	}
	for _, part := range parts {
		if _, err := a.sendTelegramMessage(ctx, chatID, topicID, part, nil); err != nil {
			return err
		}
	}
	return nil
}

func (a *Adapter) sendRenderedText(ctx context.Context, chatID string, topicID int, text string) error {
	rendered, err := RenderMarkdown(text)
	if err != nil {
		return err
	}
	return a.sendText(ctx, chatID, topicID, rendered)
}

func (a *Adapter) topicForChat(chatID string) int {
	if state, ok := a.state.topicForChat(chatID); ok {
		return state.TopicID
	}
	return 0
}

// sessionTopicID resolves the topic_id to send into given a
// session ChatID. In shared mode (default), ChatID is the raw
// chat_id and we look up the (single) topic for that chat. In
// separate mode, ChatID has the form "chat_id:topic_id" and we
// parse the trailing integer.
func (a *Adapter) sessionTopicID(sessionID string) int {
	if a == nil {
		return 0
	}
	if a.config.TopicMode == "separate" {
		if chatID, topicID, ok := splitSessionID(sessionID); ok {
			if state, ok2 := a.state.topic(chatID, topicID); ok2 {
				return state.TopicID
			}
			return topicID
		}
		return 0
	}
	return a.topicForChat(sessionID)
}

// sessionChatID constructs the inbound ChatID from the raw
// Telegram chat_id and topic_id. In shared mode, all topics in a
// chat share the same ChatSession; in separate mode, each topic
// gets its own ChatSession.
func (a *Adapter) sessionChatID(chatID string, topicID int) string {
	if a != nil && a.config.TopicMode == "separate" && topicID > 0 {
		return chatID + ":" + strconv.Itoa(topicID)
	}
	return chatID
}

// splitSessionID parses a separate-mode ChatID "chat_id:topic_id"
// into its parts. Returns ok=false if ChatID has no ":" suffix.
func splitSessionID(sessionID string) (string, int, bool) {
	idx := strings.LastIndex(sessionID, ":")
	if idx <= 0 || idx >= len(sessionID)-1 {
		return sessionID, 0, false
	}
	chatID := sessionID[:idx]
	topicID, err := strconv.Atoi(sessionID[idx+1:])
	if err != nil {
		return sessionID, 0, false
	}
	return chatID, topicID, true
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
