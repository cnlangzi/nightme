// Package copilot is the nightme bridge for the GitHub Copilot CLI.
//
// Copilot CLI natively supports ACP via `copilot --acp --stdio`
// (NDJSON over stdio, same wire surface as opencode / cursor).
// Requires Copilot CLI >= 1.0.x — older preview builds (e.g.
// 0.0.361) reject `--acp` with "unknown option" and must be
// upgraded via `npm install -g @github/copilot@latest`.
//
// Two spawn paths (mirroring cursor / opencode):
//
//   - Start (long-lived chat session) → `copilot --allow-all-tools
//     --acp --stdio` under PTY. Reuses the generic ACP bridge.
//     No sessionUpdate / MethodHandler needed — Copilot's wire
//     surface is fully ACP-spec conformant (per
//     docs.github.com/en/copilot/reference/acp-server); the
//     generic fallback covers all standard sessionUpdate kinds.
//
//   - RunOnce (one-shot: /gtw commit, buildAgentPrompt) →
//     `copilot --allow-all-tools -p "prompt" -s`. The process
//     outputs the agent's final response to stdout and exits —
//     no JSON events, no multi-turn, no events channel. `-s`
//     (`--silent`) suppresses the post-answer stats decoration
//     so stdout is just the final text (verified on 1.0.81).
//
// Permission defaults match the other ChatOps bridges (Claude
// --permission-mode bypassPermissions, Codex approvalPolicy=
// never + sandboxMode=danger-full-access, cursor --force
// --trust --sandbox disabled, dsh DSH_PERMISSION_MODE=danger-
// full-access): nightme does NOT rewrite ~/.copilot/ — it only
// injects spawn-time `--allow-all-tools` so the IM session can
// act without per-tool approval prompts. `--allow-all-tools`
// is the canonical long form Copilot exposes (verified against
// `copilot --help`; the older `--yolo` alias is NOT a flag in
// the actual binary).
package copilot

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/bridge/telemetry"
	"github.com/cnlangzi/nightme/internal/proc"
)

// FullAccessArgs is the spawn-time permission flag that turns
// off all per-tool approval prompts. Prepended to both the ACP
// start path and the print-mode RunOnce path.
//
// Per docs/bridge/acp.md §1.1 + agent-no-config-tampering
// principle: this is the ONLY behavior-modifying flag the
// bridge injects. Model, provider, MCP servers, and tool
// allow-lists flow from the user's ~/.copilot/ settings.
//
// Verified against `copilot --help` on v1.0.81 (the currently
// installed GA build on 2026-08-29) and per
// docs.github.com/en/copilot/concepts/agents/copilot-cli/
// autopilot. Older `--yolo` alias mentioned in third-party
// docs is NOT a flag in the actual binary — `--allow-all-
// tools` is the canonical long form.
var FullAccessArgs = []string{"--allow-all-tools"}

// telemetryArgs disables the Copilot CLI's client telemetry for the
// child the bridge spawns. `--disable-client-telemetry` is a
// parent-level flag (verified against `copilot --help` on 1.0.105);
// it disables the telemetry the CLI sends and leaves server-side
// telemetry alone.
//
// The flag is missing from older builds (the 1.0.80 npm package has
// no such flag) and the CLI rejects an unknown flag outright ("error:
// unexpected argument"), so the bridge asks the binary whether it
// accepts the flag before adding it. See supportsTelemetryFlag.
//
// Applied unless the user opts out of nightme's telemetry policy —
// see internal/bridge/telemetry.
var telemetryArgs = []string{"--disable-client-telemetry"}

// acpTransportArgs are the ACP server transport flags. They follow the
// parent-level flags in the spawn argv.
var acpTransportArgs = []string{"--acp", "--stdio"}

// DefaultACPArgs is the argv nightme registers for the
// copilot builtin: `--allow-all-tools` first (parent-level
// permission flag), then `--acp --stdio` (ACP server transport).
//
// The order does not matter for Copilot (all three are top-level
// flags, not subcommand-prefixed), but we put permission before
// transport for symmetry with cursor / opencode.
var DefaultACPArgs = append(
	append([]string(nil), FullAccessArgs...),
	acpTransportArgs...,
)

// bridgeName is the AgentName stamped on every AgentEvent.
// Stable across the package; consumed by the runtime's
// translator to attribute events to the copilot bridge.
const bridgeName = "copilot"

