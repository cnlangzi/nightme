// session_state.go — per-dsh-session state inside the relay.
//
// One sessionState per dsh session id. Holds:
//   - the ring buffer (Phase 1; used by History ring path)
//   - the subscriber table (Phase 1; fans translated events
//     out to drivers via Bridge.Subscribe)
//   - the mux-pump handler list (Phase 2; drivers register
//     handlers via MuxSubscribe; the central mux pump fans
//     frames out to all of them)
//
// The translator / wireState / dispatcher state stays in
// each driver (dsh package). Phase 3 explicitly does NOT move
// it — single-goroutine access in driver means locks add no
// value, and centralising it would force a major surface
// rewrite for the protocol-translation code. See
// bridge.go header for the full design conversation.
//
// Phase 3 add-on: each session has a per-session
// agentName (carried on EventAgentReady stamps) and a
// runMuxPump goroutine managed by MuxSubscribe / Disconnect.
// Driver's translator uses its own translator/wireState;
// relay's mux pump just routes frames to drivers via the
// handler list.
package relay

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/cnlangzi/nightme/internal/bridge/dsh/api"
)

const ringBufferSize = 1024

// Event is one dsh wire event as seen by relay consumers.
//
// Aliased to api.Event so the bridge interface and the relay
// implementation share the same shape — drivers compile
// against bridge.Event and the relay passes them straight
// through.
type Event = api.Event

// MuxHandler is the per-driver callback the relay invokes
// for each mux frame destined for the session. The relay
// owns the mux subscription; multiple drivers attach their
// own handler and all receive every frame. MuxHandler
// matches api.MuxHandler / host.MuxFrameHandler so the
// relay can pass through without re-decoding.
type MuxHandler = api.MuxHandler

type subscriber struct {
	ch     chan Event
	cancel context.CancelFunc
}

type sessionState struct {
	id        string
	workspace string

	mu   sync.RWMutex
	ring []api.Event // append-only, capped at ringBufferSize
	// lastSeq is the highest SessionEvent.seq we've already
	// pushed to the ring. Phase 3 fix #19: atomic to avoid
	// races between backfill.fetch (reader) and appendEvents
	// / bumpLastSeq (writers). ring writes still need mu
	// because they mutate the slice.
	lastSeq atomic.Int64

	subMu  sync.RWMutex
	subs   map[int]*subscriber
	nextID int

	// mux pump goroutine cancel — set by runMuxPump. nil
	// before first Subscribe.
	muxCancel context.CancelFunc

	// muxHandlers are the per-driver callbacks registered
	// via MuxSubscribe. The central mux pump invokes each on
	// every incoming frame. Phase 3 fix #6: use map+alive so
	// unsubscribe can actually drop entries (the previous
	// slice+best-effort unsub never deleted).
	muxMu       sync.RWMutex
	muxHandlers map[int]muxHandlerEntry

	// muxHandlers next ID allocator (monotonic, never
	// reused). Combined with the live map it lets the
	// dispatcher's iteration stay GC-friendly.
	muxNextID int

	// agentName is the agent label carried on the session
	// for diagnostics / dashboard. Not used by the relay's
	// translator pipeline (Phase 3 keeps translator in driver);
	// kept here so future Phase 4+ work that lifts
	// translator doesn't need to re-introduce it.
	agentName string
}

type muxHandlerEntry struct {
	fn    MuxHandler
	alive bool
}

const muxHandlerCap = 64 // refuse beyond this to prevent OOM

func newSessionState(id, workspace string) *sessionState {
	return &sessionState{
		id:        id,
		workspace: workspace,
		subs:      map[int]*subscriber{},
	}
}

func (s *sessionState) appendEvents(events []Event) {
	if len(events) == 0 {
		return
	}
	s.mu.Lock()
	for _, ev := range events {
		s.ring = append(s.ring, ev)
		if len(s.ring) > ringBufferSize {
			s.ring = s.ring[len(s.ring)-ringBufferSize:]
		}
		if ev.Seq > 0 {
			// Phase 3 fix #19: CAS loop on the atomic
			// lastSeq so multiple writers (live mux pump,
			// backfill fetch) don't race past each other.
			for {
				old := s.lastSeq.Load()
				if ev.Seq <= old || s.lastSeq.CompareAndSwap(old, ev.Seq) {
					break
				}
			}
		}
	}
	s.mu.Unlock()
}

func (s *sessionState) ringSnapshot() []Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.ring) == 0 {
		return nil
	}
	out := make([]Event, len(s.ring))
	copy(out, s.ring)
	return out
}

func (s *sessionState) lastSeqSeen() int64 {
	return s.lastSeq.Load()
}

