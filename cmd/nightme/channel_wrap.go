// Package main — channelWrap bridges the outbound.Emitter
// interface (concrete, owned by cmd/nightme) into the
// chatsession.Channel interface (abstract, used by the command
// runtime after the Commander refactor).
//
// The two interfaces use different message types:
//   - outbound.Emitter / Channel accept gateway.OutboundMessage
//   - chatsession.Channel accepts chatsession.OutboundMessage
//
// The wrap performs a field-by-field copy between them at every
// outbound call, then routes through Emitter so the unified
// SessionContext footer (F-45/F-48) is stamped on slash-command
// replies — pre-wrap these replies bypassed runtime stamping
// entirely.
//
// PATCH semantics: chatsession.OutboundMessage.PatchBotMsgID
// is non-empty → wrap sets gateway.OutboundMessage.Kind =
// OutCardPatch and copies the PATCH body fields. Otherwise
// Kind = OutCommandReply (slash command reply).
package main

import (
	"context"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/gateway"
	"github.com/cnlangzi/nightme/internal/gateway/outbound"
)

// cardKindFromString maps chatsession.Card.Kind (string) to
// gateway.CardKind (int enum). Stable across the daemon's
// lifetime — commands and gtw use the canonical string names.
// Unknown / empty strings default to CardKindDecision (the
// most permissive kind, what /gtw falls back to).
func cardKindFromString(s string) gateway.CardKind {
	switch s {
	case "permission":
		return gateway.CardKindPermission
	case "preview":
		return gateway.CardKindPreview
	case "decision", "":
		return gateway.CardKindDecision
	}
	return gateway.CardKindDecision
}

// channelWrap implements chatsession.Channel by delegating to
// an outbound.Emitter. The runtime shim in run.go wires one of
// these per ChatSession via cs.WithChannel after GetOrCreate.
//
// Why route through Emitter (instead of the underlying gateway.Channel
// directly): pre-outbound-package, slash-command replies sent via
// cs.Channel().Send() skipped the SessionContext footer because
// the stamping logic lived at the runtime pump, not on the channel
// path. The Emitter now owns stamping, so wrapping it gives every
// reply the same treatment — slash-command, runtime-pump, and
// MessageState subscriber all flow through the same stamping hook.
type channelWrap struct {
	em outbound.Emitter
}

// newChannelWrap constructs a wrap. The underlying emitter must be
// non-nil.
func newChannelWrap(em outbound.Emitter) chatsession.Channel {
	return &channelWrap{em: em}
}

// Send posts a plain-text reply (Kind = OutCommandReply).
// PATCH fields on msg are ignored — use Patch for in-place edits.
func (w *channelWrap) Send(ctx context.Context, msg chatsession.OutboundMessage) error {
	gw := chatsessionToGateway(msg)
	gw.Kind = gateway.OutCommandReply
	return w.em.Send(ctx, gw)
}

// SendCard posts an interactive card (Kind = OutCard). msg.Card
// must be non-nil per the chatsession.Channel contract.
func (w *channelWrap) SendCard(ctx context.Context, msg chatsession.OutboundMessage) (string, error) {
	gw := chatsessionToGateway(msg)
	gw.Kind = gateway.OutCard
	return w.em.SendCard(ctx, gw)
}

// Patch edits an existing bot message in place (Kind = OutCardPatch).
// msg.PatchBotMsgID must be non-nil per the chatsession.Channel
// contract.
func (w *channelWrap) Patch(ctx context.Context, msg chatsession.OutboundMessage) error {
	gw := chatsessionToGateway(msg)
	gw.Kind = gateway.OutCardPatch
	gw.Text = msg.PatchResult
	return w.em.Send(ctx, gw)
}

// chatsessionToGateway translates chatsession.OutboundMessage to
// gateway.OutboundMessage. Field copy is direct; CardKind is
// converted via cardKindFromString. PATCH fields are copied as-is
// — the wrap sets Kind appropriately based on which method called
// (Send / SendCard / Patch).
func chatsessionToGateway(in chatsession.OutboundMessage) gateway.OutboundMessage {
	out := gateway.OutboundMessage{
		ChatID:  in.ChatID,
		Text:    in.Text,
		ReplyTo: in.ReplyTo,
	}
	if in.Card != nil {
		card := chatsessionCardToGateway(*in.Card)
		out.Card = &card
	}
	if in.PatchBotMsgID != "" {
		out.ReplyTo = in.PatchBotMsgID
		if in.PatchResult != "" && out.Text == "" {
			out.Text = in.PatchResult
		}
		if in.ChosenChoiceEmoji != "" {
			out.ReplyTo = in.PatchBotMsgID
		}
		// CardTitle / CardBody / CardChoices / CardRequestID for PATCH
		// are passed through the same Card field; the gateway
		// interprets them when Kind = OutCardPatch (see
		// feishu/adapter.go: case OutCardPatch).
		if in.CardTitle != "" || in.CardBody != "" || len(in.CardChoices) > 0 {
			card := chatsessionCardToGateway(chatsession.Card{
				Title:       in.CardTitle,
				Body:        in.CardBody,
				Choices:     in.CardChoices,
				RequestID:   in.CardRequestID,
				ChosenEmoji: in.ChosenChoiceEmoji,
			})
			out.Card = &card
		}
	}
	return out
}

// chatsessionCardToGateway translates chatsession.Card to
// gateway.Card. CardKind is the only non-trivial field (string ↔
// enum).
func chatsessionCardToGateway(in chatsession.Card) gateway.Card {
	return gateway.Card{
		Kind:              cardKindFromString(in.Kind),
		Title:             in.Title,
		Body:              in.Body,
		Choices:           chatChoicesToGateway(in.Choices),
		RequestID:         in.RequestID,
		Disabled:          in.Disabled,
		ChosenChoiceEmoji: in.ChosenEmoji,
	}
}

// chatChoicesToGateway translates []chatsession.CardChoice to
// []gateway.CardChoice. Direct field copy.
func chatChoicesToGateway(in []chatsession.CardChoice) []gateway.CardChoice {
	if len(in) == 0 {
		return nil
	}
	out := make([]gateway.CardChoice, len(in))
	for i, c := range in {
		out[i] = gateway.CardChoice{
			Emoji:  c.Emoji,
			Label:  c.Label,
			Action: c.Action,
		}
	}
	return out
}