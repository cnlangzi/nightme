package slack

import (
	"context"
	"fmt"
	"strings"

	slackgo "github.com/slack-go/slack"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/messages"
)

// Reaction names used for the message-state track. Slack takes emoji
// NAMES (no colons), not unicode.
const (
	reactionQueued    = "eyes"
	reactionSubmitted = "brain"
	reactionDone      = "white_check_mark"
	reactionError     = "x"
)

// actionValueSep joins requestID and optionID inside a button's
// value. Slack caps the value at 2000 chars, which is far more than
// these two ids need.
const actionValueSep = "::"

// Send is the sole outbound egress for the Slack channel.
//
// Routing splits three ways:
//
//   - stream chunks — the rolling placeholder for the turn
//     (OutReply, OutThinking, OutResult, OutTool*, OutTask*)
//   - standalone messages — kinds that must stay visible and
//     clickable independently of the stream's lifecycle
//     (OutChoice, OutCommandReply, OutError)
//   - reactions — the message-state track on the user's own message
//
// See docs/channel/slack.md §4.
func (a *Adapter) Send(ctx context.Context, msg messages.OutboundMessage) error {
	a.health.recordOutbound()

	switch msg.Kind {
	case messages.OutReply, messages.OutThinking:
		return a.sendStreamText(ctx, msg, false)

	case messages.OutResult:
		// The final answer is the payload the user is waiting for;
		// holding it for the throttle window buys nothing.
		return a.sendStreamText(ctx, msg, true)

	case messages.OutToolStart:
		return a.sendToolStart(ctx, msg)

	case messages.OutToolEnd:
		return a.sendToolEnd(ctx, msg)

	case messages.OutTaskCreate, messages.OutTaskUpdate:
		return a.sendTaskList(ctx, msg)

	case messages.OutHeartbeat:
		return a.sendHeartbeat(ctx, msg)

	case messages.OutInit:
		return a.stampFooter(msg)

	case messages.OutChoice:
		return a.sendChoice(ctx, msg)

	case messages.OutChoicePatch:
		return a.patchChoice(ctx, msg)

	case messages.OutCommandReply:
		return a.sendStandalone(ctx, msg, "❯ "+msg.Text)

	case messages.OutError:
		return a.sendStandalone(ctx, msg, a.formatError(msg))

	case messages.OutMessageState:
		return a.applyMessageState(ctx, msg)

	case messages.OutMessageStateRemoved:
		return a.removeMessageState(ctx, msg)
	}
	return nil
}

// streamFor resolves (and lazily opens) the placeholder for a turn.
// A message with no ReplyTo has no turn to attach to.
func (a *Adapter) streamFor(msg messages.OutboundMessage) (*turnStream, bool) {
	if msg.ReplyTo == "" {
		return nil, false
	}
	_, channelID, threadTS, ok := splitSessionID(msg.ChatID)
	if !ok {
		return nil, false
	}
	// The turn's placeholder threads under the user's message when
	// the conversation is not already inside a thread.
	anchor := threadTS
	if anchor == "" {
		anchor = msg.ReplyTo
	}
	deps := streamDeps{
		api:      a.api,
		limiter:  a.limiter,
		retry:    a.retry,
		logger:   a.log(),
		state:    a.state,
		throttle: a.throttle,
	}
	stream, _ := a.streams.getOrCreate(msg.ChatID, msg.ReplyTo, func() *turnStream {
		return newTurnStream(msg.ChatID, channelID, anchor, msg.ReplyTo, deps)
	})
	return stream, true
}

// sendStreamText appends agent prose to the turn's placeholder,
// falling back to a standalone message when there is no turn.
func (a *Adapter) sendStreamText(ctx context.Context, msg messages.OutboundMessage, urgent bool) error {
	text := msg.Text
	if text == "" {
		return nil
	}
	if msg.Kind == messages.OutThinking {
		text = "💭 " + text
	}
	stream, ok := a.streamFor(msg)
	if !ok {
		return a.sendStandalone(ctx, msg, text)
	}
	if lines := statusBarLines(&msg); len(lines) > 0 {
		stream.stampFooter(lines)
	}
	err := stream.appendMarkdown(ctx, text, urgent)
	if err == errStreamClosed {
		// The turn already ended; deliver it rather than lose it.
		return a.sendStandalone(ctx, msg, text)
	}
	return err
}

// sendToolStart opens a task card for a tool invocation.
func (a *Adapter) sendToolStart(ctx context.Context, msg messages.OutboundMessage) error {
	stream, ok := a.streamFor(msg)
	if !ok {
		return nil
	}
	title := toolTitle(msg.Tool)
	id := stream.beginTool(title)
	return ignoreClosed(stream.appendTask(ctx, id, title,
		slackgo.TaskCardStatusInProgress, "", false))
}

