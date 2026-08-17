// router.go — per-session mux subscription table + shared pending
// approval/question channel table.
//
// This is the bit that makes the 1:N architecture safe for
// concurrent sessions:
//   - Each ChatSession / AgentSession subscribes by its dsh sessionId.
//   - mux frames carrying that sessionId in their payload are
//     routed to the subscriber.
//   - Pending approval/question response channels are keyed by
//     (sessionId, frame.rpcId) so the AnswerPending call writes to
//     the right channel across all sessions.
//
// # Why payload-level sessionId extraction (not envelope-level)
//
// mux frames are uniform server-request envelopes
// (dsh-api.md §3.3: {type:"server-request", rpcId, method, payload}).
// The discriminator lives inside `payload.sessionId` for every
// session-scoped method we care about (session/*, approval/*,
// question/*). Extracting it once here means subscribers see a
// payload-agnostic callback signature and don't have to know about
// sessionId parsing themselves.
//
// # Ordering guarantee
//
// mux is a single stream read by a single pump goroutine. Frames
// are delivered to subscribers in server-push order. Per-session
// ordering is therefore preserved (downstream consumers can rely on
// it for F-52 textBuf correctness etc.). Cross-session ordering
// is NOT preserved (the server may interleave; downstream
// consumers must discriminate by sessionId — which is exactly
// why we route here).
//
// # What router does NOT do
//
//   - It does NOT decode session/event's inner envelope (the
//     handler does that — handlers see the raw payload bytes).
//   - It does NOT translate to agent.AgentEvent (that's the
//     ChatSession integration in Phase 2).
//   - It does NOT enforce timeouts on pending channels (callers
//     can spawn their own watchdog goroutines, mirroring the
//     pre-fix per-driver registerApproval timeout behavior).

package host

import (
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
)

// MuxFrameHandler is the per-session mux callback. The router
// guarantees that consecutive calls for the SAME sessionId happen
// in server-push order (mux is single-threaded from the read
// pump's view). Cross-session ordering is NOT guaranteed.
type MuxFrameHandler func(method, rpcID string, payload json.RawMessage)

// HostFrameHandler is the host-stream callback. Host frames don't
// carry a sessionId — only one host handler is meaningful per
// daemon (host events describe daemon-global lifecycle).
type HostFrameHandler func(method, rpcID string, payload json.RawMessage)

// Subscription is one row of the mux subscription table — what
// Client.RecoverSubscriptions walks to re-attach every session on
// the new dsh instance after a respawn. The cwd is needed because
// session.create is keyed on (sessionId, cwd) and a mismatch would
// return session-conflict (dsh-api.md §2.1.3).
type Subscription struct {
	SessionID string
	CWD       string
}

// Router is the per-session mux subscription table + shared
// pending-approvals/questions table.
//
// Concurrency: every method is safe for concurrent use. Subscribe /
// Unsubscribe take the write lock; DispatchMux / DispatchHost take
// the read lock; the pending table uses the same RWMutex (so
// RegisterPending blocks all dispatches — fine because dispatches
// don't read the pending table; they're routing-only).
type Router struct {
	log *slog.Logger

	mu          sync.RWMutex
	muxSubs     map[string]MuxFrameHandler // sessionId → handler
	cwdBySess   map[string]string          // sessionId → cwd (for respawn recovery)
	hostHandler HostFrameHandler

	// pending is keyed by "<sessionId>:<frameRpcId>" — the wire
	// contract (dsh-api.md §2.12, §3.4.6, §3.4.9) is that /api/respond
	// echoes the frame's rpcId, so the same key the server uses to
	// track the pending request is what we use to look up the
	// response channel. Session-scoped key prevents approval from
	// session A accidentally answering a question from session B.
	pending map[string]chan string
}

// NewRouter constructs an empty router. Pass nil for log to use
// slog.Default().
func NewRouter(log *slog.Logger) *Router {
	if log == nil {
		log = slog.Default()
	}
	return &Router{
		log:       log,
		muxSubs:   make(map[string]MuxFrameHandler),
		cwdBySess: make(map[string]string),
		pending:   make(map[string]chan string),
	}
}

