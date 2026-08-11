// Package stop — /stop's "pause the in-flight turn" logic.
//
// One entry point, singleAgent-scoped to the chat's selectedAgent:
//
//	StopSelectedAgent(c *Cmd) — /stop (no args) path
//
// Distinct from /close in two ways:
//
//   - scope: only the selectedAgent (cs.SelectedAgentSession()),
//     NOT the whole cwd-scoped batch. /close acts on every
//     AgentSession whose Cwd == activeCwd; /stop acts on exactly
//     one entry — the one the user is interacting with right now.
//   - effect: bridge.Stop (signal / RPC / structured cancel)
//     instead of bridge.Close (process termination + pool
//     cleanup). The AgentSession entry is preserved; the chat
//     layer's TryFlush loop picks up the next queued prompt once
//     IsReady flips.
//
// Stop is fire-and-forget. The bridge may emit a clean terminal
// event, exit the child, or neither — the runtime coordinates the
// turn-end → next-submit transition via KindPromptEnded. The /stop
// caller does not block on settle; the reply is sent as soon as
// Stop returns.
//
// Per-bridge behavior (mirroring the agent.Agent.Stop contract):
//
//	pi        — `abort` JSON-RPC; agent_settled event; SessionID kept
//	opencode  — /interrupt HTTP; session.idle; SessionID kept
//	codex     — SIGINT app-server; turn/failed settled; SessionID kept
//	acp       — SIGINT over PTY; native Ctrl-C; SessionID kept
//	claudecode — SIGINT over pipe; best-effort (may exit child)
//	pty       — ErrNotSupported (handler surfaces "use /close")
//
// Daemon shutdown (cmd/nightme/run.go) does NOT call this — agents
// survive nightme restart via the Detached registry state.
package stop

import (
	"context"
	"errors"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
)

// Cmd is the per-call /stop context. CS is the ChatSession whose
// selectedAgent to operate on; Ctx is the parent context for the
// Stop call (forwarded to the bridge).
type Cmd struct {
	CS  *chatsession.ChatSession
	Ctx context.Context
}

// ErrNoContext is returned when Cmd.CS is nil. Validation should
// happen at the handler / gateway layer; this is the safety net.
var ErrNoContext = errors.New("stop: nil ChatSession")

// Result is one row of the /stop reply. It captures what happened
// to the selectedAgent during StopSelectedAgent so the handler can
// render a per-agent status instead of a bare success / failure.
type Result struct {
	Agent  string // e.g. "claude", "codex"
	Cwd    string // e.g. "/code/A"
	Action string // "stopped" | "noop" | "not-supported" | "failed"
	Error  error  // nil on success
}

// StopSelectedAgent halts execution of the in-flight turn on
// c.CS.SelectedAgentSession(). Returns chatsession.ErrNoSelectedAgent
// when selectedAS is nil.
//
// Per-call behavior:
//
//   - selectedAS == nil                 → ErrNoSelectedAgent
//   - selectedAS.Handle() == nil        → Action="noop" (not running)
//   - selectedAS.IsReady() == true      → Action="noop" (no turn)
//   - Stop(ctx) returns nil             → Action="stopped"
//   - Stop(ctx) returns ErrNotSupported → Action="not-supported"
//   - Stop(ctx) returns other error     → Action="failed", Error set
//
// Fire-and-forget: does NOT block on IsReady. The chat layer's
// TryFlush will pick up the next queued prompt once the bridge
// settles; /stop's reply is sent as soon as this function returns.
//
// Pool / selectedAS / agent_sessions.json state is intentionally
// NOT touched here. /stop is "pause execution"; /close is "tear down".
func StopSelectedAgent(c *Cmd) (Result, error) {
	if c == nil || c.CS == nil {
		return Result{}, ErrNoContext
	}
	cs := c.CS
	as := cs.SelectedAgentSession()
	if as == nil {
		return Result{}, chatsession.ErrNoSelectedAgent
	}

	result := Result{Agent: as.Agent, Cwd: as.Cwd}

	if as.IsReady() {
		// No in-flight turn — nothing to stop. Reply as a soft
		// no-op so the user gets feedback ("you sent /stop but
		// there's nothing running"). Distinct from
		// chatsession.ErrNoSelectedAgent, which signals a
		// misconfiguration (no active agent at all).
		result.Action = "noop"
		return result, nil
	}

	h := as.Handle()
	if h == nil {
		// AS exists but the bridge handle has not been spawned
		// (or has already exited). Same semantic as IsReady: no
		// live turn to stop.
		result.Action = "noop"
		return result, nil
	}

	if err := h.Stop(c.Ctx); err != nil {
		if errors.Is(err, agent.ErrNotSupported) {
			result.Action = "not-supported"
			result.Error = err
			return result, nil
		}
		result.Action = "failed"
		result.Error = err
		return result, nil
	}

	result.Action = "stopped"
	return result, nil
}