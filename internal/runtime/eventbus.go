// eventbus.go — per-ChatSession event-bus subscription
// installation + state restore.
//
// WireRuntimeCallbacksAndRestore is the public helper the
// orchestrator (runtime.go's runDaemon) calls. It is exported
// because the cmd/nightme tests pin the F-38 silent-failure
// contract (handler installation must precede RestoreFromRegistry).
//

package runtime

import (
	"context"
	"log/slog"

	"github.com/cnlangzi/nightme/internal/agentsession"
	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/gateway/outbound"
	"github.com/cnlangzi/nightme/internal/messages"
)



// WireRuntimeCallbacksAndRestore installs the per-ChatSession
// outbound handlers (EventHandler for AgentEvent → OutboundMessage
// translation; MessageStateBus subscriber for F-31 lifecycle reactions)
// via Manager.WithOnCreate, then restores persisted ChatSessions
// from disk. The two calls MUST happen in this order —
// RestoreFromRegistry fires onCreate for every restored
// ChatSession, so any callback registered after RestoreFromRegistry
// silently misses every restored chat (outgoing events become
// invisible: no logs, no channel.Send, no reactions).
//
// Bundling both calls in one helper makes the order impossible
// to get wrong at the call site. See cmd/nightme/run_test.go
// for the regression coverage that pins this contract.
//
// Bug history: F-think (commit 5725a90) introduced the
// WithOnCreate wiring but called it AFTER RestoreFromRegistry —
// the silent failure went unnoticed because MessageState
// reactions are subtle (⏳/🔄/✅/❌ only). F-38 (which flipped
// the default ToolsMode to Hide, deepening the handler's
// runtime dependency on being actually installed) surfaced the
// bug when the user restarted between F-38 implementation and
// first interaction. Manager-level contract is covered in
// chatsession/manager_test.go; this helper's test covers the
// cmd/nightme/run.go wiring specifically.
//
// ch carries the channel whose OnPromptEnded method the
// PromptEndBus subscriber calls when ChatSession.endPrompt
// fires (EventAgentDone / EventAgentError in the readpump).
// Phase 2.1 moved the implementation onto channel.Channel —
// Feishu transitions the receipt card to PromptDone + adds
// the ✅ reaction; echo is a no-op. We pass the Channel
// (rather than its OnPromptEnded method value) so a single
// per-cs subscriber closure can call ch.OnPromptEnded for
// every PromptEndedEvent without rebuilding the method
// value on every event.
func WireRuntimeCallbacksAndRestore(
	mgr *chatsession.Manager,
	em outbound.Emitter,
	logger *slog.Logger,
	sbDeps chatsession.GitStatusDeps,
	ch channel.Channel,
) error {
	mgr.WithOnCreate(func(cs *chatsession.ChatSession) {
		// Startup audit trail: one line per chat, bounded by the
		// number of persisted chats — confirms the outbound
		// wiring (AgentEventBus / MessageStateBus / PromptEndBus /
		// PumpEvents) is actually installed for every
		// restored-or-new ChatSession. See the bug history note on
		// WireRuntimeCallbacksAndRestore above: a missing/misordered
		// handler here is a silent failure (no logs, no
		// channel.Send, no reactions), so this line is the cheapest
		// signal that wiring succeeded.
		//
		// Debug-level (not Info) so a daemon with hundreds of
		// persisted chats doesn't flood the log at startup; use
		// `Logging.Level: debug` to surface the audit trail when
		// investigating handler-installation regressions.
		if logger != nil {
			logger.Debug("runtime: handlers installed for chat",
				"chat_id", cs.ChatID,
				"cs_id", cs.ID)
		}

		// F-54: subscribe to the per-ChatSession event buses.
		// Each handler is a typed lambda on the corresponding
		// envelope; the Bus fires them in registration order with
		// panic isolation. Multiple subscribers may coexist; we
		// register exactly one per bus here. New subscribers
		// (audit, metrics, HUD) can register alongside.

		// AgentEventBus — translates bridge AgentEvent to
		// OutboundMessage and dispatches via Channel.Send. The
		// per-cs handler closure is built ONCE here (not inside
		// the Subscribe callback) — newEventHandler itself
		// allocates a closure, so calling it on every Publish
		// would burn one allocation per event.
		agentHandler := NewEventHandler(em, cs, mgr, logger, sbDeps)
		cs.AgentEventBus.Subscribe(func(env chatsession.AgentEventEnvelope) bool {
			agentHandler(env)
			return false
		})

		// F-48: wrap the gateway's OnMessageState so the runtime
		// can stamp StatusBar on MessageSubmitted (the
		// Feishu placeholder card needs the footer). The gateway's
		// bare OnMessageState doesn't accept StatusBar — the
		// runtime is the right owner of the stamp because it has
		// access to the AgentSession via cs.SelectedAgentSession().
		// We bypass gwImpl.OnMessageState entirely here: the
		// runtime wrapper does the same thing (validate, build
		// OutboundMessage, send) plus the stamp on MessageSubmitted.
		// Other states (MessageQueued / MessageDropped) don't need
		// the stamp — they don't create UI.
		//
		// F-54 review note: this handler routes via `em` (the single
		// Emitter passed into WireRuntimeCallbacksAndRestore). Today
		// there is one Channel in production, so the difference is
		// observable only in tests that wire multiple channels via
		// gateway.Bind/chatToChan. For multi-channel deployments,
		// route via a per-chatID Channel lookup in front of `em`
		// (the wrap currently does the type conversion; a multi-channel
		// variant would resolve the underlying channel.Channel first
		// and wrap-emit it). The AgentEventBus and PromptEndBus
		// handlers above have the same latent gap; the same one-line
		// fix applies to both.
		cs.MessageStateBus.Subscribe(func(e chatsession.MessageStateEvent) bool {
			// Review fix: replicate the gateway's identifier
			// validation. Empty chatID / userMsgID would produce
			// an OutboundMessage with empty routing fields, which
			// the Feishu adapter rejects ("feishu: OutMessageState
			// missing MessageID") and which previously was a
			// silent no-op via gwImpl.OnMessageState's early return.
			if e.ChatID == "" || e.UserMsgID == "" {
				return false
			}
			out := messages.OutboundMessage{
				Kind:    messages.OutMessageState,
				ChatID:  e.ChatID,
				ReplyTo: e.UserMsgID,
				MessageState: &messages.MessageStatePayload{
					State:     e.State,
					MessageID: e.UserMsgID,
				},
			}
			// fix-placehold-card: read the AS identity off the
			// chat's selectedAS at publish time and stamp it
			// onto the OutboundMessage so the Feishu adapter
			// can render AgentBar on the placeholder card from
			// the very first MessageQueued emit.
			//
			// Why this works (and why we don't need to
			// denormalize fields onto MessageStateEvent): the
			// dispatcher now resolves the AS BEFORE emitting
			// MessageQueued / MessageSubmitted (see
			// internal/runtime/dispatcher.go and
			// internal/chatsession/manager.go::HandleInbound),
			// so cs.selectedAS is set by the time this
			// subscriber runs. The Feishu adapter then calls
			// statusbar.StatusBarLines(&msg) (instead of the pre-fix
			// `nil`) and AgentBar renders on the first frame.
			//
			// Empty fields are safe: statusbar.StatusBarLines
			// treats the all-empty case as "no AgentBar line"
			// (back-compat with framework slash / shell
			// dispatches that still emit via PublishMessageState
			// without a resolved AS).
			//
			// Concurrency (cs-side): Publish runs synchronously
			// inside EmitMessageState without cs.mu held, so the
			// SelectedAgentSession() read below is race-free —
			// it takes cs.mu.RLock internally and releases
			// before we dereference as.
			//
			// Concurrency (AS-side): the field reads as.Agent /
			// as.Cwd / as.SessionID() are NOT lock-protected
			// here. They are safe today because the fields are
			// set once at construction (NewAgentSession) and
			// never mutated; as.SessionID() takes asMu.RLock
			// internally and is also safe. If a future refactor
			// introduces a SetCwd or SetAgent on AgentSession
			// (e.g. for /use across running ASes), add explicit
			// asMu.RLock around the field reads here — the
			// pre-fix assumption won't carry over.
			//
			// F-54 review note: this handler routes via `em`
			// (the single Emitter passed into
			// WireRuntimeCallbacksAndRestore). Today there is
			// one Channel in production, so the difference is
			// observable only in tests that wire multiple
			// channels via gateway.Bind/chatToChan. For
			// multi-channel deployments, route via a
			// per-chatID Channel lookup in front of `em` (the
			// wrap currently does the type conversion; a
			// multi-channel variant would resolve the underlying
			// channel.Channel first and wrap-emit it).
			if as := cs.LookupAS(e.AgentSessionID); as != nil {
				out.AgentName = as.Agent
				out.Workspace = as.Cwd
				out.SessionID = as.SessionID()
			} else if as := cs.SelectedAgentSession(); as != nil {
				// Fallback for legacy events without AgentSessionID.
				out.AgentName = as.Agent
				out.Workspace = as.Cwd
				out.SessionID = as.SessionID()
			}
			if err := em.Send(context.Background(), out); err != nil {
				logger.Warn("runtime: MessageState send failed",
					"chat_id", e.ChatID,
					"state", e.State,
					"err", err)
			}
			return false
		})

		// F-53 follow-up: when ChatSession.endPrompt fires
		// (EventAgentDone / EventAgentError in the readpump), route the
		// terminal event to the Feishu adapter so the receipt
		// card transitions to PromptDone and the ✅ reaction
		// is added on the card. No user-message reaction is
		// emitted from this path — the user-message surface is
		// now minimal (⏳ only).
		cs.PromptEndBus.Subscribe(func(e agentsession.PromptEndedEvent) bool {
			if e.ChatID == "" || e.UserMsgID == "" {
				return false
			}
			// The adapter call is fire-and-forget: failures are
			// logged inside SetPromptState. We use
			// context.Background() because the readpump-driven
			// endPrompt happens off the inbound message path;
			// there's no inbound ctx to chain. The runtime
			// injects the Feishu-specific implementation; for
			// non-Feishu channels the callback is a no-op.
			ch.OnPromptEnded(context.Background(), e.ChatID, e.UserMsgID)
			return false
		})

		// CS-AS 边界重构 Phase 1: launch the per-chat pumpEvents
		// goroutine. The pump drains cs.ActiveEvents() and dispatches
		// each EnrichedEvent by Kind:
		//   - KindAgentEvent   → cs.AgentEventBus (subscriber above)
		//   - KindPromptEnded  → writebackMessageState (built into
		//                        cs.PumpEvents — publishes on PromptEndBus)
		//   - KindLifecycle    → log + flip AgentSession.SetExited (in pump_events)
		// Replaces the per-CS StartReadPump / readRunPump that the
		// pre-Phase-1 readpump.go file used to install. The new model
		// has readpump per-AS (started by Spawn), so the chat layer
		// only consumes the enriched event stream.
		//
		// The goroutine ends when cs.ActiveEvents() returns !ok,
		// which happens when the active AS is Shutdown (for /use this
		// happens at daemon exit; for /kill, immediately).
		go cs.PumpEvents(context.Background())
	})
	return nil // lazy restore via GetOrCreate → Bootstrap (docs/CHATSTORE.md)
}