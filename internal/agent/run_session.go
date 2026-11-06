// run_session.go — the shared session-shaped one-shot helper.
//
// RunSession drives ONE turn over a bridge's session path
// (Starter.Start → Agent.SendBlocks → drain → Agent.Close). Every
// one-shot turn in the codebase runs over this transport.
//
// Why the session path and not argv:
//
//   - A prompt passed as an argv element is capped by the platform:
//     Linux caps one argv string at 131072 bytes and Windows caps the
//     whole command line at 32767 characters, so a prompt larger than
//     that fails to spawn ("argument list too long").
//   - Every session transport writes the prompt to a pipe, and pipes
//     have no length limit.
//
// The turn ends on whichever terminator the bridge emits first:
//
//   - EventAgentResult — claudecode, dsh, omp, pi. The reply rides on
//     the result event.
//   - EventAgentDone{Reason: "settled"} — codex and the ACP family
//     (opencode / copilot / cursor), which stream the reply as
//     EventAgentText and signal turn-end separately.
//
// dsh / omp / pi emit the result event first and the settled Done
// right after it, so the result event is their terminator in practice.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// doneReasonSettled is the AgentDoneEvent.Reason a bridge sets when a
// turn ended cleanly but the process stays alive (long-lived session
// bridges). It is the turn-end signal the session drain waits for.
const doneReasonSettled = "settled"

// permissionAllowOnce is the fallback answer for a permission request
// that carries no options. It matches the approval vocabulary the
// bridges use ("Allow once" for approvals, the first question label
// for AskUserQuestion).
const permissionAllowOnce = "Allow once"

// RunSession runs one turn over the agent's session path and returns
// the turn's result.
//
// The shape is Start → SendBlocks → drain → Close: the caller gets a
// single synchronous turn, but the prompt travels over the bridge's
// protocol pipe instead of argv. cfg.Workspace is the agent's cwd for
// the turn.
//
// cfg.SessionID is dropped before Start: a one-shot turn always opens
// a fresh session, so it never splices itself into the caller's
// conversation. ResumeSession is the counterpart that keeps it.
//
// The session is closed before RunSession returns, whether the turn
// succeeded or failed — no session outlives the call.
//
// opts configures per-call behaviour (see RunOnceOption). Today only
// WithEventSink is defined; the sink observes every event the turn
// produced and must not block.
func RunSession(ctx context.Context, s Starter, cfg StartConfig, blocks []ContentBlock, opts ...RunOnceOption) (RunResult, error) {
	cfg.SessionID = ""
	return ResumeSession(ctx, s, cfg, blocks, opts...)
}

// ResumeSession runs one turn over an EXISTING agent session.
//
// The bridge reopens the session named by cfg.SessionID instead of
// opening a fresh one, so the agent keeps the conversation it already
// had — the turn continues it rather than starting over. That is the
// whole difference from RunSession, which drops the session id.
//
// The shape is the same as RunSession's: Start → SendBlocks → drain
// → Close, and the session is closed before the call returns.
//
// A bridge that refuses the requested resume (the session id no
// longer exists on the agent's side) fails the Start with
// ErrResumeUnhealthy rather than silently opening a fresh session.
func ResumeSession(ctx context.Context, s Starter, cfg StartConfig, blocks []ContentBlock, opts ...RunOnceOption) (RunResult, error) {
	name := s.Info().Name

	a, err := s.Start(ctx, cfg)
	if err != nil {
		return RunResult{}, fmt.Errorf("agent %s: spawn: %w", name, err)
	}
	defer a.Close()

	return DrainSession(ctx, a, blocks, ParseRunOnceOptions(opts).OnEvent)
}