// Subscribe installs a per-session mux handler with cwd tracking.
// Calling Subscribe twice for the same sessionId replaces the prior
// handler and cwd. The replacement is atomic w.r.t. dispatch —
// handlers running in the pump goroutine won't see a torn pointer.
//
// cwd is required: it lets Client.RecoverSubscriptions re-attach
// every subscribed session after a dsh respawn (session.create is
// keyed on (sessionId, cwd); without cwd we'd have to drop the
// subscription entirely).
func (r *Router) Subscribe(sessionID, cwd string, h MuxFrameHandler) {
	if sessionID == "" || h == nil {
		return
	}
	r.mu.Lock()
	r.muxSubs[sessionID] = h
	r.cwdBySess[sessionID] = cwd
	r.mu.Unlock()
}

// Unsubscribe removes the sessionId's mux handler AND drops all
// pending approval/question channels for that session. Use this
// when an AgentSession is being dropped — leaving stale pending
// channels around would let a delayed user reply satisfy nothing.
func (r *Router) Unsubscribe(sessionID string) {
	if sessionID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.muxSubs, sessionID)
	delete(r.cwdBySess, sessionID)
	// Drop pending channels for this session. Prefix-match the
	// pendingKey ("<sessionId>:<frameRpcId>") so we don't touch
	// other sessions' entries.
	prefix := sessionID + ":"
	for k, ch := range r.pending {
		if strings.HasPrefix(k, prefix) {
			closeIfPending(ch)
			delete(r.pending, k)
		}
	}
}

// EnumerateSubscriptions returns a snapshot of every active
// subscription. Used by Client.RecoverSubscriptions after a dsh
// respawn to re-attach every session on the new dsh instance.
//
// The returned slice is a copy; the caller may iterate without
// holding the router lock.
func (r *Router) EnumerateSubscriptions() []Subscription {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Subscription, 0, len(r.muxSubs))
	for sid, h := range r.muxSubs {
		if h == nil {
			continue
		}
		out = append(out, Subscription{SessionID: sid, CWD: r.cwdBySess[sid]})
	}
	return out
}

// SetHostHandler installs the global host-stream handler. Only one
// host handler is meaningful — host events are daemon-global.
// Calling SetHostHandler twice replaces the prior handler.
func (r *Router) SetHostHandler(h HostFrameHandler) {
	r.mu.Lock()
	r.hostHandler = h
	r.mu.Unlock()
}

// DispatchMux routes a mux frame. sessionId is extracted from the
// payload's top-level `sessionId` field (the wire discriminator for
// all session-scoped mux methods). Frames whose payload has no
// sessionId — currently only mux-frame-level `approval/asked`
// per dsh/handle_mux.go:131-143 — are dropped at debug level.
// Subscribers receive raw payload bytes and decode per-method
// themselves.
func (r *Router) DispatchMux(method, rpcID string, payload json.RawMessage) {
	sessionID := extractSessionID(payload)
	if sessionID == "" {
		r.log.Debug("dsh.host: mux frame without sessionId; dropping",
			"method", method, "rpc_id", rpcID)
		return
	}
	r.mu.RLock()
	h, ok := r.muxSubs[sessionID]
	r.mu.RUnlock()
	if !ok {
		r.log.Debug("dsh.host: mux frame for unsubscribed session",
			"session_id", sessionID, "method", method, "rpc_id", rpcID)
		return
	}
	h(method, rpcID, payload)
}

// DispatchHost routes a host frame. The host stream has no
// sessionId — every host event goes to the single registered host
// handler.
func (r *Router) DispatchHost(method, rpcID string, payload json.RawMessage) {
	r.mu.RLock()
	h := r.hostHandler
	r.mu.RUnlock()
	if h == nil {
		r.log.Debug("dsh.host: host frame with no handler", "method", method)
		return
	}
	h(method, rpcID, payload)
}

