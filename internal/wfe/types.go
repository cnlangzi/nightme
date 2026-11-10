// Package wfe — workflow engine. Pure library: parses YAML,
// matches events to workflows, drives a state machine one step at
// a time, evaluates ${{ }} expressions. Zero I/O. Every external
// dependency is injected through the Runtime interface (see
// runtime.go).
package wfe

import "time"

// Workflow is the parsed+validated workflow definition.
type Workflow struct {
	Name       string         `yaml:"name" validate:"required"`
	Workspaces []string       `yaml:"workspaces" validate:"required,min=1"`
	Worker     int            `yaml:"worker"`
	Agent      string         `yaml:"agent"`
	On         Trigger        `yaml:"on" validate:"required"`
	Jobs       map[string]Job `yaml:"jobs" validate:"required,min=1"`

	// Computed at parse time, not from YAML.
	jobOrder []string // topological order of jobs
}

// Job is a named sequence of steps.
type Job struct {
	Needs []string `yaml:"needs"`
	If    string   `yaml:"if"`
	Steps []Step   `yaml:"steps" validate:"required,min=1"`
}

// Step is one of three kinds: run / prompt / use. The corresponding
// field is set; the others are empty.
type Step struct {
	Name            string            `yaml:"name"`
	ID              string            `yaml:"id"`
	If              string            `yaml:"if"`
	Env             map[string]string `yaml:"env"`
	ContinueOnError bool              `yaml:"continue-on-error"`
	Shell           string            `yaml:"shell"` // run only

	// run
	Run string `yaml:"run"`

	// prompt
	Prompt string `yaml:"prompt"`
	Agent  string `yaml:"agent"`

	// use
	Use  string         `yaml:"use"`
	With map[string]any `yaml:"with"`
}

// StepKind is the dispatch category of a Step.
type StepKind int

const (
	StepKindNone StepKind = iota
	StepKindRun
	StepKindPrompt
	StepKindUse
)

func (s Step) Kind() StepKind {
	switch {
	case s.Run != "":
		return StepKindRun
	case s.Prompt != "":
		return StepKindPrompt
	case s.Use != "":
		return StepKindUse
	}
	return StepKindNone
}

// Trigger is the parsed `on:` block. Only one kind is set per workflow.
type Trigger struct {
	Schedule    []ScheduleEntry `yaml:"schedule"`
	PullRequest *PRTrigger      `yaml:"pull_request"`
	Branch      *BranchTrigger  `yaml:"branch"`
	Issue       *EventFilter    `yaml:"issue"`
	Mention     *MentionTrigger `yaml:"mention"`
}

// ScheduleEntry is one cron entry.
type ScheduleEntry struct {
	Cron string `yaml:"cron" validate:"required"`
}

// PRTrigger fires on PR events; optional branch / event filters.
type PRTrigger struct {
	Branches []string `yaml:"branches"`
	Events   []string `yaml:"events"`
}

// BranchTrigger fires on branch push events; optional pattern / event filters.
type BranchTrigger struct {
	Patterns []string `yaml:"patterns"`
	Events   []string `yaml:"events"`
}

// EventFilter is a generic event filter (used for issue).
type EventFilter struct {
	Events []string `yaml:"events"`
}

// MentionTrigger fires when @owner is mentioned in a PR/issue comment
// (sourced via gitProvider). Optional command whitelist; empty means
// any mention fires.
type MentionTrigger struct {
	Commands []string `yaml:"commands"`
}

// Event is what the trigger pipeline produces. Type is one of
// "schedule" / "pull_request" / "branch" / "issue" / "mention".
// Data carries the trigger-specific payload.
type Event struct {
	Kind string
	Time time.Time
	Data map[string]any
}

// RunState is the per-run mutable state, owned by bot and persisted
// to StateStore. Tick is the pure transition: in-state, runtime;
// out-state.
type RunState struct {
	RunID        string                       `json:"run_id"`
	WorkflowName string                       `json:"workflow"`
	Workspace    string                       `json:"workspace"`
	Status       Status                       `json:"status"`
	CurrentJob   string                       `json:"current_job"`
	CurrentStep  string                       `json:"current_step"`
	Env          map[string]string            `json:"env"`
	StepOutputs  map[string]map[string]string `json:"step_outputs"`
	Event        Event                        `json:"event"`
	ChatID       string                       `json:"chat_id"`
	StartedAt    time.Time                    `json:"started_at"`
	UpdatedAt    time.Time                    `json:"updated_at"`
	Attempts     map[string]int               `json:"attempts"`
}

// Status is the lifecycle state of a run.
type Status string

const (
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// DefaultMaxActions is the action budget of a playbook that does not
// set max_actions.
const DefaultMaxActions = 30

// Playbook is a human-written, ordered list of steps that bounds
// what the agent loop may do. It carries no workspace and no
// trigger: the working directory comes from the chat that starts
// the run.
type Playbook struct {
	Name       string         `yaml:"name" json:"name"`
	Args       []string       `yaml:"args" json:"args"`
	Goal       string         `yaml:"goal" json:"goal"`
	MaxActions int            `yaml:"max_actions" json:"max_actions"`
	Instruct   bool           `yaml:"instruct" json:"instruct"`
	Steps      []PlaybookStep `yaml:"steps" json:"steps"`
}

// PlaybookStep is one step of a playbook. Run is the command the
// step performs; Allow lists the extra commands the agent may use
// inside the step; Note explains when to use them; Max caps the
// number of actions the step may take (0 means no cap of its own).
type PlaybookStep struct {
	Run   string   `yaml:"run" json:"run"`
	Note  string   `yaml:"note" json:"note"`
	Allow []string `yaml:"allow" json:"allow"`
	Max   int      `yaml:"max" json:"max"`
}

// OutcomeStatus is the machine-readable verdict of one action.
type OutcomeStatus string

const (
	OutcomeOK         OutcomeStatus = "ok"
	OutcomeNoop       OutcomeStatus = "noop"
	OutcomeIncomplete OutcomeStatus = "incomplete"
	OutcomeFailed     OutcomeStatus = "failed"
	OutcomeCancelled  OutcomeStatus = "cancelled"
	OutcomeTimeout    OutcomeStatus = "timeout"
	OutcomeUnknown    OutcomeStatus = "unknown"
)

// Outcome is the structured result of one action. It mirrors
// messages.Outcome field for field; wfe keeps its own copy so the
// engine stays independent of the message layer.
type Outcome struct {
	Status  OutcomeStatus     `json:"status"`
	Summary string            `json:"summary"`
	Data    map[string]string `json:"data,omitempty"`
}

// succeeded reports whether the outcome means the work was done.
func (o Outcome) succeeded() bool {
	return o.Status == OutcomeOK || o.Status == OutcomeNoop
}

// ActionKind is the kind of action recorded in the history.
type ActionKind string

const (
	// ActionKindRun is a command from one of the playbook's steps.
	ActionKindRun ActionKind = "run"
	// ActionKindInstruct is a free-form prompt to the work agent.
	ActionKindInstruct ActionKind = "instruct"
)

// ActionRecord is one action the run has taken. StepIndex points at
// the playbook step the action served, or -1 when it belongs to no
// step (an instruction).
type ActionRecord struct {
	Kind      ActionKind `json:"kind"`
	Command   string     `json:"command"`
	StepIndex int        `json:"step_index"`
	Outcome   Outcome    `json:"outcome"`
	At        time.Time  `json:"at"`
}