// DrainSession sends blocks to a live agent and drains its event
// stream until the turn ends.
//
// The caller owns the agent's lifetime: RunSession closes the agent
// it started, a caller that started the agent itself keeps ownership.
//
// The sink, when set, is called synchronously for every event read off
// the stream — including the startup EventAgentReady. Callers MUST
// ensure the sink is non-blocking (typically a buffered chan + drain
// goroutine, see outbound.StreamRunOnceToEmitter).
func DrainSession(ctx context.Context, a *Agent, blocks []ContentBlock, sink func(AgentEvent)) (RunResult, error) {
	res, err := drainSessionEvents(ctx, a, blocks, sink)
	if res.SessionID == "" {
		// The session id comes from the Ready event observed during
		// the drain. Agent.SessionID() is the public accessor for
		// the same value and is the fallback for a bridge that
		// fills it without a Ready event.
		res.SessionID = a.SessionID()
	}
	return res, err
}

// drainSessionEvents is the event loop behind DrainSession.
//
// A failed turn still reports the session identity it observed: the
// process is gone, but the conversation lives on the agent's side, so
// the session id is what lets the caller reopen that conversation
// later (see ResumeSession).
func drainSessionEvents(ctx context.Context, a *Agent, blocks []ContentBlock, sink func(AgentEvent)) (RunResult, error) {
	name := a.Info.Name

	var sessionID, model string
	readySeen := false
	var streamed []string

	if err := a.SendBlocks(ctx, blocks); err != nil {
		return failedTurn(sessionID, model), fmt.Errorf("agent %s: send: %w", name, err)
	}

	for {
		select {
		case ev, ok := <-a.Events():
			if !ok {
				return failedTurn(sessionID, model), fmt.Errorf(
					"agent %s: event stream closed before the turn ended%s",
					name, sessionAuditFields(sessionID, model))
			}
			if sink != nil {
				sink(ev)
			}
			// The session identity rides on every event. The startup
			// Ready is authoritative; any other event is the fallback
			// for a Ready that was consumed before the drain started.
			if !readySeen {
				if ev.SessionID != "" {
					sessionID = ev.SessionID
				}
				if ev.Model != "" {
					model = ev.Model
				}
			}

			switch ev.Kind {
			case EventAgentReady:
				// The first Ready settles the turn's identity: a
				// later Ready belongs to a re-handshake, not to this
				// turn's audit trail.
				readySeen = true
			case EventAgentText:
				streamed = append(streamed, ev.Text)
			case EventAgentPermission:
				if err := autoAllowSessionPermission(a, ev); err != nil {
					slog.Debug("agent: session auto-allow permission failed",
						"agent", name, "err", err)
				}
			case EventAgentResult:
				if ev.Result == nil {
					return failedTurn(sessionID, model), fmt.Errorf(
						"agent %s: result event with nil payload%s",
						name, sessionAuditFields(sessionID, model))
				}
				if ev.Err != nil {
					return failedTurn(sessionID, model), fmt.Errorf("agent %s: %w%s",
						name, ev.Err, sessionAuditFields(sessionID, model))
				}
				return finishedTurn(name, sessionID, model, ev.Result, streamed, driverTurnText(a))
			case EventAgentDone:
				if ev.Done == nil || ev.Done.Reason != doneReasonSettled {
					// The turn ended without a result: an aborted
					// turn ("interrupted") or the process exiting.
					// No terminator is left to wait for — the
					// session stays alive after a turn, so waiting
					// would stall until the caller's deadline.
					reason := ""
					if ev.Done != nil {
						reason = ev.Done.Reason
					}
					return failedTurn(sessionID, model), fmt.Errorf(
						"agent %s: turn ended without a result event (reason=%q)%s",
						name, reason, sessionAuditFields(sessionID, model))
				}
				return finishedTurn(name, sessionID, model, &AgentResultEvent{
					Usage: ev.Done.Usage,
				}, streamed, driverTurnText(a))
			case EventAgentError:
				if ev.Err != nil {
					return failedTurn(sessionID, model), fmt.Errorf("agent %s: %w%s",
						name, ev.Err, sessionAuditFields(sessionID, model))
				}
				return failedTurn(sessionID, model), fmt.Errorf(
					"agent %s: error event with nil payload%s",
					name, sessionAuditFields(sessionID, model))
			}
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.Canceled) {
				return failedTurn(sessionID, model), fmt.Errorf("agent %s: canceled: %w", name, ctx.Err())
			}
			return failedTurn(sessionID, model), fmt.Errorf("agent %s: %w", name, ctx.Err())
		}
	}
}

