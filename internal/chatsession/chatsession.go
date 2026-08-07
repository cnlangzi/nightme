// Package chatsession — ChatSession (v1.2 per-chat session context).
//
// ChatSession owns the persistent per-chat state and the pool of
// AgentSessions. It replaces v1.1's Session + Gateway BindingEntry
// pair with a single coherent structure.
//
// See docs/SPEC.md v1.2 §1.1 and docs/feat/F-27-chatsession.md.
package chatsession

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/registry"
)

// ChatSession is the persistent per-chat session context.
//
// One ChatSession is bound 1:1 to an IM chat (chatID), enforced by
// the UNIQUE constraint on registry.ChatSessionEntry.ChatID. Each
// ChatSession owns a pool of AgentSessions keyed by (agent, cwd);
// the active one (or nil) is tracked in activeAS.
//
// Concurrency: state fields (activeCwd, activeAgent, primaryAgent,
// pool, activeAS) are guarded by mu. Reads take RLock; writes take
// Lock. /use / /cwd take RLock for the mutation + Lock for the
// pool mutation when an AgentSession is added.
type ChatSession struct {
	ID     string
	ChatID string

	mu sync.RWMutex

	// Active routing state. activeAgent is mutable via /use;
	// activeCwd via /cwd. primaryAgent is the cfg.Primary snapshot
	// at ChatSession construction; read-only post-construction
	// (Q-A: no /default command, no per-chat override).
	//
	// At New() time activeAgent is seeded from primaryAgent so the
	// runtime never sees an empty activeAgent on a fresh chat.
	activeCwd    string
	activeAgent  string
	primaryAgent string

	// F-watch §3.1.1: per-chat message-watch mode. Default
	// WatchModeMention (only @ bot or @_all messages are
	// processed); /watch on switches to WatchModeAll. See
	// docs/SPEC.md §3.1.1 + docs/channel/feishu.md §6.10/§6.11.
	// DM chats always behave as if WatchMode==WatchModeAll
	// (every DM message is "addressed to bot"); the gate logic in
	// gateway.Handle enforces that — this field is only consulted
	// for group chats via the channel-supplied HasMention bool.
	watchMode WatchMode

	// F-think §3.1.2: per-chat thinking-content visibility. Default
	// ThinkModeShow (runtime forwards OutThinking to the Channel,
	// which renders it as a lark_md card in the user-message
	// thread); /think off switches to ThinkModeHide (runtime
	// drops OutThinking at the EventHandler gate). Unlike
	// WatchMode this is chat-type-independent — DMs and group
	// chats behave identically. See docs/SPEC.md §3.1.2.
	thinkMode ThinkMode

	// F-38 §3.1.3: per-chat tool-event visibility. Default
	// ToolsModeHide (runtime drops OutToolStart and OutToolEnd at
	// the EventHandler gate — tool spam is the loudest part of
	// the agent progress stream and most users do not want it by
	// default); /tools on switches to ToolsModeShow (Feishu
	// adapter merges each pair into a single thread reply via
	// PATCH on the same message_id). Like ThinkMode and unlike
	// WatchMode, this is chat-type-independent — DMs and group
	// chats behave identically. See docs/SPEC.md §3.1.3. The
	// default direction is OPPOSITE of ThinkMode's: ThinkMode's
	// zero value is Show (preserve existing F-thread-route UX);
	// ToolsMode's zero value is Hide (quiet by default; opt in).
	toolsMode agent.ToolsMode

	// Pool of AgentSessions keyed by (agent, cwd).
	pool map[agentCwdKey]*AgentSession

	// Currently active AgentSession (pointer into pool). nil means
	// no active session (ChatSession exists but no /cwd + /use yet).
	activeAS *AgentSession

	// Timestamps.
	createdAt        time.Time
	lastInteractionAt time.Time

	// Persistence handles (optional — nil means no persistence).
	csFile *registry.ChatSessionFile
	asFile *registry.AgentSessionFile

	// spawner is used by LookupActiveAgentSession to fork new
	// children on miss. nil means no spawn (test-friendly default;
	// production wires a registrySpawner at runtime).
	spawner Spawner

	// inputBuffer is the per-ChatSession FSM that queues user
	// messages while the active AgentSession is Busy. Lazily
	// created via ensureBuffer() so tests that don't dispatch
	// messages don't pay for it.
	inputBuffer *InputBuffer

	// commit 8c: per-ChatSession readPump controller. Only one
	// pump is active at a time (the active AgentSession's pump);
	// /use swaps the pump by StopReadPump + StartReadPump.
	pumpMu      sync.Mutex
	pump        EventPumpState
	pumpRunning atomic.Bool // true while a pump goroutine is alive

	// eventHandler is the runtime-installed EventHandler invoked
	// for each event drained from the active AgentSession. Set
	// once at startup (or first dispatch); persists across /use.
	eventHandler EventHandler

	// onMessageState is the runtime-installed callback fired when
	// this ChatSession's message lifecycle advances (F-31). Set
	// once at startup. Reads from currentTurnUserMsgID (mutated
	// by FlushHook) so EventDone/Error can emit the terminal
	// MessageState for the anchor user message.
	//
	// nil = no observer; emitMessageState becomes a no-op.
	onMessageState func(chatID, userMsgID string, state agent.MessageState)

	// currentTurnUserMsgID is the single anchor for the in-flight
	// (or just-completed) agent turn. Updated by
	// defaultFlushHookLocked when InputBuffer flushes; consumed
	// by the outbound EventHandler to stamp
	// OutboundMessage.ReplyTo, and by runReadPump on
	// EventDone/Error to emit MessageState(Done/Error) for
	// the anchor user message. Empty when no turn is in flight.
	//
	// v1.3 (SPEC §0.1): single userMsgID per turn (was a slice
	// in v1.2/F-31). Buffered batch flushes anchor to the LAST
	// userMsgID in the batch (one card / thread / DOM node per
	// turn; the other user messages in the batch still receive
	// their own MessageState fan-out per design choice).
	currentTurnUserMsgID string

	// exitObserver is the runtime-installed callback fired when
	// an active AgentSession's process exits. nil = no observer.
	exitObserver AgentExitObserver

	// ctx is the per-ChatSession context. Lives for the chat's
	// lifetime (until daemon shutdown). It is the PARENT context
	// every AgentSession active on this chat derives its own
	// per-AS ctx from. promoteActiveLocked owns that handover: on
	// every change of active AS it Background()s the outgoing one
	// (cancelling its derived ctx) and Activate(cs.ctx)s the
	// incoming one. Cancelling cs.ctx itself cascades through every
	// active AS (used by the runtime during graceful shutdown).
	//
	// Per-AS lifecycle control lives on AgentSession, not on
	// ChatSession — the chat holds the parent, the AS owns the
	// derived child. ChatSession does not need its own AS-level
	// or turn-level ctx fields.
	//
	// Guarded by ctxMu; reads via Context(), writes via
	// ResetContext().
	ctx    context.Context
	cancel context.CancelFunc
	ctxMu  sync.Mutex

	// --- F-45 teamflow (gtw) state ------------------------------
	//
	// REMOVED in F-51: gtwContext, gtwDrafts, and the
	// onReaction reaction handler all moved to
	// gtw.Manager (see internal/command/gtw/manager.go).
	// chatsession is no longer aware of gtw — it has no
	// GTWContext, no GTWDraft, no ReactionEvent type, no
	// SetActionHandler / HandleAction methods.
}

// New creates a fresh ChatSession in memory. The caller is
// responsible for persisting via Persist().
//
// primaryAgent is the cfg.Primary snapshot at creation time. It
// seeds activeAgent so the runtime always has an effective agent
// to dispatch to (no runtime fallback: the lookup only ever reads
// activeAgent). The snapshot itself is read-only post-construction
// (Q-A: no /default command, no per-chat override).
func New(chatID, primaryAgent string) *ChatSession {
	cs := &ChatSession{
		ID:               deriveIDFromChatID(chatID),
		ChatID:           chatID,
		activeAgent:      primaryAgent, // init seed
		primaryAgent:     primaryAgent, // historical snapshot, read-only
		pool:             make(map[agentCwdKey]*AgentSession),
		watchMode:        WatchModeMention, // F-watch default
		thinkMode:        ThinkModeShow,    // F-think default
		toolsMode:        agent.ToolsModeHide, // F-38 default (quiet by default)
		createdAt:        time.Now(),
		lastInteractionAt: time.Now(),
	}
	cs.ctx, cs.cancel = context.WithCancel(context.Background())
	return cs
}

