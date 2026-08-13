// policy.go — OutboundPolicy chain.
//
// NewEventHandler's per-event flow has three orthogonal policy
// decisions layered after the wire-protocol plumbing (Translate
// + ReplyTo stamp) and before em.Send:
//
//   1. StatusBar stamp — add a footer to the four main-chat
//      Kinds so the channel card carries the model + usage
//      context (F-45 §2.5).
//   2. /think gate — drop OutThinking when the chat's
//      ThinkMode == Hide (F-think §3.1.2).
//   3. /tools gate — drop OutToolStart / OutToolEnd when the
//      chat's ToolsMode == Hide (F-38 §3.1.3).
//
// Pre-Phase-2.4 these three blocks were inlined in
// NewEventHandler. The inlined form made the handler awkward to
// extend (a future "filter by chat role" or "audit outbound
// to file" policy would have to edit NewEventHandler) and
// awkward to unit-test (each policy's decision logic was
// tangled with Translate + Send).
//
// OutboundPolicy is the extracted interface. The runtime wires
// the default three policies via DefaultPolicies(). Tests can
// substitute a custom set via NewEventHandler(..., policies...)
// to exercise edge cases without spinning up an AgentSession.

package runtime

import (
	"log/slog"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/statusbar"
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
	Apply(out *messages.OutboundMessage, env chatsession.AgentEventEnvelope) (drop bool)
}

// PolicyFunc adapts an ordinary function to the OutboundPolicy
// interface, so callers can write one-liners without declaring
// a named type. Used by the default-policy constructors below.
type PolicyFunc func(out *messages.OutboundMessage, env chatsession.AgentEventEnvelope) (drop bool)

// Apply implements OutboundPolicy.
func (f PolicyFunc) Apply(out *messages.OutboundMessage, env chatsession.AgentEventEnvelope) (drop bool) {
	if f == nil {
		return false
	}
	return f(out, env)
}

// DefaultPolicies returns the three policies every production
// daemon installs (statusbar stamp + think gate + tools gate).
// The order matters: statusbar runs first so dropped events
// don't accidentally incur a stamp, and the two gates run last
// so a dropped event still records a debug log on the gate's
// owner.
//
// Pass the result of this directly to NewEventHandler:
//
//	NewEventHandler(em, cs, mgr, logger, sbDeps,
//	    DefaultPolicies(sbDeps, cs, logger)...)
func DefaultPolicies(sbDeps statusbar.Deps, cs *chatsession.ChatSession, logger *slog.Logger) []OutboundPolicy {
	return []OutboundPolicy{
		StatusBarStampPolicy(sbDeps),
		ThinkModeGatePolicy(cs, logger),
		ToolsModeGatePolicy(cs, logger),
	}
}

// StatusBarStampPolicy stamps out.StatusBar on the four
// main-chat Kinds (OutReply, OutResult, OutTaskCreate,
// OutTaskUpdate). Other Kinds skip — thread-only / lifecycle
// / init payloads would only inflate the wire payload.
//
// F-45 §2.5 改动 C: this is the F-48 stamp path. The
// dispatcher-side MessageStateBus subscriber stamps the
// MessageSubmitted transition (so the Feishu placeholder card
// has the footer from the first send); this handler stamps
// the agent-content transitions.
func StatusBarStampPolicy(sbDeps statusbar.Deps) OutboundPolicy {
	return PolicyFunc(func(out *messages.OutboundMessage, env chatsession.AgentEventEnvelope) bool {
		switch out.Kind {
		case messages.OutReply, messages.OutResult,
			messages.OutTaskCreate, messages.OutTaskUpdate:
			statusbar.StampFromAS(out, env.AgentSession, sbDeps)
		}
		return false
	})
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
	return PolicyFunc(func(out *messages.OutboundMessage, env chatsession.AgentEventEnvelope) bool {
		if out.Kind != messages.OutThinking {
			return false
		}
		if cs == nil || cs.ThinkMode() != chatsession.ThinkModeHide {
			return false
		}
		if logger != nil {
			logger.Info("think dropped",
				"chat_id", env.ChatID,
				"user_msg_id", env.UserMsgID,
				"agent_session_id", env.AgentSession.ID)
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
	return PolicyFunc(func(out *messages.OutboundMessage, env chatsession.AgentEventEnvelope) bool {
		if out.Kind != messages.OutToolStart && out.Kind != messages.OutToolEnd {
			return false
		}
		if cs == nil || cs.ToolsMode() != chatsession.ToolsModeHide {
			return false
		}
		if logger != nil {
			logger.Info("tools dropped",
				"chat_id", env.ChatID,
				"user_msg_id", env.UserMsgID,
				"agent_session_id", env.AgentSession.ID,
				"kind", out.Kind.String())
		}
		return true
	})
}

// (envAgentSession helper was removed — the only caller
// inlined env.AgentSession directly. Add back if tests
// need a per-call seam.)