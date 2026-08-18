package wfe

import (
	"context"
	"time"
)

// Runtime is the injection point for all external capabilities.
// Each method is a "channel" from wfe to a specific resource.
// wfe itself has zero I/O; the bot implements this interface
// and wires each method to a real resource (os/exec, the nightme
// channel via bot.Incoming → gateway → ChatSession, the
// ActionRegistry, etc.).
//
// Bot implementation contract:
//   - RunShell: direct os/exec; cwd = run.Workspace
//   - SendPrompt: push msg into bot.Incoming(); block waiting on
//     the per-run reply channel; reply is delivered via bot.Send
//     (called by the gateway's outbound.Emitter).
//   - RunAction: ActionRegistry.Run(spec.Name, spec.With, spec.Env)
//   - Now: time.Now (injected so wfe has no clock dependency)
type Runtime interface {
	// RunShell executes a shell command. spec.Cwd falls back to
	// env["WORKSPACE_CWD"] then "".
	RunShell(ctx context.Context, spec ShellSpec) (*ShellResult, error)

	// SendPrompt pushes a message into bot's channel and blocks
	// until the agent reply comes back via bot.Send. spec.Timeout
	// bounds the wait.
	SendPrompt(ctx context.Context, spec PromptSpec) (*Reply, error)

	// RunAction resolves spec.Name through the ActionRegistry and
	// executes the corresponding action.
	RunAction(ctx context.Context, spec ActionSpec) (*ActionResult, error)

	// Now returns the current time. Injected so wfe has no
	// time.Now() calls (testable).
	Now() time.Time
}

// ShellSpec is the input to RunShell.
type ShellSpec struct {
	Cwd     string
	Command string
	Env     map[string]string
	Shell   string
}

// ShellResult is the output of RunShell.
type ShellResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	Outputs  map[string]string
}

// PromptSpec is the input to SendPrompt.
type PromptSpec struct {
	ChatID  string
	Agent   string
	Prompt  string
	Env     map[string]string
	Timeout time.Duration
}

// Reply is the output of SendPrompt — the agent's reply text and
// any structured outputs.
type Reply struct {
	Text    string
	Outputs map[string]string
}

// ActionSpec is the input to RunAction.
type ActionSpec struct {
	Name string
	With map[string]any
	Env  map[string]string
}

// ActionResult is the output of RunAction.
type ActionResult struct {
	Outputs map[string]any
}

// ExprCtx is the evaluation context for ${{ }} expressions.
// Populated by Tick from RunState and provided to EvalString /
// EvalMap / EvalCond.
type ExprCtx struct {
	Event map[string]any
	Steps map[string]map[string]string
	Needs map[string]map[string]string
	Env   map[string]string
	Now   time.Time
}
