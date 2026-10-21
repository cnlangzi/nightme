// Package omp implements a bridge to the omp (oh-my-pi) CLI
// (github.com/can1357/oh-my-pi). omp is a terminal-first coding
// agent that exposes a long-lived `omp --mode rpc` transport
// (LF-delimited JSON commands, responses, and events over real
// stdio pipes — NOT PTY) and a one-shot `omp -p --mode json`
// print mode. This package owns the protocol: spawn, handshake,
// request/response correlation, event translation, and turn-end
// resolution.
//
// omp wire shape (per https://omp.sh):
//
//	command  : {"id":<id>, "type":<command name>, ...fields}
//	response : {"id":<id>, "type":"response", "command":<name>, "success":<bool>, "data":<obj|absent>, "error":<string|absent>}
//	event    : {"type":<event name>, ...event-specific fields}
//	ready    : {"type":"ready", "protocolVersion":1, ...}  (RPC mode opening handshake)
//
// Strict LF framing. stdout carries protocol frames only; omp
// requires stderr to remain on its own pipe so logs do not
// interleave with JSON. See protocol.go for the typed structs
// and rpc.go for the framing/correlation layer.
//
// This package is independent of internal/bridge/pi — omp was
// forked from pi but has diverged (terminal event name is
// `agent_end{isTerminal:true}`, not `agent_settled`; thinking
// arrives on dedicated content blocks; control commands have a
// different vocabulary). Re-using the pi bridge would couple
// the two wires and double-impact every protocol change.
//
// Scope (MVP, issue #443):
//
//   - `omp --mode rpc` long-lived chat session.
//   - `omp -p --mode json --approval-mode yolo` one-shot print mode.
//   - Events: message_start / message_update / message_end /
//     tool_execution_start / tool_execution_end / turn_* / agent_*
//     / agent_end{isTerminal:true} (sole turn-end signal).
//   - Control commands: prompt, abort, new_session.
//
// Out of scope (deliberately):
//
//   - `omp acp` (separate protocol; the PTY-merge in
//     internal/bridge/acp blocks it for omp's "stdout is
//     protocol-only" contract).
//   - set_model / set_thinking_level / steer / follow_up /
//     compact / switch_session.
//   - rpc_chunk / negotiate_protocol v2 large-frame reassembly.
//   - Prompt-image transmission over RPC.
package omp

import (
	"log/slog"
	"os"
	"strings"
)

// bridgeName is stamped on every AgentEvent delivered to the
// runtime. Stable across the package; consumed by the runtime's
// translator to attribute events to the omp bridge.
const bridgeName = "omp"

// ompDebug toggles the bridge's detailed debug logging.
// Default ON so a "why is omp hung" incident produces a usable
// breadcrumb trail in the daemon log; silence with
// NIGHTME_OMP_DEBUG=0 (or "false", "no", "off", case-folded).
//
// Mirrors the pi / copilot / cursor / opencode / codex cLog
// pattern so log scrapers see a consistent component label
// across all bridges.
var ompDebug = ompDebugEnabled()

func ompDebugEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("NIGHTME_OMP_DEBUG"))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// oLog emits an info-level message tagged [omp] (component=
// "omp") when ompDebug is enabled. Info level (not Debug) so
// the default production slog handler surfaces breadcrumbs
// without configuration.
func oLog(msg string, args ...any) {
	if !ompDebug {
		return
	}
	all := make([]any, 0, len(args)+2)
	all = append(all, slog.String("component", "omp"))
	all = append(all, args...)
	slog.Default().Info("[omp] "+msg, all...)
}
