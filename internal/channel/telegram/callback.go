package telegram

import (
	"context"
	"errors"
	"strconv"
	"strings"

	commandServices "github.com/cnlangzi/nightme/internal/command/services"
	"github.com/cnlangzi/nightme/internal/messages"
)

// handleCallbackQuery is the entry point for every telegram
// inline_keyboard click. Telegram only allows ONE callback_data
// string per button (<= 64 bytes), so we encode:
//   - "c:<shortRequestID>:<optionIndex>" - option click
//   - "i:<shortRequestID>"             - "Type your answer" click
//
// Telegram also delivers CallbackQuery.Message which carries the
// MessageID of the prompt; we use it to recover the choice state
// when the short request id maps to multiple candidates.
//
// Permission / Question clicks are dispatched as
// InboundMessage.Action so the existing permission /
// AskUserQuestion machinery picks them up unchanged. Decision
// clicks (gtw) are dispatched as InboundMessage.Reaction so the
// gtw ReactionRouter picks them up unchanged. This mirrors the
// Feishu adapter's split.
func (a *Adapter) handleCallbackQuery(ctx context.Context, callback *CallbackQuery) {
	if callback == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil && a.logger != nil {
			a.logger.Error("telegram: handleCallbackQuery panicked", "panic", r)
		}
	}()
	data := strings.TrimSpace(callback.Data)
	if data == "" {
		a.answerCallback(ctx, callback.ID, "", false)
		return
	}
	parts := strings.SplitN(data, ":", 3)
	if len(parts) < 2 {
		a.answerCallback(ctx, callback.ID, "Unsupported action", true)
		return
	}
	switch parts[0] {
	case "c":
		a.handleChoiceClick(ctx, callback, parts[1], parts[2])
	case "i":
		a.handleInputClick(ctx, callback, parts[1])
	default:
		a.answerCallback(ctx, callback.ID, "Unsupported action", true)
	}
}

// handleChoiceClick processes an inline_keyboard option click.
func (a *Adapter) handleChoiceClick(ctx context.Context, callback *CallbackQuery, shortRequestID, optionIndexStr string) {
	requestID := a.resolveRequestID(shortRequestID, callback)
	state, ok := a.state.choiceByRequestID(requestID)
	if !ok {
		a.answerCallback(ctx, callback.ID, "This prompt has expired", true)
		return
	}
	if state.Settled {
		a.answerCallback(ctx, callback.ID, "Already settled", true)
		return
	}
	optionIndex, err := strconv.Atoi(optionIndexStr)
	if err != nil {
		a.answerCallback(ctx, callback.ID, "Invalid option", true)
		return
	}
	if optionIndex < 0 {
		a.answerCallback(ctx, callback.ID, "Invalid option", true)
		return
	}
	options := state.Choice.Options
	questionStep := -1
	if len(state.Choice.Questions) > 0 {
		if state.Step >= len(state.Choice.Questions) {
			a.answerCallback(ctx, callback.ID, "Prompt is complete", true)
			return
		}
		questionStep = state.Step
		options = state.Choice.Questions[state.Step].Options
	}
	if optionIndex >= len(options) {
		a.answerCallback(ctx, callback.ID, "Invalid option", true)
		return
	}
	option := options[optionIndex]
	switch state.Choice.Kind {
	case messages.ChoiceKindPermission:
		a.handlePermissionClick(ctx, callback, state, option)
	case messages.ChoiceKindQuestion:
		a.handleQuestionClick(ctx, callback, state, option, questionStep)
	case messages.ChoiceKindDecision:
		a.handleDecisionClick(ctx, callback, state, option)
	default:
		a.answerCallback(ctx, callback.ID, "Unsupported choice", true)
	}
}

// handlePermissionClick is the simple one-shot path.
func (a *Adapter) handlePermissionClick(ctx context.Context, callback *CallbackQuery, state *ChoiceState, option messages.ChoiceOption) {
	a.markSettled(state, option.ID)
	a.answerCallback(ctx, callback.ID, "Recorded", false)
	a.publishAction(state.ChatID, state.RequestID, option.ID, callback)
}