// sendToolEnd completes the task card opened by sendToolStart.
//
// Pairing is positional (FIFO) because messages.ToolInfo carries no
// call id — same constraint the Feishu adapter works around.
func (a *Adapter) sendToolEnd(ctx context.Context, msg messages.OutboundMessage) error {
	stream, ok := a.streamFor(msg)
	if !ok {
		return nil
	}
	id, title, paired := stream.endTool()
	if !paired {
		// An end without a start still deserves a card.
		id = stream.newStandaloneToolID()
		title = toolTitle(msg.Tool)
	}
	status := slackgo.TaskCardStatusComplete
	details := ""
	if msg.Tool != nil {
		details = msg.Tool.Output
	}
	if msg.Err != nil || (msg.Tool != nil && msg.Tool.Err != nil) {
		status = slackgo.TaskCardStatusError
		details = firstNonEmpty(errText(msg.Err), errText(toolErr(msg.Tool)), details)
	}
	return ignoreClosed(stream.appendTask(ctx, id, title, status,
		collapseWhitespace(details), false))
}

// sendTaskList renders the agent's task snapshot as task cards.
//
// Every event carries the full list, and Slack merges task_update
// chunks by id, so re-sending the whole snapshot converges on the
// right state without the adapter diffing anything.
func (a *Adapter) sendTaskList(ctx context.Context, msg messages.OutboundMessage) error {
	stream, ok := a.streamFor(msg)
	if !ok || msg.TaskList == nil {
		return nil
	}
	if lines := statusBarLines(&msg); len(lines) > 0 {
		stream.stampFooter(lines)
	}
	for i, item := range msg.TaskList.Items {
		id := fmt.Sprintf("task-%d", i)
		if item.ID != "" {
			id = "task-" + item.ID
		}
		title := firstNonEmpty(item.Subject, item.ActiveForm, "task")
		// ActiveForm is the live "…writing unit tests" hint; it only
		// adds information when it differs from the subject.
		details := ""
		if item.ActiveForm != "" && item.ActiveForm != item.Subject {
			details = item.ActiveForm
		}
		if err := ignoreClosed(stream.appendTask(ctx, id, title,
			taskStatus(item.Status), details, false)); err != nil {
			return err
		}
	}
	return nil
}

// taskStatus maps nightme's task status onto Slack's card states.
func taskStatus(status agent.AgentTaskStatus) slackgo.TaskCardStatus {
	switch status {
	case agent.TaskInProgress:
		return slackgo.TaskCardStatusInProgress
	case agent.TaskCompleted:
		return slackgo.TaskCardStatusComplete
	case agent.TaskCancelled:
		return slackgo.TaskCardStatusError
	default:
		return slackgo.TaskCardStatusPending
	}
}

// sendHeartbeat drives the "thinking" indicator.
//
// The assistant status API is the right home for this: 600/min per
// app, versus appendStream's Tier 4 budget which the content stream
// needs. When it is unavailable (the app has no AI features enabled)
// we fall back to a plan_update chunk, the one mutable slot in an
// append-only stream.
func (a *Adapter) sendHeartbeat(ctx context.Context, msg messages.OutboundMessage) error {
	if msg.Heartbeat == nil || msg.Heartbeat.Empty() {
		return nil
	}
	_, channelID, threadTS, ok := splitSessionID(msg.ChatID)
	if !ok {
		return nil
	}
	anchor := threadTS
	if anchor == "" {
		anchor = msg.ReplyTo
	}
	text := heartbeatText(msg.Heartbeat)

	if anchor != "" {
		err := a.api.SetAssistantStatus(ctx, channelID, anchor, text)
		if err == nil {
			return nil
		}
		a.log().Debug("slack: assistant status unavailable, using plan chunk", "err", err)
	}

	stream, ok := a.streamFor(msg)
	if !ok {
		return nil
	}
	return ignoreClosed(stream.appendPlan(ctx, text))
}

// stampFooter records the StatusBar snapshot for the turn. It is
// rendered once, as chat.stopStream finalization blocks.
func (a *Adapter) stampFooter(msg messages.OutboundMessage) error {
	stream, ok := a.streamFor(msg)
	if !ok {
		return nil
	}
	lines := statusBarLines(&msg)
	if len(lines) == 0 {
		return nil
	}
	stream.stampFooter(lines)
	return nil
}

