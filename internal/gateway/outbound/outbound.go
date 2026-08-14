// Package outbound is the unified egress chokepoint for every
// message the runtime wants to deliver to a user.
//
// Lifecycle of an outbound message:
//
//  1. Some caller (the runtime event pump, a slash command handler,
//     the message-dispatcher error path, ...) builds an
//     messages.OutboundMessage.
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
//     (messages.OutboundMessage, gateway.OutboundKind, etc.)
//   - gateway does NOT import outbound — gateway is the shared
//     type hub, outbound is the send-side behaviour
//   - chatsession keeps its own outbound.Emitter interface
//     (takes chatsession.OutboundMessage); cmd/nightme's
//     outbound.Emitter adapts that to messages.OutboundMessage
//     and routes through Emitter, so slash command replies also
//     get a StatusBar attached.
//
// See docs/SPEC.md §3.x for the broader hub-and-spoke rationale.
package outbound

import (
	"context"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/messages"
)

// Channel adapters (Feishu, echo test stub, ...) implement
// channel.Channel with all six methods; that automatically
// satisfies the constructor's channel.Channel parameter. No
// alias needed — outbound takes channel.Channel directly.
//
// F-CLAUDE-PRINT-002: Options.Source is gone. The previous
// "if msg.StatusBar is nil, attach from Source" defensive
// fallback was an anti-pattern — it hid bugs by silently
// patching missing data, and it coupled the gateway to
// statusbar (creating an import cycle). The new model:
//
//   - chatsession owns the GitStatus snapshot
//     (cs.GitStatus() / cs.RefreshGitStatus()).
//   - The runtime event hook (internal/runtime/handler.go)
//     stamps chatsession.GitStatus onto out.GitStatus
//     directly, at translate-time.
//   - Slash-command replies (commander.Dispatch, etc.) and
//     one-shot dispatchers stamp their own out.GitStatus
//     before calling em.Send.
//   - Outbound is now a pure transport. No Source, no fallback.
//
// Options is retained as a struct (instead of removed entirely)
// so call sites that already pass `outbound.Options{}` keep
// working — no churn at the construction site.
type Options struct{}

// Emitter is the public surface every outbound caller holds.
// Constructed once per daemon (in cmd/nightme/run.go); passed to
// every component that needs to send to a chat — the runtime
// event pump, the slash command dispatcher, the message
// dispatcher closure, the MessageStateBus subscribers.
type Emitter interface {
	Send(ctx context.Context, msg messages.OutboundMessage) error
	SendCard(ctx context.Context, msg messages.OutboundMessage) (msgID string, err error)
}

// New constructs the default Emitter implementation. ch must be
// non-nil; opts may be its zero value.
func New(ch channel.Channel, opts Options) Emitter {
	return &emitImpl{ch: ch}
}

type emitImpl struct {
	ch channel.Channel
}

func (e *emitImpl) Send(ctx context.Context, msg messages.OutboundMessage) error {
	return e.ch.Send(ctx, msg)
}

func (e *emitImpl) SendCard(ctx context.Context, msg messages.OutboundMessage) (string, error) {
	return e.ch.SendCard(ctx, msg)
}