// WithPersistence attaches registry stores. Both can be nil (no
// persistence); typically both are non-nil in production.
func (cs *ChatSession) WithPersistence(csFile *registry.ChatSessionFile, asFile *registry.AgentSessionFile) *ChatSession {
	cs.mu.Lock()
	cs.csFile = csFile
	cs.asFile = asFile
	cs.mu.Unlock()
	return cs
}

// WithSpawner attaches a Spawner used by LookupActiveAgentSession
// to fork child processes. nil means no spawn (lookup returns
// AgentSession with status=Detached, no process running).
func (cs *ChatSession) WithSpawner(spawner Spawner) *ChatSession {
	cs.mu.Lock()
	cs.spawner = spawner
	cs.mu.Unlock()
	return cs
}

// Spawner returns the configured Spawner (nil if none).
func (cs *ChatSession) Spawner() Spawner {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.spawner
}

// deriveIDFromChatID produces a deterministic ID from the chat ID
// for the 1:1 invariant. Real implementation should use a hash; for
// commit 6 we use a plain prefix to keep things readable.
func deriveIDFromChatID(chatID string) string {
	if chatID == "" {
		return ""
	}
	return "cs_" + chatID
}

// ErrNoActiveCwd is returned by LookupActiveAgentSession when the
// ChatSession has no activeCwd set (user has not /cwd'd yet).
var ErrNoActiveCwd = errors.New("chatsession: no active workspace (send /cwd first)")

// ErrUnknownAgent indicates the requested agent is not registered.
// (Validation should happen at the gateway layer; this is a safety net.)
var ErrUnknownAgent = errors.New("chatsession: unknown agent")

// killGraceTotal is the overall timeout for graceful shutdown of all
// running AgentSessions in the pool. Each bridge has its own 2s graceful
// window + SIGKILL fallback; this 5s budget is the outer bound covering
// (a) bridge internal grace + SIGKILL, (b) race detector margin, and
// (c) bridges that take a beat to surface exit through ObserveClose.
// Tuned for "user waits for /kill to confirm"; rarely triggers.
const killGraceTotal = 5 * time.Second

// KillResult is one row of the /kill reply. It captures what happened
// to a single pool entry during KillAll so the handler can render a
// per-agent status instead of a bare count.
//
// See docs/feat/F-43-kill-new-graceful-and-reset.md §4.3.
type KillResult struct {
	Agent       string // e.g. "claude", "codex"
	Cwd         string // e.g. "/code/A"
	BeforeState Status // StatusRunning / StatusDetached / StatusExited
	Action      string // "killed" | "stale-cleared"
	Error       error  // nil on success
}

// ResetResult is one row of the /new reply. It captures what happened
// to a single pool entry during NewActiveAgentSessions so the handler
// can render a per-agent status instead of a bare count.
//
// See docs/feat/F-43-kill-new-graceful-and-reset.md §5.2.
type ResetResult struct {
	Agent       string // e.g. "claude", "codex"
	Cwd         string // e.g. "/code/A"
	BeforeState Status // StatusRunning / StatusDetached / StatusExited
	Action      string // "in-place-reset" | "marked-fresh"
	Error       error  // nil on success

	// Session is the underlying AgentSession — populated so the
	// caller (handleNew in F-45) can perform per-row follow-up
	// actions such as ResetCumulative + PersistAgentSession
	// without re-walking the pool. nil only when targets were
	// empty (matched == 0); always set otherwise.
	Session *AgentSession
}

// SetActiveCwd changes the active workspace. Does NOT spawn or kill
// any AgentSession; the pool is preserved. Next message triggers
// LookupActiveAgentSession which may spawn or reuse.
func (cs *ChatSession) SetActiveCwd(cwd string) error {
	if cwd == "" {
		return errors.New("chatsession: empty cwd")
	}
	cs.mu.Lock()
	cs.activeCwd = cwd
	cs.lastInteractionAt = time.Now()
	cs.mu.Unlock()
	cs.persistChatEntry()
	return nil
}

// SetWatchMode changes the per-chat message-watch mode. Persists to
// registry on success so it survives daemon restart. No spawn /
// kill / event side-effects — purely a routing-state mutation.
//
// F-watch §3.1.1: a no-op in DM chats at the gateway layer (DM
// messages always pass HasMention=true), but the mode value is
// still written to keep /watch semantics consistent across chat
// types — switching from group to DM and back preserves the
// user's last-set preference.
func (cs *ChatSession) SetWatchMode(mode WatchMode) error {
	cs.mu.Lock()
	cs.watchMode = mode
	cs.lastInteractionAt = time.Now()
	cs.mu.Unlock()
	cs.persistChatEntry()
	return nil
}

// SetThinkMode changes the per-chat thinking-content visibility.
// Persists to registry on success so /think off survives daemon
// restart. No spawn / kill / re-dispatch side effects.
//
// State semantics:
//   - ThinkModeShow — runtime forwards every OutThinking event to
//     the Channel (rendered as a lark_md card in the user-message
//     thread; see internal/channel/feishu/thinking_card.go).
//   - ThinkModeHide — runtime drops OutThinking events at the
//     EventHandler gate (after Translate + ReplyTo stamping,
//     before ch.Send). Other OutboundKinds are unaffected.
//
// Concurrency: same pattern as SetWatchMode — take ChatSession
// mutex, write, persist, release. The lock is NOT held across
// any channel.Send reply call.
func (cs *ChatSession) SetThinkMode(mode ThinkMode) error {
	cs.mu.Lock()
	cs.thinkMode = mode
	cs.lastInteractionAt = time.Now()
	cs.mu.Unlock()
	cs.persistChatEntry()
	return nil
}

// SetActiveAgent changes the active agent name. Does NOT spawn or
// kill; caller must invoke LookupActiveAgentSession to materialize.
func (cs *ChatSession) SetActiveAgent(agent string) error {
	if agent == "" {
		return errors.New("chatsession: empty agent")
	}
	cs.mu.Lock()
	cs.activeAgent = agent
	// /use switches the AgentSession; the previous turn's anchor
	// must NOT survive or the new AS's events would be stamped
	// with the OLD userMsgID (channel routes them to the old
	// receipt card). Clear under the same lock as activeAgent
	// write so the two are atomic relative to readPump reads.
	cs.currentTurnUserMsgID = ""
	cs.lastInteractionAt = time.Now()
	cs.mu.Unlock()
	cs.persistChatEntry()
	return nil
}

// ActiveCwd returns the current active workspace.
func (cs *ChatSession) ActiveCwd() string {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.activeCwd
}

// ActiveAgent returns the current active agent name.
func (cs *ChatSession) ActiveAgent() string {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.activeAgent
}

// WatchMode returns the current per-chat message-watch mode.
// Default value when never set is WatchModeMention (set in New
// when the registry has no persisted value). See
// docs/SPEC.md §3.1.1 + docs/channel/feishu.md §6.11.
func (cs *ChatSession) WatchMode() WatchMode {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.watchMode
}

// ThinkMode returns the current per-chat thinking-content
// visibility. Default value when never set is ThinkModeShow
// (set in New when the registry has no persisted value). See
// docs/SPEC.md §3.1.2.
func (cs *ChatSession) ThinkMode() ThinkMode {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.thinkMode
}

