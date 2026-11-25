package command

import (
	"context"

	"github.com/cnlangzi/nightme/internal/me"
)

// MeRequest is a request to run a workflow.
//
// The types here are the command package's own view of a run: the
// command layer names what a run needs, and the personal agent reads
// it. The command description is the personal agent's own, so the run
// and the model read the same summary.
type MeRequest struct {
	// Name is the workflow's name.
	Name string
	// Args are the arguments of the command line. A token of the form
	// name=value assigns that argument by name; every other token
	// fills the next declared argument, in order.
	Args []string
	// Iterations overrides the round count of every job of the
	// workflow. Nil means the workflow file's own counts stand.
	Iterations *int
	// ChatID is the chat the run works in.
	ChatID string
	// Cwd is the working directory the run works in.
	Cwd string
	// Owner is the user the run speaks for.
	Owner string
	// Commands are the nightme commands the run may issue outside the
	// workflow's steps.
	Commands []me.CommandInfo
	// Agent is the name of the chat's agent.
	Agent string
	// MessageID is the id of the message that started the run. The
	// run's events hang under it: the chat folds them into the card
	// it already holds for that message.
	MessageID string
}

// MeController is the personal agent the /me command drives.
//
// One run is active per chat: the chat that starts a run is the chat
// the run works in, and the chat that gets its overview and answers
// its questions.
type MeController interface {
	// StartWorkflow starts a workflow and returns the run's id and the
	// name of the workflow it started.
	StartWorkflow(ctx context.Context, req MeRequest) (string, string, error)
	// Reply hands the user's reply to the run that waits for one.
	Reply(ctx context.Context, chatID, text string) error
	// Pause asks the run to stop after the step it is on. The message
	// id is the message the operator asked from: the answer of the
	// command and the state of the run go on the card of that message.
	Pause(ctx context.Context, chatID, messageID string) error
	// Resume runs a step of the round again. It returns what the
	// command did with that step: its id, and whether the command
	// recorded a decision. The message id is the message the operator
	// asked from: the answer of the command and the state of the run go
	// on the card of that message. The text is what the operator typed
	// after the subcommand: it names the step the command acts on when
	// its first token is a step of the workflow, and the rest of it is
	// the operator's instruction.
	Resume(ctx context.Context, chatID, messageID, text string) (me.OperatorDecision, error)
	// Next skips the steps of the round up to a step. It returns what
	// the command did with that step: its id, and whether the command
	// recorded a decision. The message id is the message the operator
	// asked from: the answer of the command and the state of the run go
	// on the card of that message. The text is what the operator typed
	// after the subcommand: it names the step the command lands on when
	// its first token is a step of the workflow, and the rest of it is
	// the operator's instruction.
	Next(ctx context.Context, chatID, messageID, text string) (me.OperatorDecision, error)
	// Stop cancels the run of a chat. The message id is the message
	// the operator stopped the run from: the record of the session
	// rides on the card of that message.
	Stop(ctx context.Context, chatID, messageID string) error
	// Status posts the state of the run of a chat. The message id is
	// the message the operator asked from: the state goes on the card
	// of that message.
	Status(ctx context.Context, chatID, messageID string) error
	// DisabledReason says why the personal agent cannot run, or
	// returns an empty string when it can.
	DisabledReason() string
}
