// Package heartbeat provides event-driven heartbeat for long-running agent
// sessions. It tracks tick count from stream-json events and surfaces "still
// alive" feedback to a CardRef (Feishu card note line).
//
// Design principles (see docs/feat/F-23-heartbeat.md):
//   - Tick source is events, NOT time. Every stream-json event increments tickCount.
//   - DEAD detection is process-level (signal 0 + stdout EOF), NOT time-threshold.
//   - User has sovereignty over /kill. nightme never auto-kills.
//   - Never fake Claude's internal state ("thinking", "Bash running"). Only
//     render observed facts (event count + timestamp).
package heartbeat

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"syscall"
	"time"
)

// CardRef is the minimal interface heartbeat needs to surface status to a
// Feishu card note line. Implementations should update the existing note
// in-place (same line, replace text), not append new elements.
type CardRef interface {
	UpdateNote(text string) error
}

// DeadReason enumerates why a session is reported dead. The two valid reasons
// are process-level facts; time thresholds, network errors, and rate limits
// are explicitly NOT valid reasons.
type DeadReason int

const (
	DeadProcessExited DeadReason = iota + 1 // process.Signal(0) failed
	DeadStdoutEOF                           // stdout pipe closed
)

// String returns the user-facing error message for a dead reason.
// The message is deterministic (no "may have disconnected" hedging).
func (r DeadReason) String() string {
	switch r {
	case DeadProcessExited:
		return "❌ Claude Code 已退出"
	case DeadStdoutEOF:
		return "❌ Claude Code 输出流已关闭"
	default:
		return "❌ Claude Code 异常"
	}
}

// ProcessProbe abstracts the OS-level liveness checks heartbeat performs.
// Implementations must be safe to call concurrently.
type ProcessProbe interface {
	// Signal sends a signal to the process. signal 0 is used for liveness
	// probing (no actual signal delivered).
	Signal(sig os.Signal) error
	// StdoutEOF reports whether the stdout pipe has been closed by the
	// process (read returned io.EOF).
	StdoutEOF() bool
	// ExitCode returns the process exit code, or -1 if not yet exited.
	ExitCode() int
}

// Heartbeat tracks event ticks and surfaces "still alive" status.
// One Heartbeat per agent session. Lifecycle:
//
//	hb := heartbeat.New(...)
//	go hb.Watch(ctx)
//	// On each stream-json event:
//	hb.OnEvent()
type Heartbeat struct {
	emoji    string
	interval time.Duration // idle check interval (e.g. 2s)
	idleMin  time.Duration // only show "idle" after this threshold (e.g. 30s)

	tickCount atomic.Int64
	lastEvent atomic.Int64 // unix nanos of last OnEvent call
	stopped   atomic.Bool  // set true when DEAD reported or Close called
	process   ProcessProbe
	card      CardRef
	exitCode  atomic.Int32 // captured at DEAD time for the error message
}

// New creates a Heartbeat bound to a card and process probe.
// interval is the idle-check tick (typically 2s).
// idleMin is the threshold below which "idle" is not shown (typically 30s).
func New(emoji string, card CardRef, proc ProcessProbe, interval, idleMin time.Duration) *Heartbeat {
	return &Heartbeat{
		emoji:    emoji,
		interval: interval,
		idleMin:  idleMin,
		process:  proc,
		card:     card,
	}
}

// OnEvent is called on every stream-json event. It increments tickCount
// and updates the card note to "⏳ N · HH:MM:SS".
// Safe to call concurrently.
func (h *Heartbeat) OnEvent() {
	if h.stopped.Load() {
		return
	}
	now := time.Now().UnixNano()
	h.lastEvent.Store(now)
	n := h.tickCount.Add(1)
	h.updateNote(n, false)
}

// Watch runs the idle check + DEAD detection loop until ctx is cancelled or
// the session is reported dead. Call in a goroutine:
//
//	ctx, cancel := context.WithCancel(parent)
//	go hb.Watch(ctx)
//	defer cancel()
func (h *Heartbeat) Watch(ctx context.Context) {
	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if h.stopped.Load() {
				return
			}
			if dead, reason := h.checkDead(); dead {
				h.reportDead(reason)
				return
			}
			h.refreshIdle()
		}
	}
}

// Stop marks the heartbeat as stopped. Future OnEvent calls become no-ops.
// Idempotent.
func (h *Heartbeat) Stop() {
	h.stopped.Store(true)
}

// TickCount returns the current event tick count.
func (h *Heartbeat) TickCount() int64 {
	return h.tickCount.Load()
}

// LastEventAt returns the timestamp of the most recent OnEvent call.
// Returns the zero time if no event has been received.
func (h *Heartbeat) LastEventAt() time.Time {
	ns := h.lastEvent.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// refreshIdle re-renders the card note with "idle Xs/Xm" suffix when the
// last event is older than idleMin. No-op below idleMin to avoid noise.
func (h *Heartbeat) refreshIdle() {
	n := h.tickCount.Load()
	if n == 0 {
		return
	}
	last := h.LastEventAt()
	if last.IsZero() {
		return
	}
	age := time.Since(last)
	if age < h.idleMin {
		return
	}
	h.updateNote(n, true)
}

// updateNote renders and pushes the note text to the card.
// Errors are swallowed (logged at most) — heartbeat is best-effort.
func (h *Heartbeat) updateNote(n int64, idle bool) {
	if h.stopped.Load() {
		return
	}
	last := h.LastEventAt()
	ts := last.Format("15:04:05")
	var text string
	if !idle {
		text = fmt.Sprintf("%s %d · %s", h.emoji, n, ts)
	} else {
		age := time.Since(last)
		text = fmt.Sprintf("%s %d · %s · idle %s", h.emoji, n, ts, formatDuration(age))
	}
	_ = h.card.UpdateNote(text) // best-effort
}

// checkDead performs the two process-level liveness checks.
// Returns (true, reason) only when one of the checks definitively fails.
// Never returns true based on time, rate limits, or network errors.
func (h *Heartbeat) checkDead() (bool, DeadReason) {
	// 1. signal 0 — process still exists?
	if err := h.process.Signal(syscall.Signal(0)); err != nil {
		return true, DeadProcessExited
	}
	// 2. stdout pipe — still open?
	if h.process.StdoutEOF() {
		return true, DeadStdoutEOF
	}
	return false, 0
}

// reportDead renders the dead reason message and stops the heartbeat.
// The exit code is captured if the process has exited, so the user can
// diagnose the failure (e.g. exit code 137 = OOM killed).
func (h *Heartbeat) reportDead(reason DeadReason) {
	if !h.stopped.CompareAndSwap(false, true) {
		return
	}
	msg := reason.String()
	if reason == DeadProcessExited {
		if ec := h.process.ExitCode(); ec >= 0 {
			msg = fmt.Sprintf("%s（exit code: %d）", msg, ec)
		}
	}
	_ = h.card.UpdateNote(msg)
}