// SetToolsMode changes the per-chat tool-event visibility.
// Persists to registry on success so /tools on survives daemon
// restart. No spawn / kill / re-dispatch side effects.
//
// State semantics:
//   - ToolsModeShow — runtime forwards OutToolStart / OutToolEnd
//     to the Channel. Feishu adapter merges each pair into a
//     single thread reply via PATCH on the start message_id (see
//     internal/channel/feishu/tool_thread_merge.go).
//   - ToolsModeHide — runtime drops OutToolStart and OutToolEnd
//     at the EventHandler gate (after Translate + ReplyTo
//     stamping, before ch.Send). Other OutboundKinds are
//     unaffected.
//
// Concurrency: same pattern as SetWatchMode / SetThinkMode — take
// ChatSession mutex, write, persist, release. The lock is NOT
// held across any channel.Send reply call.
func (cs *ChatSession) SetToolsMode(mode agent.ToolsMode) error {
	cs.mu.Lock()
	cs.toolsMode = mode
	cs.lastInteractionAt = time.Now()
	cs.mu.Unlock()
	cs.persistChatEntry()
	return nil
}

// ToolsMode returns the current per-chat tool-event visibility.
// Default value when never set is ToolsModeHide (set in New when
// the registry has no persisted value). Direction is OPPOSITE of
// ThinkMode's default — see internal/agent/tools_mode.go doc
// for the rationale. See docs/SPEC.md §3.1.3.
func (cs *ChatSession) ToolsMode() agent.ToolsMode {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.toolsMode
}

// PrimaryAgent returns the per-chat primary agent (snapshot of
// cfg.Primary at ChatSession creation). v1.2 (Q-A) does not
// allow post-creation mutation; the field is read-only. The
// activeAgent is seeded from this value at construction; /use
// overrides activeAgent but does NOT mutate primaryAgent.
func (cs *ChatSession) PrimaryAgent() string {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.primaryAgent
}

// Pool returns a snapshot of all AgentSessions in the pool.
func (cs *ChatSession) Pool() []*AgentSession {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	out := make([]*AgentSession, 0, len(cs.pool))
	for _, as := range cs.pool {
		out = append(out, as)
	}
	return out
}

// ActiveAgentSession returns the current active AgentSession (or nil).
func (cs *ChatSession) ActiveAgentSession() *AgentSession {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.activeAS
}

// --- InputBuffer FSM (commit 9) ----------------------------------------

// ensureBuffer lazily creates the InputBuffer on first use. Called
// from QueueUserMessage / SetBusy / SetIdle / OnTurnEnded so tests
// that don't dispatch messages don't allocate the FSM.
//
// Construction installs a default FlushHook that sends queued
// blocks to cs.activeAS (current active AgentSession). The runtime
// can override via SetFlushHook if it needs receipts or other
// side effects.
//
// commit 9+ fix: without a hook, QueueUserMessage on an Idle
// buffer silently drops the message (InputBuffer.Add returns nil
// without forwarding). The default hook closes that gap: any
// ChatSession with an active AgentSession will route user messages
// to the agent.
func (cs *ChatSession) ensureBuffer() *InputBuffer {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.inputBuffer == nil {
		cs.inputBuffer = NewInputBuffer(cs.defaultFlushHookLocked(), 50, 100*1024)
	}
	return cs.inputBuffer
}

// defaultFlushHookLocked returns the built-in FlushHook that
// forwards user blocks to the current active AgentSession. Caller
// must hold cs.mu (Lock).
//
// v1.3 (SPEC §2.2): captures the LAST userMsgID into
// currentTurnUserMsgID — the single anchor for the entire turn.
// All outbound events from this turn carry ReplyTo=anchor; Channel
// PATCHes the same receipt card / thread / DOM node. Earlier
// userMsgIDs in the buffered batch are still tracked separately
// for MessageState fan-out (see emitMessageStateForCurrentTurn —
// that one still iterates the full batch for honest per-message
// progress feedback).
func (cs *ChatSession) defaultFlushHookLocked() FlushHook {
	return func(combined []agent.ContentBlock, userMsgIDs []string) error {
		// Anchor = last userMsgID in the batch. A 1-message
		// turn anchors to itself; a buffered batch anchors to
		// the most recent user message (matches ChatGPT-style
		// "submit all → reply on last" UX).
		//
		// IMPORTANT: the closure body runs WITHOUT cs.mu held
		// (InputBuffer.OnTurnEnded releases its b.mu before
		// invoking the hook). We must acquire cs.mu here to
		// synchronize with the read side in runReadPump. Writing
		// without the lock is a data race; the race detector
		// catches it under buffered-batch + concurrent event
		// drain.
		//
		// The ctx we hand to SendBlocks is the AS-owned per-AS
		// ctx (as.OpContext()), not a turn-scoped child — the
		// bridge's SendBlocks derives its own per-call ctx
		// (callCtx) from whatever we pass, so the per-turn
		// boundary is now owned entirely by the bridge layer.
		//
		// Race note (F-32 2026-08-06 follow-up): a concurrent
		// active-AS switch (e.g. /use) runs promoteActiveLocked,
		// which Background()s the outgoing AS between our read of
		// activeAS/OpContext and the SendBlocks call. That
		// cancels the ctx mid-send and SendBlocks returns
		// context.Canceled — the message would otherwise be
		// silently dropped (runReadPump discards OnTurnEnded's
		// error). Surface a structured error so the operator can
		// distinguish "lost on /use" from a transport failure.
		var as *AgentSession
		if n := len(userMsgIDs); n > 0 {
			cs.mu.Lock()
			cs.currentTurnUserMsgID = userMsgIDs[n-1]
			as = cs.activeAS
			cs.mu.Unlock()
		} else {
			cs.mu.RLock()
			as = cs.activeAS
			cs.mu.RUnlock()
		}
		if as == nil || as.Handle() == nil {
			return ErrNotRunning
		}
		err := as.SendBlocks(as.OpContext(), combined)
		if errors.Is(err, context.Canceled) {
			return fmt.Errorf("flush: AS backgrounded during send (likely /use): %w", err)
		}
		return err
	}
}

// Context returns the per-ChatSession ctx. Every AgentSession
// active on this chat derives its own per-AS ctx from
// Context() via AgentSession.Activate(parent). The runtime reads
// Context() when wiring up a freshly-activated AS; consumers
// (InputBuffer FlushHook, runtime EventHandler) don't reach for
// it — they go through AgentSession.OpContext() instead.
//
// To tear down every operation on this chat at once (graceful
// shutdown path), call ResetContext(). All active ASes'
// derived ctxs cascade.
//
// Safe to call concurrently; the underlying context is replaced
// atomically under ctxMu.
func (cs *ChatSession) Context() context.Context {
	cs.ctxMu.Lock()
	defer cs.ctxMu.Unlock()
	return cs.ctx
}

// ResetContext cancels the per-ChatSession ctx and installs a
// fresh one derived from context.Background(). Reserved for the
// runtime's graceful-shutdown path — an active-AS switch does NOT
// use this; per-AS teardown belongs to promoteActiveLocked.
//
// After ResetContext, every AgentSession whose opCtx was derived
// from the previous cs.ctx has its operations cancelled
// (cascade). The fresh cs.ctx is what the next promotion's
// Activate() call will derive from.
//
// Always installs a fresh ctx; idempotent in spirit.
func (cs *ChatSession) ResetContext() {
	cs.ctxMu.Lock()
	defer cs.ctxMu.Unlock()
	if cs.cancel != nil {
		cs.cancel()
	}
	cs.ctx, cs.cancel = context.WithCancel(context.Background())
}

// QueueUserMessage enqueues a structured user turn. Idle: flush
// immediately via the hook. Busy: queue. Behavior mirrors v1.1's
// InputBuffer.Add but is owned by ChatSession.
func (cs *ChatSession) QueueUserMessage(blocks []agent.ContentBlock, userMsgID string) error {
	return cs.ensureBuffer().Add(blocks, userMsgID)
}

// SetBusy marks the FSM as busy (agent is processing a turn).
// Called by the runtime event pump on non-terminal events.
func (cs *ChatSession) SetBusy() {
	cs.ensureBuffer().SetState(StateBusy)
}

