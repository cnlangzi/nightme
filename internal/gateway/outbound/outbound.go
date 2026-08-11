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
// Pre-rename this package used `Stamper` (function type) +
// `stampIfNeeded` (the Emitter method) + `SessionContext` (the
// flat data struct). All three renamed together so the new
// terminology is consistent across discussion, docs, and code:
// `StatusBarSource` (the function type — produces a StatusBar)
// + `attachStatusBarIfMissing` (the Emitter method — gates on
// "missing") + `StatusBar` (the typed payload — sub-bar struct
// with GitBar / AgentBar / UsageBar). See package doc for the
// broader hub-and-spoke rationale and the GitBar-always-present
// rule.
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
)

// Channel adapters (Feishu, echo test stub, ...) implement
// channel.Channel with all six methods; that automatically
// satisfies the constructor's channel.Channel parameter. No
// alias needed — outbound takes channel.Channel directly.

// StatusBarSource produces the StatusBar attached to a chat's
// outbound messages. The runtime injects the implementation at
// Emitter construction time; outbound itself knows nothing about
// AgentSession, chatsession, git status, etc.
//
// Returning nil means "skip the status bar this turn" — caller
// has already populated msg.StatusBar OR there is no chat / no
// workspace for the chat. Emitter treats nil the same way:
// don't attach, just forward.
//
// Pre-rename this was called `Stamper`. Renamed to
// StatusBarSource because "Stamper" described the verb (stamp /
// 盖章) but not the noun (the metadata envelope being stamped
// onto the message). StatusBarSource describes both — it
// produces a StatusBar.
type StatusBarSource func(chatID string) *gateway.StatusBar

// Options configures optional Emitter behaviour. The zero value
// is valid: Emitter becomes a pure Channel.Send / SendCard
// passthrough with no StatusBar attachment.
type Options struct {
	// Source, if non-nil, is invoked for every Send / SendCard
	// whose msg.StatusBar is nil. The returned StatusBar (if
	// non-nil) is attached to msg before forwarding.
	//
	// Pre-rename this was `Stamper Stamper`; renamed to
	// `Source StatusBarSource` for the same reasons as the
	// StatusBarSource type itself.
	Source StatusBarSource
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
	source StatusBarSource
}

func (e *emitImpl) Send(ctx context.Context, msg gateway.OutboundMessage) error {
	e.attachStatusBarIfMissing(&msg)
	return e.ch.Send(ctx, msg)
}

func (e *emitImpl) SendCard(ctx context.Context, msg gateway.OutboundMessage) (string, error) {
	e.attachStatusBarIfMissing(&msg)
	return e.ch.SendCard(ctx, msg)
}

// attachStatusBarIfMissing attaches a StatusBar to msg when (a)
// the caller didn't already set one and (b) the source returns a
// non-nil value. Pointer receiver because we mutate msg in
// place — the caller observes its pre-attach msg, but the
// Channel sees the post-attach version. That's intentional
// (callers don't need to see the StatusBar they didn't ask for;
// channels do).
//
// "attachIfMissing" rather than "overwrite" because callers
// that explicitly pre-filled msg.StatusBar (e.g. the runtime
// pump using the source-AS semantics) win over the default
// source lookup. Pre-rename this was `stampIfNeeded`; the new
// name makes the "missing / present" gate explicit.
//
// Co-location (F-55): when the source produced StatusBar
// without a UsageBar but the message itself carries Usage
// (typically on OutResult after gateway.Translate), copy it
// across into StatusBar.UsageBar. The footer render path reads
// sb.UsageBar.InputTokens (not the top-level msg.Usage) so a
// missing co-located value would silently drop Line 2 of the
// footer for usage-bearing events.
func (e *emitImpl) attachStatusBarIfMissing(msg *gateway.OutboundMessage) {
	if msg.StatusBar != nil {
		return
	}
	if e.source == nil {
		return
	}
	sb := e.source(msg.ChatID)
	if sb == nil {
		return
	}
	if sb.UsageBar == nil && msg.Usage != nil {
		sb.UsageBar = &gateway.UsageStatusBar{UsageInfo: msg.Usage}
	}
	msg.StatusBar = sb
}
