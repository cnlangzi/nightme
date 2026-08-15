// Package timeouts centralises the task-timeout policy for nightme.
//
// Every shell / agent / hook / CLI / reply deadline in the codebase
// derives from here so the numbers stay consistent and any future
// tuning touches one file. There are intentionally no user-facing
// knobs: the values below are the policy.
//
// Two rationales drive the longer budgets (Shell / Agent / Hook):
//
//   - The LLM era. A single fix cycle is multi-turn
//     (read → edit → test → iterate), and tools like
//     `npm install` / `cargo build` routinely exceed 5 minutes.
//     Any budget below 30 min will silently truncate real work.
//   - Bridge without idle signals. Some agent bridges (notably
//     PTY fallback) lack a structured "turn ended" event, so the
//     only way to surface a hung child is a wall-clock cap on
//     the parent context. Without one, the daemon can park on a
//     silent process forever.
//
// Reply is a different category (outbound IM delivery, not
// subprocess execution) and is intentionally short.
package timeouts

import "time"

const (
	// Shell caps a single !cmd user-driven shell invocation.
	// Aligned with Agent because the user's !cmd is often an
	// LLM-driven tool call.
	Shell = 30 * time.Minute

	// Agent caps a single RunOnce invocation (/gtw commit,
	// /gtw pr). 30 min covers the P95 of a Sonnet/Opus-class
	// multi-file fix cycle; beyond that the user has likely
	// /stop'd by hand.
	Agent = 30 * time.Minute

	// Hook caps a single gtw before/after hook execution. Aligned
	// with Agent because hooks are typically build/test gates
	// that LLM agents run.
	Hook = 30 * time.Minute

	// CLI caps git / gh / glab subprocess calls. 5 min is generous
	// for normal network RTT against GitHub / GitLab APIs; anything
	// longer is almost always a network stall — kill and surface,
	// don't hang the daemon. runCmd applies CLI as a fallback only
	// when the caller didn't already set a deadline — caller always
	// wins.
	CLI = 5 * time.Minute

	// Reply caps the outbound IM summary-card send (e.g. shell
	// dispatcher's post-run reply). Different category from the
	// execution budgets above: it's network RTT to the channel,
	// not a long-running subprocess, so 5 s is plenty.
	Reply = 5 * time.Second
)