// SetIdle marks the FSM as idle and flushes queued messages
// (typically called together by the runtime on EventDone / Error).
func (cs *ChatSession) SetIdle() {
	cs.ensureBuffer().SetState(StateIdle)
}

// OnTurnEnded flushes the buffer. Call after SetIdle() when the
// active AgentSession's turn ends.
func (cs *ChatSession) OnTurnEnded() error {
	return cs.ensureBuffer().OnTurnEnded()
}

// BufferPending returns the current queue size (0 if no
// InputBuffer yet).
func (cs *ChatSession) BufferPending() int {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if cs.inputBuffer == nil {
		return 0
	}
	return cs.inputBuffer.Pending()
}

// ClearBuffer drops every buffered message without sending them.
// Returns the number dropped. Used by handleUse when swapping the
// active AS: any messages queued for the previous (hung) AS
// should NOT auto-forward to the new AS — the user explicitly
// switched away and the queued turns belong to the abandoned
// context. After Clear, the FSM state is left untouched —
// callers that need IDLE should also call SetIdle().
func (cs *ChatSession) ClearBuffer() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.inputBuffer == nil {
		return 0
	}
	return cs.inputBuffer.Clear()
}

// BufferState returns the current FSM state (StateIdle if no
// InputBuffer yet).
func (cs *ChatSession) BufferState() SessionState {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if cs.inputBuffer == nil {
		return StateIdle
	}
	return cs.inputBuffer.State()
}

// SetFlushHook installs (or replaces) the runtime-provided flush
// hook. The hook receives (combined blocks, userMsgIDs) and is
// expected to SendBlocks on the active AgentSession.
//
// Switching hooks (e.g., on /use) is supported: the runtime calls
// SetFlushHook with a fresh closure pointing at the new active
// AgentSession; queued messages flush to the new target on the
// next OnTurnEnded.
func (cs *ChatSession) SetFlushHook(h FlushHook) {
	cs.ensureBuffer().SetFlushHook(h)
}

// SetEventHandler installs the per-event callback. The runtime
// typically installs this once at first message dispatch; the
// handler closes over (channel, ctx, etc.) and translates each
// AgentEvent to a channel.Send call.
//
// commit 8c: the handler persists across /use (we want outbound
// translation to follow the new active AgentSession naturally).
func (cs *ChatSession) SetEventHandler(h EventHandler) {
	cs.mu.Lock()
	cs.eventHandler = h
	cs.mu.Unlock()
}

// EventHandler returns the installed outbound event handler, or
// nil if none has been set. Exposed for tests that need to verify
// runtime installation (e.g. RestoreFromRegistry regression
// coverage in manager_test.go).
func (cs *ChatSession) EventHandler() EventHandler {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.eventHandler
}

// SetMessageStateHandler installs the callback fired when this
// ChatSession's message lifecycle advances (F-31). The runtime
// (cmd/nightme) wires gw.OnMessageState into every ChatSession at
// startup; ChatSession calls it on:
//
//   - StateReceived: ChatSession accepts a user message for
//     dispatch (called from dispatchMessage before spawn work).
//   - StateForwarded: message dispatched to AgentSession
//     (called from dispatchMessage after LookupActiveAgentSession
//     success).
//   - StateDone: active AgentSession emitted EventDone for the
//     messages in the just-completed turn.
//   - StateError: active AgentSession emitted EventError.
//
// nil clears the handler (emitMessageState becomes a no-op).
//
// Scope constraint: MessageState events are NOT produced for slash
// commands (/cwd /use /kill etc.); those go through different
// paths that don't reach QueueUserMessage. See F-31 §3.2.
func (cs *ChatSession) SetMessageStateHandler(h func(chatID, userMsgID string, state agent.MessageState)) {
	cs.mu.Lock()
	cs.onMessageState = h
	cs.mu.Unlock()
}

// MessageStateHandler returns the installed message-lifecycle
// callback, or nil if none has been set. Exposed for tests; see
// EventHandler() comment.
func (cs *ChatSession) MessageStateHandler() func(chatID, userMsgID string, state agent.MessageState) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.onMessageState
}

// EmitMessageState fires the onMessageState callback for a single
// userMsgID. Public entry point for external lifecycle triggers
// (e.g. dispatchMessage in cmd/nightme calling cs.EmitMessageState
// (userMsgID, StateReceived) before spawn). Internal lifecycle
// hooks call this too. No-op if no handler is installed.
//
// Caller MUST NOT hold cs.mu (handler is invoked synchronously and
// may call back into ChatSession methods).
func (cs *ChatSession) EmitMessageState(userMsgID string, state agent.MessageState) {
	cs.mu.RLock()
	h := cs.onMessageState
	chatID := cs.ChatID
	cs.mu.RUnlock()
	if h == nil {
		return
	}
	h(chatID, userMsgID, state)
}

// emitMessageStateForCurrentTurn fires onMessageState for the
// single currentTurnUserMsgID. Called from runReadPump on terminal
// agent events (EventDone/Error) so the anchor user message
// receives its final state event.
//
// v1.3 (SPEC §2.5): terminal MessageState fires for the anchor
// only. Earlier userMsgIDs in a buffered batch keep their own
// MessageState at StateForwarded until they themselves anchor a
// future turn — a deliberate UX choice to keep the per-message
// progress indicator honest. Channel rendering of forward-only
// reactions (🔄 without ✅) is acceptable for buffered-batch
// intermediate messages; if a fan-out is later preferred,
// re-introduce the slice here.
//
// Clears currentTurnUserMsgID after emission so a subsequent
// turn (e.g. OnTurnEnded flushing queued messages) starts fresh.
func (cs *ChatSession) emitMessageStateForCurrentTurn(state agent.MessageState) {
	cs.mu.Lock()
	id := cs.currentTurnUserMsgID
	cs.currentTurnUserMsgID = ""
	h := cs.onMessageState
	chatID := cs.ChatID
	cs.mu.Unlock()
	if h == nil || id == "" {
		return
	}
	h(chatID, id, state)
}

// BufferClear discards queued messages without sending. Returns
// the number cleared.
func (cs *ChatSession) BufferClear() int {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if cs.inputBuffer == nil {
		return 0
	}
	return cs.inputBuffer.Clear()
}

// LookupInPool returns the AgentSession matching (agent, cwd) if
// present in the pool (regardless of status). Returns
// ErrAgentNotFound if not in pool.
func (cs *ChatSession) LookupInPool(agent, cwd string) (*AgentSession, error) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	as, ok := cs.pool[agentCwdKey{Agent: agent, Cwd: cwd}]
	if !ok {
		return nil, ErrAgentNotFound
	}
	return as, nil
}

// promoteActiveLocked makes as the chat's active AgentSession and
// owns the per-AS context lifecycle for that promotion.
//
// Context wiring belongs here, at the single point where the active
// AgentSession changes, rather than in individual handlers. Before
// this, only /use called Activate — so any chat that never ran /use
// left every AgentSession unactivated. Two consequences:
//
//   - opCtx stayed at the constructor's Background value (or, for
//     sessions restored from disk, nil — which panicked the daemon on
//     the first message after every restart, because the default
//     FlushHook passes OpContext() straight into bridge.SendBlocks and
//     the pi bridge dereferences it on entry);
//   - Background() was a silent no-op (opCancel nil), so /use could
//     not actually interrupt an in-flight turn on the outgoing AS, and
//     ResetContext could not cascade a shutdown into it.
//
// Idempotent by design. Activate CANCELS the previous opCtx, so
// calling it on every lookup — the per-message hot path — would kill
// the running turn. We only (re)activate when the active AgentSession
// actually changes, or when it has never been activated.
//
// Caller must hold cs.mu. Takes cs.ctxMu (via Context()) and the
// target's asMu; neither is ever held while acquiring cs.mu, so the
// ordering is safe.
func (cs *ChatSession) promoteActiveLocked(as *AgentSession) {
	prev := cs.activeAS
	cs.activeAS = as
	if as == nil {
		return
	}
	if prev == as && as.IsActivated() {
		return
	}
	if prev != nil && prev != as {
		// Tear down the outgoing AS's operations before the new one
		// takes over — same contract /use has always implemented by
		// hand.
		prev.Background()
	}
	as.Activate(cs.Context())
}