// handleQuestionClick advances the AskUserQuestion wizard one step.
func (a *Adapter) handleQuestionClick(ctx context.Context, callback *CallbackQuery, state *ChoiceState, option messages.ChoiceOption, questionStep int) {
	if questionStep < 0 || questionStep >= len(state.Choice.Questions) {
		a.answerCallback(ctx, callback.ID, "Invalid question", true)
		return
	}
	state.Picks[questionStep] = option.ID
	if questionStep+1 < len(state.Choice.Questions) {
		state.Step = questionStep + 1
		if err := a.state.putChoice(state); err != nil && a.logger != nil {
			a.logger.Warn("telegram: put choice state failed", "request_id", state.RequestID, "err", err)
		}
		text := renderChoice(state)
		keyboard := a.choiceKeyboard(state)
		if err := a.editTelegramMessage(ctx, state.ChatID, state.MessageID, text, keyboard); err != nil && a.logger != nil {
			a.logger.Warn("telegram: edit choice failed", "request_id", state.RequestID, "err", err)
		}
		a.answerCallback(ctx, callback.ID, "Recorded", false)
		return
	}
	batch := buildQuestionBatch(state.Choice.Questions, state.Picks)
	if batch != "" {
		a.markSettled(state, batch)
		a.answerCallback(ctx, callback.ID, "Recorded", false)
		a.publishAction(state.ChatID, state.RequestID, batch, callback)
		return
	}
	a.markSettled(state, "")
	a.answerCallback(ctx, callback.ID, "All questions skipped", false)
	a.publishAction(state.ChatID, state.RequestID, "", callback)
}

// handleDecisionClick turns a button click into a gtw reaction event.
func (a *Adapter) handleDecisionClick(ctx context.Context, callback *CallbackQuery, state *ChoiceState, option messages.ChoiceOption) {
	emoji := option.Emoji
	if emoji == "" {
		emoji = option.Label
	}
	if emoji == "" {
		emoji = option.ID
	}
	a.markSettled(state, option.ID)
	a.answerCallback(ctx, callback.ID, "Recorded", false)
	reaction := &commandServices.ReactionEvent{
		TargetMsgID: messageIDString(callback.Message),
		RequestID:   state.RequestID,
		Emoji:       emoji,
		UserID:      strconv.FormatInt(callback.From.ID, 10),
		ChatID:      state.ChatID,
	}
	select {
	case a.incoming <- messages.InboundMessage{
		ChatID:     state.ChatID,
		Reaction:   reaction,
		MessageID:  messageIDString(callback.Message),
		HasMention: true,
	}:
	case <-a.ctxDone():
	}
}

// handleInputClick triggers the "Type your answer" prompt.
func (a *Adapter) handleInputClick(ctx context.Context, callback *CallbackQuery, shortRequestID string) {
	requestID := a.resolveRequestID(shortRequestID, callback)
	state, ok := a.state.choiceByRequestID(requestID)
	if !ok {
		a.answerCallback(ctx, callback.ID, "This prompt has expired", true)
		return
	}
	if state.Settled {
		a.answerCallback(ctx, callback.ID, "Already settled", true)
		return
	}
	promptText := "Type your answer"
	if len(state.Choice.Questions) > 0 && state.Step < len(state.Choice.Questions) {
		promptText = "Q: " + state.Choice.Questions[state.Step].Question
	}
	replyMarkup := map[string]any{
		"force_reply":             true,
		"input_field_placeholder": "Type your answer...",
		"selective":               true,
	}
	result, err := a.sendTelegramMessage(ctx, state.ChatID, state.TopicID, escapeHTML(promptText), replyMarkup)
	if err != nil {
		a.answerCallback(ctx, callback.ID, "Failed to open input", true)
		if a.logger != nil {
			a.logger.Warn("telegram: force reply send failed", "request_id", requestID, "err", err)
		}
		return
	}
	step := -1
	questionID := ""
	if len(state.Choice.Questions) > 0 && state.Step < len(state.Choice.Questions) {
		step = state.Step
		questionID = state.Choice.Questions[state.Step].ID
	}
	state.Input = &InputState{
		PromptMessageID: result.MessageID,
		QuestionID:      questionID,
		Step:            step,
		Kind:            choiceKindName(state.Choice.Kind),
		OwnerID:         callback.From.ID,
	}
	if err := a.state.putChoice(state); err != nil && a.logger != nil {
		a.logger.Warn("telegram: put choice state failed", "request_id", state.RequestID, "err", err)
	}
	a.answerCallback(ctx, callback.ID, "Type your answer", false)
}

