package discord

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/messages"
)

// Send implements channel.Channel. Routes by OutboundKind:
//
//	OutInit                — single header message announcing the
//	                         session (no native receipt concept).
//	OutReply /             — standalone Discord messages via
//	OutCommandReply /        CreateMessage. AllowedMentions{Parse:[]}
//	OutResult                suppresses @user / @role pings.
//
//	OutThinking /           — bubble per kind (prefix / shape matches
//	OutToolStart /            Claude Code's terminal UX). Each emits
//	OutToolEnd                via CreateMessage.
//
//	OutTaskCreate /         — single bubble with a markdown
//	OutTaskUpdate            checklist ("📋 Tasks" header).
//
//	OutMessageState         — 👌 reaction on the user message
//	                         (Phase 1; verified unchanged).
//	OutMessageStateRemoved  — RemoveOwnReaction (Phase 1).
//	OutError                — bubble + optional stderr tail.
//
//	OutChoice               — V1 component card (callback.go owns
//	                         the rendering helpers).
//	OutChoicePatch          — REST edit; settles the card in place.
//
//	OutHeartbeat /          — silent drop (no native surface;
//	OutPromptEnded            OnPromptEnded owns the lifecycle stamp).
//
// The outbound liveness clock is stamped at the platform write
// boundary (createMessage / editMessage / addReaction /
// removeOwnReaction), so a kind that silent-drops — an empty body,
// a repeated message state, the heartbeat kinds — never advances it.
// Discord has no card model of its own: the id of the message a kind
// becomes is not handed back, so the returned id is empty.
func (a *Adapter) Send(ctx context.Context, msg messages.OutboundMessage) (string, error) {
	return "", a.sendOutbound(ctx, msg)
}

// createMessage posts a message to Discord and stamps the outbound
// liveness clock when the post lands.
func (a *Adapter) createMessage(ctx context.Context, channelID string, payload CreateMessagePayload) (Message, error) {
	sent, err := a.api.CreateMessage(ctx, channelID, payload)
	if err == nil {
		a.recordOutbound(time.Now())
	}
	return sent, err
}

// editMessage edits a message in place and stamps the outbound
// liveness clock when the edit lands.
func (a *Adapter) editMessage(ctx context.Context, channelID, messageID string, payload EditMessagePayload) (Message, error) {
	edited, err := a.api.EditMessage(ctx, channelID, messageID, payload)
	if err == nil {
		a.recordOutbound(time.Now())
	}
	return edited, err
}

// addReaction places a reaction and stamps the outbound liveness
// clock when it lands.
func (a *Adapter) addReaction(ctx context.Context, channelID, messageID, emoji string) error {
	if err := a.api.AddReaction(ctx, channelID, messageID, emoji); err != nil {
		return err
	}
	a.recordOutbound(time.Now())
	return nil
}

// removeOwnReaction removes a reaction and stamps the outbound
// liveness clock when the removal lands.
func (a *Adapter) removeOwnReaction(ctx context.Context, channelID, messageID, emoji string) error {
	if err := a.api.RemoveOwnReaction(ctx, channelID, messageID, emoji); err != nil {
		return err
	}
	a.recordOutbound(time.Now())
	return nil
}

// sendOutbound dispatches one outbound message to its Discord
// surface. Every kind routes to the helper that owns its rendering.
func (a *Adapter) sendOutbound(ctx context.Context, msg messages.OutboundMessage) error {
	raw := rawChannelIDFromSession(msg.ChatID)
	switch msg.Kind {
	case messages.OutMessageSettled:
		// The structured outcome of an inbound message. It exists
		// for programs that drive nightme, not for humans.
		return nil
	case messages.OutInit:
		return a.sendInit(ctx, raw, msg)
	case messages.OutReply, messages.OutCommandReply:
		return a.sendPlainReply(ctx, raw, msg.Text)
	case messages.OutResult:
		return a.sendResult(ctx, raw, msg)
	case messages.OutThinking:
		return a.sendPlainReply(ctx, raw, "💭 "+msg.Text)
	case messages.OutToolStart:
		body := formatToolStartCall(toolName(msg), toolArgs(msg))
		if body == "" {
			return nil
		}
		return a.sendPlainReply(ctx, raw, body)
	case messages.OutToolEnd:
		body := toolResultLine(toolName(msg), toolResultOutput(msg), toolResultSummary(msg), toolErr(msg))
		if body == "" {
			return nil
		}
		return a.sendPlainReply(ctx, raw, body)
	case messages.OutTaskCreate, messages.OutTaskUpdate:
		body := formatTaskListBody(msg)
		if body == "" {
			return nil
		}
		return a.sendPlainReply(ctx, raw, body)
	case messages.OutMessageState:
		return a.sendMessageState(ctx, raw, msg)
	case messages.OutMessageStateRemoved:
		return a.sendMessageStateRemoved(ctx, raw, msg)
	case messages.OutError:
		return a.sendError(ctx, raw, msg)
	case messages.OutChoice:
		return a.sendChoice(ctx, raw, msg)
	case messages.OutChoicePatch:
		return a.patchChoice(ctx, raw, msg)
	default:
		// OutHeartbeat / OutPromptEnded are silent-drops — the
		// per-turn lifecycle is owned by OnPromptEnded, not Send.
		return nil
	}
}