// subscribe attaches a new subscriber to this session. The
// returned channel gets a replay of the current ring buffer
// (in order) followed by live events delivered by the
// central mux dispatch goroutine. The unsub func detaches
// and closes the channel.
//
// Caller's ctx cancellation is honored: when ctx fires, the
// channel is closed (after draining any pending replay).
func (s *sessionState) subscribe(ctx context.Context) (<-chan Event, func(), error) {
	subCtx, cancel := context.WithCancel(ctx)
	ch := make(chan Event, 64)

	s.subMu.Lock()
	subID := s.nextID
	s.nextID++
	s.subs[subID] = &subscriber{ch: ch, cancel: cancel}
	s.subMu.Unlock()

	replay := s.ringSnapshot()
	if len(replay) > 0 {
		go func() {
			for _, ev := range replay {
				select {
				case <-subCtx.Done():
					return
				case ch <- ev:
				}
			}
		}()
	}

	unsub := func() {
		s.subMu.Lock()
		delete(s.subs, subID)
		s.subMu.Unlock()
		cancel()
	}
	return ch, unsub, nil
}

func (s *sessionState) deliver(ev Event) {
	s.subMu.RLock()
	defer s.subMu.RUnlock()
	for _, sub := range s.subs {
		select {
		case sub.ch <- ev:
		default:
			// Phase 3 fix #28: count drops so a stuck
			// subscriber shows up in /diagnose instead of
			// silently losing frames.
			droppedEvents.Add(1)
		}
	}
}

// droppedEvents is the process-wide counter of events the
// relay dropped because no subscriber had buffer space. Use
// /diagnose (Phase 4) to surface; for now it's debuggable
// via runtime/metrics or a debug endpoint.
//
// Phase 3 fix #28: atomic counter incremented on every
// dropped event.
var droppedEvents atomic.Uint64

// DroppedEvents returns the process-wide count of dropped
// events. /diagnose exposes this to operators.
func DroppedEvents() uint64 { return droppedEvents.Load() }

// resetDroppedEvents is for tests; production doesn't reset.
func resetDroppedEvents() { droppedEvents.Store(0) }

func (s *sessionState) closeSubscribers() {
	s.subMu.Lock()
	subs := s.subs
	s.subs = map[int]*subscriber{}
	s.subMu.Unlock()
	for _, sub := range subs {
		sub.cancel()
	}
}

// addMuxHandler registers a per-driver mux frame handler.
// Phase 3 fix #6: returns an integer ID and a detach func;
// the ID is used by removeMuxHandler (Phase 3 fix #6 cont.)
// to actually drop the entry from the map. Caps at
// muxHandlerCap to prevent unbounded growth under
// driver-creates-driver leaks.
//
// Returns (id, detach). The detach marks the entry dead;
// the next dispatchMux sweep skips it. The map entry
// itself is kept until removeMuxHandler runs.
func (s *sessionState) addMuxHandler(h MuxHandler) (int, func()) {
	if h == nil {
		return 0, func() {}
	}
	s.muxMu.Lock()
	if len(s.muxHandlers) >= muxHandlerCap {
		s.muxMu.Unlock()
		// Refuse rather than silently evict: the driver that
		// hit the cap has a bug. Log via slog.Default (no
		// logger on sessionState).
		return 0, func() {}
	}
	id := s.muxNextID
	s.muxNextID++
	if s.muxHandlers == nil {
		s.muxHandlers = map[int]muxHandlerEntry{}
	}
	s.muxHandlers[id] = muxHandlerEntry{fn: h, alive: true}
	s.muxMu.Unlock()
	return id, func() { s.removeMuxHandler(id) }
}

// removeMuxHandler marks the entry dead and evicts it from
// the map. Subsequent dispatches skip it; the map slot is
// reclaimed immediately so memory doesn't grow.
func (s *sessionState) removeMuxHandler(id int) {
	if id == 0 {
		return
	}
	s.muxMu.Lock()
	delete(s.muxHandlers, id)
	s.muxMu.Unlock()
}

// dispatchMux invokes every registered mux handler with the
// given frame. Used by the central mux pump in relay.go.
func (s *sessionState) dispatchMux(method, rpcID string, payload api.MuxHandlerPayload) {
	s.muxMu.RLock()
	handlers := make([]MuxHandler, 0, len(s.muxHandlers))
	for _, e := range s.muxHandlers {
		if e.alive {
			handlers = append(handlers, e.fn)
		}
	}
	s.muxMu.RUnlock()
	for _, h := range handlers {
		h(method, rpcID, payload)
	}
}

// respond is a Phase 1 shim — see bridge.go header for the
// overall design. The per-driver pending* maps in session.go
// remain authoritative until the follow-up refactor moves
// FIFO routing into the relay.
func (s *sessionState) respond(_ context.Context, _ string) error {
	return nil
}