// RegisterPendingApproval stores a response channel under
// (sessionID, frameRpcID). Returns the channel that AnswerPending
// will write to. The frame's rpcID is what the server correlates
// with /api/respond — this is also what we put into the
// (sessionID, frameRpcID) key.
//
// Callers should spawn their own watchdog goroutine that fires
// "rejected" (or whatever the timeout default is) after a
// bounded wait — see the existing dsh/permissions.go
// registerApproval watchdog for the pattern.
//
// The returned channel is buffered (cap 1) so the caller can
// immediately receive from it without coordinating with the
// writer's exact timing.
func (r *Router) RegisterPendingApproval(sessionID, frameRpcID string) chan string {
	ch := make(chan string, 1)
	key := pendingKey(sessionID, frameRpcID)
	r.mu.Lock()
	r.pending[key] = ch
	r.mu.Unlock()
	return ch
}

// AnswerPending writes outcome to the channel registered under
// (sessionID, frameRpcID). Returns true if the channel accepted the
// write; false if no pending registration exists (timeout fired
// first, or Unsubscribe dropped it) or the channel was already
// drained.
//
// The wire correlation is the frame's rpcId — callers should also
// invoke RPCClient.Respond(ctx, frameRpcID, ...) to actually tell
// dsh what to do with the answer. This method only delivers the
// value to the in-process handler waiting on the channel.
func (r *Router) AnswerPending(sessionID, frameRpcID, outcome string) bool {
	key := pendingKey(sessionID, frameRpcID)
	r.mu.RLock()
	ch, ok := r.pending[key]
	r.mu.RUnlock()
	if !ok {
		return false
	}
	select {
	case ch <- outcome:
		return true
	default:
		// Channel already drained (likely the timeout watchdog
		// fired and wrote "rejected"). Don't lose the user's
		// answer silently — just report that the in-process
		// handler didn't get it. The /api/respond wire call
		// still goes out if the caller does it.
		return false
	}
}

// DropPending removes the pending channel under (sessionID,
// frameRpcID) WITHOUT writing to it. Use this when the in-process
// handler has finished without a user answer (e.g., the session
// died before the user replied). Equivalent to a non-write cleanup
// of RegisterPending.
func (r *Router) DropPending(sessionID, frameRpcID string) {
	key := pendingKey(sessionID, frameRpcID)
	r.mu.Lock()
	if ch, ok := r.pending[key]; ok {
		closeIfPending(ch)
		delete(r.pending, key)
	}
	r.mu.Unlock()
}

// PendingCount returns the number of live pending channels. Useful
// for ops triage + tests.
func (r *Router) PendingCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.pending)
}

// SubscriberCount returns the number of registered mux subscribers.
// Tests use this to verify Subscribe/Unsubscribe took effect.
func (r *Router) SubscriberCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.muxSubs)
}

// ─── helpers ───────────────────────────────────────────────────────

// extractSessionID pulls sessionId from a mux frame payload. Most
// session-scoped mux methods (session/*, approval/requested,
// approval/resolved, approval/asked, question/requested,
// question/resolved) put sessionId at the payload top level — see
// the field comments on muxSessionEvent, muxSessionSubscribed,
// muxApprovalRequested, muxApprovalResolved, muxQuestionRequested
// in dsh/protocol.go and the wire shapes documented in
// docs/bridge/dsh-api.md §3.4.
//
// Returns "" if the payload doesn't carry sessionId. The caller
// logs and drops those frames.
func extractSessionID(payload json.RawMessage) string {
	if len(payload) == 0 {
		return ""
	}
	var p struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return ""
	}
	return p.SessionID
}

// pendingKey builds the (sessionId, frameRpcId) key. Both must
// be non-empty (callers enforce); a bad key with empty parts
// would silently collide across sessions or rpcs.
func pendingKey(sessionID, frameRpcID string) string {
	return sessionID + ":" + frameRpcID
}

// closeIfPending drains any pending value out of ch without
// blocking. We don't actually close(ch) — the receiver expects a
// single value write from AnswerPending, and closing it would
// signal "no more values" which the receiver doesn't handle.
//
// Used by Unsubscribe / DropPending to clean up after a handler
// has gone away.
func closeIfPending(ch chan string) {
	select {
	case <-ch:
	default:
	}
}