// telemetryFlagSupport caches, per copilot binary, whether it accepts
// --disable-client-telemetry. Keyed by the command path so a
// cfg.Agents override is probed on its own.
var telemetryFlagSupport sync.Map

// supportsTelemetryFlag reports whether the copilot binary at command
// accepts --disable-client-telemetry.
//
// The CLI rejects an unknown flag outright ("error: unexpected
// argument"), so a build that predates the flag would abort the
// spawn instead of merely leaving telemetry on. The probe asks the
// binary itself — `copilot --help` prints the options it accepts —
// and the answer is cached per binary so the cost is paid once per
// process, not once per spawn.
//
// A probe that cannot run (missing binary, a CLI that hangs) counts
// as "not supported": the knob is best-effort, the spawn must not
// fail because of it.
func supportsTelemetryFlag(command string) bool {
	if v, ok := telemetryFlagSupport.Load(command); ok {
		return v.(bool)
	}
	supported := probeTelemetryFlag(command)
	telemetryFlagSupport.Store(command, supported)
	return supported
}

// probeTelemetryFlag runs `copilot --help` and looks for the flag.
//
// The needle carries a single leading dash so it matches both the
// CLI's own help ("--disable-client-telemetry") and a flag listing
// that prints one dash.
//
// WaitDelay is what keeps the probe bounded. On Windows the CLI runs
// under cmd.exe, which spawns node, which spawns the binary: killing
// the direct child leaves the grandchildren holding the output pipe,
// and CombinedOutput would then wait for an EOF that never comes.
// WaitDelay closes the pipes after the grace window, so the probe
// always returns.
//
// The exit status is deliberately ignored: what matters is whether
// the CLI's help lists the flag, and the help is in the output even
// when the pipes had to be force-closed (or the CLI was killed
// after printing it). A CLI that never printed anything — a missing
// binary, a timeout before the first line — has no flag to find.
func probeTelemetryFlag(command string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), telemetryFlagProbeTimeout)
	defer cancel()
	cmd := proc.New(ctx, command, "--help")
	cmd.WaitDelay = telemetryFlagProbeGrace
	out, _ := cmd.CombinedOutput()
	return strings.Contains(string(out), "-disable-client-telemetry")
}

// telemetryFlagProbeTimeout bounds the `copilot --help` probe.
const telemetryFlagProbeTimeout = 10 * time.Second

// telemetryFlagProbeGrace is how long the probe waits for the
// probed process's output pipes to close after the process itself is
// gone. Past it, os/exec closes the pipes and the probe gives up.
const telemetryFlagProbeGrace = 2 * time.Second

// childArgs returns the argv for a spawned copilot child: the
// telemetry knob leads the parent-level flags. Copilot parses its
// options in any order, so the knob lands ahead of `--acp --stdio`.
//
// The knob is dropped when the binary does not accept it — a build
// that predates the flag would refuse to start at all.
func childArgs(args []string, command string) []string {
	if telemetry.Allowed() || !supportsTelemetryFlag(command) {
		return args
	}
	out := make([]string, 0, len(args)+len(telemetryArgs))
	out = append(out, telemetryArgs...)
	return append(out, args...)
}

// copilotDebug toggles the bridge's detailed debug logging.
// Default ON so a "why is copilot stuck" incident produces a
// usable breadcrumb trail. Silence with NIGHTME_COPILOT_DEBUG=0
// (also accepts "false", "no", "off", case-folded).
//
// Mirrors the cursor / opencode / codex / pi cLog pattern so
// log scrapers see a consistent component label across all
// bridges.
var copilotDebug = copilotDebugEnabled()

func copilotDebugEnabled() bool {
	v := os.Getenv("NIGHTME_COPILOT_DEBUG")
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "1", "true", "yes", "on":
		return true
	}
	return false
}

// cLog emits an info-level message tagged [copilot] (component=
// "copilot") when debug is enabled. Mirrors the cursor / opencode
// / codex / pi / claudecode cLog pattern.
func cLog(msg string, args ...any) {
	if !copilotDebug {
		return
	}
	all := make([]any, 0, len(args)+2)
	all = append(all, "component", "copilot")
	all = append(all, args...)
	slog.Default().Info("[copilot] "+msg, all...)
}
