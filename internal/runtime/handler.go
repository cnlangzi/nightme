// handler.go — per-ChatSession AgentEvent → OutboundMessage
// translation, including per-chat /think and /tools gates and
// StatusBar stamping.
//
// The handler factory NewEventHandler is exported so the
// cmd/nightme tests (and any future runtime-internal callers)
// can install it on a per-cs basis without going through the
// full Manager.WithOnCreate → WireRuntimeCallbacksAndRestore
// path.

package runtime

import (
	"context"
	"log/slog"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/gateway/outbound"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/statusbar"
)

// NewEventHandler returns the per-event callback installed on
// every ChatSession by the runtime. The callback translates
// AgentEvent → OutboundMessage and dispatches via the channel.
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
// no anchor (startup EventAgentReady, post-/use while no Prompt is
// active, etc.) — Channel falls back to plain text in that case.
//
// v1.3 (SPEC §2.2): 1 turn : 1 anchor. Receipt rendering and
// FSM are Channel-internal; Gateway only knows about userMsgID.
//
// Per-cs construction (not per-Mgr): the F-think gate reads
// cs.ThinkMode() on every OutThinking event, and the readPump
// fires only for ChatSessions that already exist, so the
// ChatSession is statically known at install time. Capturing it
// in the closure eliminates the per-event mgr.Get round-trip
// (RLock + map lookup). mgr is still passed because EventAgentReady
// persistence needs mgr.PersistAgentSession, which is the cold
// path (once per AgentSession lifetime, not per event).
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
	sbDeps statusbar.Deps,
) func(env chatsession.AgentEventEnvelope) {
	// Per-cs closure. No per-handler mutable state needed anymore:
	// the bridge layer now attaches per-turn Usage to the SAME
	// AgentResultEvent that delivers the final text (claudecode
	// result.usage + result.modelUsage; Pi message_end.usage), so
	// the runtime does not buffer OutResult waiting for a follow-up
	// usage event. F-52 / AgentEvent-flattening unified the data on
	// a single wire event; this collapsing was the bridge-layer
	// artefact it dissolved.
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
		// stampFromAS below) → channel footer. The runtime
		// is a passive pass-through; no accumulation, no dedup,
		// no priority. Usage rides on EventAgentResult (populated by
		// the bridges via AgentResultEvent.Usage) — AgentDoneEvent.Usage is
		// not currently surfaced because gateway.Translate drops
		// EventAgentDone events before the runtime stamps
		// StatusBar. Bridges that only emit EventAgentDone-with-
		// Usage (no EventAgentResult) will not see their Usage in the
		// footer; only pi-style bridges that have an explicit
		// EventAgentResult path will.
		//
		// PersistIfDirty is no longer driven from here (the
		// cumulative-dirty trigger is gone with cross-turn usage
		// aggregation). Future per-AgentSession dirty state can
		// hook in without changing call sites — see
		// AgentSession.PersistIfDirty for the new contract.
		// F-49 compaction tracking removed: bridges no longer
		// emit a dedicated compaction event; runtime is a pure
		// pass-through. The per-cycle counter / Footer 🗜 N
		// segment is dropped across the runtime. The pre-F-49
		// `case agent.EventAgentCompaction: ... return` block was
		// removed entirely; no per-event short-circuit remains.

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
			// channel (EventAgentDone, EventAgentCompaction (F-49 deleted), thread-only
			// kinds, etc.). The runtime no longer folds usage
			// anywhere — Agent is a passive pass-through,
			// and the channel-side footer reads ctx.Usage directly
			// from OutboundMessage on the OK path below. Done.
			// Usage, if any, dies with the dropped event (channel
			// never sees it). See docs/feat/F-45-session-footer.md
			// §1.5 / §1.6 for the per-turn snapshot contract.
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
		// stays empty for orphan events (EventAgentReady at startup,
		// internal logs) — Channel renders those as plain text.
		out.ReplyTo = userMsgID
		// F-45 §2.5 改动 C: stamp StatusBar snapshot on the
		// four main-chat Kinds. Other Kinds (thread-only,
		// lifecycle, init/usage payloads themselves) skip the
		// footer — they don't surface in the main chat timeline
		// and would only inflate payload size.
		//
		// F-48 follow-up: MessageState events are stamped at
		// the wireRuntimeCallbacksAndRestore wrapper (not here)
		// — they don't flow through gateway.Translate + this
		// handler, so the case below would never fire.
		switch out.Kind {
		case messages.OutReply, messages.OutResult,
			messages.OutTaskCreate, messages.OutTaskUpdate:
			statusbar.StampFromAS(&out, s, sbDeps)
		}
		// F-think §3.1.2: per-chat OutThinking gate. When the
		// chat has /think off, drop OutThinking events here
		// (after Translate + ReplyTo stamping, before em.Send)
		// so the Feishu adapter never sees them. Other
		// OutboundKinds — OutReply / OutResult / OutToolStart /
		// OutToolEnd / OutInit / OutUsage — are unaffected.
		// (F-49: OutCompaction deleted — the runtime consumed
		// EventAgentCompaction directly via AgentSession.RecordCompaction()
		// pre-F-49 and produced no OutboundMessage, so the Channel never saw
		// this kind at all.)
		//
		// cs is captured in the closure (per-cs handler
		// factory), so this lookup is a direct field read —
		// no mgr.Get round-trip, no map probe, no second RLock.
		if out.Kind == messages.OutThinking && cs != nil && cs.ThinkMode() == chatsession.ThinkModeHide {
			if logger != nil {
				// Info level (not Debug): operators running
				// with default log level must see drops, or
				// /think off silently swallows events. Matches
				// the F-watch drop convention at
				// gateway.go:362 (log.Printf).
				logger.Info("think dropped",
					"chat_id", chatID,
					"user_msg_id", userMsgID,
					"agent_session_id", s.ID)
			}
			return
		}
		// F-38 §3.1.3: per-chat tool-event gate. When the chat
		// has /tools off (default), drop OutToolStart and
		// OutToolEnd events here (after Translate + ReplyTo
		// stamping, before em.Send) so the Feishu adapter never
		// sees them. Other OutboundKinds — OutReply / OutResult
		// / OutThinking / OutInit / OutUsage — are unaffected.
		// (F-49: OutCompaction deleted — see note above in the
		// /think-off block.) The merge rendering (PATCH on start
		// message_id when /tools on) is a Feishu adapter
		// concern; this gate just decides whether the event
		// reaches the Channel at all.
		//
		// cs is captured in the closure (per-cs handler
		// factory), so this lookup is a direct field read —
		// same pattern as the ThinkMode gate above.
		if (out.Kind == messages.OutToolStart || out.Kind == messages.OutToolEnd) &&
			cs != nil && cs.ToolsMode() == chatsession.ToolsModeHide {
			if logger != nil {
				logger.Info("tools dropped",
					"chat_id", chatID,
					"user_msg_id", userMsgID,
					"agent_session_id", s.ID,
					"kind", out.Kind.String())
			}
			return
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