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
// Starter is reusable: agent.Builtins holds ONE *Starter for the
// "dsh" name; every Spawn call invokes starter.Start and gets back
// an independent *agent.Agent wrapping a fresh driver.
package dsh

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

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
	name    string
	command string
	args    []string
}

// NewStarter constructs the dsh spawn recipe. Entry point used at
// registration time (cmd/nightme/agents.go calls it from init()).
//
// args is nil on purpose: the bridge no longer spawns a subprocess
// directly — the shared-host web is the only spawn path, and it
// lives in the host package. Info().Args mirrors that.
func NewStarter(name string) *Starter {
	return &Starter{
		name:    name,
		command: name,
		args:    nil,
	}
}

// Info returns the fixed metadata for this starter. Observable at
// any time; used by `nightme agents` and any other spec-only
// consumer. Args is nil — see Starter struct doc.
func (s *Starter) Info() agent.Info {
	return agent.NewInfo(s.name, agent.ModeJSONIO, s.command, s.args, nil)
}

// Detect verifies the `dsh` binary resolves on PATH. Called by
// Spawner before Start; an error aborts session creation with a
// clear "dsh not installed" message.
func (s *Starter) Detect() error {
	_, err := exec.LookPath(s.command)
	return err
}

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
func (s *Starter) RunOnce(ctx context.Context, cfg agent.StartConfig, blocks []agent.ContentBlock, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	cfg2 := agent.ParseRunOnceOptions(opts)

	// RunOnce always creates a fresh session, even when the caller
	// passes cfg.SessionID. Strip it before delegating to Start so
	// the handshake path doesn't try to `session.fork`.
	fresh := cfg
	fresh.SessionID = ""

	a, err := s.Start(ctx, fresh)
	if err != nil {
		return agent.RunResult{}, fmt.Errorf("agent %s: spawn: %w", s.Info().Name, err)
	}
	// R4: defer Close drives Router.Unsubscribe + session.cancel +
	// workspace.delete (session.go::Close). No new code needed —
	// the chat-session lifecycle path is reused.
	defer a.Close()
	// dsh's ensureFullAccess posts a `/permission danger-full-access`
	// priming slash on every fresh session, which empirically fires a
	// real model turn (host does NOT intercept leading `/`). Tell
	// drainForRunResult to swallow that priming turn's (Result, Done)
	// pair so it reads the SECOND Result as our actual prompt output.
	// Skip is 2 (Result + Done of the priming turn); 0 when priming
	// failed (no priming events to skip, first terminal is the real one).
	skipPriming := 0
	if d, ok := a.Driver().(*driver); ok && d.permissionPrimed {
		skipPriming = 2
	}
	return drainForRunResult(ctx, a, blocks, cfg2.OnEvent, skipPriming)
}

// Review implements the `/review` slash command for dsh. It
// delegates to agent.ReviewWithOcr — the shared three-tier dispatch
// (docs/REVIEW.md §2): Go-side precompute of diff/file-list/coverage
// is common to all delegate bridges; `ocr delegate` rule matching kicks
// in when ocr is on $PATH (LLM-free), else the built-in rubric.
// ReviewWithOcr drives s.RunOnce with the assembled prompt, so the
// host agent's LLM runs the review in an isolated fresh session.
//
// The /review dispatcher (internal/command/review/cmd.go) wraps the
// returned text in agent.FormatReviewMessage and routes it both to
// the AS (so the main agent can act on "fix the blockers" follow-ups)
// and to the channel emitter (so the user sees findings immediately).
// Because every RunOnce is a fresh sessionId on the shared host
// (and Close archives the session on return), Review cannot leak
// review reasoning back into the main chat session's context.
func (s *Starter) Review(ctx context.Context, cfg agent.StartConfig, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	if agent.OcrAvailable() {
		return agent.ReviewWithOcr(ctx, s, cfg, opts...)
	}
	return agent.ReviewWithPrompt(ctx, s, cfg, opts...)
}


