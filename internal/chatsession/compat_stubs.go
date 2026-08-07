// Package chatsession — Phase 1 transition stubs (readpump.go 删除).
//
// readpump.go was deleted in T13 of the CS-AS 边界重构. The methods
// and types it provided (EventHandler / StartReadPump / StopReadPump /
// HasPump / EventPumpState / AgentExitObserver) had callers in
// cmd/nightme/run.go that depended on them.
//
// Rather than rewrite every caller mid-refactor, this file provides
// minimal stubs so the package still compiles. The T14 follow-up
// (runtime 改流驱动) properly removes these stubs and migrates
// callers to the new EnrichedEvent stream.
//
// All stubs are no-ops (or type-only). Real semantics will move to
// the new pipeline in T14.
package chatsession

import (
	"github.com/cnlangzi/nightme/internal/agent"
)

// EventHandler is the runtime-installed callback that processed
// each bridge event. Phase 1+ replaces it with the EnrichedEvent
// stream; this type alias is kept for source compatibility.
type EventHandler func(chatID string, s *AgentSession, ev agent.AgentEvent, userMsgID string)

// EventPumpState — see EventHandler header. Stub for transitions.
type EventPumpState struct{}

// AgentExitObserver — see EventHandler header. Stub for transitions.
type AgentExitObserver func(as *AgentSession)

// StartReadPump is a no-op stub. The readpump now lives per-AS
// (started by AS.Activate); CS-level starting is meaningless.
func (cs *ChatSession) StartReadPump() error { return nil }

// StopReadPump is a no-op stub. The readpump is now per-AS; CS-level
// stopping no longer applies.
func (cs *ChatSession) StopReadPump() {}

// HasPump is a no-op stub. Always returns false; CS no longer
// tracks pump state (the per-AS readpump owns its own lifecycle).
func (cs *ChatSession) HasPump() bool { return false }

// SetEventHandler is a no-op stub. The runtime-installed callback
// has been replaced by EnrichedEvent consumption (T14). Kept for
// compile-time compatibility with cmd/nightme/run.go.
//  ↑ Note: SetEventHandler is also defined in chatsession.go (line
//  872); the stub here is removed — that declaration already exists.
// SetEventHandler is documented in this file as the migration target.

// SetAgentExitObserver is a no-op stub. F-27 §5.1.5 reserved this
// hook but the runtime never registered one; Phase 1 still leaves
// it on the to-do list (see tasks/wip.md).
func (cs *ChatSession) SetAgentExitObserver(o AgentExitObserver) {}

// AgentExitObserver returns nil. Stub for F-27 §5.1.5.
func (cs *ChatSession) AgentExitObserver() AgentExitObserver { return nil }

// StartObserveClose is a no-op stub. F-27 §5.1.5 reserved the
// observer pattern for respawn-on-death; the runtime never wired
// one. Phase 1 leaves it on the to-do list.
func (cs *ChatSession) StartObserveClose(as *AgentSession) {}
