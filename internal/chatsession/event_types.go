// Package chatsession — typed event payloads (F-54).
//
// These four structs are the domain vocabulary consumed by
// `services.Bus[T]` instances held on ChatSession. Each Bus takes
// one struct type as its type parameter, which gives subscribers
// compile-time signature checking and eliminates the per-event
// "what were the positional args again?" friction.
//
// File lives in the chatsession main package (no subpackage) on
// purpose — the structs depend only on `agent.MessageState` and the
// internal `PromptEndReason` / `Status` types defined here. F-54
// §3.1.
package chatsession

import (
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// AgentEventEnvelope wraps a bridge AgentEvent with the receipt
// anchor and a reference to the AgentSession that produced it.
//
// Replaces the v1.3.x single-observer callback:
//	func(chatID string, s *AgentSession, ev agent.AgentEvent, userMsgID string)
// — which had a flat parameter list, no PromptID, and was
// single-subscriber.
//
// UserMsgID is the receipt anchor (last message id of the in-flight
// Prompt); empty for EventAgentReady and lifecycle events that
// don't anchor.
//
// AgentSession is ALWAYS non-nil in production. The publisher
// (pump_events.go:routeEvent) explicitly guards `if as == nil {
// return }` before constructing the envelope, so subscribers can
// dereference env.AgentSession unconditionally. If you add a new
// publish site that omits this guard, the runtime handler will
// panic — fix the publisher, not the subscriber.
type AgentEventEnvelope struct {
	ChatID       string
	UserMsgID    string
	PromptID     string
	AgentSession *AgentSession
	Event        *agent.AgentEvent
}

// MessageStateEvent fires when a Message transitions stage
// (Queued → Submitted → Dropped). Replaces the v1.3.x
// `onMessageState func(chatID, userMsgID string, state agent.MessageState)`.
//
// ChatID and UserMsgID are denormalized so subscribers (e.g. the
// feishu adapter) don't need to reach back into ChatSession to
// look up the message. State carries the new stage; At is the wall
// time at which the transition was observed.
//
// PromptID and AgentSessionID identify the source Prompt / AS for
// the transition. They're denormalized so subscribers don't need
// to consult `cs.selectedAS` (which only routes input, not source).
// Empty when no Prompt is involved (e.g. queued-only transitions).
type MessageStateEvent struct {
	ChatID         string
	UserMsgID      string
	State          agent.MessageState
	At             time.Time
	PromptID       string
	AgentSessionID string
}

// PromptEndedEvent fires when a Prompt terminates. Replaces the
// v1.3.x `onPromptEnd func(userMsgID string, reason PromptEndReason)`.
//
// The feishu adapter uses this to flip the receipt from 🔄 (running)
// to ✅ (clean) or ❌ (error). UserMsgID is the receipt anchor;
// PromptID identifies the just-ended Prompt for diagnostics /
// writeback; Reason / EndedAt are denormalized for subscribers that
// don't want to reach back into *Prompt.
//
// AgentSessionID identifies the AS that owned the Prompt; empty
// when no AS is involved.
type PromptEndedEvent struct {
	ChatID         string
	UserMsgID      string
	PromptID       string
	Reason         PromptEndReason
	EndedAt        time.Time
	AgentSessionID string
}