// drainForRunResult is the shared RunOnce / Review drain logic.
// Mirrors acp/starter.go::collectResult in shape — both dsh and acp
// are "live *Agent" bridges where RunOnce is a Start + drain +
// Close variant.
//
// We seed session id + model from the underlying *driver (set during
// handshake) and also accept updates from a subsequent
// EventAgentReady (the first one wins; subsequent Ready events after
// a resume / re-handshake would belong to a different turn's audit
// trail). The final text + per-turn metadata come from
// EventAgentResult. EventAgentDone without a preceding
// EventAgentResult is an error path (claudecode stream-json's
// `result` event never fired) — we surface that rather than silently
// returning an empty RunResult.
//
// Priming-turn skip (dsh-r--fix): ensureFullAccess posts
// `/permission danger-full-access` via session.prompt right after
// session.create, which empirically fires a full model turn on dsh
// 0.1.x (the doc said "host intercepts leading `/`" but the host
// actually passes it to the model). That priming turn completes
// BEFORE our actual prompt's turn, so without a skip drain would
// return the priming turn's "Understood... danger-full-access..." text
// as the review output. RunOnce passes skipPriming=2 to swallow the
// priming turn's trailing (Result, Done) pair and read the SECOND
// Result as the real output. Mock tests pass 0 (no priming turn in
// the mock — ensureFullAccess's slash post returns OK without
// synthesizing a response, so the priming events never arrive and
// a non-zero skip would deadlock).
func drainForRunResult(ctx context.Context, a *agent.Agent, blocks []agent.ContentBlock, sink func(agent.AgentEvent), skipPriming int) (agent.RunResult, error) {
	name := a.Info.Name

	// Seed from the *driver. By the time RunOnce calls drain, the
	// handshake has already set d.sessionID and d.model; reading
	// them here ensures RunResult has them even when the caller
	// (e.g. the runtime readpump or a test) has already drained
	// EventAgentReady off a.Events() before we got here.
	var sessionID, model string
	if d, ok := a.Driver().(*driver); ok {
		sessionID = d.sessionID
		model = d.model
	}
	var readySeen = sessionID != "" // already seeded from driver; suppress duplicate Ready

	if err := a.SendBlocks(ctx, blocks); err != nil {
		return agent.RunResult{}, fmt.Errorf("agent %s: send: %w", name, err)
	}

	// deliverToSink forwards every event to the caller-supplied
	// sink (when set). Synchronous — the bridge's own goroutine
	// drives this. Callers MUST ensure the sink is non-blocking
	// (typically via a buffered chan + drain goroutine, see
	// outbound.StreamRunOnceToEmitter). One-shot flows don't
	// await sink response; sink is observational only.
	deliverToSink := func(ev agent.AgentEvent) {
		if sink != nil {
			sink(ev)
		}
	}

	for {
		select {
		case ev, ok := <-a.Events():
			if !ok {
				return agent.RunResult{}, fmt.Errorf(
					"agent %s: events closed before result%s",
					name, auditFields(sessionID, model))
			}
			deliverToSink(ev)
			switch ev.Kind {
			case agent.EventAgentReady:
				if !readySeen {
					readySeen = true
					sessionID = ev.SessionID
					model = ev.Model
				}
			case agent.EventAgentResult:
				if skipPriming > 0 {
					skipPriming--
					continue
				}
				if ev.Result == nil {
					return agent.RunResult{}, fmt.Errorf(
						"agent %s: result event with nil payload%s",
						name, auditFields(sessionID, model))
				}
				return agent.RunResult{
					Text:       strings.TrimSpace(ev.Result.Text),
					Usage:      ev.Result.Usage,
					SessionID:  sessionID,
					Model:      model,
					DurationMs: ev.Result.DurationMs,
					Subtype:    ev.Result.Subtype,
				}, nil
			case agent.EventAgentDone:
				if skipPriming > 0 {
					skipPriming--
					continue
				}
				return agent.RunResult{}, fmt.Errorf(
					"agent %s: turn ended without result event%s",
					name, auditFields(sessionID, model))
			case agent.EventAgentError:
				if ev.Err != nil {
					return agent.RunResult{}, fmt.Errorf("agent %s: %w", name, ev.Err)
				}
				return agent.RunResult{}, fmt.Errorf(
					"agent %s: error event with nil payload%s",
					name, auditFields(sessionID, model))
			}
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.Canceled) {
				return agent.RunResult{}, fmt.Errorf("agent %s: canceled: %w", name, ctx.Err())
			}
			return agent.RunResult{}, fmt.Errorf("agent %s: %w", name, ctx.Err())
		}
	}
}

// auditFields returns the "[session_id=…] [model=…]" suffix used on
// dsh RunOnce failure paths. Reuses the shared agent.FormatSessionID
// + agent.FormatModel helpers so the format stays grep-consistent
// with acp.appendAuditFields (same signature).
//
// claudecode and pi have their own signature variants (taking
// agent.RunResult directly); dsh follows the acp shape because we
// only need session id + model — Usage/Subtype/etc. are noise on a
// "drain exited early" error.
func auditFields(sessionID, model string) string {
	return agent.FormatSessionID(sessionID) + agent.FormatModel(model)
}
