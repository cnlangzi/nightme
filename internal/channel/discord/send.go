package discord

import (
	"context"
	"errors"
	"strings"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/messages"
)

// Send implements channel.Channel. Routes by OutboundKind:
// OutInit mirrors feishu's receipt-footer stamping (Discord has
// no receipt-card concept, so the adapter sends a single header
// message — see sendInit for the analogue). OutReply /
// OutCommandReply / OutResult all become standalone Discord
// messages via CreateMessage. OutMessageState places a 👌
// reaction on the user message; OutMessageStateRemoved removes
// it. OutError prepends the agent diagnostic when present.
//
// Phase 1 silent-drops the rest of the OutboundKind enum
// (OutThinking / OutToolStart / OutToolEnd / OutTaskCreate /
// OutTaskUpdate / OutChoice / OutChoicePatch / OutHeartbeat /
// OutPromptEnded). Phase 2 picks them up.
func (a *Adapter) Send(ctx context.Context, msg messages.OutboundMessage) error {
	raw := rawChannelIDFromSession(msg.ChatID)
	switch msg.Kind {
	case messages.OutInit:
		return a.sendInit(ctx, raw, msg)
	case messages.OutReply, messages.OutCommandReply, messages.OutResult:
		body := msg.Text
		if msg.Result != nil && msg.Result.Text != "" {
			body = msg.Result.Text
		}
		if strings.TrimSpace(body) == "" {
			return nil
		}
		payload := CreateMessagePayload{
			Content:         body,
			AllowedMentions: &AllowedMentions{Parse: []string{}},
		}
		_, err := a.api.CreateMessage(ctx, raw, payload)
		return err
	case messages.OutMessageState:
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
		if err := a.api.AddReaction(ctx, raw, msg.MessageState.MessageID, emoji); err != nil {
			return err
		}
		a.rememberMessageState(msg.MessageState.MessageID, emoji)
		return nil
	case messages.OutMessageStateRemoved:
		if msg.MessageState == nil || msg.MessageState.MessageID == "" {
			return nil
		}
		emoji := mapStateToDiscordEmoji(msg.MessageState.State)
		if emoji == "" {
			return nil
		}
		return a.api.RemoveOwnReaction(ctx, raw, msg.MessageState.MessageID, emoji)
	case messages.OutError:
		body := msg.Text
		if msg.Diagnostic != nil && msg.Diagnostic.StderrTail != "" {
			body += "\n```\n" + msg.Diagnostic.StderrTail + "\n```"
		}
		_, err := a.api.CreateMessage(ctx, raw, CreateMessagePayload{
			Content:         body,
			AllowedMentions: &AllowedMentions{Parse: []string{}},
		})
		return err
	default:
		// Phase 1 silent-drop for OutThinking / OutToolStart /
		// OutToolEnd / OutTaskCreate / OutTaskUpdate / OutChoice /
		// OutChoicePatch / OutHeartbeat / OutPromptEnded. Phase 2
		// picks them up.
		return nil
	}
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
	payload := CreateMessagePayload{
		Content:         body,
		AllowedMentions: &AllowedMentions{Parse: []string{}},
	}
	_, err := a.api.CreateMessage(ctx, rawChannelID, payload)
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
// user message when the prompt ended cleanly, or ❌ on error.
// Discord reactions are append-only, so multiple emojis stack:
// 👌 from OutMessageState + 🎉 / ❌ from OnPromptEnded both
// remain visible. Mirrors the feishu / telegram convention.
func (a *Adapter) OnPromptEnded(ctx context.Context, chatID, userMsgID string, reason agent.PromptEndReason) {
	if chatID == "" || userMsgID == "" {
		return
	}
	emoji := "🎉"
	if reason.IsError() {
		emoji = "❌"
	}
	raw := rawChannelIDFromSession(chatID)
	_ = a.api.AddReaction(ctx, raw, userMsgID, emoji)
}
