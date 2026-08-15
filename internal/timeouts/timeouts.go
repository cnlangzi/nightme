// Package timeouts centralises the task-timeout policy for nightme.
//
// All shell / agent / hook / CLI execution paths derive their
// deadlines from here so the numbers stay consistent and any
// future tuning touches one file. There are intentionally no
// user-facing knobs: the values below are the policy.
//
// Two rationales drive every value:
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
	// don't hang the daemon. Applied as an additive safety net in
	// runCmd only when the caller didn't already set a deadline.
	CLI = 5 * time.Minute
)