// sendChoice posts an interactive prompt.
//
// This deliberately does NOT go through the stream. Two reasons:
// a blocking prompt must not wait for the throttle window, and a
// prompt embedded in a stream would have its clickability tied to
// that stream's lifecycle. A standalone message with plain section +
// actions blocks is unambiguous — and those block types are accepted
// by chat.postMessage, unlike the agent-UI blocks that require the
// chunks transport.
func (a *Adapter) sendChoice(ctx context.Context, msg messages.OutboundMessage) error {
	if msg.Choice == nil {
		return nil
	}
	_, channelID, threadTS, ok := splitSessionID(msg.ChatID)
	if !ok {
		return fmt.Errorf("slack: OutChoice with unparseable chat id %q", msg.ChatID)
	}
	anchor := threadTS
	if anchor == "" {
		anchor = msg.ReplyTo
	}
	blocks := choiceBlocks(msg.Choice)
	fallback := firstNonEmpty(msg.Choice.Title, msg.Choice.Body, "Action needed")

	ts, err := a.post(ctx, channelID, anchor, "👉 "+fallback, blocks, false)
	if err != nil {
		return err
	}
	a.state.putChoice(&ChoiceState{
		RequestID: msg.Choice.RequestID,
		ChatID:    msg.ChatID,
		ChannelID: channelID,
		TS:        ts,
		ThreadTS:  anchor,
	})
	return nil
}

// patchChoice settles a previously posted prompt in place.
func (a *Adapter) patchChoice(ctx context.Context, msg messages.OutboundMessage) error {
	if msg.Choice == nil {
		return nil
	}
	state, ok := a.state.choice(msg.Choice.RequestID)
	if !ok {
		return nil
	}
	text := settledText(msg.Choice)
	err := withTransientRetry(ctx, a.retry, a.log(), "chat.update", func() error {
		if err := a.limiter.Wait(ctx); err != nil {
			return err
		}
		return a.api.UpdateMessage(ctx, state.ChannelID, state.TS, text, nil)
	})
	if err != nil {
		return err
	}
	state.Settled = true
	state.SelectedID = msg.Choice.SelectedID
	a.state.putChoice(state)
	return nil
}

// sendStandalone posts a message outside the stream.
func (a *Adapter) sendStandalone(ctx context.Context, msg messages.OutboundMessage, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	_, channelID, threadTS, ok := splitSessionID(msg.ChatID)
	if !ok {
		return fmt.Errorf("slack: unparseable chat id %q", msg.ChatID)
	}
	anchor := threadTS
	if anchor == "" {
		anchor = msg.ReplyTo
	}
	body := text
	if lines := statusBarLines(&msg); len(lines) > 0 {
		body = text + "\n\n" + strings.Join(lines, "\n")
	}
	_, err := a.post(ctx, channelID, anchor, body, nil, false)
	return err
}

func (a *Adapter) post(ctx context.Context, channelID, threadTS, text string, blocks []slackgo.Block, broadcast bool) (string, error) {
	var ts string
	err := withTransientRetry(ctx, a.retry, a.log(), "chat.postMessage", func() error {
		if err := a.limiter.Wait(ctx); err != nil {
			return err
		}
		var innerErr error
		ts, innerErr = a.api.PostMessage(ctx, channelID, threadTS, text, blocks, broadcast)
		return innerErr
	})
	return ts, err
}

// applyMessageState renders the message-state track as a reaction on
// the user's own message.
//
// Slack allows removing a reaction the bot placed, so states replace
// each other instead of stacking. Feishu's API is append-only (the
// user ends up with all three emoji) and Telegram allows exactly one
// emoji per message; Slack is the only one of the three that can
// express this as a genuine state machine.
func (a *Adapter) applyMessageState(ctx context.Context, msg messages.OutboundMessage) error {
	if msg.MessageState == nil || msg.MessageState.MessageID == "" {
		return nil
	}
	_, channelID, _, ok := splitSessionID(msg.ChatID)
	if !ok {
		return nil
	}
	next := msg.MessageState.State
	name := reactionFor(next)
	if name == "" {
		return nil
	}

	key := msg.ChatID + "|" + msg.MessageState.MessageID
	a.muStates.Lock()
	prev, had := a.messageStates[key]
	if had && prev == next {
		a.muStates.Unlock()
		return nil
	}
	a.messageStates[key] = next
	a.muStates.Unlock()

	if had {
		if prevName := reactionFor(prev); prevName != "" && prevName != name {
			if err := a.api.RemoveReaction(ctx, channelID, msg.MessageState.MessageID, prevName); err != nil {
				a.log().Debug("slack: remove previous reaction failed", "err", err)
			}
		}
	}
	return a.api.AddReaction(ctx, channelID, msg.MessageState.MessageID, name)
}

func (a *Adapter) removeMessageState(ctx context.Context, msg messages.OutboundMessage) error {
	if msg.MessageState == nil || msg.MessageState.MessageID == "" {
		return nil
	}
	_, channelID, _, ok := splitSessionID(msg.ChatID)
	if !ok {
		return nil
	}
	name := reactionFor(msg.MessageState.State)
	if name == "" {
		return nil
	}
	key := msg.ChatID + "|" + msg.MessageState.MessageID
	a.muStates.Lock()
	delete(a.messageStates, key)
	a.muStates.Unlock()
	return a.api.RemoveReaction(ctx, channelID, msg.MessageState.MessageID, name)
}