// LookupActiveAgentSession resolves the active AgentSession.
//
// Single-path resolution (no runtime fallback):
//
//   - activeAgent is always non-empty for a ChatSession constructed
//     by Manager.GetOrCreate (init-time seed from cfg.Primary
//     snapshot). The runtime never needs to choose between two
//     agents at lookup time.
//   - Resolve pool[(activeAgent, activeCwd)]:
//     · hit (StatusRunning) → reuse
//     · miss (or non-Running, e.g. Detached after daemon restart,
//       or Exited after CLI died) → spawn (activeAgent, activeCwd)
//
// Returns ErrNoActiveCwd if activeCwd is empty. Returns
// ErrNoActiveAgent if activeAgent is empty (misconfigured daemon —
// cfg.Primary snapshot was empty at ChatSession creation).
func (cs *ChatSession) LookupActiveAgentSession() (*AgentSession, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if cs.activeCwd == "" {
		return nil, ErrNoActiveCwd
	}

	if cs.activeAgent == "" {
		// Misconfigured: Manager.GetOrCreate should have seeded
		// activeAgent from cfg.Primary at construction. An empty
		// primary at construction means the daemon has no global
		// default configured; the runtime cannot choose an agent.
		return nil, ErrNoActiveAgent
	}

	// commit fix-6: pool hit only returns if the entry is still
	// Running. A Detached entry (process state unknown after
	// restart) or Exited entry (CLI died) falls through to the
	// spawn path below.
	if as, ok := cs.pool[agentCwdKey{Agent: cs.activeAgent, Cwd: cs.activeCwd}]; ok && as.Status() == StatusRunning && as.Handle() != nil {
		cs.promoteActiveLocked(as)
		return as, nil
	}

	// Reuse a non-Running pool entry (Detached after daemon restart,
	// or Exited after CLI died) when one exists for this (agent,
	// cwd) tuple. The existing entry preserves identity and
	// — critically — the captured ResumeID from the prior run, so
	// the next Spawn replays `--resume <id>` to the bridge. Creating
	// a fresh entry here would discard the resume id and force a
	// brand-new agent session after every daemon restart.
	newAS, hadPrior := cs.pool[agentCwdKey{Agent: cs.activeAgent, Cwd: cs.activeCwd}]
	if !hadPrior {
		newAS = NewAgentSession(
			newAgentSessionID(),
			cs.ID,
			cs.activeAgent,
			cs.activeCwd,
			nil,
		)
		cs.pool[agentCwdKey{Agent: cs.activeAgent, Cwd: cs.activeCwd}] = newAS
	}
	// If hadPrior, the entry's ID + ResumeID + Args are preserved
	// from the prior construction or RestoreFromRegistry. Spawn
	// will fork a new process and SetRunning will clear the stale
	// exit code and flip stat back to Running.
	cs.promoteActiveLocked(newAS)
	if cs.asFile != nil {
		_ = cs.asFile.Upsert(newAS.Entry())
	}

	// commit 7: actually fork the child via the configured Spawner.
	// If no Spawner is set (test-friendly default), the
	// AgentSession stays in status=Detached with no process — the
	// caller can still see it in the pool, but SendBlocks will
	// return ErrNotRunning until a Spawner is wired in.
	if cs.spawner != nil {
		// Spawn outside of cs.mu to avoid holding the write lock
		// across a fork+exec. We re-acquire mu for the subsequent
		// persistence + activeAS assignment.
		spawner := cs.spawner
		cs.mu.Unlock()
		spawnErr := newAS.Spawn(context.Background(), spawner)
		cs.mu.Lock()

		if spawnErr != nil {
			// Spawn failed; keep the entry in the pool but mark
			// detached so the next lookup can re-attempt. Caller
			// will see an error from LookupActiveAgentSession.
			return newAS, fmt.Errorf("chatsession: spawn failed (activeAgent=%q, cwd=%q): %w", cs.activeAgent, cs.activeCwd, spawnErr)
		}
		// Refresh registry entry with updated PID/Status.
		if cs.asFile != nil {
			_ = cs.asFile.Upsert(newAS.Entry())
		}
	}

	cs.persistChatEntryLocked()

	// commit 8c: readPump is NOT auto-started here. The runtime
	// (cmd/nightme) explicitly calls cs.StartReadPump() after
	// the spawn resolves, typically from the /use handler or first
	// message dispatch. Tests that don't go through the runtime
	// are unaffected (no leak).

	return newAS, nil
}

// KillAll gracefully terminates every AgentSession in the pool via
// each bridge's Close() path, then clears the persistent registry
// entries so the next spawn won't resume the dead sessions.
//
// Per-entry outcome is returned in []KillResult so the caller can
// render a per-agent status list (`FormatKillResults` handles the
// formatting). The overall error is non-nil only for hard failures
// (e.g. registry corruption); per-entry bridge errors are captured
// in each KillResult.Error.
//
// F-43 design invariants (docs/feat/F-43-kill-new-graceful-and-reset.md
// §4.1):
//   - activeCwd / activeAgent / InputBuffer are NOT touched — /kill
//     owns only the agent process lifecycle.
//   - currentTurnUserMsgID is cleared (next inbound turn opens a new
//     receipt anchor).
//   - Pool entry identities are preserved while bridges shut down
//     (children may still be alive during the wg.Wait window); the
//     new empty pool is installed AFTER all bridges confirm exit.
//   - agent_sessions.json entries are deleted AFTER the process is
//     dead, never before, so a late read can't resurrect a corpse.
//
// Concurrency: safe to call concurrently with LookupActiveAgentSession.
// The per-entry as.Close() is fan-out (each bridge drives its own
// goroutine); wg.Wait + 5s outer timeout guards against a wedged
// bridge that bypasses its own SIGKILL fallback.
func (cs *ChatSession) KillAll() ([]KillResult, error) {
	// 0. stop pump FIRST so the dying bridge's final events don't
	//    drain into the channel after /kill has been confirmed, and
	//    so the preserved-input-buffer's FlushHook isn't accidentally
	//    fired by SetIdle/OnTurnEnded emitted from a zombie pump.
	cs.StopReadPump()

	// 1. snapshot pool under read lock; do not mutate cs.pool until
	//    every bridge has confirmed shutdown.
	cs.mu.RLock()
	snapshot := make([]*AgentSession, 0, len(cs.pool))
	for _, as := range cs.pool {
		snapshot = append(snapshot, as)
	}
	cs.mu.RUnlock()

	// 2. classify each snapshot entry and fan out graceful shutdown
	//    for the ones whose process is still believed alive.
	//
	//    StatusRunning   → alive, has handle → Close()
	//    StatusDetached  → "process alive but nightme no longer holds
	//                      it" (per SetDetached doc). Two flavors:
	//                        a) mid-life (e.g. SIGTERM without --cleanup):
	//                           handle is still set → Close() it.
	//                        b) post-restart (FromAgentSessionEntry
	//                           rehydrated StatusDetached): handle is nil
	//                           → orphan, no way to signal, just delete
	//                           the disk entry.
	//    StatusExited    → dead, nothing to do but clean disk.
	//
	//    Each goroutine returns its outcome via chan; we collect
	//    outcomes after wg.Wait so per-bridge Close errors are not
	//    silently dropped (review finding #B2: previously `_ =
	//    as.Close()` was discarded, so any Close failure was
	//    reported as ✓ success).
	closeCh := make(chan closeOutcome, len(snapshot))
	results := make([]KillResult, len(snapshot))
	var wg sync.WaitGroup
	for i, as := range snapshot {
		results[i] = KillResult{
			Agent:       as.Agent,
			Cwd:         as.Cwd,
			BeforeState: as.Status(),
		}
		isAlive := as.Status() == StatusRunning ||
			(as.Status() == StatusDetached && as.Handle() != nil)
		if !isAlive {
			// StatusExited or post-restart StatusDetached (no handle)
			results[i].Action = "stale-cleared"
			continue
		}
		results[i].Action = "killed"
		wg.Add(1)
		go func(i int, as *AgentSession) {
			defer wg.Done()
			closeCh <- closeOutcome{idx: i, err: as.Close()}
		}(i, as)
	}

	// 3. wait for all bridges to confirm exit. Bridge Close sets the
	//    events chan closed; the per-entry ObserveClose goroutine then
	//    flips as.Status to Exited. We do NOT add a second escalation
	//    here — nightme-side SIGTERM would race with the bridge's
	//    local watchdog.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(killGraceTotal):
		// Bridge's own watchdog should have SIGKILL'd by now. If we
		// still hit this, the child is wedged in a way even SIGKILL
		// can't fix (zombie / uninterruptible IO). Log and proceed —
		// we still want to clean our state. The persistent entries
		// will be deleted in step 5; the bridge goroutine may still
		// be running but it's "ownerless" (we no longer reference it).
		slog.Warn("killAll: graceful shutdown timeout",
			"chat_id", cs.ChatID,
			"limit", killGraceTotal)
	}
	close(closeCh)

	// 4. fold each goroutine's outcome into its KillResult.
	for oc := range closeCh {
		if oc.err != nil {
			results[oc.idx].Error = oc.err
			results[oc.idx].Action = "kill-failed"
		}
	}

	// 5. clear activeAS pointer BEFORE removing from disk so a
	//    follow-up inbound message sees "no active" and goes through
	//    LookupActiveAgentSession -> spawn fresh.
	cs.mu.Lock()
	cs.pool = make(map[agentCwdKey]*AgentSession)
	cs.activeAS = nil
	cs.currentTurnUserMsgID = ""
	cs.mu.Unlock()

	// 6. delete persistent entries. Use GetByChatPool walk (not just
	//    snapshot IDs) so orphan entries on disk — `entries` that
	//    owned this chat's ID but never made it into cs.pool (e.g.
	//    from a prior /cwd swap that left a stale entry, or a crash
	//    between Upsert and pool installation) — are also GC'd.
	//    (Review finding #B4: old `KillAll` did this; F-43 first
	//    cut narrowed to snapshot-only, reintroducing orphan
	//    accumulation across /kill cycles.)
	if cs.asFile != nil {
		ids := make([]string, 0, len(snapshot))
		for _, as := range snapshot {
			ids = append(ids, as.ID)
		}
		// Review finding #B6: DeleteMany already exists with the
		// exact "batch GC where calling Delete in a loop would
		// rewrite the file N times" rationale. Use it.
		_ = ids // unused — we call DeleteMany on the orphan set below
		_ = cs.asFile.DeleteMany(collectIDsForDelete(cs, ids))
	}
	cs.persistChatEntry()
	return results, nil
}

