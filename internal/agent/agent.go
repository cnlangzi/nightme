// Package agent defines the abstract Agent interface that wraps any
// AI Coding CLI behind a single structured event stream.
//
// Two layers:
//
//   - Agent       — static metadata + factory for sessions (Name, Mode, Detect, Start).
//   - AgentSession — a running instance: events channel + control plane
//     (SendText, SendPermission, Close).
//
// The Mode selector (ACP / SDK / PTY) lets the Bridge pick a backend
// without leaking protocol details to Session Manager. See
// docs/feat/F-09-agent-abstraction.md and F-21-agent-modes.md.
package agent

import (
	"context"
	"errors"
	"fmt"
)

// Mode identifies how the Bridge should communicate with the agent.
type Mode int

const (
	// ModeACP is the preferred mode: Agent Client Protocol over stdio
	// (JSON-RPC). Provides structured events, permission requests, and
	// tool progress out of the box.
	ModeACP Mode = iota

	// ModeSDK is a vendor-specific SDK (e.g. Claude Code Agent SDK).
	// Used when the CLI does not implement ACP.
	ModeSDK

	// ModePTY is the transparent fallback: spawn the CLI in a PTY and
	// pipe bytes. No structured events; ANSI / spinner / progress
	// indicators appear as raw text in the IM channel.
	ModePTY
)

// String renders a Mode for logs and error messages.
func (m Mode) String() string {
	switch m {
	case ModeACP:
		return "acp"
	case ModeSDK:
		return "sdk"
	case ModePTY:
		return "pty"
	default:
		return fmt.Sprintf("mode(%d)", int(m))
	}
}

// EventKind discriminates the payload attached to an AgentEvent.
type EventKind int

const (
	// EventText is a stream of text bytes from the CLI (PTY mode) or a
	// structured assistant message chunk (ACP / SDK modes).
	EventText EventKind = iota

	// EventPermission is a permission request from the agent. The
	// Channel renders it as an interactive UI (e.g. Feishu card with
	// buttons) and the user choice is fed back via SendPermission.
	EventPermission

	// EventToolStart marks the beginning of a tool invocation.
	EventToolStart

	// EventToolEnd marks the end of a tool invocation (success or error).
	EventToolEnd

	// EventDone signals the session has ended cleanly (or with an exit
	// code). After EventDone no further events will be emitted.
	EventDone

	// EventError carries an unrecoverable error during the session.
	EventError
)

// String renders an EventKind for logs.
func (k EventKind) String() string {
	switch k {
	case EventText:
		return "text"
	case EventPermission:
		return "permission"
	case EventToolStart:
		return "tool_start"
	case EventToolEnd:
		return "tool_end"
	case EventDone:
		return "done"
	case EventError:
		return "error"
	default:
		return fmt.Sprintf("event(%d)", int(k))
	}
}

// PermissionRequest is the structured payload for EventPermission.
//
// The Bridge exposes the available options (e.g. "once", "session",
// "reject") and the Channel renders them. The user's choice is sent
// back via ResponseCh exactly once; the Bridge consumes it and
// forwards the decision to the agent.
type PermissionRequest struct {
	// Tool is the tool name (e.g. "Bash", "Write", "Edit").
	Tool string

	// Action is a human-readable description of the action being
	// requested (e.g. "Run command: rm -rf build/").
	Action string

	// Options enumerates the user-selectable choices. The first option
	// is treated as the default / safe choice.
	Options []string

	// ResponseCh receives the user's selected option string. Buffer 1,
	// non-blocking — the Bridge reads it once and proceeds.
	ResponseCh chan string
}

// ToolStartEvent carries metadata when a tool invocation begins.
type ToolStartEvent struct {
	// ID is opaque, stable for the lifetime of one invocation. Pair
	// with ToolEndEvent.ID to correlate start/end.
	ID string

	// Name is the tool name (e.g. "Bash", "Read", "Write").
	Name string

	// Args are the raw or structured arguments passed to the tool.
	Args string
}

// ToolEndEvent carries the result of a finished tool invocation.
type ToolEndEvent struct {
	// ID matches the corresponding ToolStartEvent.ID.
	ID string

	// Name mirrors the tool name for symmetry with ToolStartEvent.
	Name string

	// Err is non-nil when the tool failed.
	Err error
}

