// starter.go — the spawn recipe for the dsh bridge.
//
// Starter is held in agent.Builtins as a singleton per agent name.
// Lifecycle:
//
//	Build-time:    NewStarter → Register(Starter) → Builtins holds *Starter
//	Spawn-time:    Builtins.Get → Starter.Info/Detect/RunOnce → RunResult
//	Chat session:  Builtins.Get → Starter.Start returns error (not implemented)
//
// The runtime state for print-mode lives in runPrintMode (print.go)
// and is re-created per RunOnce call. There is no long-lived child
// process — dsh --profile headless is one-shot, and the bridge
// mirrors that lifecycle.
package dsh

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/cnlangzi/nightme/internal/agent"
)

// Starter is the dsh spawn recipe. Held in agent.Builtins as a
// singleton for the registered name (typically "dsh").
//
// command is the executable name resolved via PATH at RunOnce
// time (exec.LookPath in runPrintMode). The Starter itself does
// not cache the absolute path so user-side PATH edits take effect
// without restarting the daemon.
type Starter struct {
	name    string
	command string
	args    []string
}

// NewStarter constructs the dsh spawn recipe. Entry point used at
// registration time (cmd/nightme/agents.go calls it from init()).
//
// args are the agent's static argv defaults (["--profile",
// "headless"]). Defensively copied.
func NewStarter(name string) *Starter {
	return &Starter{
		name:    name,
		command: name,
		args:    []string{"--profile", "headless"},
	}
}

// Info returns the fixed metadata for this starter. Observable at
// any time; used by `nightme agents` and any other spec-only
// consumer.
//
// Mode is reported as ModeJSONIO because dsh's `--profile headless`
// returns its result on stdout in a structured (single-message)
// form — closest match to the existing mode taxonomy. The bridge
// does not stream events, but Mode is metadata-only and does not
// gate capability checks elsewhere.
func (s *Starter) Info() agent.Info {
	return agent.NewInfo(s.name, agent.ModeJSONIO, s.command, s.args, nil)
}

// Detect verifies the dsh binary resolves on PATH. Called by
// Spawner before RunOnce (or before any future Start path); an
// error aborts invocation with a clear install hint.
//
// PATH check only — we deliberately do NOT probe dsh's internal
// config (`~/.dsh/settings.yaml`, `.credentials.yaml`) because
// per the agent-no-config-tampering principle, nightme does not
// own dsh's configuration lifecycle. If dsh is on PATH but its
// config is broken, the user will see dsh's own error message
// when runPrintMode spawns it.
//
// Note: this only verifies existence — we discard the resolved
// path. PATH resolution at spawn time happens implicitly inside
// exec.CommandContext at cmd.Start(); runPrintMode passes the
// unresolved name to agent.NewCmd and the kernel + Go stdlib do
// the rest. We don't cache the LookPath result because user-side
// PATH edits should take effect without restarting the daemon.
func (s *Starter) Detect() error {
	if _, err := exec.LookPath(s.command); err != nil {
		return fmt.Errorf("dsh: %q not found in PATH. Install via `npm install -g @deepseek-ai/dsh`", s.command)
	}
	return nil
}

// Start acquires a session on the shared dsh host. It does NOT spawn
// a new dsh subprocess — that's cmd/nightme/main.go's responsibility,
// which runs once at daemon boot. Start does:
//   1. Run the resume-or-create handshake via the shared host.RPCClient.
//   2. Subscribe to this sessionId's mux frames via host.Router.
//   3. Emit EventAgentReady with the resolved sessionId + model.
//
// The returned *agent.Agent streams events on its Events channel for
// as long as the host keeps the session attached.
//
// cfg.Workspace is the dsh session's cwd (passed to session.create).
//
// cfg.SessionID, when non-empty, triggers resume: the handshake
// calls POST /api/session.create {sessionId, cwd} which re-attaches
// the existing session (dashboard select). On attach failure
// (session-conflict, transport, mismatched id) we refuse to spawn
// rather than silently mint a new session — see handshakeSession.
//
// PID is 0 in the shared-host architecture — the dsh subprocess
// belongs to the daemon, not to this session. Phase 1.5 (lifecycle
// wrapper) will surface the host PID via host.Client for `/diagnose`
// output; for now agent.Agent displays "shared host" in lieu of
// a per-session pid.
func (s *Starter) Start(ctx context.Context, cfg agent.StartConfig) (*agent.Agent, error) {
	d, err := newDriver(ctx, s, cfg)
	if err != nil {
		return nil, err
	}
	return agent.NewAgent(s.Info(), 0 /* shared-host pid; see comment above */, d.events, d), nil
}

// RunOnce is the one-shot counterpart to Start. Spawns a fresh
// `dsh --profile headless -- "<prompt>"` process, captures the
// final assistant text from stdout, and returns it as RunResult.
//
// dsh headless does NOT support stream-json / structured events
// (unlike codex exec, claude -p, or pi --print). It writes the
// final answer verbatim to stdout and exits. So the body here is
// leaner than the other print-mode bridges: no NDJSON parser, no
// tmpfile, no events chan. Plain spawn + read stdout.
//
// cmd.Dir is set to cfg.Workspace so the agent operates in the
// user's chat workspace (per /cwd). dsh's bash / fs plugins read
// `process.cwd()` (set by cmd.Dir) — we deliberately do NOT set
// DSH_CWD env var because it would be redundant with cmd.Dir and
// would conflict with the agent-no-config-tampering principle
// (which says: don't override agent configuration knobs).
func (s *Starter) RunOnce(ctx context.Context, cfg agent.StartConfig, blocks []agent.ContentBlock) (agent.RunResult, error) {
	result, err := runPrintMode(ctx, s, cfg, blocks)
	if err != nil {
		return agent.RunResult{}, fmt.Errorf("agent %s: %w", s.Info().Name, err)
	}
	return result, nil
}

// Review implements /review for dsh: delegate to shared
// StandardPrompt. dsh's chat agent reads git diff and outputs
// the structured review.
func (s *Starter) Review(ctx context.Context, rc agent.ReviewContext) error {
	return agent.Review(ctx, s, rc)
}

// ListSessions runs a one-shot list-sessions query. Bridge-specific
// extension used by the runtime's resume picker.
//
// Each call spawns and closes a fresh dsh web process — just
// enough to hit POST /api/session.list. The dsh web cold start
// is ~1.5s (per docs/bridge/dsh.md §8.4) so this adds noticeable
// latency to picker interactions; we accept the cost because
// dsh's session.list is daemon-global (not per-CWD) and a stale
// cached list would mislead the user.
//
// The `limit` int parameter is accepted for signature parity with
// the opencode bridge's Starter.ListSessions (opencode/starter.go
// §147-157) so the runtime's picker path can dispatch uniformly
// across bridges — but it is INTENTIONALLY ignored on the wire: the
// 2026-08-15 实机 probe against dsh 0.1.0-rc.6 confirmed that dsh's
// session.list ignores every paging field we tried (limit, pageSize,
// count, max, etc.). The parameter is kept so the runtime can pass
// its preferred default without branching per bridge.
func (s *Starter) ListSessions(ctx context.Context, cfg agent.StartConfig, limit int) ([]Session, error) {
	if cfg.Workspace == "" {
		return nil, fmt.Errorf("dsh: workspace is required")
	}
	a, err := s.Start(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer a.Close()
	d := a.Driver().(interface {
		ListSessions(ctx context.Context) ([]Session, error)
	})
	return d.ListSessions(ctx)
}
