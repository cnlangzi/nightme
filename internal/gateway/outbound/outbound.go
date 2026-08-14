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
// F-CLAUDE-PRINT-002 + fix-status-bar-git: GitStatus stamping is
// now done by the Emitter at the single chokepoint, not at every
// caller. The model:
//
//   - chatsession owns the GitStatus snapshot
//     (cs.GitStatus(ctx) — pull-on-read with cache-miss refresh).
//   - SetSelectedCwd proactively refreshes; ClearSelectedCwd
//     drops the cache; /gtw commit pre/post + /gtw pr refresh
//     explicitly. RefreshGitStatus stays the single mutator.
//   - The Emitter's GitStatusLookup (wired once by runtime) is
//     invoked for every Send / SendCard whose msg.GitStatus is
//     nil. Returns the chat's pull-on-read snapshot.
//   - Business code (runtime pump, slash commands, gtw replies)
//     sends through em.Send without touching GitStatus directly.
//
// The gitStatusLookup closure reaches the chatsession via
// mgr.Get(chatID) and calls cs.GitStatus(ctx). The closure-based
// wiring avoids the outbound → chatsession import cycle.
//
// Options is retained as a struct (instead of removed entirely)
// so call sites that already pass `outbound.Options{}` keep
// working — no churn at the construction site. GitStatusLookup
// is the only field for now; nil is safe (skips stamping).
type Options struct {
	// GitStatusLookup, if non-nil, is invoked for every Send /
	// SendCard whose msg.GitStatus is nil. Returns the chat's
	// pull-on-read snapshot; nil means "no chat / no workspace"
	// and the renderer drops the git line.
	GitStatusLookup func(ctx context.Context, chatID string) *messages.GitStatus
}

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
// non-nil; opts.GitStatusLookup may be nil.
func New(ch channel.Channel, opts Options) Emitter {
	return &emitImpl{ch: ch, gitStatusLookup: opts.GitStatusLookup}
}

type emitImpl struct {
	ch              channel.Channel
	gitStatusLookup func(context.Context, string) *messages.GitStatus
}

func (e *emitImpl) Send(ctx context.Context, msg messages.OutboundMessage) error {
	e.stampGitStatus(ctx, &msg)
	return e.ch.Send(ctx, msg)
}

func (e *emitImpl) SendCard(ctx context.Context, msg messages.OutboundMessage) (string, error) {
	e.stampGitStatus(ctx, &msg)
	return e.ch.SendCard(ctx, msg)
}

// stampGitStatus attaches the chat's git snapshot to msg if not
// already set. Single chokepoint — every outbound path (runtime
// pump / slash command / MessageState / gtw) flows through here.
//
// Three guards:
//   - gitStatusLookup nil: deps not wired (e.g. unit tests); skip.
//   - msg.GitStatus non-nil: caller pre-stamped (e.g. one-shot
//     dispatchers that want to override); respect it.
//   - msg.ChatID empty: non-routed message (e.g. internal log);
//     nothing to look up.
func (e *emitImpl) stampGitStatus(ctx context.Context, msg *messages.OutboundMessage) {
	if e.gitStatusLookup == nil || msg.GitStatus != nil || msg.ChatID == "" {
		return
	}
	msg.GitStatus = e.gitStatusLookup(ctx, msg.ChatID)
}
