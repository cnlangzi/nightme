// Package chatsession — compat stubs (F-54).
//
// F-54 replaced the single-observer EventHandler callback with
// services.Bus[AgentEventEnvelope] (see AgentEventBus). The
// `EventHandler` type alias defined here in v1.3.x is now removed:
// there are no callers left.
//
// Earlier this file held additional stubs (StartReadPump,
// StopReadPump, HasPump, EventPumpState, AgentExitObserver,
// SetAgentExitObserver) that were deleted in T13-T14 when the
// readpump moved per-AS. F-54 finishes the cleanup by removing the
// EventHandler alias; the file remains so any future compat shim
// has a clear home.
package chatsession