// closeOutcome carries one goroutine's Close result back to the
// orchestrator. idx indexes into the snapshot slice so the outer
// loop can update the corresponding KillResult in place.
type closeOutcome struct {
	idx int
	err error
}

// collectIDsForDelete returns the union of (a) snapshot IDs and
// (b) any orphan disk entries owned by this chat (detected via
// `GetByChatPool`). Pass-through helper for the DeleteMany call.
func collectIDsForDelete(cs *ChatSession, snapshotIDs []string) []string {
	if cs.asFile == nil {
		return snapshotIDs
	}
	seen := make(map[string]struct{}, len(snapshotIDs))
	for _, id := range snapshotIDs {
		seen[id] = struct{}{}
	}
	// Add orphan IDs not already in the snapshot.
	for _, e := range cs.asFile.GetByChatPool(cs.ID) {
		if _, ok := seen[e.ID]; ok {
			continue
		}
		seen[e.ID] = struct{}{}
		snapshotIDs = append(snapshotIDs, e.ID)
	}
	return snapshotIDs
}

// FormatKillResults produces a human-readable summary of /kill's
// per-entry outcomes. The output is suitable for channel.Send
// (plain text, Feishu-renderable).
//
// Output templates (selected by the (killed, stale, failed) tuple):
//   - all empty:           "No active agents to kill."
//   - all killed:          "Stopped N agent session(s):\n  ✓ <name> @ <cwd>\n..."
//   - all stale:           "Cleared N stale agent session(s) (no live processes):\n  • <name> @ <cwd> — already exited, entry cleaned\n..."
//   - mixed:               "<header>:\n  ✓ ... \n  • ... \n  ✗ <name> @ <cwd> — <action>: <err>\n..."
//
// Errors are surfaced explicitly per-entry (never swallowed). Output is
// capped to 4 KB total bytes (Feishu single-message limit) + a
// "...and N more" tail to stay under the limit (review finding #B5:
// the previous 20-line cap was insufficient when individual lines
// were large — CJK / long paths / long bridge errors all blow past
// 4 KB before the 20th line).
//
// Rows are sorted by **typed priority** (success → failure → skipped)
// and then by (Agent, Cwd) within each priority bucket. The previous
// `sort.Strings` on the formatted strings made `•` (U+2022) sort
// before `✓` (U+2713) before `✗` (U+2717) — i.e. stale-cleared rows
// always preceded killed rows, contrary to the spec's "success-first"
// ordering (review finding #B7).
//
// See docs/feat/F-43-kill-new-graceful-and-reset.md §6 for the wording
// variants and the ✓/✗/• icon legend.
func FormatKillResults(results []KillResult) string {
	if len(results) == 0 {
		return "No active agents to kill."
	}

	rows := make([]resultRow, 0, len(results))
	var killed, stale, failed int
	for _, r := range results {
		row, bucket := renderKillRow(r)
		switch bucket {
		case bucketSuccess:
			killed++
		case bucketSkipped:
			stale++
		case bucketFailure:
			failed++
		}
		rows = append(rows, row)
	}

	// Sort by typed priority (success → failure → skipped), then by
	// (Agent, Cwd) for stable display.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].priority != rows[j].priority {
			return rows[i].priority < rows[j].priority
		}
		if rows[i].agent != rows[j].agent {
			return rows[i].agent < rows[j].agent
		}
		return rows[i].cwd < rows[j].cwd
	})

	header := buildKillHeader(killed, stale, failed)
	return truncateByBytes(header, rows, formatTail)
}

// FormatResetResults produces a human-readable summary of /new's
// per-entry outcomes. Companion to FormatKillResults; same plain-text
// shape, same byte-based cap, same typed-priority sort.
//
// See docs/feat/F-43-kill-new-graceful-and-reset.md §6.2.
func FormatResetResults(results []ResetResult) string {
	if len(results) == 0 {
		return "Reset 0 sessions."
	}

	rows := make([]resultRow, 0, len(results))
	var running, dead, failed int
	for _, r := range results {
		row, bucket := renderResetRow(r)
		switch bucket {
		case bucketSuccess:
			running++
		case bucketSkipped:
			dead++
		case bucketFailure:
			failed++
		}
		rows = append(rows, row)
	}

	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].priority != rows[j].priority {
			return rows[i].priority < rows[j].priority
		}
		if rows[i].agent != rows[j].agent {
			return rows[i].agent < rows[j].agent
		}
		return rows[i].cwd < rows[j].cwd
	})

	header := buildResetHeader(running, dead, failed)
	return truncateByBytes(header, rows, formatTail)
}

// humanAction returns a short human-readable verb for an Action
// string (used in error messages). The name matches the design doc
// (docs/feat/F-43-kill-new-graceful-and-reset.md §6.6). Future
// actions can be added here.
func humanAction(action string) string {
	switch action {
	case "killed":
		return "kill"
	case "stale-cleared":
		return "stale-clear"
	default:
		return action
	}
}

