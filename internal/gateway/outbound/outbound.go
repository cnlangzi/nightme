// Package outbound is the unified egress chokepoint for every
// message the runtime wants to deliver to a user.
//
// Lifecycle of an outbound message:
//
//  1. Some caller (the runtime event pump, a slash command handler,
//     the message-dispatcher error path, ...) builds an
//     messages.OutboundMessage.
//  2. The caller passes it to Emitter.Send.
//  3. Emitter stamps the chatsession's GitStatus snapshot (via
//     the injected GitStatusLookup closure) when the message
//     doesn't carry one yet — workspace / git context is always
//     attached when the chat has a workspace. Then forwards to
//     the Channel adapter for actual rendering.
//
// Why a single chokepoint: pre-fix-status-bar-git, GitStatus
// stamping lived at five caller sites (runtime pump,
// MessageState eventbus, and each gtw dispatch), all calling
// cs.GitStatus() without a context. Without the chokepoint the
// footer was sometimes stale or empty, and the same snapshot
// could land on multiple outbound paths with subtly different
// mutation semantics. The Emitter is now the single chokepoint:
// GitStatusLookup (set once in cmd/nightme/run.go's runDaemon)
// is invoked for every Send whose msg.GitStatus is
// nil. Business code never touches GitStatus directly —
// runtime/handler.go and eventbus.go drop the stamps, gtw
// dispatchers stop pre-filling out.GitStatus. ChatSession owns
// the snapshot per-chat and rebuilds it fresh on every lookup
// (no per-chat cache layer; freshness is the contract).
//
// Relationship to internal/gateway:
//
//   - outbound imports gateway for the message types
//     (messages.OutboundMessage, gateway.OutboundKind, etc.)
//   - gateway does NOT import outbound — gateway is the shared
//     type hub, outbound is the send-side behaviour
//   - chatsession keeps its own messages.Emitter interface
//     (takes chatsession.OutboundMessage); cmd/nightme's
//     messages.Emitter adapts that to messages.OutboundMessage
//     and routes through Emitter, so slash command replies also
//     pick up the GitStatus stamp.
//
// See docs/SPEC.md §3.x for the broader hub-and-spoke rationale.
package outbound

import (
	"context"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/messages"
)

// Channel adapters (Feishu, echo test stub, ...) implement
// channel.Channel; that automatically satisfies the constructor's
// channel.Channel parameter. No alias needed — outbound takes
// channel.Channel directly.
//
// F-CLAUDE-PRINT-002 + fix-status-bar-git: GitStatus stamping is
// now done by the Emitter at the single chokepoint, not at every
// caller. The model:
//
//   - chatsession owns the GitStatus snapshot directly:
//     ChatSession.GitStatus(ctx) rebuilds a fresh snapshot on
//     every call — git status runs against the workspace, the
//     PR is a synchronous prcache.Cache.PR() read. There is no
//     per-chat cache layer; freshness is the explicit goal.
//   - The Emitter's GitStatusLookup (wired once by runtime) is
//     invoked for every Send whose msg.GitStatus is
//     nil, returning the freshly-built snapshot.
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
	// GitStatusLookup, if non-nil, is invoked for every Send
	// whose msg.GitStatus is nil. Returns the chat's
	// pull-on-read snapshot; nil means "no chat / no workspace"
	// and the renderer drops the git line.
	GitStatusLookup func(ctx context.Context, chatID string) *messages.GitStatus
}

// Emitter is the public surface every outbound caller holds.
// Constructed once per daemon (in cmd/nightme/run.go); passed to
// every component that needs to send to a chat — the runtime
// event pump, the slash command dispatcher, the message
// dispatcher closure, the MessageStateBus subscribers.
//
// F-CODEX-RUNONCE-REVIEW-EVENT: the Emitter interface itself
// moved to internal/messages (as messages.Emitter) so that
// chatsession can hold an Emitter field without importing
// gateway/outbound. Without this move, adding gateway/outbound
// → chatsession dependency (for the policy move) would re-open
// the import cycle that the original closure-based wiring
// worked around (see outbound.go:73-75 historical comment).
// The implementation `emitImpl` stays here; only the interface
// declaration moved.

// New constructs the default Emitter implementation. ch must be
// non-nil; opts.GitStatusLookup may be nil.
func New(ch channel.Channel, opts Options) messages.Emitter {
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
//
// Kind guard (fix-git-lock-file, 2026-08-29):
//   - OutToolStart / OutToolEnd — a Bash tool may be mid-flight
//     holding .git/index.lock; another `git status` here races
//     with that lock holder and is exactly how the stale-lock
//     issue manifested.
//   - OutThinking — reasoning metadata the user doesn't see;
//     stamping a fresh `git status` for it is pure overhead.
//   - OutHeartbeat — per-turn progress tick (ThinkCount /
//     ToolCount / LastBeatAt) for the receipt's top header.
//     Pure internal state propagation; the user sees a counter
//     refresh, not a new event boundary. A long turn with N
//     heartbeats would otherwise trigger N `git status` calls.
//
// Every other kind (OutReply, OutResult, OutMessageState,
// OutMessageStateRemoved, OutError, OutChoice, OutChoicePatch,
// OutInit, OutCommandReply, OutTaskCreate/Update, …) still
// stamps so the footer stays fresh at every user-visible state
// boundary.
//
// Future OutboundKind additions: pick explicitly. If the new
// kind is "tool-state-like" (Bash in flight, reasoning, internal
// progress tick — see the skip list above), add it to the case
// statement. Otherwise let it fall through to stamp.
func (e *emitImpl) stampGitStatus(ctx context.Context, msg *messages.OutboundMessage) {
	if e.gitStatusLookup == nil || msg.GitStatus != nil || msg.ChatID == "" {
		return
	}
	switch msg.Kind {
	case messages.OutToolStart,
		messages.OutToolEnd,
		messages.OutThinking,
		messages.OutHeartbeat:
		return
	}
	msg.GitStatus = e.gitStatusLookup(ctx, msg.ChatID)
}
