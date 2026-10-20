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

	// Review caps a single /review RunOnce invocation. Identical
	// to Agent (30 min) — review is an LLM-driven audit task with
	// multi-step git inspection, so the LLM-era budget applies.
	//
	// Why we can be generous: /review runs the review in a
	// RunOnce subprocess (see internal/agent/review.go's
	// Review). The 30-min deadline only kills that subprocess;
	// the main chat session is untouched and gets a clean
	// "review timed out" reply. Pre-RunOnce architectures (where
	// /review blocked the main turn) had to be conservative
	// because a stuck review also killed the user's main
	// conversation — that's no longer the case.
	Review = 30 * time.Minute

	// Command caps a single /gtw <subcommand> invocation as a
	// whole — the entire pipeline (pre-hooks + the main RunFix /
	// RunClose / RunCommit / RunPR / RunBack / RunSync /
	// RunNewDraft / RunNewCreate call + post-hooks). Sits above
	// the per-step budgets (CLI / Hook / Agent) so that a hang
	// not covered by any single one of them — e.g. a stuck
	// worktree-move branch, a wedged "inside the agent bridge"
	// call, an agent session that never returns — still gets a
	// wall-clock cap and releases the per-chat run lock.
	//
	// Aligned with Shell / Agent / Hook / Review (30 min) on
	// purpose: a single /gtw fix often runs one or more
	// 30-min agent dispatches internally, so capping the command
	// at less than 30 min would silently truncate real work.
	// Mirrors runCmd's "caller's deadline wins" contract: when
	// the runtime passed a deadline'd ctx, we don't override it.
	Command = 30 * time.Minute

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
