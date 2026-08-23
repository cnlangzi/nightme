// policy.go — OutboundPolicy chain.
//
// OutboundPolicy is a per-event decision hook that runs AFTER
// wire-protocol plumbing (Translate + ReplyTo stamp) and BEFORE
// em.Send. Each policy may:
//   - inspect / mutate the OutboundMessage (e.g. status-bar
//     stamping), or
//   - return drop=true to short-circuit the chain (the runtime
//     skips em.Send for that event).
//
// The runtime wires the default policy set via DefaultPolicies();
// tests can substitute a custom set via
// NewEventHandler(..., policies...) to exercise edge cases
// without spinning up an AgentSession.
//
// F-CODEX-RUNONCE-REVIEW-EVENT: this file used to live in
// internal/runtime/policy.go. The move to internal/gateway/outbound
// is a pure relocation (no logic change) driven by the fact that
// StreamRunOnceToEmitter::dispatchSinkEvent (one-shot path) now
// applies the same gates + Heartbeat observe as the long-lived
// runtime.NewEventHandler. Policy is about outbound rendering
// decisions (think/tools gates), not about chat session internals
// — it belongs in the outbound package, alongside Translate and
// Emitter. Both runtime.NewEventHandler and dispatchSinkEvent
// now call outbound.DefaultPolicies / outbound.ThinkModeGatePolicy
// / outbound.ToolsModeGatePolicy directly, sharing the same
// policy implementation. No new interface, no new struct.
//
// The Emitter interface itself lives in internal/messages (it
// moved there in the same change so chatsession can hold an
// Emitter field without importing outbound — that move is what
// makes the outbound → chatsession import direction cycle-free).

package outbound

import (
	"log/slog"

	"github.com/cnlangzi/nightme/internal/agentsession"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/messages"
)

// OutboundPolicy inspects / modifies an outbound message before
// it is handed to the channel. Returning drop=true short-
// circuits the chain — the runtime skips em.Send for that
// event. The runtime applies policies in registration order;
// the first drop wins.
//
// Implementations are pure with respect to side effects:
// they may read ChatSession state (think/tools modes) and may
// mutate out.StatusBar, but they MUST NOT call em.Send,
// cs.QueueUserMessage, or any other channel/agent sink.
// Side-effecting observers belong on the Bus subscriptions
// (MessageStateBus / AgentEventBus), not on the policy chain.
type OutboundPolicy interface {
	Apply(out *messages.OutboundMessage, env agentsession.AgentEventEnvelope) (drop bool)
}

// PolicyFunc adapts an ordinary function to the OutboundPolicy
// interface, so callers can write one-liners without declaring
// a named type. Used by the default-policy constructors below.
type PolicyFunc func(out *messages.OutboundMessage, env agentsession.AgentEventEnvelope) (drop bool)

// Apply implements OutboundPolicy.
func (f PolicyFunc) Apply(out *messages.OutboundMessage, env agentsession.AgentEventEnvelope) (drop bool) {
	if f == nil {
		return false
	}
	return f(out, env)
}

// DefaultPolicies returns the policies every production daemon
// installs (think gate + tools gate).
//
// F-CLAUDE-PRINT-002: StatusBarStampPolicy is gone. The runtime
// event hook (handler.go) stamps chatsession.GitStatus onto
// out.GitStatus directly — no policy needed for that.
//
// Pass the result of this directly to NewEventHandler:
//
//	NewEventHandler(em, cs, mgr, logger, sbDeps,
//	    DefaultPolicies(sbDeps, cs, logger)...)
//
// F-CODEX-RUNONCE-REVIEW-EVENT: StreamRunOnceToEmitter's drain
// goroutine calls this too — same gate logic, one source of truth.
func DefaultPolicies(sbDeps chatsession.GitStatusDeps, cs *chatsession.ChatSession, logger *slog.Logger) []OutboundPolicy {
	return []OutboundPolicy{
		ThinkModeGatePolicy(cs, logger),
		ToolsModeGatePolicy(cs, logger),
	}
}

// ThinkModeGatePolicy drops OutThinking events when the chat
// has /think off (ThinkMode == ThinkModeHide). All other
// Kinds pass through — the gate is intentionally narrow so
// /think off can't accidentally silence OutReply / OutResult.
//
// Logs an Info-level "think dropped" line when a drop fires
// so operators running at default log level can see the
// silent-swallow (mirrors the F-watch drop convention).
// A nil logger means "silent" — production never passes nil
// but a future caller might.
func ThinkModeGatePolicy(cs *chatsession.ChatSession, logger *slog.Logger) OutboundPolicy {
	return PolicyFunc(func(out *messages.OutboundMessage, env agentsession.AgentEventEnvelope) bool {
		if out.Kind != messages.OutThinking {
			return false
		}
		if cs == nil || cs.ThinkMode() != chatsession.ThinkModeHide {
			return false
		}
		if logger != nil {
			// env.AgentSession is documented as ALWAYS non-nil in
			// production (the publisher guards as == nil), but a
			// future publisher that misses the guard would
			// produce a nil-deref panic. Defend here so a policy
			// regression doesn't take down the runtime.
			asID := ""
			if env.AgentSession != nil {
				asID = env.AgentSession.ID
			}
			logger.Info("think dropped",
				"chat_id", env.ChatID,
				"user_msg_id", env.UserMsgID,
				"agent_session_id", asID)
		}
		return true
	})
}

// ToolsModeGatePolicy drops OutToolStart / OutToolEnd events
// when the chat has /tools off (ToolsMode == ToolsModeHide).
// Other Kinds pass through — OutReply / OutResult /
// OutThinking / OutInit / OutUsage are unaffected. The
// merge rendering (PATCH on start message_id when /tools
// on) is a Feishu-adapter concern; this gate just decides
// whether the event reaches the Channel at all.
func ToolsModeGatePolicy(cs *chatsession.ChatSession, logger *slog.Logger) OutboundPolicy {
	return PolicyFunc(func(out *messages.OutboundMessage, env agentsession.AgentEventEnvelope) bool {
		if out.Kind != messages.OutToolStart && out.Kind != messages.OutToolEnd {
			return false
		}
		if cs == nil || cs.ToolsMode() != chatsession.ToolsModeHide {
			return false
		}
		if logger != nil {
			// Defend against a future publisher that misses the
			// env.AgentSession nil guard (see ThinkModeGatePolicy).
			asID := ""
			if env.AgentSession != nil {
				asID = env.AgentSession.ID
			}
			logger.Info("tools dropped",
				"chat_id", env.ChatID,
				"user_msg_id", env.UserMsgID,
				"agent_session_id", asID,
				"kind", out.Kind.String())
		}
		return true
	})
}
