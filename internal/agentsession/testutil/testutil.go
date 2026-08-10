// Package testutil is the discoverable namespace for test-only
// helpers that drive an AgentSession's internal state without
// going through Spawn / Submit / Close.
//
// Background:
//
// internal/agentsession exposes the production lifecycle surface
// (NewAgentSession, Spawn, Submit, Close, etc.). Cross-package
// tests in internal/chatsession and internal/command/stop need to
// stand up a "running" AgentSession with a fake bridge handle —
// a setup that has no production equivalent. That machinery used
// to live as 7 methods named `XForTest` directly on
// *agentsession.AgentSession, polluting the production API with
// symbols production code should never call.
//
// This package segregates that machinery under a clear test-only
// namespace. The underlying methods still live on
// *agentsession.AgentSession (Go's package visibility means
// unexported fields are unreachable from a sub-package), but the
// shims here provide:
//
//   - a single discoverable import path for tests
//     (`import "internal/agentsession/testutil"`) instead of
//     grepping the agentsession package for XForTest symbols
//   - a documented "this is for tests" namespace, with the same
//     internal/ rule restricting importers to this module
//   - the consolidated AttachHandle entry point (combines the
//     legacy SetHandle + SetStatus pair under one lock)
//
// Production code MUST NOT import this package or call any
// XForTest method. The heuristic is "if your file is not _test.go,
// you shouldn't be using testutil". A future lint rule could
// enforce this; until then, code review catches it.
package testutil

import (
	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentsession"
)

// AttachHandle wires a pre-built bridge handle into the session
// and marks it as the supplied status under a single lock
// acquisition. This is the canonical "running AgentSession without
// forking a real child process" pattern — every cross-package test
// that drives ChatSession / KillAgent / stop logic starts from
// here.
//
// Caller is responsible for keeping handle alive for the duration
// of any usage.
func AttachHandle(as *agentsession.AgentSession, handle *agent.Agent, status agentsession.Status) {
	as.AttachHandleForTest(handle, status)
}

// SetPID sets the OS PID directly. Used by tests that exercise
// handle-level state (e.g. SIGTERM paths that read as.pid).
func SetPID(as *agentsession.AgentSession, pid int) {
	as.SetPIDForTest(pid)
}

// SetCurrentPrompt installs the in-flight Prompt directly.
// Used by tests that bypass Submit() and want to inspect / mutate
// the AgentSession's view of the active turn.
func SetCurrentPrompt(as *agentsession.AgentSession, p *agentsession.Prompt) {
	as.SetCurrentPromptForTest(p)
}

// SetIsReady toggles the isReady atomic flag. Used by tests that
// gate behavior on AgentSession.IsReady() without exercising the
// real prompt-end FSM.
func SetIsReady(as *agentsession.AgentSession, v bool) {
	as.SetIsReadyForTest(v)
}

// EndPrompt publishes a KindPromptEnded event and clears the
// in-flight Prompt. Wraps the internal endPrompt — same semantics
// as production code's prompt-end handler.
func EndPrompt(as *agentsession.AgentSession, reason agentsession.PromptEndReason) {
	as.EndPromptForTest(reason)
}

// StartReadPump kicks off the bridge-events drain goroutine.
// Idempotent (gated by readpumpStarted). Production code calls
// this through Spawn / Activate; tests call it directly to set up
// the EventBus subscription before publishing fake events.
func StartReadPump(as *agentsession.AgentSession) {
	as.StartReadPumpForTest()
}