// NewActiveAgentSessions resets the conversation context on the
// AgentSessions associated with cs.ActiveCwd(). Filters the pool by
// activeCwd, optionally narrowing further by agentName (when non-empty),
// AND by Status == StatusRunning — only RUNNING sessions have a live
// bridge handle and therefore have conversation context to reset.
//
// F-34 §3.4 + §6 Q-N4: this is the core batch primitive behind the
// `/new` slash command. Behavior contract:
//
//   - Pool identity preserved: AgentSession.ID / Cwd / Agent / args
//     are NOT touched. Only the underlying bridge's conversation
//     state is reset (via AgentSession.New → bridge.New).
//   - Serial execution: AgentSession.New calls are issued one at a
//     time so stdin / RPC traffic does not interleave.
//   - Status filter: ONLY StatusRunning entries are candidates.
//     Exited / Detached entries are skipped SILENTLY (do not count
//     as matched; do not trigger a lazy spawn just to clear
//     conversation that does not exist). This was clarified by
//     product on 2026-08-04: "如果没启动过的agentsession,则不需要
//     去启动AgentSession.因为它本身就没启动,所以不需要New".
//   - InputBuffer cleared: queued user messages are dropped before
//     the function returns, regardless of how many bridges succeeded.
//     This matches /kill semantics (F-34 §6 Q-N4).
//   - currentTurnUserMsgID NOT cleared: the next user message after
//     /new naturally opens a new turn + new anchor + cold-creates a
//     new receipt (Channel-autonomous per v1.3 §2.4).
//
// Returns:
//   - matched: pool entries that passed ALL filters (cwd + agentName +
//     Status == Running). These are the entries that were attempted.
//   - reset:   pool entries whose bridge.New succeeded.
//   - firstErr: the first non-nil bridge.New error (other errors are
//     counted into matched but not reset; callers report them in the
//     reply via the returned error).
func (cs *ChatSession) NewActiveAgentSessions(ctx context.Context, agentName string) (matched, reset int, results []ResetResult, firstErr error) {
	cs.mu.RLock()
	cwd := cs.activeCwd
	if cwd == "" {
		cs.mu.RUnlock()
		return 0, 0, nil, nil
	}
	cs.mu.RUnlock()

	// 1. Snapshot ALL (cwd, [agentName]) targets (no lock held across
	//    bridge calls). F-43 §5.4: dead/detached entries are NOT
	//    silently skipped — their stale ResumeID would resurrect a
	//    dead session on next spawn, defeating /new's intent.
	cs.mu.RLock()
	targets := make([]*AgentSession, 0)
	for _, as := range cs.pool {
		if as.Cwd != cwd {
			continue
		}
		if agentName != "" && as.Agent != agentName {
			continue
		}
		targets = append(targets, as)
	}
	cs.mu.RUnlock()

	// F-34 Phase 3 review #1: the queue must be cleared even when
	// no targets matched (e.g. empty pool, wrong cwd, or /new <agent>
	// with no <agent> in cwd). Otherwise the user's queued message
	// stays stuck behind an unresponsive session until they /kill or
	// /cwd.
	if cs.inputBuffer != nil {
		cs.inputBuffer.Clear()
	}

	if len(targets) == 0 {
		return 0, 0, nil, nil
	}

	results = make([]ResetResult, 0, len(targets))

	// 2. Serial reset. Each entry's action branches on its Status:
	//    - StatusRunning:   in-place reset via bridge.New(ctx)
	//    - StatusDetached / StatusExited: clear ResumeID (no spawn)
	for _, as := range targets {
		matched++
		result := ResetResult{
			Agent:       as.Agent,
			Cwd:         as.Cwd,
			BeforeState: as.Status(),
			Session:     as,
		}

		if as.Status() != StatusRunning {
			// F-43 §5.4: dead/detached entry has no live conversation
			// to reset, but its stale ResumeID must NOT be replayed on
			// the next spawn. Clear ResumeID in-memory + persist so the
			// next LookupActiveAgentSession spawns fresh.
			//
			// NO lazy spawn (F-34 §6 Q-N4 / product clarification
			// 2026-08-04): spawning just to reset a dead agent would
			// waste resources and implicitly activate it.
			as.SetResumeID("")
			if cs.asFile != nil {
				// Review finding #B3: previously `_ = ...` swallowed
				// the write error. If Upsert fails after the in-memory
				// ResumeID clear, the on-disk entry still carries the
				// stale ResumeID, and the next restore on daemon restart
				// would replay `--resume <dead-id>` — the exact bug
				// F-43 §5.4 was designed to fix. Surface the error so
				// the handler can report it (and the handler treats any
				// per-row error as ✗ rather than ✓).
				if err := cs.asFile.Upsert(as.Entry()); err != nil {
					result.Error = err
				}
			}
			result.Action = "marked-fresh"
			results = append(results, result)
			reset++
			continue
		}

		// Running: in-place reset via bridge.New(ctx).
		//
		// Active AgentSession pump coordination (F-34 Phase 3 review):
		//
		// The readPump captures as.Events() ONCE before its loop, so
		// a kill+respawn that swaps the bridge handle would orphan the
		// pump on the old (now-closed) events chan. Worse, the pump's
		// closing branch calls as.SetExited(0) which races with our
		// SetRunning after the swap.
		//
		// Pump coordination: capture the activeAS identity and
		// current handle under one RLock, then stop the pump (if
		// this AS is the active one), call as.New, and restart the
		// pump on the new handle. The handleChanged flag is
		// needed because kill+respawn swaps the bridge handle (and
		// its events chan); for in-place reset the chan is
		// unchanged but the pump goroutine still needs to be
		// restarted because StopReadPump signaled it to exit.
		isActive := false
		oldHandle := agent.AgentSession(nil)
		cs.mu.RLock()
		isActive = (as == cs.activeAS)
		if isActive {
			oldHandle = as.Handle()
		}
		cs.mu.RUnlock()
		if isActive {
			cs.StopReadPump()
		}
		err := as.New(ctx, cs.spawner)
		handleChanged := isActive && (as.Handle() != oldHandle)
		if isActive {
			// Restart the pump regardless of in-place vs
			// kill+respawn — StopReadPump signaled the prior
			// goroutine to exit, so we must launch a new one.
			_ = cs.StartReadPump()
		}
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			result.Action = "in-place-reset"
			result.Error = err
			results = append(results, result)
			continue
		}
		reset++
		result.Action = "in-place-reset"
		results = append(results, result)
		// Persist after a successful kill+respawn so the registry
		// stays in sync with the in-memory state. For in-place
		// resets the bridge will also emit a fresh EventInit which
		// the runtime's eventHandler captures via
		// PersistAgentSession; for kill+respawn the new child hasn't
		// started yet, so this Upsert captures the new PID +
		// cleared ResumeID before any subsequent EventInit arrives
		// (PTY's new child won't emit init at all, so this is the
		// ONLY persistence opportunity for that path).
		if handleChanged && cs.asFile != nil {
			_ = cs.asFile.Upsert(as.Entry())
		}
	}

	return matched, reset, results, firstErr
}

// Entry returns a snapshot of this ChatSession as a registry entry.
func (cs *ChatSession) Entry() *registry.ChatSessionEntry {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.entryLocked()
}

// CreatedAt returns when this ChatSession was first created.
func (cs *ChatSession) CreatedAt() time.Time {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.createdAt
}

// LastInteractionAt returns when this ChatSession last had user
// interaction.
func (cs *ChatSession) LastInteractionAt() time.Time {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.lastInteractionAt
}

// entryLocked is the same as Entry but assumes cs.mu is held.
func (cs *ChatSession) entryLocked() *registry.ChatSessionEntry {
	agentIDs := make([]string, 0, len(cs.pool))
	var activeASID *string
	for _, as := range cs.pool {
		agentIDs = append(agentIDs, as.ID)
		if cs.activeAS != nil && as.ID == cs.activeAS.ID {
			id := as.ID
			activeASID = &id
		}
	}
	return &registry.ChatSessionEntry{
		ID:                   cs.ID,
		ChatID:               cs.ChatID,
		ActiveCwd:            cs.activeCwd,
		ActiveAgent:          cs.activeAgent,
		PrimaryAgent:         cs.primaryAgent,
		AgentSessionIDs:      agentIDs,
		ActiveAgentSessionID: activeASID,
		CreatedAt:            cs.createdAt,
		LastInteractionAt:    cs.lastInteractionAt,
		WatchMode:            cs.watchMode,
		ThinkMode:            cs.thinkMode,
		ToolsMode:            cs.toolsMode,
	}
}

