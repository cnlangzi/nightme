package claudecode

import (
	"log/slog"
	"strings"

	"github.com/cnlangzi/nightme/internal/agent"
)

// fatalStderrPrefix is Claude Code's own marker for a failure it
// reports on stderr. The CLI writes the line and stops working on
// the turn — the bridge ends that turn with the CLI's words and
// replaces the CLI.
const fatalStderrPrefix = "[claude-code:"

// isFatalStderr reports whether a stderr line is Claude Code's own
// failure marker.
//
// The marker is a literal prefix, not a word search: Claude Code
// prints ordinary warnings on stderr too, and a turn must not end on
// one of those.
func isFatalStderr(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), fatalStderrPrefix)
}

// reportFatalStderr ends the in-flight turn with the failure the CLI
// reported about itself, and replaces the CLI.
//
// The CLI's words are the reason the turn failed, so the chat shows
// that line instead of a generic "bridge died". The CLI is closed
// afterwards: it reads its configuration at startup, so a CLI that
// failed on a bad configuration only picks up the fixed one after a
// restart. The session's respawn path brings the replacement up with
// the same resume id.
func (d *driver) reportFatalStderr(line string) {
	// The daemon is tearing the bridge down; its exit is expected
	// and the shutdown path reports it.
	select {
	case <-d.closed:
		return
	default:
	}

	// A fatal line outside a turn is not a turn failure: the CLI
	// printed it during the startup handshake, or after the turn
	// already closed. It stays in the log and in the stderr tail.
	if !d.turnActive.Load() {
		return
	}

	// The CLI can print several fatal lines for one failure, and it
	// can exit right after the first one. The chat sees one error.
	if !d.claimError() {
		return
	}

	slog.Warn("claudecode: agent CLI reported its own failure; ending the turn",
		"agent", d.agentName, "workspace", d.workspace, "line", line)

	ev := agent.AgentEvent{
		Kind: agent.EventAgentError,
		Err:  agent.FatalStderrError(line),
		Diagnostic: &agent.BridgeDiagnostic{
			// The CLI has not exited yet — the bridge is about to
			// close it — so there is no exit kind to report. The
			// CLI's own line carries the reason.
			StderrTail: d.stderrTailString(),
			AgentName:  d.agentName,
		},
		AgentName: d.agentName,
		Workspace: d.workspace,
	}
	select {
	case d.events <- ev:
	case <-d.closed:
		return
	}

	// Replace the CLI. Close() runs in its own goroutine because it
	// waits for the exit signal that lifecycle() raises after the
	// sibling pumps drain — and the stderr drain is one of them.
	go func() { _ = d.Close() }()
}

// claimError reserves the bridge's single error slot for a failed
// turn. The fatal-stderr path and the exit path both claim it, so a
// CLI that reports a failure and exits right after produces one
// error, not two.
func (d *driver) claimError() bool {
	return d.errReported.CompareAndSwap(false, true)
}
