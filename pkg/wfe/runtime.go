package wfe

import (
	"time"
)

// The engine is a pure library: it parses, validates, computes the
// plan of a run and evaluates expressions. It performs no I/O and
// holds no clock — the runner (internal/me) owns the external
// capabilities and hands the engine their results.
//
// The types below are the vocabulary those capabilities speak.

// ShellSpec is the input to a shell command.
type ShellSpec struct {
	Cwd     string
	Command string
	Env     map[string]string
	Shell   string
}

// ShellResult is the output of a shell command.
type ShellResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	Outputs  map[string]string
}

// ActionSpec is the input to an action.
type ActionSpec struct {
	Name string
	With map[string]any
	Env  map[string]string
}

// ActionResult is the output of an action.
type ActionResult struct {
	Outputs map[string]any
}

// ExprCtx is the evaluation context for ${{ }} expressions.
//
// Populated by the engine from the run state.
type ExprCtx struct {
	Event map[string]any
	Steps map[string]map[string]string
	Needs map[string]map[string]string
	Env   map[string]string
	Args  map[string]string
	Now   time.Time
}
