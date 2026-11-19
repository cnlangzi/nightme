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
	// StartWorkflow starts a workflow and returns the run's id.
	StartWorkflow(ctx context.Context, req MeRequest) (string, error)
	// Reply hands the user's reply to the run that waits for one.
	Reply(ctx context.Context, chatID, text string) error
	// Pause asks the run to stop after the step it is on. The message
	// id is the message the operator asked from: the state of the run
	// hangs under it.
	Pause(ctx context.Context, chatID, messageID string) error
	// Resume runs the step the run is on again. It returns the id of
	// the step the command acted on. The message id is the message the
	// operator asked from: the state of the run hangs under it. The
	// note is the operator's instruction.
	Resume(ctx context.Context, chatID, messageID, note string) (string, error)
	// Next skips the step the run is on. It returns the id of the step
	// the command acted on. The message id is the message the operator
	// asked from: the state of the run hangs under it. The note is the
	// operator's instruction.
	Next(ctx context.Context, chatID, messageID, note string) (string, error)
	// Stop cancels the run of a chat.
	Stop(ctx context.Context, chatID string) error
	// Status posts the state of the run of a chat. The message id is
	// the message the operator asked from: the state hangs under it.
	Status(ctx context.Context, chatID, messageID string) error
	// DisabledReason says why the personal agent cannot run, or
	// returns an empty string when it can.
	DisabledReason() string
}
