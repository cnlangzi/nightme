// Package outbound is the unified egress chokepoint for every
// message the runtime wants to deliver to a user.
//
// Lifecycle of an outbound message:
//
//  1. Some caller (the runtime event pump, a slash command handler,
//     the message-dispatcher error path, ...) builds an
//     gateway.OutboundMessage.
//  2. The caller passes it to Emitter.Send / Emitter.SendCard.
//  3. Emitter optionally invokes the injected StatusBarSource to
//     attach a StatusBar (F-45/F-48) — workspace / git context
//     is always attached when the chat has a workspace, plus
//     optional AgentBar / UsageBar sub-bars. Then forwards to
//     the Channel adapter for actual rendering.
//
// Why a single chokepoint: pre-outbound-package, "stamp the
// status bar before sending" was open-coded at a handful of
// call sites (cmd/nightme/run.go's stampFromAS) and most
// outbound messages never got stamped at all — slash command
// replies bypassed it because they reached the channel via the
// outbound.Emitter wrap, not the runtime pump. Every outbound
// message now flows through Emitter, so every outbound message
// gets the same StatusBar treatment.
//
// StatusBar stamping lives in internal/statusbar (Source +
// AttachIfMissing). Outbound is now a thin consumer: Send /
// SendCard call statusbar.AttachIfMissing before forwarding to
// the Channel. The "attach if missing" / "co-locate UsageBar"
// logic is owned by statusbar, not by outbound. See package
// doc for the broader hub-and-spoke rationale and the
// GitBar-always-present rule.
//
// Relationship to internal/gateway:
//
//   - outbound imports gateway for the message types
//     (gateway.OutboundMessage, gateway.OutboundKind, etc.)
//   - gateway does NOT import outbound — gateway is the shared
//     type hub, outbound is the send-side behaviour
//   - chatsession keeps its own outbound.Emitter interface
//     (takes chatsession.OutboundMessage); cmd/nightme's
//     outbound.Emitter adapts that to gateway.OutboundMessage
//     and routes through Emitter, so slash command replies also
//     get a StatusBar attached.
//
// See docs/SPEC.md §3.x for the broader hub-and-spoke rationale.
package outbound

import (
	"context"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/gateway"
	"github.com/cnlangzi/nightme/internal/statusbar"
)

// Channel adapters (Feishu, echo test stub, ...) implement
// channel.Channel with all six methods; that automatically
// satisfies the constructor's channel.Channel parameter. No
// alias needed — outbound takes channel.Channel directly.

// Options configures optional Emitter behaviour. The zero value
// is valid: Emitter becomes a pure Channel.Send / SendCard
// passthrough with no StatusBar attachment.
type Options struct {
	// Source, if non-nil, is invoked for every Send / SendCard
	// whose msg.StatusBar is nil. The returned StatusBar (if
	// non-nil) is attached to msg before forwarding. The
	// canonical type lives in internal/statusbar (Source) —
	// outbound is now a thin consumer of that interface.
	Source statusbar.Source
}

// Emitter is the public surface every outbound caller holds.
// Constructed once per daemon (in cmd/nightme/run.go); passed to
// every component that needs to send to a chat — the runtime
// event pump, the slash command dispatcher, the message
// dispatcher closure, the MessageStateBus subscribers.
type Emitter interface {
	Send(ctx context.Context, msg gateway.OutboundMessage) error
	SendCard(ctx context.Context, msg gateway.OutboundMessage) (msgID string, err error)
}

// New constructs the default Emitter implementation. ch must be
// non-nil; opts may be its zero value.
func New(ch channel.Channel, opts Options) Emitter {
	return &emitImpl{
		ch:     ch,
		source: opts.Source,
	}
}

type emitImpl struct {
	ch     channel.Channel
	source statusbar.Source
}

func (e *emitImpl) Send(ctx context.Context, msg gateway.OutboundMessage) error {
	statusbar.AttachIfMissing(&msg, e.source)
	return e.ch.Send(ctx, msg)
}

func (e *emitImpl) SendCard(ctx context.Context, msg gateway.OutboundMessage) (string, error) {
	statusbar.AttachIfMissing(&msg, e.source)
	return e.ch.SendCard(ctx, msg)
}
