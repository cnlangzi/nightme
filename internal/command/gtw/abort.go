// Package gtw — /gtw abort: cancel the /gtw command that currently
// holds the chat's serial chain.
//
// /gtw subcommands are serialised per chat by the run lock in
// Manager.runs, so a /gtw command that never returns blocks every
// later /gtw command in that chat until the timeouts.Command
// (30 min) budget fires. /gtw abort is the user's way out: it
// cancels the chain holder's context (ErrAborted), which tears down
// its in-flight subprocesses, and the commands queued behind it
// start normally.
//
// The abort handler deliberately runs OUTSIDE the run lock —
// taking the lock would queue the abort behind the very command it
// has to cancel.
package gtw

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
)

// abortCmdSpec declares /gtw abort's argv grammar for the shared
// lexer: no flags, no positional args. Routing through
// ParseCmdArgs (rather than a bare `len(input.Args) > 2` check)
// buys the flag/positional distinction for free — `/gtw abort
// --force` says "unknown flag" while `/gtw abort foo` says
// "unexpected positional argument".
var abortCmdSpec = command.CmdSpec{
	Name:    "/gtw abort",
	Usage:   "/gtw abort",
	MinArgs: 0,
	MaxArgs: 0,
}

// aborted reports whether ctx was cancelled by /gtw abort.
//
// context.Cause walks to the root cause, so a context derived
// with context.WithTimeout(ctx, timeouts.Agent) (as dispatchCommit
// does) also reports ErrAborted once its parent is aborted. A run
// that ended because a timeout expired reports
// context.DeadlineExceeded and is not treated as aborted.
func aborted(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), ErrAborted)
}

// runAbort handles `/gtw abort`: cancel the /gtw command currently
// running in this chat.
//
// No RequireActiveCwd preflight — the abort must work in a chat
// that never picked a workspace.
//
// The card is returned as a SlashOutput reply; the runtime emits it
// as one OutCommandReply.
func (f *Factory) runAbort(ctx context.Context, _ *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {
	if _, err := command.ParseCmdArgs(input.Args[2:], abortCmdSpec); err != nil {
		reportOutcome(ctx, command.FailedOutcome("usage", "the command line is not valid"))
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ %v", err),
			Consumed: true,
		}, nil
	}

	sub, elapsed, ok := f.mgr.abortRun(input.ChatID)
	if !ok {
		reportOutcome(ctx, command.NoopOutcome("no /gtw command was running", nil))
		return &command.SlashOutput{
			Reply:    "✨ nothing to abort",
			Consumed: true,
		}, nil
	}
	reportOutcome(ctx, messages.Outcome{
		Status:  messages.OutcomeOK,
		Summary: fmt.Sprintf("aborted /gtw %s", sub),
		Data: map[string]string{
			"subcommand": sub,
			"elapsed":    elapsed.String(),
		},
	})
	return &command.SlashOutput{
		Reply:    abortCard(sub, elapsed),
		Consumed: true,
	}, nil
}

// abortCard renders the /gtw abort card: the gtw card title on the
// first line, the running time as a Format 1 field on the second.
func abortCard(sub string, elapsed time.Duration) string {
	return fmt.Sprintf("✅ aborted `/gtw %s`\n→ was running: %s", sub, formatElapsed(elapsed))
}

// formatElapsed renders a duration the way the abort card shows
// it: seconds below a minute, minutes + seconds below an hour,
// hours + minutes above that.
func formatElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
