// handler.go — per-ChatSession AgentEvent → OutboundMessage
// translation.
//
// NewEventHandler is the per-cs factory the runtime installs
// via WireRuntimeCallbacksAndRestore. The handler translates
// AgentEvent → OutboundMessage and dispatches via the channel,
// with a configurable OutboundPolicy chain layered between
// translate and send (see policy.go).
//
// The handler factory is exported so cmd/nightme tests (and
// any future runtime-internal caller) can install it on a
// per-cs basis without going through the full
// Manager.WithOnCreate → WireRuntimeCallbacksAndRestore path.

package runtime

import (
	"context"
	"log/slog"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/gateway/outbound"
	"github.com/cnlangzi/nightme/internal/messages"
)

// NewEventHandler returns the per-event callback installed on
// every ChatSession by the runtime. The callback translates
// AgentEvent → OutboundMessage and dispatches via the channel.
//
// The optional policies slice lets callers customise the
// post-translate behaviour (/think gate, /tools gate are the
// defaults; see DefaultPolicies). GitStatus stamping is no longer
// a policy — it happens at the outbound Emitter chokepoint
// (outbound.Options.GitStatusLookup) and is invisible here. When
// policies is empty, DefaultPolicies is used. To add a custom
// policy, append it after DefaultPolicies(...) — order
// matters; the first drop wins.
//
// chatID is passed by ChatSession's readPump directly (the
// ChatSession knows its own ChatID); the handler doesn't need
// to look it up.
//
// userMsgID is the current turn's single anchor (passed by
// readPump from AgentSession.currentPrompt.LastMessageID; F-53
// moved it there from the deleted cs.currentTurnUserMsgID
// scalar). The handler stamps it onto OutboundMessage.ReplyTo
// so each Channel can route the event to its own per-userMsgID
// receipt (card / thread / DOM node). Empty when the event has
// no anchor (startup EventAgentReady, post-/use while no Prompt
// is active, etc.) — Channel falls back to plain text.
//
// v1.3 (SPEC §2.2): 1 turn : 1 anchor. Receipt rendering and
// FSM are Channel-internal; Gateway only knows about userMsgID.
//
// Per-cs construction (not per-Mgr): the F-think / F-tools
// gates (now extracted as OutboundPolicy) read cs.ThinkMode() /
// cs.ToolsMode() on every relevant event, and the readPump
// fires only for ChatSessions that already exist, so the
// ChatSession is statically known at install time. Capturing
// it in the closure eliminates the per-event mgr.Get round-
// trip (RLock + map lookup). mgr is still passed because
// EventAgentReady persistence needs mgr.PersistAgentSession,
// which is the cold path (once per AgentSession lifetime,
// not per event).
//
// F-54: the handler takes a typed chatsession.AgentEventEnvelope
// (delivered by cs.AgentEventBus). The legacy
// `chatsession.EventHandler` callback signature is gone — see
// docs/feat/F-54-event-bus.md §3.5.
func NewEventHandler(
	em outbound.Emitter,
	cs *chatsession.ChatSession,
	mgr *chatsession.Manager,
	logger *slog.Logger,
	sbDeps chatsession.GitStatusDeps,
	policies ...OutboundPolicy,
) func(env chatsession.AgentEventEnvelope) {
	// Per-cs closure. No per-handler mutable state needed anymore:
	// the bridge layer now attaches per-turn Usage to the SAME
	// AgentResultEvent that delivers the final text (claudecode
	// result.usage + result.modelUsage; Pi message_end.usage), so
	// the runtime does not buffer OutResult waiting for a follow-up
	// usage event. F-52 / AgentEvent-flattening unified the data on
	// a single wire event; this collapsing was the bridge-layer
	// artefact it dissolved.
	if len(policies) == 0 {
		policies = DefaultPolicies(sbDeps, cs, logger)
	}
	return func(env chatsession.AgentEventEnvelope) {
		chatID, s, ev, userMsgID := env.ChatID, env.AgentSession, env.Event, env.UserMsgID
		// Capture the agent's own session id from EventAgentReady so the
		// next respawn can replay `--resume <id>`. We persist
		// immediately (rather than waiting for the next status
		// transition) so a daemon crash after this event still
		// remembers the id. The capture is idempotent.
		//
		// Guard: only overwrite an existing (non-empty) SessionID when
		// the new id is non-empty. Some bridges re-emit EventAgentReady
		// after a child restart with a blank SessionID; we don't
		// want to wipe a previously-captured id in that case.
		if ev.Kind == agent.EventAgentReady && ev.SessionID != "" {
			s.SetSessionID(ev.SessionID)
			if mgr != nil {
				if err := mgr.PersistAgentSession(s); err != nil && logger != nil {
					logger.Warn("persist agent session (init) failed",
						"chat_id", chatID,
						"agent_session_id", s.ID,
						"err", err)
				}
			}
		}
		// F-45 §1.4: capture model on first EventAgentReady. Independent
		// of the SessionID capture above (some bridges may emit
		// one without the other). SetModel is idempotent — empty
		// incoming values don't overwrite.
		if ev.Kind == agent.EventAgentReady && ev.Model != "" {
			s.SetModel(ev.Model)
		}
		// Per-event usage flows from bridge → out.Usage (via
		// gateway.Translate) → StatusBar.Usage (via
		// StatusBarStampPolicy → StampFromAS) → channel footer. The
		// runtime is a passive pass-through; no accumulation, no
		// dedup, no priority. Usage rides on EventAgentResult
		// (populated by the bridges via AgentResultEvent.Usage) —
		// AgentDoneEvent.Usage is not currently surfaced because
		// gateway.Translate drops EventAgentDone events before the
		// runtime applies policies.
		//
		// PersistIfDirty is no longer driven from here (the
		// cumulative-dirty trigger is gone with cross-turn usage
		// aggregation). Future per-AgentSession dirty state can
		// hook in without changing call sites — see
		// AgentSession.PersistIfDirty for the new contract.
		// F-49 compaction tracking removed: bridges no longer
		// emit a dedicated compaction event; runtime is a pure
		// pass-through.

		// Translate the AgentEvent to an OutboundMessage.
		out, ok := outbound.Translate(chatID, *ev)
		if !ok {
			if logger != nil {
				logger.Debug("runtime: Translate dropped event",
					"chat_id", chatID,
					"kind", ev.Kind.String(),
					"agent_session_id", s.ID)
			}
			// Translate drops events that don't surface to the
			// channel (EventAgentDone, EventAgentCompaction
			// (F-49 deleted), thread-only kinds, etc.). The
			// runtime no longer folds usage anywhere — Agent is a
			// passive pass-through, and the channel-side footer
			// reads ctx.Usage directly from OutboundMessage on the
			// OK path below. Done. Usage, if any, dies with the
			// dropped event (channel never sees it). See
			// docs/feat/F-45-session-footer.md §1.5 / §1.6 for
			// the per-turn snapshot contract.
			return
		}
		// The runtime is a passive pass-through: out.Usage is
		// populated by gateway.Translate from the bridge event
		// (AgentResultEvent.Usage / AgentDoneEvent.Usage) and rendered
		// verbatim by the channel footer. No accumulation, no
		// dedup, no priority — bridges are free to populate
		// either field and the channel reads out.Usage directly.
		//
		// Stamp the current turn's anchor so the Channel can
		// route to the right per-userMsgID receipt. ReplyTo
		// stays empty for orphan events (EventAgentReady at
		// startup, internal logs) — Channel renders those as
		// plain text.
		out.ReplyTo = userMsgID

		// F-CLAUDE-PRINT-002: identity (Agent / Model / SessionID)
		// is sticky on the AgentSession (set by EventAgentReady
		// once at session start). Per-event bridge events
		// (EventAgentResult, EventAgentText, etc.) don't carry
		// these fields — translate leaves the flat identity
		// fields empty on those events. Stamp them here from the
		// AS state so the Channel always has them. Out-of-band
		// /override: dispatcher that wants a different model
		// (one-shot /gtw commit on a cheaper model) sets
		// out.Model itself; the `if == ""` guard below respects
		// that override.
		if out.AgentName == "" {
			out.AgentName = s.Agent
		}
		if out.Model == "" {
			out.Model = s.Model()
		}
		if out.SessionID == "" {
			out.SessionID = s.SessionID()
		}
		if out.Workspace == "" {
			out.Workspace = s.Cwd
		}

		// ══════════════════════════════════════════════════════════════════
		// ⭐ F-63 核心不变量:Heartbeat 观测必须在 Policy 链之前 ⭐
		// ══════════════════════════════════════════════════════════════════
		//
		// /think off (ThinkModeGatePolicy) 和 /tools off
		// (ToolsModeGatePolicy) 是显示策略,不是 agent 行为策略。它们
		// 在下面的 Policy.Apply 时 drop 消息,但 agent 实际上仍在
		// thinking / 调工具。计数器必须反映真实动作,放在 policy
		// 之前才能保证 /think off / /tools off 期间数字也照常累计。
		//
		// 守护测试:handler_test.go::TestEventHandler_ThinkOff_StillCounts
		// 和 TestEventHandler_ToolsOff_StillCounts。任一破坏即 test fail。
		//
		// OutHeartbeat 自身不发到这里——这是 OutboundKind 由
		// gateway.Translate 从 AgentEvent 翻译出来的产物,
		// OutHeartbeat 是观测结果经 em.Send 二次产出,不会进入
		// Observe 路径,所以也不会自递归。
		if userMsgID != "" && cs != nil && cs.Heartbeat() != nil {
			if cs.Heartbeat().Observe(userMsgID, out.Kind) {
				snap := cs.Heartbeat().Snapshot(userMsgID)
				// F-63 §3.8 #6: if the tracker entry was LRU-evicted
				// between Observe (which created/incremented the
				// entry and returned true) and Snapshot (which
				// returns zero for absent keys), drop the follow-up
				// here rather than sending an empty OutHeartbeat
				// that the adapter's Empty() guard would discard
				// anyway. Saves a cross-channel round-trip and a
				// log line for a no-op.
				if snap.Empty() {
					if logger != nil {
						logger.Debug("heartbeat dropped (tracker entry empty post-observe)",
							"chat_id", chatID,
							"user_msg_id", userMsgID,
							"kind", out.Kind.String())
					}
				} else {
					hb := messages.OutboundMessage{
						ChatID:    chatID,
						Kind:      messages.OutHeartbeat,
						ReplyTo:   userMsgID,
						Heartbeat: &snap,
					}
					if err := em.Send(context.Background(), hb); err != nil && logger != nil {
						logger.Warn("heartbeat follow-up send failed",
							"chat_id", chatID,
							"user_msg_id", userMsgID,
							"err", err)
					}
				}
			}
		}

		// Apply OutboundPolicy chain. Each policy may mutate
		// out (e.g. StatusBarStampPolicy fills out.StatusBar)
		// or short-circuit with drop=true (e.g. ThinkMode /
		// ToolsMode gates when the corresponding mode is Hide).
		// The runtime never inspects a policy's verdict — it
		// just hands the message to the next policy / em.Send.
		//
		// F-63: the heartbeat observe above runs BEFORE this loop,
		// so even when a policy drops the original Out* (e.g. /think
		// off hides OutThinking), the heartbeat counter has already
		// been incremented and the OutHeartbeat follow-up has
		// already been sent. See F-63 §3.2 for the invariant.
		for _, p := range policies {
			if p.Apply(&out, env) {
				return
			}
		}

		if err := em.Send(context.Background(), out); err != nil && logger != nil {
			logger.Warn("channel send failed",
				"chat_id", chatID,
				"user_msg_id", userMsgID,
				"agent_session_id", s.ID,
				"err", err)
		}
		if logger != nil {
			logger.Debug("runtime: ch.Send dispatched",
				"chat_id", chatID,
				"user_msg_id", userMsgID,
				"agent_session_id", s.ID,
				"kind", out.Kind.String(),
				"text_len", len(out.Text))
		}
	}
}