// Package agentstest is the test-only helper surface for
// internal/agentsession. It exposes cross-package affordances for
// driving AgentSession state from tests in other packages
// (chatsession, command/stop, …) without leaking *ForTest methods
// into the production AgentSession API.
//
// Every helper here delegates to a documented public method on
// AgentSession. The package is purely a convenience namespace —
// test code reads `agentstest.EndPrompt(as, agent.PromptEndClean)`
// instead of `as.EndPromptForTest(agent.PromptEndClean)`, so the
// "test-only" intent is obvious at every call site.
//
// Production code MUST NOT import this package.
package agentstest

import (
	"runtime"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentsession"
)

// EndPrompt mirrors the deleted EndPromptForTest: simulate a
// prompt-end by running the public EndPrompt surface. Idempotent
// against no-op when currentPrompt is nil (EndPrompt skips cleanly).
func EndPrompt(as *agentsession.AgentSession, reason agent.PromptEndReason) {
	as.EndPrompt(reason)
}

// StartReadPump mirrors the deleted StartReadPumpForTest: launch
// the readpump loop for tests that bypass Spawn (e.g. tests that
// attach an AgentSession without going through Spawner's Spawn).
func StartReadPump(as *agentsession.AgentSession) {
	as.StartReadPump()
}

// SetCurrentPrompt installs an in-flight Prompt directly, bypassing
// Submit. Used by tests which need to drive endPrompt without paying
// ing for SendBlocks.
func SetCurrentPrompt(as *agentsession.AgentSession, p *agentsession.Prompt) {
	as.SetCurrentPrompt(p)
}

// SetIsReady toggles the isReady atomic flag. Used by tests that
// need a specific ready/not-ready state without driving Submit /
// EndPrompt.
func SetIsReady(as *agentsession.AgentSession, v bool) {
	as.SetIsReady(v)
}

// SetHandle injects a bridge handle directly. Used by tests that
// build an AgentSession around a fake / recording driver without
// going through Spawner's Spawn.
func SetHandle(as *agentsession.AgentSession, h *agent.Agent) {
	as.SetHandle(h)
}

// SetStatus sets the runtime status directly.
func SetStatus(as *agentsession.AgentSession, s agentsession.Status) {
	as.SetStatus(s)
}

// SetPID sets the OS PID directly.
func SetPID(as *agentsession.AgentSession, pid int) {
	as.SetPID(pid)
}

// WaitReady polls IsReady until it returns true or timeout elapses.
// Returns true on ready, false on timeout.
//
// Use after EndPrompt in tests that immediately call into code
// paths gated on IsReady (e.g. ChatSession.TryFlush). On slow CI
// runners (Windows VM) the goroutine scheduler can deliver the
// atomic store's memory visibility after the test thread's next
// line — without this guard, those platforms see a phantom
// !IsReady and TryFlush SKIPs the queued batch.
//
// Bounded wait: 10ms first poll, runtime.Gosched for sub-1ms
// intervals, hard cap at the caller's timeout. The typical
// recording-handle path finishes in well under 1ms; tests with
// deeply-nested mock handles pass with a 1-second budget.
//
// The production path drives TryFlush from the readpump after
// endPrompt returns, so it has its own synchronization — this
// helper exists only to make tests deterministic.
func WaitReady(as *agentsession.AgentSession, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if as.IsReady() {
			return true
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		wait := 10 * time.Millisecond
		if wait > remaining {
			wait = remaining
		}
		if wait > 1*time.Millisecond {
			time.Sleep(wait)
		} else {
			runtime.Gosched()
		}
	}
}
