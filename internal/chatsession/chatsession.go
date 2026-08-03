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
	ID       string
	ChatID   string
	ChatType string

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
}

// New creates a fresh ChatSession in memory. The caller is
// responsible for persisting via Persist().
//
// primaryAgent is the cfg.Primary snapshot at creation time. It
// seeds activeAgent so the runtime always has an effective agent
// to dispatch to (no runtime fallback: the lookup only ever reads
// activeAgent). The snapshot itself is read-only post-construction
// (Q-A: no /default command, no per-chat override).
func New(chatID, chatType, primaryAgent string) *ChatSession {
	return &ChatSession{
		ID:               deriveIDFromChatID(chatID),
		ChatID:           chatID,
		ChatType:         chatType,
		activeAgent:      primaryAgent, // init seed
		primaryAgent:     primaryAgent, // historical snapshot, read-only
		pool:             make(map[agentCwdKey]*AgentSession),
		createdAt:        time.Now(),
		lastInteractionAt: time.Now(),
	}
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
		if n := len(userMsgIDs); n > 0 {
			cs.mu.Lock()
			cs.currentTurnUserMsgID = userMsgIDs[n-1]
			as := cs.activeAS
			cs.mu.Unlock()
			if as == nil || as.Handle() == nil {
				return ErrNotRunning
			}
			return as.SendBlocks(context.Background(), combined)
		}
		cs.mu.RLock()
		as := cs.activeAS
		cs.mu.RUnlock()
		if as == nil || as.Handle() == nil {
			return ErrNotRunning
		}
		return as.SendBlocks(context.Background(), combined)
	}
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
		cs.activeAS = as
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
	cs.activeAS = newAS
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

// KillAll kills every AgentSession in the pool and clears the pool.
// activeAS is set to nil. Old receipts (if any) are NOT touched
// (they're gateway-managed and will be disposed by the gateway on
// next EventError from this chat).
//
// v1.2 commit 6: this is a data-only operation — no actual signal
// is sent (commit 7 will wire SIGTERM).
//
// commit 8c: also stops the readPump + clears the InputBuffer
// (queued messages are lost on /kill — user must re-send).
func (cs *ChatSession) KillAll() error {
	// commit 8c: stop the readPump FIRST so the goroutine exits
	// before we tear down the pool (avoids "events draining into
	// a nil pool" races).
	cs.StopReadPump()

	cs.mu.Lock()
	cs.pool = make(map[agentCwdKey]*AgentSession)
	cs.activeAS = nil
	cs.currentTurnUserMsgID = ""
	cs.mu.Unlock()

	// commit 8c: discard queued user messages. They can't reach
	// a child process now (no agent), and we don't want stale
	// messages flushed on next spawn.
	if cs.inputBuffer != nil {
		cs.inputBuffer.Clear()
	}

	if cs.asFile != nil {
		// Best-effort: clear all entries owned by this ChatSession.
		for _, e := range cs.asFile.GetByChatPool(cs.ID) {
			_ = cs.asFile.Delete(e.ID)
		}
	}
	cs.persistChatEntry()
	return nil
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
		ChatType:             cs.ChatType,
		ActiveCwd:            cs.activeCwd,
		ActiveAgent:          cs.activeAgent,
		PrimaryAgent:         cs.primaryAgent,
		AgentSessionIDs:      agentIDs,
		ActiveAgentSessionID: activeASID,
		CreatedAt:            cs.createdAt,
		LastInteractionAt:    cs.lastInteractionAt,
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