// Package chatsession — F-61 watchdog (fallback).
//
// Phase 2.5's synchronous respawn handles the common case (bridge
// crashes, readpump detects, lifecycle → respawn) in ~1s without
// any timer. The watchdog covers the residual failure modes that
// the synchronous path can't:
//
//   1. HungPrompt: bridge alive but stdin/RPC stuck — no
//      KindPromptEnded arrives, no events channel close, no
//      synchronous respawn trigger.
//   2. FastAck:    inbound arrived, but no outbound reaction for
//      T1s. Suggests dispatcher is wedged on a different chat or
//      the bridge is overloaded.
//
// Both timers attach to a single ChatSession. They re-arm on every
// KindPromptEnded / outbound event. On fire they call into the
// prober's markSuspect + lazy respawn path (with the 5min cooldown).
package chatsession

import (
	"log/slog"
	"sync"
	"time"
)

// watchdogTimerConfig (F-61) holds the timeout tunables. Defaults
// match the F-61 §3.5 design: FastAck is short (imperceptible UX
// failure if no ⏳ appears), HungPrompt is generous (depends on
// the agent's worst-case thinking time).
var watchdogTimerConfig = struct {
	FastAck    time.Duration
	HungPrompt time.Duration
}{
	FastAck:    10 * time.Second,
	HungPrompt: 5 * time.Minute,
}

// Watchdog (F-61) is the per-ChatSession diagnostic timer. It does
// NOT own respawn directly — on fire it calls into the AgentProber's
// markSuspect which respects the 5min cooldown and skips the user-
// closed path.
type Watchdog struct {
	cs *ChatSession

	mu            sync.Mutex
	fastAckTimer  *time.Timer
	hungTimer     *time.Timer
	lastOutbound  time.Time
	lastPromptEnd time.Time
}

// newWatchdog constructs a watchdog bound to the chat. Caller
// owns the lifecycle — Start / Stop in pump_events subscribe path.
func newWatchdog(cs *ChatSession) *Watchdog {
	return &Watchdog{cs: cs}
}

// ArmFastAck (F-61) starts (or resets) the FastAck timer.
// Exported because the dispatcher (cmd/nightme/run.go) is in
// a different package and needs to arm the timer on every
// inbound dispatch.
func (w *Watchdog) ArmFastAck() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fastAckTimer != nil {
		w.fastAckTimer.Stop()
	}
	w.fastAckTimer = time.AfterFunc(watchdogTimerConfig.FastAck, w.onFastAck)
}

// ArmHungPrompt (F-61) starts (or resets) the HungPrompt timer.
// Called from TryFlush after a successful Submit (a prompt is
// now in flight). HungPrompt's disarmHungPrompt is called from
// routeEvent's KindPromptEnded (turn complete). Exported for the
// TryFlush call site.
func (w *Watchdog) ArmHungPrompt() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.hungTimer != nil {
		w.hungTimer.Stop()
	}
	w.hungTimer = time.AfterFunc(watchdogTimerConfig.HungPrompt, w.onHungPrompt)
}

// disarmHungPrompt clears the HungPrompt timer. Called from
// routeEvent when KindPromptEnded arrives (turn complete).
func (w *Watchdog) disarmHungPrompt() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.hungTimer != nil {
		w.hungTimer.Stop()
		w.hungTimer = nil
	}
	w.lastPromptEnd = time.Now()
}

// ObserveOutbound clears the FastAck timer. Exported because
// the channel adapter (internal/channel/feishu/adapter.go)
// calls it from the outbound path (add_reaction, send_card).
// When an outbound lands we know FastAck is satisfied —
// nothing is wedged.
func (w *Watchdog) ObserveOutbound() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fastAckTimer != nil {
		w.fastAckTimer.Stop()
		w.fastAckTimer = nil
	}
	w.lastOutbound = time.Now()
}

// Stop cancels both timers. Idempotent.
func (w *Watchdog) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fastAckTimer != nil {
		w.fastAckTimer.Stop()
		w.fastAckTimer = nil
	}
	if w.hungTimer != nil {
		w.hungTimer.Stop()
		w.hungTimer = nil
	}
}

// onFastAck (F-61) is the FastAck fire handler. Marks the active
// AS as suspect for "no_fast_ack". The synchronous respawn path
// won't trigger from a stuck dispatcher (no KindLifecycle event
// fires), so the AgentProber is the only recovery channel. Sets
// SuspectReason="no_fast_ack" + SuspectSince=now; prober sees
// the suspect state on its next tick and gates on cooldown.
func (w *Watchdog) onFastAck() {
	as := w.cs.SelectedAgentSession()
	if as == nil {
		return
	}
	slog.Warn("chatsession: watchdog FastAck fired (no outbound reaction)",
		"chat_id", w.cs.ChatID, "as_id", as.ID)
	as.SetSuspect("no_fast_ack")
}

// onHungPrompt (F-61) is the HungPrompt fire handler. Marks the
// active AS as suspect for "hung_prompt". The prober / a future
// process-ping probe (Phase 4 v2) handles the kill / restart.
func (w *Watchdog) onHungPrompt() {
	as := w.cs.SelectedAgentSession()
	if as == nil {
		return
	}
	slog.Warn("chatsession: watchdog HungPrompt fired (no KindPromptEnded)",
		"chat_id", w.cs.ChatID, "as_id", as.ID)
	as.SetSuspect("hung_prompt")
}

// WatchdogSnapshot is the read-only view used by `nightme doctor`.
// Mirrors AgentProberSnapshot for consistency.
type WatchdogSnapshot struct {
	Active          bool
	FastAckSeconds  int
	HungMins        int
	LastOutbound    time.Time
	LastPromptEnd   time.Time
}

// Snapshot returns the watchdog state.
func (w *Watchdog) Snapshot() WatchdogSnapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	return WatchdogSnapshot{
		FastAckSeconds: int(watchdogTimerConfig.FastAck / time.Second),
		HungMins:       int(watchdogTimerConfig.HungPrompt / time.Minute),
		LastOutbound:   w.lastOutbound,
		LastPromptEnd:  w.lastPromptEnd,
	}
}