// maxMessageRunes is Discord's ceiling for a message body. The API
// rejects a content field longer than 2000 characters outright — a
// 400 with no delivery — so a longer body has to be split before it
// goes on the wire.
const maxMessageRunes = 2000

// splitMessageBody cuts body into pieces of at most maxRunes runes,
// preferring a paragraph, then line, then space boundary so a split
// does not land inside a word.
//
// The cut is rune-safe: Discord counts characters and a body carries
// CJK text, so splitting a multi-byte rune would leave a broken
// character in the message.
func splitMessageBody(body string, maxRunes int) []string {
	if maxRunes <= 0 {
		return []string{body}
	}
	runes := []rune(body)
	if len(runes) <= maxRunes {
		return []string{body}
	}
	var out []string
	for len(runes) > maxRunes {
		cut := softCut(runes, maxRunes)
		out = append(out, string(runes[:cut]))
		runes = runes[cut:]
	}
	if len(runes) > 0 {
		out = append(out, string(runes))
	}
	return out
}

// softCut finds the best split point at or below maxRunes.
func softCut(runes []rune, maxRunes int) int {
	window := runes[:maxRunes]
	// Paragraph boundary is the least disruptive.
	if idx := lastIndexOfPair(window, '\n', '\n'); idx > 0 {
		return idx + 1
	}
	for i := len(window) - 1; i > 0; i-- {
		if window[i] == '\n' {
			return i + 1
		}
	}
	for i := len(window) - 1; i > 0; i-- {
		if window[i] == ' ' {
			return i + 1
		}
	}
	return maxRunes
}

// lastIndexOfPair returns the index of the second rune of the last
// a/b pair in runes, or -1 when there is none.
func lastIndexOfPair(runes []rune, a, b rune) int {
	for i := len(runes) - 2; i >= 0; i-- {
		if runes[i] == a && runes[i+1] == b {
			return i + 1
		}
	}
	return -1
}

// sendBody puts body on the wire as one or more messages.
//
// Discord rejects a body over its 2000-character limit whole, so a
// body that does not fit is split and sent as consecutive messages.
// Nothing is dropped: the chunks carry the body in full.
func (a *Adapter) sendBody(ctx context.Context, rawChannelID, body string) ([]Message, error) {
	chunks := splitMessageBody(body, maxMessageRunes)
	sent := make([]Message, 0, len(chunks))
	for _, chunk := range chunks {
		msg, err := a.createMessage(ctx, rawChannelID, CreateMessagePayload{
			Content:         chunk,
			AllowedMentions: &AllowedMentions{Parse: []string{}},
		})
		if err != nil {
			return sent, err
		}
		sent = append(sent, msg)
	}
	return sent, nil
}

// sendPlainReply is the shared text-bubble emit used by every
// standalone-message OutboundKind. Empty / whitespace-only text
// silent-drops so the runtime's "no content this turn" signal
// doesn't produce an empty Discord message.
func (a *Adapter) sendPlainReply(ctx context.Context, rawChannelID, body string) error {
	if strings.TrimSpace(body) == "" {
		return nil
	}
	_, err := a.sendBody(ctx, rawChannelID, body)
	return err
}

// sendResult splits OutResult out of Send so the runtime's
// result-message-id can be recorded for OnPromptEnded's 🎉 / ❌
// reaction path. Empty body silent-drops (matches OutReply /
// OutThinking).
func (a *Adapter) sendResult(ctx context.Context, raw string, msg messages.OutboundMessage) error {
	body := strings.TrimSpace(msg.Text)
	if msg.Result != nil && msg.Result.Text != "" {
		body = strings.TrimSpace(msg.Result.Text)
	}
	if body == "" {
		return nil
	}
	sent, err := a.sendBody(ctx, raw, body)
	if err != nil {
		return err
	}
	// A body that did not fit was sent as several messages — the
	// last one is the visual end of the result, so that is where
	// the 🎉 lands.
	if len(sent) > 0 {
		a.rememberResultMessageID(sessionChatID(raw), msg.ReplyTo, string(sent[len(sent)-1].ID))
	}
	return nil
}

