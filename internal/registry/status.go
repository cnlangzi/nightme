// Package registry — Session lifecycle Status (v1.2 schema).
//
// The Status enum is shared by the v1.2 ChatSession and AgentSession
// persistence schemas. The previous v0.1 File struct (and the
// dedicated registry.json file) was removed in v1.3; the AgentSession
// store simply keeps the same Status vocabulary so downstream
// consumers (e.g. `nightme list`) can reason about aliveness
// uniformly.
package registry

// Status enumerates the lifecycle states recorded for an AgentSession.
type Status string

const (
	// StatusRunning means the child process is alive and nightme is
	// attached to it.
	StatusRunning Status = "running"

	// StatusDetached means the child process is alive but nightme no
	// longer holds it (e.g. SIGTERM was sent to nightme). The next
	// cleanup sweep can still see it for re-attachment.
	StatusDetached Status = "detached"

	// StatusExited means the child process has terminated. ExitCode
	// carries the exit code (or nil if the process was killed).
	StatusExited Status = "exited"
)
