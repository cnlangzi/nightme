// Package outbound is the unified egress chokepoint for every
// message the runtime wants to deliver to a user.
//
// Lifecycle of an outbound message:
//
//  1. Some caller (the runtime event pump, a slash command handler,
//     the message-dispatcher error path, ...) builds an
//     gateway.OutboundMessage.
//  2. The caller passes it to Emitter.Send / Emitter.SendCard.
//  3. Emitter optionally invokes the injected Stamper to attach
//     a SessionContext footer (F-45/F-48), then forwards to the
//     Channel adapter for actual rendering.
//
// Why a single chokepoint: pre-outbound-package, "stamp the
// SessionContext footer before sending" was open-coded at a handful
// of call sites (cmd/nightme/run.go's sessionContextInto) and most
// outbound messages never got stamped at all — slash command
// replies bypassed it because they reached the channel via the
// outbound.Emitter wrap, not the runtime pump. Every outbound
// message now flows through Emitter, so every outbound message gets
// the same treatment.
//
// Relationship to internal/gateway:
//
//   - outbound imports gateway for the message types
//     (gateway.OutboundMessage, gateway.OutboundKind, etc.)
//   - gateway does NOT import outbound — gateway is the shared type
//     hub, outbound is the send-side behaviour
//   - chatsession keeps its own outbound.Emitter interface
//     (takes chatsession.OutboundMessage); cmd/nightme's
//     outbound.Emitter adapts that to gateway.OutboundMessage and routes
//     through Emitter, so slash command replies also get stamped.
//
// See docs/SPEC.md §3.x for the broader hub-and-spoke rationale.
package outbound

import (
	"context"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/gateway"
)

// Channel is the minimum contract outbound needs from an IM
// adapter. The full Channel interface lives in internal/channel;
// outbound embeds it as a private field so no one outside
// outbound can hold a reference to the underlying IM adapter.
//
// Channel adapters (Feishu, echo test stub, ...) implement
// channel.Channel with all six methods; that automatically
// satisfies the embedded field. No explicit wrapper or
// assertion needed.
type Channel = channel.Channel

// Stamper produces the F-45/F-48 SessionContext footer for a
// chat's outbound messages. The runtime injects the
// implementation at Emitter construction time; outbound itself
// knows nothing about AgentSession, chatsession, git status, etc.
//
// Returning nil means "skip the footer this turn" — caller has
// already populated msg.SessionContext OR there is no active
// session / nothing meaningful to render. Emitter treats nil
// the same way: don't stamp, just forward.
type Stamper func(chatID string) *gateway.SessionContext

// Options configures optional Emitter behaviour. The zero value
// is valid: Emitter becomes a pure Channel.Send passthrough with
// no stamping, no error hooks — equivalent to the legacy
// outbound.Emitter behaviour minus the type-conversion step.
type Options struct {
	// Stamper, if non-nil, is invoked for every Send / SendCard
	// whose msg.SessionContext is nil. The returned SessionContext
	// (if non-nil) is attached to msg before forwarding.
	Stamper Stamper

	// OnError, if non-nil, is invoked when Channel.Send /
	// Channel.SendCard returns a non-nil error. The error is
	// also returned to the caller — OnError is for logging /
	// metrics side effects only.
	OnError func(msg gateway.OutboundMessage, err error)
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
func New(ch Channel, opts Options) Emitter {
	return &emitImpl{
		ch:      ch,
		stamper: opts.Stamper,
		onError: opts.OnError,
	}
}

type emitImpl struct {
	ch      channel.Channel
	stamper Stamper
	onError func(gateway.OutboundMessage, error)
}

func (e *emitImpl) Send(ctx context.Context, msg gateway.OutboundMessage) error {
	e.stampIfNeeded(&msg)
	if err := e.ch.Send(ctx, msg); err != nil {
		if e.onError != nil {
			e.onError(msg, err)
		}
		return err
	}
	return nil
}

func (e *emitImpl) SendCard(ctx context.Context, msg gateway.OutboundMessage) (string, error) {
	e.stampIfNeeded(&msg)
	msgID, err := e.ch.SendCard(ctx, msg)
	if err != nil {
		if e.onError != nil {
			e.onError(msg, err)
		}
		return msgID, err
	}
	return msgID, nil
}

// stampIfNeeded attaches SessionContext to msg when (a) the caller
// didn't already set one and (b) the stamper returns a non-nil
// value. Pointer receiver because we mutate msg in place — the
// caller observes its pre-stamp msg, but the Channel sees the
// post-stamp version. That's intentional (callers don't need to
// see the stamp they didn't ask for; channels do).
//
// F-55 co-location: when the stamper produced SessionContext
// without a Usage field but the message itself carries Usage
// (typically on OutResult after gateway.Translate), copy it
// across. The footer render path keys off ctx.Usage (not the
// top-level msg.Usage) so a missing co-located value would
// silently drop Line 2 of the footer for usage-bearing events.
func (e *emitImpl) stampIfNeeded(msg *gateway.OutboundMessage) {
	if msg.SessionContext != nil {
		return
	}
	if e.stamper == nil {
		return
	}
	sc := e.stamper(msg.ChatID)
	if sc == nil {
		return
	}
	if sc.Usage == nil && msg.Usage != nil {
		sc.Usage = msg.Usage
	}
	msg.SessionContext = sc
}