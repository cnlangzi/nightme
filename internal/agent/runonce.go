package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// RunOnceDrain sends blocks on a live *Agent and drains Events()
// until the turn ends. Used by structured bridges (claudecode /
// codex / pi / acp) — PTY has its own heuristic impl because it
// has no structured result event.
//
// Contract: caller is responsible for spawning the live *Agent and
// closing it (defer live.Close() before calling RunOnceDrain).
//
// Return semantics:
//   - EventAgentResult: returns Result.Text (the agent's final text).
//   - EventAgentDone with exit 0 but no preceding Result: error
//     "turn ended without result event".
//   - EventAgentDone with non-zero exit: error mentioning exit code.
//   - EventAgentError: wrapped error.
//   - ctx canceled: ctx.Err().
//   - events channel closed without result: error.
//
// The name arg is used purely for error messages — pass the
// Info().Name so users see "claude" not "agent.Agent".
func RunOnceDrain(ctx context.Context, live *Agent, blocks []ContentBlock, name string) (string, error) {
	if err := live.SendBlocks(ctx, blocks); err != nil {
		return "", fmt.Errorf("agent %s: send: %w", name, err)
	}
	for {
		select {
		case ev, ok := <-live.Events():
			if !ok {
				return "", fmt.Errorf("agent %s: event stream closed without result", name)
			}
			switch ev.Kind {
			case EventAgentResult:
				if ev.Result == nil {
					return "", fmt.Errorf("agent %s: result event with nil payload", name)
				}
				// TrimSpace so the dispatcher's success card has
				// consistent whitespace across bridges (PTY already
				// trims; structured bridges historically did not).
				return strings.TrimSpace(ev.Result.Text), nil
			case EventAgentDone:
				exit := 0
				if ev.Done != nil {
					exit = ev.Done.ExitCode
				}
				if exit != 0 {
					return "", fmt.Errorf("agent %s: turn ended with exit %d (no result text)", name, exit)
				}
				return "", fmt.Errorf("agent %s: turn ended without result event", name)
			case EventAgentError:
				if ev.Err != nil {
					return "", fmt.Errorf("agent %s: %w", name, ev.Err)
				}
				return "", fmt.Errorf("agent %s: agent error event with nil payload", name)
			}
			// EventAgentText / EventAgentToolStart / etc. — keep draining.
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.Canceled) {
				return "", fmt.Errorf("agent %s: canceled: %w", name, ctx.Err())
			}
			return "", fmt.Errorf("agent %s: %w", name, ctx.Err())
		}
	}
}