// handleForceReply consumes the user's typed answer.
func (a *Adapter) handleForceReply(ctx context.Context, message *Message) bool {
	if message == nil || message.ReplyToMessage == nil {
		return false
	}
	if message.From == nil {
		return false
	}
	chatID := strconv.FormatInt(message.Chat.ID, 10)
	replyToID := message.ReplyToMessage.MessageID
	state, ok := a.state.pendingInput(chatID, message.From.ID, replyToID)
	if !ok {
		return false
	}
	if state.Settled {
		return false
	}
	text := strings.TrimSpace(message.Text)
	if text == "" {
		text = strings.TrimSpace(message.Caption)
	}
	switch state.Choice.Kind {
	case messages.ChoiceKindPermission:
		a.markSettled(state, text)
		a.publishAction(state.ChatID, state.RequestID, text, nil)
	case messages.ChoiceKindQuestion:
		if state.Input == nil || state.Input.Step < 0 || state.Input.Step >= len(state.Picks) {
			return false
		}
		state.Picks[state.Input.Step] = messages.StoreQuestionCustom(text)
		if state.Input.Step+1 < len(state.Choice.Questions) {
			state.Step = state.Input.Step + 1
			state.Input = nil
			if err := a.state.putChoice(state); err != nil && a.logger != nil {
				a.logger.Warn("telegram: put choice state failed", "request_id", state.RequestID, "err", err)
			}
			text := renderChoice(state)
			keyboard := a.choiceKeyboard(state)
			if err := a.editTelegramMessage(ctx, state.ChatID, state.MessageID, text, keyboard); err != nil && a.logger != nil {
				a.logger.Warn("telegram: edit choice failed", "request_id", state.RequestID, "err", err)
			}
		} else {
			batch := buildQuestionBatch(state.Choice.Questions, state.Picks)
			a.markSettled(state, batch)
			a.publishAction(state.ChatID, state.RequestID, batch, nil)
		}
	case messages.ChoiceKindDecision:
		a.markSettled(state, text)
		reaction := &commandServices.ReactionEvent{
			TargetMsgID: strconv.Itoa(state.MessageID),
			RequestID:   state.RequestID,
			Emoji:       text,
			UserID:      strconv.FormatInt(message.From.ID, 10),
			ChatID:      state.ChatID,
		}
		select {
		case a.incoming <- messages.InboundMessage{
			ChatID:     state.ChatID,
			Reaction:   reaction,
			MessageID:  strconv.Itoa(state.MessageID),
			HasMention: true,
		}:
		case <-a.ctxDone():
		}
	default:
		return false
	}
	return true
}

// resolveRequestID maps a short request id back to the long id.
func (a *Adapter) resolveRequestID(shortRequestID string, callback *CallbackQuery) string {
	if callback != nil && callback.Message != nil {
		if state, ok := a.state.choiceByMessageID(callback.Message.MessageID); ok && state != nil {
			return state.RequestID
		}
	}
	if state, ok := a.state.choiceByShortID(shortRequestID); ok && state != nil {
		return state.RequestID
	}
	return shortRequestID
}

// publishAction pushes an InboundMessage.Action onto incoming.
func (a *Adapter) publishAction(chatID, requestID, option string, callback *CallbackQuery) {
	var raw any = callback
	var targetMsgID string
	if callback != nil {
		targetMsgID = messageIDString(callback.Message)
	}
	select {
	case a.incoming <- messages.InboundMessage{
		ChatID:     chatID,
		MessageID:  targetMsgID,
		HasMention: true,
		Action: &messages.ActionPayload{
			RequestID: requestID,
			Option:    option,
			Raw:       raw,
		},
	}:
	case <-a.ctxDone():
	}
}

// markSettled updates the local ChoiceState copy.
func (a *Adapter) markSettled(state *ChoiceState, selectedID string) {
	state.Settled = true
	if selectedID != "" {
		state.SelectedID = selectedID
	}
	state.Input = nil
	_ = a.state.putChoice(state)
}

// answerCallback is the user-facing toast on the click.
func (a *Adapter) answerCallback(ctx context.Context, callbackID, text string, alert bool) {
	if callbackID == "" {
		return
	}
	params := map[string]any{
		"callback_query_id": callbackID,
	}
	if text != "" {
		params["text"] = text
		params["show_alert"] = alert
	}
	if err := a.api.call(ctx, "answerCallbackQuery", params, nil); err != nil && a.logger != nil {
		a.logger.Debug("telegram: answer callback failed", "callback_id", callbackID, "err", err)
	}
}

// buildQuestionBatch assembles the final AskUserQuestion payload.
func buildQuestionBatch(questions []messages.ChoiceQuestion, picks []string) string {
	if len(questions) == 0 {
		return ""
	}
	if len(picks) < len(questions) {
		diff := len(questions) - len(picks)
		for index := 0; index < diff; index++ {
			picks = append(picks, "")
		}
	}
	out := make([]messages.QuestionPick, 0, len(questions))
	for index, question := range questions {
		if question.ID == "" {
			continue
		}
		out = append(out, messages.ParseStoredQuestionPick(question.ID, picks[index]))
	}
	if len(out) == 0 {
		return ""
	}
	return messages.EncodeQuestionPicks(out)
}

// messageIDString is the channel-native MessageID formatter.
func messageIDString(message *Message) string {
	if message == nil {
		return ""
	}
	return strconv.Itoa(message.MessageID)
}

// choiceKindName renders a ChoiceKind for the InputState audit trail.
func choiceKindName(kind messages.ChoiceKind) string {
	switch kind {
	case messages.ChoiceKindPermission:
		return "permission"
	case messages.ChoiceKindQuestion:
		return "question"
	case messages.ChoiceKindDecision:
		return "decision"
	}
	return ""
}

// errTelegramCallback is a sentinel kept for stable test identifiers.
var errTelegramCallback = errors.New("telegram: callback handler error")