// reactionFor maps a message state onto an emoji name. States the
// channel chooses not to render return "" and are dropped.
func reactionFor(state agent.MessageState) string {
	switch state {
	case agent.MessageQueued:
		return reactionQueued
	case agent.MessageSubmitted:
		return reactionSubmitted
	case agent.MessageDone:
		return reactionDone
	}
	return ""
}

// OnPromptEnded closes the turn's placeholder and marks the user's
// message done.
//
// The stream MUST be closed here: unlike a Feishu card, which simply
// stops updating, a Slack stream left open keeps rendering as
// in-progress.
func (a *Adapter) OnPromptEnded(ctx context.Context, chatID, userMsgID string) {
	stream, ok := a.streams.lookup(chatID, userMsgID)
	if ok {
		if err := stream.finish(ctx); err != nil {
			a.log().Warn("slack: closing stream on prompt end failed",
				"chat_id", chatID, "err", err)
		}
		a.streams.purge(chatID, userMsgID)
	}

	if userMsgID == "" {
		return
	}
	_, channelID, _, parsed := splitSessionID(chatID)
	if !parsed {
		return
	}
	// Clear any lingering "thinking" indicator.
	if stream != nil && stream.threadTS != "" {
		_ = a.api.SetAssistantStatus(ctx, channelID, stream.threadTS, "")
	}
	if err := a.api.AddReaction(ctx, channelID, userMsgID, reactionDone); err != nil {
		a.log().Debug("slack: done reaction failed", "err", err)
	}
}

// choiceBlocks renders a Choice as section + actions blocks.
func choiceBlocks(choice *messages.Choice) []slackgo.Block {
	body := firstNonEmpty(choice.Body, choice.Title)
	blocks := []slackgo.Block{
		slackgo.NewSectionBlock(
			slackgo.NewTextBlockObject(slackgo.MarkdownType, "*👉 "+choice.Title+"*\n"+body, false, false),
			nil, nil,
		),
	}
	options := choice.Options
	if len(options) == 0 && len(choice.Questions) > 0 {
		options = choice.Questions[0].Options
	}
	if len(options) == 0 {
		return blocks
	}

	elements := make([]slackgo.BlockElement, 0, len(options))
	for _, opt := range options {
		label := opt.Label
		if opt.Emoji != "" {
			label = opt.Emoji + " " + label
		}
		btn := slackgo.NewButtonBlockElement(
			"nightme_choice_"+opt.ID,
			encodeActionValue(choice.RequestID, opt.ID),
			// Button text is plain_text only — Slack rejects mrkdwn here.
			slackgo.NewTextBlockObject(slackgo.PlainTextType, truncateRunes(label, 75), true, false),
		)
		elements = append(elements, btn)
	}
	blocks = append(blocks, slackgo.NewActionBlock("nightme_choice", elements...))
	return blocks
}

func settledText(choice *messages.Choice) string {
	label := choice.SelectedID
	for _, opt := range choice.Options {
		if opt.ID == choice.SelectedID {
			label = opt.Label
			break
		}
	}
	if label == "" {
		return "👉 " + choice.Title + "\n_settled_"
	}
	return "👉 " + choice.Title + "\n_selected: " + label + "_"
}

func encodeActionValue(requestID, optionID string) string {
	return requestID + actionValueSep + optionID
}

func parseActionValue(value string) (requestID, optionID string, ok bool) {
	idx := strings.Index(value, actionValueSep)
	if idx < 0 {
		return "", "", false
	}
	requestID = value[:idx]
	optionID = value[idx+len(actionValueSep):]
	if requestID == "" {
		return "", "", false
	}
	return requestID, optionID, true
}

func (a *Adapter) formatError(msg messages.OutboundMessage) string {
	text := msg.Text
	if text == "" && msg.Err != nil {
		text = msg.Err.Error()
	}
	if text == "" {
		text = "agent error"
	}
	out := "⚠️ " + text
	if msg.Diagnostic != nil && msg.Diagnostic.StderrTail != "" {
		out += "\n```\n" + truncateRunes(msg.Diagnostic.StderrTail, 1500) + "\n```"
	}
	return out
}

// ignoreClosed swallows errStreamClosed: a late event for a finished
// turn is expected, not a failure.
func ignoreClosed(err error) error {
	if err == errStreamClosed {
		return nil
	}
	return err
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func toolErr(tool *messages.ToolInfo) error {
	if tool == nil {
		return nil
	}
	return tool.Err
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
