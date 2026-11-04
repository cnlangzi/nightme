// starter.go — the spawn recipe for the dsh bridge.
//
// As of the 2026-08-22 RunOnce/Review migration (F-RUNONCE-WEB), the
// dsh bridge no longer has a print-mode / headless subprocess path.
// Both Starter.Start (long-lived chat) and Starter.RunOnce / Review
// (one-shot) use the same shared-host web sessionId path: each call
// drives a session.create on the shared `dsh --profile web` daemon
// (canonical port 3080; see internal/bridge/dsh/host/lifecycle.go).
//
// dsh is therefore structurally a "live *Agent" bridge for both
// Start and RunOnce: Starter.RunOnce is implemented as
//
//	s.Start(ctx, cfg) + SendBlocks + drain → RunResult + defer a.Close()
//
// where Close() invokes the existing driver.Close path which already
// does Router.Unsubscribe + session.cancel + workspace.delete for
// the driver-owned workspace (session.go::Close). RunOnce never
// spawns its own subprocess and
// never uses cfg.SessionID — every RunOnce is a fresh sessionId on
// the shared host.
//
// The drain is agent.DrainSession, the shared session one-shot helper
// (internal/agent/run_session.go) — the same implementation every
// bridge's session path uses.
//
// Starter is reusable: agent.Builtins holds ONE *Starter for the
// "dsh" name; every Spawn call invokes starter.Start and gets back
// an independent *agent.Agent wrapping a fresh driver.
package dsh

import (
	"context"

	"github.com/cnlangzi/nightme/internal/agent"
)

// Starter is the dsh spawn recipe. Held in agent.Builtins as a
// singleton for the "dsh" name.
//
// args is intentionally nil: Starter.Start does not spawn — it
// looks up the shared *host.Client (lazy-started by
// host.EnsureSharedHost). The actual `dsh --profile web` invocation
// lives in internal/bridge/dsh/host/lifecycle.go::spawnAndWire. We
// surface nil (rather than `["--profile", "web"]`) so Info().Args
// does not mislead callers into thinking Starter is the spawner.
type Starter struct {
	name           string
	command        string
	defaultCommand string
	args           []string
}

// NewStarter constructs the dsh spawn recipe. Entry point used at
// registration time (cmd/nightme/agents.go calls it from init()).
//
// args is nil on purpose: the bridge no longer spawns a subprocess
// directly — the shared-host web is the only spawn path, and it
// lives in the host package. Info().Args mirrors that.
func NewStarter(name string) *Starter {
	return &Starter{
		name:           name,
		command:        name,
		defaultCommand: name,
		args:           nil,
	}
}

// Info returns the fixed metadata for this starter. Observable at
// any time; used by `nightme agents` and any other spec-only
// consumer. Args is nil — see Starter struct doc.
func (s *Starter) Info() agent.Info {
	return agent.NewInfo(s.name, agent.ModeJSONIO, s.command, s.args, nil)
}

// Detect verifies the configured command resolves to an invokable
// binary (absolute path or PATH-relative name). Called by Spawner
// before Start; an error aborts session creation.
func (s *Starter) Detect() error {
	return agent.ResolveCommand(s.command)
}

// Init overrides the executable path used by Detect and
// Info. cfg.Agents path overrides flow through
// agentregistry.Build. The mutation hits the singleton
// held in agent.Builtins; tests that exercise cfg.Agents
// overrides should snapshot and restore via Command().
func (s *Starter) Init(command string) {
	if command == "" {
		s.command = s.defaultCommand
		return
	}
	s.command = command
}

// Command returns the current executable path. Used by tests
// to snapshot Builtins state before mutations from
// agentregistry.Build.
func (s *Starter) Command() string { return s.command }

// Start acquires a session on the shared dsh host. It does NOT spawn
// a new dsh subprocess — that's cmd/nightme/main.go's responsibility,
// which runs once at daemon boot via host.StartSharedHost (and stays
// alive for the daemon's lifetime, with watchdog respawn).
//
// Start does:
//  1. Run the resume-or-create handshake via the shared host.RPCClient.
//     cfg.SessionID is honored: when non-empty, Start dials
//     `session.fork` (strict resume; failures bubble as
//     agent.ErrResumeUnhealthy). When empty, Start creates a
//     fresh session.
//  2. Subscribe to this sessionId's mux frames via host.Router.
//  3. Emit EventAgentReady with the resolved sessionId + model.
//
// The returned *agent.Agent streams events on its Events channel for
// as long as the host keeps the session attached. cfg.Workspace is
// the dsh session's cwd (passed to session.create).
//
// PID is 0 in the shared-host architecture — the dsh subprocess
// belongs to the daemon, not to this session. agent.Agent displays
// "shared host" in lieu of a per-session pid; the host's own PID
// is reachable via host.GetSharedHost().PID() for /diagnose.
func (s *Starter) Start(ctx context.Context, cfg agent.StartConfig) (*agent.Agent, error) {
	d, err := newDriver(ctx, s, cfg)
	if err != nil {
		return nil, err
	}
	return agent.NewAgent(s.Info(), 0 /* shared-host pid; see comment above */, d.events, d), nil
}

// RunOnce is the one-shot counterpart to Start. Structurally it is
//
//	s.Start(ctx, cfg) + SendBlocks + drain → RunResult + defer a.Close()
//
// so every RunOnce call opens a fresh sessionId on the shared host
// (R2 — explicit isolation from the chat session's context, no
// implicit reliance on dsh CLI's "did it read ~/.dsh shared state"
// behaviour the way the old `--profile headless` path did) and
// tears down the driver-owned workspace via workspace.delete as part of
// Close (R4 — dsh web's in-memory store doesn't pile up).
//
// cfg.SessionID is always ignored on RunOnce: every RunOnce is a
// fresh sessionId. Callers that need to resume a specific session
// must use Start directly.
//
// As of 2026-08-22, RunOnce no longer spawns any subprocess. The
// shared dsh web daemon (started once per nightme daemon lifetime)
// serves both this and Start; there is no `--profile headless`
// code path left in this package.
//
// The turn is driven by agent.RunSession — the shared session one-shot
// helper every bridge's session path uses. It opens the fresh session,
// sends the prompt, drains the turn and closes the session.
func (s *Starter) RunOnce(ctx context.Context, cfg agent.StartConfig, blocks []agent.ContentBlock, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	return agent.RunSession(ctx, s, cfg, blocks, opts...)
}

// Review implements the `/review` slash command for dsh. It
// delegates to agent.ReviewWithOcr — the shared three-tier dispatch
// (docs/REVIEW.md §2): Go-side precompute of diff/file-list/coverage
// is common to all delegate bridges; `ocr delegate` rule matching kicks
// in when ocr is on $PATH (LLM-free), else the built-in rubric.
// ReviewWithOcr drives one session turn per rule group with the
// assembled prompt, so the host agent's LLM runs the review in an
// isolated fresh session.
//
// The /review dispatcher (internal/command/review/cmd.go) wraps the
// returned text in agent.FormatReviewMessage and routes it both to
// the AS (so the main agent can act on "fix the blockers" follow-ups)
// and to the channel emitter (so the user sees findings immediately).
// Because every group runs on a fresh sessionId on the shared host
// (and Close archives the session on return), Review cannot leak
// review reasoning back into the main chat session's context.
func (s *Starter) Review(ctx context.Context, cfg agent.StartConfig, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	return agent.ReviewDispatch(ctx, s, cfg, opts...)
}
