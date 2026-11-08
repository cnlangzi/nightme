// Package telemetry implements nightme's agent-CLI client-telemetry
// policy.
//
// nightme spawns agent CLIs as a side effect of a chat session. Those
// CLIs phone home by default — usage analytics, error reporting —
// traffic the user never asked for and nightme cannot see. The policy
// is "off by default": every bridge that knows its CLI's telemetry
// knob adds it to the child it spawns.
//
// The user opts out of the policy in the config file:
//
//	agent_telemetry: true
//
// The daemon reads the config once at startup and hands the choice
// over through Allow. The equivalent environment variable is
// NIGHTME_AGENT_TELEMETRY, which overrides the config file (see
// internal/config).
//
// The knob reaches the spawned child only — the daemon's own
// environment is never modified, so a `claude` or `codex` the user
// runs by hand keeps its own defaults.
package telemetry

import "sync/atomic"

// allowed holds the config-level choice. false — the default — means
// nightme disables the client telemetry of every agent CLI it spawns.
var allowed atomic.Bool

// Allow records whether the agent CLIs nightme spawns may send their
// own client telemetry. Called once at startup with the config value;
// every later read observes it.
func Allow(v bool) { allowed.Store(v) }

// Allowed reports whether the agent CLIs nightme spawns may send their
// own client telemetry.
//
// The knob a bridge applies is best-effort: a CLI that ignores it (an
// older or newer build) keeps its own behaviour, and a CLI that has
// no knob at all simply receives nothing. A knob is never allowed to
// fail the spawn.
func Allowed() bool { return allowed.Load() }

// Env returns the child-environment entries that disable the client
// telemetry of the CLI that owns knobs. Nil when the policy lets the
// CLI send telemetry.
//
// The returned slice is a copy — the caller may append to it.
func Env(knobs ...string) []string {
	if Allowed() {
		return nil
	}
	return append([]string(nil), knobs...)
}

// Args returns the child-argv entries that disable the client
// telemetry of the CLI that owns flags. Nil when the policy lets the
// CLI send telemetry.
//
// The returned slice is a copy — the caller may append to it.
func Args(flags ...string) []string {
	if Allowed() {
		return nil
	}
	return append([]string(nil), flags...)
}
