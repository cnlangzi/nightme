// Package chatsession — ReadPump (commit 8c).
//
// ReadPump is the per-ChatSession event-loop goroutine that drains
// the active AgentSession's Events() channel and invokes a
// runtime-installed EventHandler for each event. It also drives the
// InputBuffer FSM (non-terminal event → SetBusy; EventDone / Error
// → SetIdle + OnTurnEnded → flush).
//
// Lifecycle:
//
//   - startReadPump(as, h)         — start a pump for AS; stops any
//                                    existing pump first.
//   - stopReadPump()              — signal current pump to exit and
//                                    wait for it (bounded by stopPumpCh).
//   - ChatSession dies             — caller must stopReadPump() to
//                                    avoid leaking goroutines.
//
// Why per-ChatSession (not per-AgentSession): only one AgentSession
// is active per chat at a time. The previous /use switches the
// active AS — we stop the old pump and start a new one, so events
// from the old process stop being consumed (PRD §4.3 "过时的不管").
//
// SetFlushHook + SetEventHandler are the two callback seams the
// runtime installs. Both are pure data; ChatSession owns the
// goroutine + lifecycle, not the translation logic.

package chatsession

import (
	"context"
	"errors"
	"sync"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/receipt"
)

// EventHandler is invoked by ChatSession for every AgentEvent from
// the active AgentSession's Events() channel. The runtime typically
// implements this as: translate to OutboundMessage + channel.Send.
//
// chatID is the owning ChatSession's ChatID (closure-free; the
// runtime can use it directly without looking up the manager).
//
// Implementations MUST be non-blocking or short (events drain
// through a buffered channel; long handlers stall the pump).
type EventHandler func(chatID string, s *AgentSession, ev agent.AgentEvent)

// EventPumpState tracks the current readPump goroutine for one
// ChatSession. Accessed only under pumpMu (read or write).
type EventPumpState struct {
	stop chan struct{}
	done chan struct{}
}

// EventPump is a per-ChatSession readPump controller. One per
// ChatSession (since only one activeAS at a time).
type EventPump struct {
	mu sync.Mutex
	cur EventPumpState
}

// ErrNoActiveAgentSession is returned by StartReadPump when called
// before LookupActiveAgentSession materializes a process.
var ErrNoActiveAgentSession = errors.New("chatsession: no active AgentSession to pump")

// StartReadPump starts a readPump goroutine for the CURRENT
// active AgentSession. If a pump is already running, it is stopped
// first (and the new one replaces it). The pump drives:
//   - cs.eventHandler (if non-nil) — invoked for each event from
//     as.Events().
//   - cs.SetBusy / SetIdle / OnTurnEnded — InputBuffer FSM.
//
// Returns ErrNoActiveAgentSession if no active AS or it's not
// spawned yet.
func (cs *ChatSession) StartReadPump() error {
	cs.mu.RLock()
	as := cs.activeAS
	cs.mu.RUnlock()
	if as == nil || as.Handle() == nil {
		return ErrNoActiveAgentSession
	}

	cs.mu.RLock()
	h := cs.eventHandler
	cs.mu.RUnlock()

	// Stop existing pump first.
	cs.StopReadPump()

	stop := make(chan struct{})
	done := make(chan struct{})
	cs.pumpMu.Lock()
	cs.pump = EventPumpState{stop: stop, done: done}
	cs.pumpMu.Unlock()
	cs.pumpRunning.Store(true)

	go cs.runReadPump(as, h, stop, done)
	return nil
}

// StopReadPump signals the current pump to exit and waits for it.
// Idempotent and safe to call when no pump is running.
func (cs *ChatSession) StopReadPump() {
	cs.pumpMu.Lock()
	cur := cs.pump
	cs.pump = EventPumpState{}
	cs.pumpMu.Unlock()

	if cur.stop == nil {
		return
	}
	// Close stop channel (signal); the goroutine sees it on its
	// next select tick.
	select {
	case <-cur.stop:
		// already closed
	default:
		close(cur.stop)
	}
	// Wait for done. The runtime should bound this with a context
	// in higher-level shutdown; here we just block.
	if cur.done != nil {
		<-cur.done
	}
}

