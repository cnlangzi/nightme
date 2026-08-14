package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// RunOnceDrain sends blocks on a live *Agent and drains Events()
// until the turn ends. Used by structured bridges whose RunOnce
// reuses the long-lived Start path (acp / codex / opencode
// today) — PTY has its own heuristic impl because it has no
// structured result event, and pi / claudecode have independent
// print-mode paths (F-PI-PRINT-001 / F-CLAUDE-PRINT-001) that
// don't go through *Agent at all.
//
// If a future migration moves acp / codex / opencode to their
// own one-shot spawn (each would need a CLI-side equivalent of
// `claude -p`), this helper becomes dead code and can be
// removed together with its tests in runonce_test.go.
//
// Contract: caller is responsible for spawning the live *Agent
// and closing it (defer live.Close() before calling
// RunOnceDrain).
//
// Return semantics:
//   - EventAgentResult: returns RunResult with Text, Usage,
//     SessionID (from the first EventAgentReady), Model (same),
//     DurationMs, Subtype populated.
//   - EventAgentDone with exit 0 but no preceding Result: error
//     "turn ended without result event".
//   - EventAgentDone with non-zero exit: error mentioning exit code.
//   - EventAgentError: wrapped error.
//   - ctx canceled: ctx.Err().
//   - events channel closed without result: error.
//
// The name arg is used purely for error messages — pass the
// Info().Name so users see "claude" not "agent.Agent".
func RunOnceDrain(ctx context.Context, live *Agent, blocks []ContentBlock, name string) (RunResult, error) {
	// Track per-session identity (model + session id) from
	// EventAgentReady so the returned RunResult carries both.
	// First Ready in this turn wins (subsequent Ready events
	// after resume / re-handshake overwrite — those don't
	// belong to this turn's audit trail). If the bridge never
	// emits Ready the fields stay empty.
	var readySeen bool
	var sessionID, model string

	if err := live.SendBlocks(ctx, blocks); err != nil {
		return RunResult{}, fmt.Errorf("agent %s: send: %w", name, err)
	}
	for {
		select {
		case ev, ok := <-live.Events():
			if !ok {
				return RunResult{}, fmt.Errorf("agent %s: event stream closed without result", name)
			}
			switch ev.Kind {
			case EventAgentReady:
				if !readySeen {
					readySeen = true
					sessionID = ev.SessionID
					model = ev.Model
				}
			case EventAgentResult:
				if ev.Result == nil {
					return RunResult{}, fmt.Errorf("agent %s: result event with nil payload", name)
				}
				// TrimSpace so the dispatcher's success card has
				// consistent whitespace across bridges (PTY already
				// trims; structured bridges historically did not).
				return RunResult{
					Text:       strings.TrimSpace(ev.Result.Text),
					Usage:      ev.Result.Usage,
					SessionID:  sessionID,
					Model:      model,
					DurationMs: ev.Result.DurationMs,
					Subtype:    ev.Result.Subtype,
				}, nil
			case EventAgentDone:
				exit := 0
				if ev.Done != nil {
					exit = ev.Done.ExitCode
				}
				if exit != 0 {
					return RunResult{}, fmt.Errorf("agent %s: turn ended with exit %d (no result text)", name, exit)
				}
				return RunResult{}, fmt.Errorf("agent %s: turn ended without result event", name)
			case EventAgentError:
				if ev.Err != nil {
					return RunResult{}, fmt.Errorf("agent %s: %w", name, ev.Err)
				}
				return RunResult{}, fmt.Errorf("agent %s: agent error event with nil payload", name)
			}
			// EventAgentText / EventAgentToolStart / etc. — keep draining.
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.Canceled) {
				return RunResult{}, fmt.Errorf("agent %s: canceled: %w", name, ctx.Err())
			}
			return RunResult{}, fmt.Errorf("agent %s: %w", name, ctx.Err())
		}
	}
}