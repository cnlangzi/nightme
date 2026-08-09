// Package main — channelWrap bridges the gateway.Channel
// interface (concrete, implemented by *feishu.Adapter and *echo
// stubs) into the chatsession.Channel interface (abstract, used
// by the command runtime after the Commander refactor).
//
// The two interfaces use different message types:
//   - gateway.Channel accepts gateway.OutboundMessage
//   - chatsession.Channel accepts chatsession.OutboundMessage
//
// The wrap performs a field-by-field copy between them at every
// outbound call. The only non-trivial conversion is CardKind
// (string ↔ int enum) — gtw's "decision" / "info" strings map
// to gateway.CardKindDecision / CardKindInfo / CardKindPermission.
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
// a gateway.Channel (concrete IM adapter). The runtime shim in
// run.go wires one of these per ChatSession via cs.WithChannel
// after GetOrCreate.
type channelWrap struct {
	ch gateway.Channel
}

// newChannelWrap constructs a wrap. The underlying ch must be
// non-nil (it's the gateway.Channel returned by gateway.ResolveChannel).
func newChannelWrap(ch gateway.Channel) chatsession.Channel {
	return &channelWrap{ch: ch}
}

// Send posts a plain-text reply (Kind = OutCommandReply).
// PATCH fields on msg are ignored — use Patch for in-place edits.
func (w *channelWrap) Send(ctx context.Context, msg chatsession.OutboundMessage) error {
	gw := chatsessionToGateway(msg)
	gw.Kind = gateway.OutCommandReply
	return w.ch.Send(ctx, gw)
}

// SendCard posts an interactive card (Kind = OutCard). msg.Card
// must be non-nil per the chatsession.Channel contract.
func (w *channelWrap) SendCard(ctx context.Context, msg chatsession.OutboundMessage) (string, error) {
	gw := chatsessionToGateway(msg)
	gw.Kind = gateway.OutCard
	return w.ch.SendCard(ctx, gw)
}

// Patch edits an existing bot message in place (Kind = OutCardPatch).
// msg.PatchBotMsgID must be non-nil per the chatsession.Channel
// contract.
func (w *channelWrap) Patch(ctx context.Context, msg chatsession.OutboundMessage) error {
	gw := chatsessionToGateway(msg)
	gw.Kind = gateway.OutCardPatch
	gw.Text = msg.PatchResult
	return w.ch.Send(ctx, gw)
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
				Title:        in.CardTitle,
				Body:         in.CardBody,
				Choices:      in.CardChoices,
				RequestID:    in.CardRequestID,
				ChosenEmoji:  in.ChosenChoiceEmoji,
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
		Kind:             cardKindFromString(in.Kind),
		Title:            in.Title,
		Body:             in.Body,
		Choices:          chatChoicesToGateway(in.Choices),
		RequestID:        in.RequestID,
		Disabled:         in.Disabled,
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