// failedTurn builds the RunResult of a turn that ended in failure.
//
// It carries the session identity observed before the failure and
// nothing else: a failed turn has no reply. The session id is the
// part that outlives the failure — the agent-side conversation is
// still there, so the caller can reopen it (ResumeSession) and let
// the agent pick the work up where it stopped.
func failedTurn(sessionID, model string) RunResult {
	return RunResult{SessionID: sessionID, Model: model}
}

// finishedTurn builds the RunResult of a completed turn.
//
// A turn that produced no reply is a failure, not an empty success:
// the caller asked for an answer and got none.
//
// exact is the driver's own report of the turn's text (see TurnText)
// and wins over the streamed join when it holds anything but
// whitespace — the bridge holds the raw reply, so its report is the
// agent's own bytes.
func finishedTurn(name, sessionID, model string, terminal *AgentResultEvent, streamed []string, exact string) (RunResult, error) {
	text := ""
	if strings.TrimSpace(exact) != "" {
		text = exact
	} else {
		text = sessionText(terminal, streamed)
	}
	if text == "" {
		return failedTurn(sessionID, model), fmt.Errorf("agent %s: turn ended without a reply%s",
			name, sessionAuditFields(sessionID, model))
	}
	return RunResult{
		Text:       text,
		Usage:      terminal.Usage,
		SessionID:  sessionID,
		Model:      model,
		DurationMs: terminal.DurationMs,
		Subtype:    terminal.Subtype,
	}, nil
}

// driverTurnText returns the exact text of the turn in progress when
// the bridge's driver can report it (see TurnText). Empty when the
// driver has no such capability, or when the turn produced no text.
func driverTurnText(a *Agent) string {
	if tt, ok := a.Driver().(TurnText); ok {
		return tt.TurnText()
	}
	return ""
}

// sessionText assembles the reply of a finished turn.
//
// It is the fallback for a driver that cannot report the turn's exact
// text (see TurnText).
//
// A bridge may deliver the reply in two pieces: the segments it
// streamed as EventAgentText while the turn ran, and the tail it
// still held when the turn ended (pi / omp / dsh carry the tail on
// the result event; claudecode / codex carry the whole reply on the
// text events and leave the result payload empty). The full reply is
// both, in order.
//
// Each text event is a self-contained piece of the reply — a message
// for codex / pi / omp / dsh / claudecode, a sentence for the ACP
// family — so consecutive pieces are joined by a newline. That is
// also how the chat layer renders them: one outbound message per
// text event.
func sessionText(terminal *AgentResultEvent, streamed []string) string {
	parts := make([]string, 0, len(streamed)+1)
	for _, s := range streamed {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
	}
	if s := strings.TrimSpace(terminal.Text); s != "" {
		parts = append(parts, s)
	}
	return strings.Join(parts, "\n")
}

// autoAllowSessionPermission answers an EventAgentPermission observed
// on the one-shot session drain.
//
// There is no interactive user on this path, so an unanswered
// approval wedges the turn until the bridge's decline watchdog fires.
// Options[0] is the bridge's own default choice — "Allow once" for
// approvals, the first question label for AskUserQuestion — so the
// turn proceeds the way it would with a user present.
func autoAllowSessionPermission(a *Agent, ev AgentEvent) error {
	if ev.Permission == nil {
		return nil
	}
	resp := permissionAllowOnce
	if len(ev.Permission.Options) > 0 {
		resp = ev.Permission.Options[0]
	}
	return a.SendPermission(resp)
}

// sessionAuditFields returns the "[session_id=…] [model=…]" suffix
// used on the session drain's failure paths. Reuses the shared
// FormatSessionID / FormatModel helpers so the format stays
// grep-consistent with the bridges' own audit suffixes.
func sessionAuditFields(sessionID, model string) string {
	return FormatSessionID(sessionID) + FormatModel(model)
}