// sendMessageState places the platform-native emoji reaction on
// the user message. Verified unchanged from Phase 1.
func (a *Adapter) sendMessageState(ctx context.Context, raw string, msg messages.OutboundMessage) error {
	if msg.MessageState == nil || msg.MessageState.MessageID == "" {
		return errors.New("discord: OutMessageState missing MessageState or MessageID")
	}
	emoji := mapStateToDiscordEmoji(msg.MessageState.State)
	if emoji == "" {
		return nil
	}
	if prev, ok := a.previousMessageState(msg.MessageState.MessageID); ok && prev == emoji {
		return nil
	}
	if err := a.addReaction(ctx, raw, msg.MessageState.MessageID, emoji); err != nil {
		return err
	}
	a.rememberMessageState(msg.MessageState.MessageID, emoji)
	return nil
}

// sendMessageStateRemoved is the inverse of sendMessageState.
func (a *Adapter) sendMessageStateRemoved(ctx context.Context, raw string, msg messages.OutboundMessage) error {
	if msg.MessageState == nil || msg.MessageState.MessageID == "" {
		return nil
	}
	emoji := mapStateToDiscordEmoji(msg.MessageState.State)
	if emoji == "" {
		return nil
	}
	return a.removeOwnReaction(ctx, raw, msg.MessageState.MessageID, emoji)
}

// sendError prepends the agent diagnostic when present. Mirrors
// Phase 1 behaviour verbatim.
func (a *Adapter) sendError(ctx context.Context, raw string, msg messages.OutboundMessage) error {
	body := msg.Text
	if msg.Diagnostic != nil && msg.Diagnostic.StderrTail != "" {
		body += "\n```\n" + msg.Diagnostic.StderrTail + "\n```"
	}
	_, err := a.sendBody(ctx, raw, body)
	return err
}

// sendInit is the Discord analogue of feishu's receipt-footer
// stamping. Feishu PATCHes an existing card; Discord has no
// receipt concept, so the adapter sends a single header message
// announcing the session / agent / model / workspace.
//
// The runtime emits OutInit once per session — the adapter does
// NOT track per-chat "have we already sent an init?" state, so
// repeated OutInit events would currently produce repeated
// header messages. The runtime's "once per session" contract is
// what keeps this from spamming the chat today; if that ever
// changes, introduce a small per-chat "init sent" set here
// (mirroring feishu's "stamp existing receipt" no-op path).
//
// The header is intentionally terse — Discord messages are
// first-class surface area, not a card footer.
func (a *Adapter) sendInit(ctx context.Context, rawChannelID string, msg messages.OutboundMessage) error {
	agentName := strings.TrimSpace(msg.AgentName)
	if agentName == "" {
		agentName = "assistant"
	}
	body := "🤖 " + agentName + " ready"
	if msg.Model != "" {
		body += " · model " + msg.Model
	}
	if msg.SessionID != "" {
		body += " · session " + msg.SessionID
	}
	if msg.Workspace != "" {
		body += "\nworkspace: " + msg.Workspace
		if msg.Branch != "" {
			body += " (" + msg.Branch + ")"
		}
	}
	_, err := a.createMessage(ctx, rawChannelID, CreateMessagePayload{
		Content:         body,
		AllowedMentions: &AllowedMentions{Parse: []string{}},
	})
	return err
}

// sendChoice renders a V1 component card and records it in the
// choice store so the INTERACTION_CREATE handler can correlate
// the eventual click / modal submit back to this Choice.
func (a *Adapter) sendChoice(ctx context.Context, rawChannelID string, msg messages.OutboundMessage) error {
	if msg.Choice == nil || msg.Choice.RequestID == "" {
		return errors.New("discord: OutChoice missing Choice or RequestID")
	}
	state := &choiceState{
		RequestID: msg.Choice.RequestID,
		ChannelID: rawChannelID,
		Choice:    cloneChoiceValue(msg.Choice),
		Step:      0,
		Picks:     make([]string, len(msg.Choice.Questions)),
	}
	content := renderChoiceContent(state)
	components := a.buildChoiceComponents(state)
	sent, err := a.createMessage(ctx, rawChannelID, CreateMessagePayload{
		Content:         content,
		Components:      components,
		AllowedMentions: &AllowedMentions{Parse: []string{}},
	})
	if err != nil {
		return err
	}
	state.MessageID = string(sent.ID)
	return a.choiceStore.Put(state)
}

