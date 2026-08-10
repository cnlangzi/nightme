// /stop reply rendering — produces the IM-friendly plain-text
// summary the handler returns via command.Reply.
//
// Sibling to internal/command/kill/format.go (FormatKillResults
// for /kill). Lives in the stop package because the /stop
// surface is fully owned by command/stop (see stop.go doc
// comment). Sharing the byte cap + tail helper would require a
// third "format" package; not worth it for two callers.
//
// Output templates (selected by Action):
//
//   - stopped         — "Stop signal sent to <agent>. Next prompt
//                        will take over."
//   - noop            — "No turn in flight on <agent>."
//   - not-supported   — "<agent> doesn't support /stop; use /kill
//                        instead."
//   - failed          — "Stop failed on <agent>: <err>"
package stop

import "fmt"

// FormatStopResult produces a human-readable summary of a single
// /stop call. The output is suitable for channel.Send (plain
// text, Feishu-renderable).
//
// Distinct from FormatKillResults in that /stop always operates
// on exactly one AgentSession (the selectedAgent), so the reply is
// always one line — no per-entry bucket sort, no byte cap.
func FormatStopResult(r Result) string {
	if r.Agent == "" {
		// Defensive: should not happen when StopSelectedAgent
		// returned a non-zero Result, but render a sane empty
		// string if it does.
		switch r.Action {
		case "noop":
			return "No active agent."
		case "not-supported":
			return "This bridge doesn't support /stop; use /kill instead."
		case "failed":
			return fmt.Sprintf("Stop failed: %v", r.Error)
		default:
			return "Stop signal sent."
		}
	}
	switch r.Action {
	case "stopped":
		return fmt.Sprintf("Stop signal sent to %s @ %s. Next prompt will take over.",
			r.Agent, r.Cwd)
	case "noop":
		return fmt.Sprintf("No turn in flight on %s @ %s.", r.Agent, r.Cwd)
	case "not-supported":
		return fmt.Sprintf("%s @ %s doesn't support /stop; use /kill instead.",
			r.Agent, r.Cwd)
	case "failed":
		if r.Error != nil {
			return fmt.Sprintf("Stop failed on %s @ %s: %v",
				r.Agent, r.Cwd, r.Error)
		}
		return fmt.Sprintf("Stop failed on %s @ %s.", r.Agent, r.Cwd)
	default:
		return fmt.Sprintf("Stop on %s @ %s: %s", r.Agent, r.Cwd, r.Action)
	}
}