// HasPump reports whether a readPump is currently running. Useful
// for tests; not a stable public API.
func (cs *ChatSession) HasPump() bool {
	return cs.pumpRunning.Load()
}

// runReadPump is the per-AgentSession event loop. It drains
// as.Events() until:
//   - The channel closes (process died; status set to Exited)
//   - stop is closed (signal from StopReadPump)
//   - ctx cancels (ChatSession shutdown)
//
// On every event, in order:
//   1. Invoke the EventHandler (if non-nil)
//   2. Drive the InputBuffer FSM
//   3. If EventDone/EventError, also flush the buffer
//
// Event order: handler runs BEFORE FSM transition, so the handler
// can observe "agent is going idle" via ev.Kind. (Most handlers
// don't care; this is for completeness.)
//
// v1.3 (F-31): on terminal events (EventDone/EventError), emit
// MessageState(Done/Error) for every userMsgID in the just-
// completed turn (tracked via currentTurnUserMsgIDs). Emit BEFORE
// SetIdle + OnTurnEnded so the next flush (if any queued messages
// remain) doesn't overwrite the userMsgIDs we just consumed.
func (cs *ChatSession) runReadPump(as *AgentSession, h EventHandler, stop, done chan struct{}) {
	defer func() {
		cs.pumpRunning.Store(false)
		close(done)
	}()

	// Tie context lifecycle to the stop signal so handlers (if any
	// that take ctx) can observe pump cancellation. Currently no
	// handlers consume ctx, but keeping the plumbing in case F-29
	// outbound translation grows cancellable side effects.
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-stop
		cancel()
	}()

	evCh := as.Events()
	if evCh == nil {
		return
	}

	for {
		select {
		case <-stop:
			return
		case ev, ok := <-evCh:
			if !ok {
				// Channel closed: process exited.
				as.SetExited(0)
				return
			}
			if h != nil {
				h(cs.ChatID, as, ev)
			}
			// FSM driving + MessageState emission (F-31).
			switch ev.Kind {
			case agent.EventDone:
				cs.emitMessageStateForCurrentTurn(receipt.StateDone)
				cs.SetIdle()
				_ = cs.OnTurnEnded()
			case agent.EventError:
				cs.emitMessageStateForCurrentTurn(receipt.StateError)
				cs.SetIdle()
				_ = cs.OnTurnEnded()
			default:
				cs.SetBusy()
			}
		}
	}
}

// OnAgentExit is the bridge between AgentSession.ObserveClose (which
// fires when Events() closes) and the ChatSession lifecycle. The
// runtime installs an Observer via SetAgentExitObserver.
//
// Default behavior: when an active AS exits, ChatSession's
// activeAS remains (status now Exited); next /use respawns, next
// message via LookupActiveAgentSession respawns (commit 7 logic).
//
// We do NOT auto-respawn here — that's commit 8c's documented
// "lazy respawn on next message" policy.
type AgentExitObserver func(s *AgentSession)

// SetAgentExitObserver registers a callback for when an active
// AgentSession's process exits (ObserveClose fires). nil clears.
func (cs *ChatSession) SetAgentExitObserver(o AgentExitObserver) {
	cs.mu.Lock()
	cs.exitObserver = o
	cs.mu.Unlock()
}

// triggerExitObserver calls the registered observer (if any).
// Caller must NOT hold cs.mu.
func (cs *ChatSession) triggerExitObserver(as *AgentSession) {
	cs.mu.RLock()
	o := cs.exitObserver
	cs.mu.RUnlock()
	if o != nil {
		o(as)
	}
}

// StartObserveClose launches the observe goroutine for an AgentSession
// (drains its events channel to detect close → triggers exit observer).
//
// This is the runtime's hook to know "the agent died". It's
// separate from StartReadPump because ObserveClose runs even for
// non-active AgentSessions (e.g., after /use leaves an old AS
// detached in the pool).
func (cs *ChatSession) StartObserveClose(as *AgentSession) {
	if as == nil || as.Handle() == nil {
		return
	}
	go func() {
		done := as.ObserveClose()
		<-done
		cs.triggerExitObserver(as)
	}()
}