// patchChoice edits the original choice card via REST in response
// to OutChoicePatch. Settled choices drop the component rows
// entirely (Discord renders an empty `components: []` as "no
// buttons" without removing the message); non-settled patches
// re-render with the new Choice content (multi-step advances).
//
// Orphan patches (no matching choice state) are silent-skipped
// to match feishu / telegram's behaviour — the runtime has already
// moved on, and a 4xx from Discord would only surface a confusing
// log line.
//
// State mutation goes through state.applyPatch so the callback
// goroutine's markSettled / publishActionChoice cannot observe a
// half-updated state under a concurrent click.
func (a *Adapter) patchChoice(ctx context.Context, rawChannelID string, msg messages.OutboundMessage) error {
	if msg.Choice == nil || msg.Choice.RequestID == "" {
		return errors.New("discord: OutChoicePatch missing Choice or RequestID")
	}
	state, ok := a.choiceStore.Get(msg.Choice.RequestID)
	if !ok {
		return nil
	}
	state.applyPatch(cloneChoiceValue(msg.Choice), msg.Choice.Settled, msg.Choice.SelectedID)
	var components []Component
	if !msg.Choice.Settled {
		components = a.buildChoiceComponents(state)
	}
	_, err := a.editMessage(ctx, state.ChannelID, state.MessageID, EditMessagePayload{
		Content:         renderChoiceContent(state),
		Components:      components,
		AllowedMentions: &AllowedMentions{Parse: []string{}},
	})
	return err
}

// mapStateToDiscordEmoji converts the agent.MessageState enum
// into the platform-native emoji the adapter stamps as a
// reaction on the user message. Mirrors the feishu / telegram
// mapping (👌 on submit, blank on done — the prompt-end path
// uses 🎉 / ❌ separately via OnPromptEnded).
func mapStateToDiscordEmoji(state agent.MessageState) string {
	switch state {
	case agent.MessageSubmitted:
		return "👌"
	case agent.MessageDone:
		return ""
	}
	return ""
}

// OnPromptEnded implements channel.Channel. Stamps 🎉 on the
// **result message id** when one was recorded by sendResult
// (OutResult path), falling back to the user message id when no
// result was sent. Falls back again to userMsgID == "" → no-op
// (matches Phase 1 silent-skip).
//
// Discord reactions are append-only, so 👌 from OutMessageState
// + 🎉 / ❌ from OnPromptEnded both remain visible. Mirrors the
// feishu / telegram convention.
func (a *Adapter) OnPromptEnded(ctx context.Context, chatID, userMsgID string, reason agent.PromptEndReason) {
	if chatID == "" || userMsgID == "" {
		return
	}
	emoji := "🎉"
	if reason.IsError() {
		emoji = "❌"
	}
	raw := rawChannelIDFromSession(chatID)
	target := a.lastResultMessageID(chatID, userMsgID)
	if target == "" {
		target = userMsgID
	}
	_ = a.addReaction(ctx, raw, target, emoji)
}

// toolName / toolArgs / toolResultOutput / toolErr read the
// OutboundMessage.Tool / Err fields safely so the OutToolStart /
// OutToolEnd branches don't repeat the nil-checks at the call site.
func toolName(msg messages.OutboundMessage) string {
	if msg.Tool == nil {
		return ""
	}
	return msg.Tool.Name
}

func toolArgs(msg messages.OutboundMessage) string {
	if msg.Tool == nil {
		return ""
	}
	return msg.Tool.Args
}

func toolResultOutput(msg messages.OutboundMessage) string {
	if msg.Tool == nil {
		return ""
	}
	return msg.Tool.Output
}

func toolErr(msg messages.OutboundMessage) error {
	return msg.Err
}

// toolResultSummary returns the verdict the producer declared safe to
// show.
func toolResultSummary(msg messages.OutboundMessage) string {
	if msg.Tool == nil {
		return ""
	}
	return msg.Tool.Summary
}

// formatTaskListBody renders the OutboundMessage.TaskList payload
// (or returns "" for nil / empty Items so Send silent-drops).
func formatTaskListBody(msg messages.OutboundMessage) string {
	if msg.TaskList == nil {
		return ""
	}
	return formatTaskList(msg.TaskList.Items)
}
