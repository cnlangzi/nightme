package statusbar

import (
	"github.com/cnlangzi/nightme/internal/agentsession"
	"github.com/cnlangzi/nightme/internal/messages"
)

// StampFromAS attaches a StatusBar to out using s as the source.
// Pre-fill helper for callers that need to bypass the default
// source lookup (which uses cs.SelectedAgentSession()). Used by:
//
//  1. runtime pump (per-AgentEvent delivery) — `s` is the source
//     AS that produced the event. Pre-fills StatusBar from `s`
//     regardless of which AS the chat has selected.
//  2. MessageStateBus MessageSubmitted — `as` is the source AS
//     identified by AgentSessionID in the event. Pre-fills
//     StatusBar from `as` so the multi-AS semantic ("source
//     AS, not selected AS") is preserved when the runtime
//     pump's stamper (selected-AS) would otherwise win.
//
// Pre-move this was `cmd/nightme/run.go stampFromAS`. Renamed
// from sessionContextInto because "Into" was vague about
// direction; the new name states explicitly that we're stamping
// using a specific AS rather than the default source.
//
// F-55: out.Usage is set by gateway.Translate from the bridge
// wire payload (AgentResultEvent.Usage / AgentDoneEvent.Usage).
// Build reads it via the `usage` parameter and copies it
// verbatim into the UsageBar so the channel footer can render
// it. Pre-F-55 the copy was missing, so footers silently
// rendered without usage data.
func StampFromAS(out *messages.OutboundMessage, s *agentsession.AgentSession, deps Deps) {
	out.StatusBar = Build(s, out.Usage, deps)
}

// AttachIfMissing attaches a StatusBar to msg when (a) the caller
// didn't already set one and (b) source returns a non-nil value.
// Mutates msg in place: the caller observes its pre-attach msg,
// but the Channel sees the post-attach version. That's
// intentional — callers don't need to see the StatusBar they
// didn't ask for; channels do.
//
// "attachIfMissing" rather than "overwrite" because callers that
// explicitly pre-filled msg.StatusBar (e.g. the runtime pump
// using the source-AS semantics via StampFromAS) win over the
// default source lookup. Pre-move this lived on
// outbound.emitImpl as `attachStatusBarIfMissing`; the move here
// keeps "what does stamping do" next to "how do you stamp".
//
// Co-location (F-55): when source produced a StatusBar without
// a UsageBar but the message itself carries Usage (typically on
// OutResult after gateway.Translate), copy it across into
// StatusBar.UsageBar. The footer render path reads
// sb.UsageBar.InputTokens (not the top-level msg.Usage) so a
// missing co-located value would silently drop Line 2 of the
// footer for usage-bearing events.
func AttachIfMissing(msg *messages.OutboundMessage, source Source) {
	if msg.StatusBar != nil {
		return
	}
	if source == nil {
		return
	}
	sb := source(msg.ChatID)
	if sb == nil {
		return
	}
	if sb.UsageBar == nil && msg.Usage != nil {
		sb.UsageBar = &messages.UsageStatusBar{UsageInfo: msg.Usage}
	}
	msg.StatusBar = sb
}
