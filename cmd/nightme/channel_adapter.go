// Package main (cmd/nightme) — channelAdapter wires
// *gateway.Channel into command.Channel.
//
// Same rationale as session_adapter.go (see top of that file):
// the adapter wraps a gateway concrete type, so it must live
// in cmd/nightme, NOT in the command package.
//
// The conversion gateway.OutboundMessage ↔ command.Outbound
// happens here. Both have similar fields (ChatID, Text, ReplyTo,
// Card) but use different Card / OutboundMessage / Channel
// types — they're parallel type hierarchies in different
// packages.
package main

import (
	"context"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/gateway"
)

// channelAdapter exposes a single channel.Channel as
// command.Channel. cmd/nightme currently has one channel per
// process (ch channel.Channel), so a single-field adapter is
// sufficient. Multi-channel deployments would need to
// dispatch by chatID; not in scope for F-51.
//
// The adapter stores channel.Channel (the channel-package
// interface) but at Send / SendCard time it asserts to
// gateway.Channel to share the outbound type. Both
// interfaces have identical signatures — only the package
// path differs — so a type assertion is sufficient.
type channelAdapter struct {
	ch channel.Channel
}

// newChannelAdapter constructs the adapter.
func newChannelAdapter(ch channel.Channel) *channelAdapter {
	return &channelAdapter{ch: ch}
}

// Send implements command.Channel. Translates command.Outbound
// → gateway.OutboundMessage. gateway.Channel.Send returns just
// error; we synthesize an empty msgID.
func (a *channelAdapter) Send(ctx context.Context, m command.Outbound) (string, error) {
	if a.ch == nil {
		return "", nil
	}
	gwCh, ok := a.ch.(gateway.Channel)
	if !ok {
		return "", nil
	}
	if err := gwCh.Send(ctx, outboundToGateway(m)); err != nil {
		return "", err
	}
	return "", nil
}

// SendCard implements command.Channel.
func (a *channelAdapter) SendCard(ctx context.Context, m command.Outbound) (string, error) {
	if a.ch == nil {
		return "", nil
	}
	gwCh, ok := a.ch.(gateway.Channel)
	if !ok {
		return "", nil
	}
	return gwCh.SendCard(ctx, outboundToGateway(m))
}

// outboundToGateway translates command.Outbound to
// gateway.OutboundMessage. The Card field is translated by
// commandCardToGateway.
func outboundToGateway(m command.Outbound) gateway.OutboundMessage {
	out := gateway.OutboundMessage{
		ChatID:  m.ChatID,
		Text:    m.Text,
		ReplyTo: m.ReplyTo,
	}
	if m.Card != nil {
		gwCard := commandCardToGateway(*m.Card)
		out.Card = &gwCard
	}
	return out
}

// commandCardToGateway translates command.Card to gateway.Card.
// Choice list and ChosenChoiceEmoji map 1:1. Kind is a
// gateway.CardKind enum; we translate the canonical strings
// (command.Card.Kind holds the F-46-era string names).
func commandCardToGateway(c command.Card) gateway.Card {
	out := gateway.Card{
		Title:              c.Title,
		Body:               c.Body,
		RequestID:          c.RequestID,
		Disabled:           c.Disabled,
		ChosenChoiceEmoji:  c.ChosenEmoji,
		Kind:               cardKindFromString(c.Kind),
	}
	if len(c.Choices) > 0 {
		out.Choices = make([]gateway.CardChoice, len(c.Choices))
		for i, ch := range c.Choices {
			out.Choices[i] = gateway.CardChoice{
				Emoji:  ch.Emoji,
				Label:  ch.Label,
				Action: ch.Action,
			}
		}
	}
	return out
}

// cardKindFromString translates a command.Card.Kind string to
// the gateway.CardKind enum. Unknown values default to
// CardKindDecision (the most common case in /gtw).
func cardKindFromString(s string) gateway.CardKind {
	switch s {
	case "permission":
		return gateway.CardKindPermission
	case "decision":
		return gateway.CardKindDecision
	case "preview":
		return gateway.CardKindPreview
	}
	return gateway.CardKindDecision
}

// Compile-time check: the adapter satisfies command.Channel.
var _ command.Channel = (*channelAdapter)(nil)