// DoneEvent is the terminal payload for a clean session end.
type DoneEvent struct {
	// ExitCode follows Unix convention: 0 = success, non-zero = error.
	// -1 indicates an abnormal termination (e.g. PTY EOF without a
	// child exit code).
	ExitCode int
}

// ErrorEvent carries an unrecoverable error from the session.
type ErrorEvent struct {
	Err error
}

// AgentEvent is the wire format on the AgentSession.Events() channel.
//
// Exactly one payload field is meaningful per Kind:
//
//	EventText       -> Text
//	EventPermission -> Permission
//	EventToolStart  -> ToolStart
//	EventToolEnd    -> ToolEnd
//	EventDone       -> Done
//	EventError      -> Error
type AgentEvent struct {
	Kind EventKind

	// Text is the payload for EventText. Other Kinds leave it empty.
	Text string

	Permission *PermissionRequest
	ToolStart  *ToolStartEvent
	ToolEnd    *ToolEndEvent
	Done       *DoneEvent
	Error      *ErrorEvent
}

// StartConfig is the per-session configuration handed to Agent.Start.
type StartConfig struct {
	// Workspace is the working directory the agent will operate in. It
	// is the session's immutable binding (created by /cwd, consumed by
	// /run).
	Workspace string

	// Args are extra argv items appended after the agent's own
	// defaults. v0.1 typically empty.
	Args []string

	// Env is extra environment variables (key=value form). v0.1
	// typically empty; users configure ANTHROPIC_API_KEY etc. in
	// their shell.
	Env []string
}

// Agent is the static description of a CLI wrapper plus a factory for
// sessions. Implementations are expected to be safe for concurrent use
// after Start returns; the registry stores values by reference.
type Agent interface {
	// Name is the unique identifier used in config and the registry.
	Name() string

	// Mode tells the Bridge which backend to instantiate.
	Mode() Mode

	// Detect verifies the agent is runnable (binary on PATH, SDK
	// available, etc.). Called before Start; an error aborts session
	// creation with a clear "X not found" message to the user.
	Detect() error

	// Start spawns (or attaches to) the agent and returns a live
	// AgentSession. The caller must Close() the session when done.
	Start(ctx context.Context, cfg StartConfig) (AgentSession, error)
}

// AgentSession is the live, per-session handle. Session Manager drives
// it via the Events channel and the three control methods.
type AgentSession interface {
	// Events streams AgentEvent values until the session ends. The
	// channel is closed by the implementation after EventDone or a
	// terminal EventError.
	Events() <-chan AgentEvent

	// PID returns the OS process id of the underlying child, or 0
	// when the session has no process (e.g. SDK backends that do
	// not spawn one). The Session Manager caches this value for
	// /run reconnect logic and for the registry.
	PID() int

	// SendText delivers user input. In PTY mode it is written as bytes
	// to the child's stdin; in ACP/SDK mode it is a structured prompt.
	SendText(text string) error

	// SendPermission responds to the most recent EventPermission. The
	// argument is the option string the user chose. Only meaningful in
	// ACP/SDK modes; PTY mode writes it verbatim to stdin.
	SendPermission(resp string) error

	// Close terminates the session and releases resources. Idempotent.
	Close() error
}

// Errors surfaced by the registry.
var (
	// ErrUnknownAgent is returned by Registry.Get when no agent with
	// the requested name has been registered.
	ErrUnknownAgent = errors.New("agent: unknown agent")
)

// sentinelErr is a small helper so tests can match errors with
// errors.Is without depending on the package-level variable identity.
type sentinelErr struct{ name string }

func (e *sentinelErr) Error() string { return "agent: " + e.name }

// duplicateRegistrationExists is returned by Registry.Register when
// an agent with the same name is already registered. Latest-wins
// semantics: the new instance replaces the old one. The bool returned
// by Register reports whether a replacement happened (useful for tests).
type duplicateError struct{ name string }

func (e *duplicateError) Error() string {
	return "agent: duplicate registration for " + e.name
}
