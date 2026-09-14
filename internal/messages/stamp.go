package messages

import "github.com/cnlangzi/nightme/internal/agent"

// StampRunResult populates AgentName, Model, SessionID, and Usage
// on msg from runnerName and result. AgentName is set from
// runnerName (caller-resolved, authoritative). Model and SessionID
// fall back to result fields when msg's fields are empty (so
// bridges that pre-stamp these on the OutboundMessage are
// respected). Usage is copied when result.Usage is non-nil.
//
// Used by slash command terminal replies (/review, /gtw pr/commit)
// so the channel footer renders the same StatusBar lines
// (agentbar / usagebar / gitbar) as streaming events do via the
// sink's identity fallback. Mirrors dispatchSinkEvent's
// fallback semantics at outbound/emitter_sink.go:240-269.
func StampRunResult(msg *OutboundMessage, runnerName string, result agent.RunResult) {
	msg.AgentName = runnerName
	if msg.Model == "" {
		msg.Model = result.Model
	}
	if msg.SessionID == "" {
		msg.SessionID = result.SessionID
	}
	if result.Usage != nil {
		msg.Usage = result.Usage
	}
}
