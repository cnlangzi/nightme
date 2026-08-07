// Package chatsession — EventHandler type alias (CS-AS 边界重构 Phase 1).
//
// `EventHandler` is the runtime-installed callback that translates
// each bridge event into an OutboundMessage. The runtime still
// installs it via ChatSession.SetEventHandler, and ChatSession.routeEvent
// (in pump_events.go) calls it for KindAgentEvent wrapping.
//
// The callbacks previously in this file (StartReadPump, StopReadPump,
// HasPump, EventPumpState, AgentExitObserver, SetAgentExitObserver)
// were deleted in T13-T14 when the readpump moved per-AS. The
// runtime no longer references them.
package chatsession

import (
	"github.com/cnlangzi/nightme/internal/agent"
)

// EventHandler is the runtime-installed callback that processes
// each bridge event. Phase 1: still installed via SetEventHandler,
// invoked by ChatSession.routeEvent for KindAgentEvent.
type EventHandler func(chatID string, s *AgentSession, ev agent.AgentEvent, userMsgID string)