// persistChatEntry writes the ChatSessionEntry to disk (if persistence
// is configured). Best-effort: errors are returned but not propagated
// through call sites (logged at higher level).
func (cs *ChatSession) persistChatEntry() {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	cs.persistChatEntryLocked()
}

// persistChatEntryLocked writes ChatSessionEntry. Caller must hold
// cs.mu (RLock or Lock).
func (cs *ChatSession) persistChatEntryLocked() {
	if cs.csFile == nil {
		return
	}
	_ = cs.csFile.Upsert(cs.entryLocked())
}

// newAgentSessionID returns a unique ID for an AgentSession. v1.2
// commit 6 uses a simple counter-based scheme for testability;
// commit 7 may swap to UUID v7.
var agentSessionCounter atomic.Uint64

func newAgentSessionID() string {
	n := agentSessionCounter.Add(1)
	return fmt.Sprintf("as_%d_%d", time.Now().UnixNano(), n)
}
// formatRowBucket is the typed priority bucket for a formatter row.
// Lower values sort first. success < failure < skipped so the user
// sees ✓ first, then ✗, then • (review finding #B7 — the previous
// alphabetical sort on the rendered strings placed `•` (U+2022)
// before `✓` (U+2713) before `✗` (U+2717), inverting the spec).
type formatRowBucket int

const (
	bucketSuccess formatRowBucket = iota
	bucketFailure
	bucketSkipped
)

// resultRow is one formatted line + the structured fields the sorter
// needs. The previous implementation sorted the formatted strings
// directly, which bound sort to Unicode codepoint ordering.
type resultRow struct {
	text             string
	priority         formatRowBucket
	agent, cwd       string
}

// renderKillRow is FormatKillResults' per-row branch. Returns the
// rendered line and the bucket it belongs to.
func renderKillRow(r KillResult) (resultRow, formatRowBucket) {
	if r.Error != nil {
		return resultRow{
			text: fmt.Sprintf("  ✗ %s @ %s — %s: %v",
				r.Agent, r.Cwd, humanAction(r.Action), r.Error),
			priority: bucketFailure,
			agent:    r.Agent,
			cwd:      r.Cwd,
		}, bucketFailure
	}
	switch r.Action {
	case "killed":
		return resultRow{
			text:     fmt.Sprintf("  ✓ %s @ %s", r.Agent, r.Cwd),
			priority: bucketSuccess,
			agent:    r.Agent,
			cwd:      r.Cwd,
		}, bucketSuccess
	case "stale-cleared":
		return resultRow{
			text:     fmt.Sprintf("  • %s @ %s — already exited, entry cleaned", r.Agent, r.Cwd),
			priority: bucketSkipped,
			agent:    r.Agent,
			cwd:      r.Cwd,
		}, bucketSkipped
	default:
		// future action: surface as success mark, generic text
		return resultRow{
			text:     fmt.Sprintf("  ✓ %s @ %s — %s", r.Agent, r.Cwd, r.Action),
			priority: bucketSuccess,
			agent:    r.Agent,
			cwd:      r.Cwd,
		}, bucketSuccess
	}
}

// renderResetRow is FormatResetResults' per-row branch.
func renderResetRow(r ResetResult) (resultRow, formatRowBucket) {
	if r.Error != nil {
		return resultRow{
			text: fmt.Sprintf("  ✗ %s @ %s — bridge reset: %v",
				r.Agent, r.Cwd, r.Error),
			priority: bucketFailure,
			agent:    r.Agent,
			cwd:      r.Cwd,
		}, bucketFailure
	}
	switch r.Action {
	case "in-place-reset":
		return resultRow{
			text:     fmt.Sprintf("  ✓ %s @ %s — reset in-place", r.Agent, r.Cwd),
			priority: bucketSuccess,
			agent:    r.Agent,
			cwd:      r.Cwd,
		}, bucketSuccess
	case "marked-fresh":
		return resultRow{
			text:     fmt.Sprintf("  ✓ %s @ %s — already exited, marked fresh", r.Agent, r.Cwd),
			priority: bucketSkipped,
			agent:    r.Agent,
			cwd:      r.Cwd,
		}, bucketSkipped
	default:
		return resultRow{
			text:     fmt.Sprintf("  ✓ %s @ %s — %s", r.Agent, r.Cwd, r.Action),
			priority: bucketSuccess,
			agent:    r.Agent,
			cwd:      r.Cwd,
		}, bucketSuccess
	}
}

// buildKillHeader mirrors the old buildKillHeader. Wording is the
// same as the spec template (kept verbatim for downstream consumers
// that pattern-match on the header text).
func buildKillHeader(killed, stale, failed int) string {
	if failed == 0 && stale == 0 {
		return fmt.Sprintf("Stopped %d agent session(s):", killed)
	}
	if killed == 0 && stale > 0 && failed == 0 {
		return fmt.Sprintf("Cleared %d stale agent session(s) (no live processes):", stale)
	}
	parts := make([]string, 0, 3)
	if killed > 0 {
		parts = append(parts, fmt.Sprintf("Stopped %d", killed))
	}
	if stale > 0 {
		parts = append(parts, fmt.Sprintf("%d stale entry cleared", stale))
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", failed))
	}
	return strings.Join(parts, ", ") + ":"
}

// buildResetHeader mirrors the old buildResetHeader. Note the wording
// differs from /kill (each command has its own header grammar).
func buildResetHeader(running, dead, failed int) string {
	if failed == 0 && dead == 0 {
		return fmt.Sprintf("Reset %d session(s):", running)
	}
	if running == 0 && dead > 0 && failed == 0 {
		return fmt.Sprintf("Marked %d session(s) fresh for next spawn:", dead)
	}
	parts := make([]string, 0, 3)
	if running > 0 {
		parts = append(parts, fmt.Sprintf("%d reset in-place", running))
	}
	if dead > 0 {
		parts = append(parts, fmt.Sprintf("%d marked fresh", dead))
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", failed))
	}
	return "Reset " + fmt.Sprintf("%d session(s), %s:", running+dead, strings.Join(parts, ", "))
}

// formatReplyByteCap is the Feishu single-message payload limit
// (page / IM message). Both /kill and /new format strings cap here
// to keep the channel side from rejecting the message outright
// (review finding #B5).
const formatReplyByteCap = 4096

// formatTail is the "...and N more" suffix appended when the output
// would otherwise exceed the byte cap. Hidden = len(rows) - linesShown.
const formatTail = "  ... and %d more"

// truncateByBytes joins rows with "\n" and caps the total byte length
// at formatReplyByteCap. Lines that would push the output over the cap
// are truncated and replaced with the formatTail summary. The header is
// always included (so the user always sees the headline counts).
//
// The cap is "soft" — the tail suffix can cause the final string to
// exceed the cap by a small number of bytes (the tail itself is
// short, ~30 bytes, and Feishu's limit is a guideline). Callers that
// need a hard cap should re-check before sending.
func truncateByBytes(header string, rows []resultRow, tailFmt string) string {
	out := header
	for i, r := range rows {
		candidate := out + "\n" + r.text
		if len(candidate)+len(tailFmtFor(i, len(rows))) > formatReplyByteCap {
			// We've exhausted the budget. Replace the
			// last appended line with the tail summary.
			hidden := len(rows) - i
			out = out + "\n" + fmt.Sprintf(tailFmt, hidden)
			return out
		}
		out = candidate
	}
	return out
}

// tailFmtFor returns the byte-length the tail would consume for the
// given (i, total) under the standard formatTail template. Used by
// truncateByBytes to estimate budget before committing the tail.
func tailFmtFor(i, total int) string {
	return fmt.Sprintf(formatTail, total-i)
}
