// Package me implements the `/me` slash command.
//
// /me starts a workflow in the chat it is typed in. The run then
// works in that chat: every step it takes is a message of the chat,
// and the chat holds the run's overview.
//
// The commands:
//
//	/me start <workflow> [args...] [-i <rounds>] — start a workflow
//	/me reply <text>                          — answer the question the run asked
//	/me pause                                 — stop the run after the current step
//	/me resume [note]                         — run the step the run is on again
//	/me next [note]                           — skip the step the run is on
//	/me stop                                  — stop the run
//	/me status                                — show the state of the run again
//
// The `-i` flag of `/me start` sets how many rounds every job of the
// workflow runs in that run. The workflow file keeps its own counts.
//
// A workflow's name is the path of its bundle under the workflows
// directory. A name that carries no owner is the workflow of that
// name when there is one, and the project's own namespace otherwise:
// "coder" is the workflow "nightme/coder".
package me

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	mesvc "github.com/cnlangzi/nightme/internal/me"
	"github.com/cnlangzi/nightme/internal/messages"
)

// The summary of a command that answers with the state of the run: the
// state card is the answer.
const (
	// stateShownSummary is the one-line account of that answer.
	stateShownSummary = "the run's state is shown again"
)

// The marks of the two commands that act on a step.
//
// The mark says what the command does with the step: it runs again, or
// it is skipped.
const (
	// resumeMark is the mark of `/me resume`.
	resumeMark = "▶"
	// nextMark is the mark of `/me next`.
	nextMark = "⏭"
)

// Factory is the command.SlashCommandFactory for /me.
type Factory struct {
	// ctrl is the personal agent. A nil value means the daemon runs
	// without one.
	ctrl command.MeController
}

// init self-registers the /me builder. SetDeps (called once by the
// orchestrator) supplies the personal agent.
func init() {
	command.RegisterBuilder(func(d command.Deps) command.SlashCommandFactory {
		return NewFactory(d.Me)
	})
}

// NewFactory constructs a Factory.
func NewFactory(ctrl command.MeController) *Factory {
	return &Factory{ctrl: ctrl}
}

// Spec implements command.SlashCommandFactory.
func (f *Factory) Spec() command.Spec {
	return command.Spec{
		Name:     "me",
		Summary:  "Run a workflow: /me start <workflow> [args...] [-i <rounds>]",
		Usage:    "/me start <workflow> [args...] [-i <rounds>]\n/me reply <text>\n/me pause\n/me resume [note]\n/me next [note]\n/me stop\n/me status",
		Category: "agent",
		Subcommands: []command.SubcommandSpec{
			{Name: "start", Summary: "Start a workflow in this chat. -i sets the rounds. A name without an owner is the project's own: coder is nightme/coder."},
			{Name: "reply", Summary: "Answer the question the run asked."},
			{Name: "pause", Summary: "Stop the run after the current step."},
			{Name: "resume", Summary: "Run the step the run is on again."},
			{Name: "next", Summary: "Skip the step the run is on."},
			{Name: "stop", Summary: "Stop the run of this chat."},
			{Name: "status", Summary: "Show the state of the run again."},
		},
	}
}

// startCmdSpec declares /me start's argv grammar. The shared lexer
// (command.ParseCmdArgs) enforces the "unknown flag" / arity contract,
// and a lone long-form dash token ("--", "—") hands the rest of the
// command line to the workflow.
var startCmdSpec = command.CmdSpec{
	Name:  "/me start",
	Usage: "/me start <workflow> [args...] [-i <rounds>]",
	Flags: map[string]command.FlagSpec{
		"-i":           {Name: "iterations", TakesValue: true},
		"--iterations": {Name: "iterations", TakesValue: true},
	},
	MinArgs: 1,
	MaxArgs: command.UnboundedArgs,
}

// Handle implements command.SlashCommandFactory.
func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices,
	mgr *chatsession.Manager, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {

	if f.ctrl == nil {
		chatsession.ReportOutcome(ctx, command.FailedOutcome("no_agent", "the personal agent is not available"))
		return command.Reply(ctx, rt, "❌ The personal agent is not available in this daemon."), nil
	}
	if len(input.Args) < 2 {
		return command.Reply(ctx, rt, "Usage: "+f.Spec().Usage), nil
	}

	switch strings.ToLower(input.Args[1]) {
	case "start":
		return f.start(ctx, rt, cs, input)
	case "reply":
		return f.reply(ctx, rt, input)
	case "pause":
		return f.pause(ctx, rt, input)
	case "resume":
		return f.resume(ctx, rt, input)
	case "next":
		return f.next(ctx, rt, input)
	case "stop":
		return f.stop(ctx, rt, input)
	case "status":
		return f.status(ctx, rt, input)
	default:
		chatsession.ReportOutcome(ctx, command.FailedOutcome("usage", "the command line is not valid"))
		return command.Reply(ctx, rt, "❌ Unknown /me subcommand: "+input.Args[1]+"\nUsage: "+f.Spec().Usage), nil
	}
}

// start runs a workflow in this chat.
func (f *Factory) start(ctx context.Context, rt command.RuntimeServices,
	cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {

	parsed, iterations, err := parseStartArgs(input.Args[2:])
	if err != nil {
		chatsession.ReportOutcome(ctx, command.FailedOutcome("usage", "the command line is not valid"))
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}
	cwd, failOut := command.RequireActiveCwd(cs)
	if failOut != nil {
		chatsession.ReportOutcome(ctx, command.FailedOutcome("no_cwd", "the command needs an active workspace"))
		return failOut, nil
	}

	runID, workflow, err := f.ctrl.StartWorkflow(ctx, command.MeRequest{
		Name:       parsed.Arg(0),
		Args:       parsed.Args[1:],
		Iterations: iterations,
		ChatID:     input.ChatID,
		Cwd:        cwd,
		Owner:      input.UserID,
		Commands:   commandInfos(command.Default().SpecsVisible()),
		Agent:      cs.SelectedAgent(),
		MessageID:  input.MessageID,
	})
	if err != nil {
		chatsession.ReportOutcome(ctx, command.FailedOutcome("error", "the workflow could not be started"))
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}
	data := map[string]string{"workflow": workflow, "run_id": runID}
	if iterations != nil {
		data["iterations"] = strconv.Itoa(*iterations)
	}
	chatsession.ReportOutcome(ctx, messages.Outcome{
		Status:  messages.OutcomeOK,
		Summary: "started " + workflow,
		Data:    data,
	})
	// The run's own card carries the acknowledgement: one command posts
	// one card.
	return &command.SlashOutput{Consumed: true}, nil
}

// parseStartArgs reads the command line of /me start: the positional
// args, and the round count the line asks for.
//
// A count that is not a whole number >= 0 is refused: the workflow
// file's own counts are the fallback, not a mistyped number. A count
// of zero means the job repeats until the run ends — the same meaning
// `iterations: 0` has in the file.
func parseStartArgs(argv []string) (command.ParsedArgs, *int, error) {
	parsed, err := command.ParseCmdArgs(argv, startCmdSpec)
	if err != nil {
		return command.ParsedArgs{}, nil, err
	}
	if !parsed.Has("iterations") {
		return parsed, nil, nil
	}
	raw := strings.TrimSpace(parsed.Value("iterations"))
	rounds, err := strconv.Atoi(raw)
	if err != nil || rounds < 0 {
		return command.ParsedArgs{}, nil, fmt.Errorf("the round count must be a whole number >= 0, got %q%s",
			raw, startCmdSpec.UsageTail())
	}
	return parsed, &rounds, nil
}

// reply hands the user's answer to the run.
func (f *Factory) reply(ctx context.Context, rt command.RuntimeServices, input command.SlashInput) (*command.SlashOutput, error) {
	text := replyText(input.Text)
	if text == "" {
		chatsession.ReportOutcome(ctx, command.FailedOutcome("usage", "the command line is not valid"))
		return command.Reply(ctx, rt, "Usage: /me reply <text>"), nil
	}
	if err := f.ctrl.Reply(ctx, input.ChatID, text); err != nil {
		chatsession.ReportOutcome(ctx, command.FailedOutcome("error", "the reply could not be delivered"))
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}
	chatsession.ReportOutcome(ctx, messages.Outcome{
		Status:  messages.OutcomeOK,
		Summary: "the reply was delivered",
	})
	return command.Reply(ctx, rt, "✅ The reply was delivered."), nil
}

// pause stops the run after the step it is on.
func (f *Factory) pause(ctx context.Context, rt command.RuntimeServices, input command.SlashInput) (*command.SlashOutput, error) {
	if err := f.ctrl.Pause(ctx, input.ChatID, input.MessageID); err != nil {
		chatsession.ReportOutcome(ctx, command.FailedOutcome("error", "the run could not be paused"))
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}
	chatsession.ReportOutcome(ctx, messages.Outcome{
		Status:  messages.OutcomeOK,
		Summary: "the run pauses after the current step",
	})
	return command.Reply(ctx, rt, "⏸ The run pauses after the current step."), nil
}

// resume runs the step the run is on again.
//
// The command decides at command time what it does with that step: it
// promises a retry only when the step ran and did not succeed. A step
// that is still running, that succeeded, or that the plan runs anyway
// is answered with the reason.
func (f *Factory) resume(ctx context.Context, rt command.RuntimeServices, input command.SlashInput) (*command.SlashOutput, error) {
	note := decisionNote(input.Text)
	d, err := f.ctrl.Resume(ctx, input.ChatID, input.MessageID, note)
	if err != nil {
		chatsession.ReportOutcome(ctx, command.FailedOutcome(refusalReason(err), err.Error()))
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}
	if !d.Act {
		return nothingDone(ctx, rt, resumeMark, d.Step, d.Why, "it was not run again"), nil
	}
	chatsession.ReportOutcome(ctx, messages.Outcome{
		Status:  messages.OutcomeOK,
		Summary: "the step runs again",
		Data:    map[string]string{"step": d.Step},
	})
	return command.Reply(ctx, rt, resumeMark+" The step runs again: "+d.Step+"."), nil
}

// next skips the step the run is on.
//
// The command decides at command time what it does with that step: it
// promises a skip only when there is a step to pass over. A step that
// succeeded is answered with the reason.
func (f *Factory) next(ctx context.Context, rt command.RuntimeServices, input command.SlashInput) (*command.SlashOutput, error) {
	note := decisionNote(input.Text)
	d, err := f.ctrl.Next(ctx, input.ChatID, input.MessageID, note)
	if err != nil {
		chatsession.ReportOutcome(ctx, command.FailedOutcome(refusalReason(err), err.Error()))
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}
	if !d.Act {
		return nothingDone(ctx, rt, nextMark, d.Step, d.Why, "there was nothing to skip"), nil
	}
	chatsession.ReportOutcome(ctx, messages.Outcome{
		Status:  messages.OutcomeOK,
		Summary: "the step is skipped",
		Data:    map[string]string{"step": d.Step},
	})
	return command.Reply(ctx, rt, nextMark+" The step is skipped: "+d.Step+". The run goes on with the workflow."), nil
}

// nothingDone answers a command that had nothing to decide: the reply
// names the step and says what did not happen, so the operator can tell
// that their command did nothing.
//
// The outcome is a noop: the request was valid, and there was nothing
// to do to the step.
func nothingDone(ctx context.Context, rt command.RuntimeServices, mark, step, why, notHappened string) *command.SlashOutput {
	chatsession.ReportOutcome(ctx, command.NoopOutcome(stateShownSummary, map[string]string{"step": step}))
	return command.Reply(ctx, rt, mark+" The step "+step+" "+why+" — "+notHappened+".")
}

// refusalReason returns the stable code of a refusal, or the generic
// one when the failure is not a refusal.
func refusalReason(err error) string {
	if reason := mesvc.RefusalReason(err); reason != "" {
		return reason
	}
	return "error"
}

// stop stops the run of this chat.
//
// The record of the session is on its way: the run posts it once it
// has unwound. It rides on the card of this message, so the stop and
// the record are one card.
func (f *Factory) stop(ctx context.Context, rt command.RuntimeServices, input command.SlashInput) (*command.SlashOutput, error) {
	if err := f.ctrl.Stop(ctx, input.ChatID, input.MessageID); err != nil {
		chatsession.ReportOutcome(ctx, command.FailedOutcome("error", "the run could not be stopped"))
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}
	chatsession.ReportOutcome(ctx, messages.Outcome{
		Status:  messages.OutcomeOK,
		Summary: "the run was stopped",
	})
	return command.Reply(ctx, rt, "⏹ The run was stopped — the record is on its way. The turn of the chat agent goes on."), nil
}

// status shows the state of the run again.
//
// The state hangs under the message the command was typed in: the
// phase of the run, the tree of the rounds and the checklist of the
// round are on the card of that message. A chat without a run has no
// state to show.
func (f *Factory) status(ctx context.Context, rt command.RuntimeServices, input command.SlashInput) (*command.SlashOutput, error) {
	err := f.ctrl.Status(ctx, input.ChatID, input.MessageID)
	if err == nil {
		chatsession.ReportOutcome(ctx, messages.Outcome{
			Status:  messages.OutcomeOK,
			Summary: stateShownSummary,
		})
		// The state is on the card of the command: the command posts no
		// reply of its own.
		return &command.SlashOutput{Consumed: true}, nil
	}
	if !errors.Is(err, mesvc.ErrNoRun) {
		chatsession.ReportOutcome(ctx, command.FailedOutcome("error", "the run's state could not be shown"))
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}
	// The personal agent cannot start a run at all: the reply says why,
	// so the operator knows the difference between a chat that has no
	// run and a daemon that cannot make one.
	if reason := f.ctrl.DisabledReason(); reason != "" {
		chatsession.ReportOutcome(ctx, command.FailedOutcome("no_agent", reason))
		return command.Reply(ctx, rt, "This chat has no run in it. — me is disabled: "+reason), nil
	}
	chatsession.ReportOutcome(ctx, command.NoopOutcome("this chat has no run in it", nil))
	return command.Reply(ctx, rt, "This chat has no run in it."), nil
}

// replyText returns the text of a /me reply: everything after the
// "reply" token, with the spacing the user typed.
//
// The command name and the subcommand are skipped as tokens, so any
// spacing between them is tolerated.
func replyText(text string) string {
	rest := skipToken(text)
	if rest == "" {
		return ""
	}
	rest = skipToken(rest)
	return strings.TrimSpace(rest)
}

// decisionNote returns the note of a /me resume or a /me next:
// everything after the subcommand token.
func decisionNote(text string) string {
	rest := skipToken(text)
	if rest == "" {
		return ""
	}
	rest = skipToken(rest)
	return strings.TrimSpace(rest)
}

// skipToken drops the first whitespace-separated token of a text.
func skipToken(text string) string {
	rest := strings.TrimLeft(text, " \t")
	idx := strings.IndexAny(rest, " \t\r\n")
	if idx < 0 {
		return ""
	}
	return rest[idx:]
}

// controller adapts the personal agent to the command package's
// view of a run.
type controller struct {
	svc *mesvc.Service
}

// NewController adapts a personal agent to the command package's
// view of a run.
func NewController(svc *mesvc.Service) command.MeController {
	if svc == nil {
		return nil
	}
	return &controller{svc: svc}
}

// StartWorkflow implements command.MeController.
func (c *controller) StartWorkflow(ctx context.Context, req command.MeRequest) (string, string, error) {
	return c.svc.StartWorkflow(ctx, mesvc.Request{
		Name:       req.Name,
		Args:       req.Args,
		Iterations: req.Iterations,
		ChatID:     req.ChatID,
		Cwd:        req.Cwd,
		Owner:      req.Owner,
		Commands:   req.Commands,
		Agent:      req.Agent,
		MessageID:  req.MessageID,
	})
}

// commandInfos renders the registered commands as the personal agent
// reads them.
//
// A hidden subcommand stays hidden: the run has no use for it.
func commandInfos(specs []command.Spec) []mesvc.CommandInfo {
	out := make([]mesvc.CommandInfo, 0, len(specs))
	for _, spec := range specs {
		info := mesvc.CommandInfo{
			Name:    spec.Name,
			Summary: spec.Summary,
			Usage:   spec.Usage,
		}
		for _, sub := range spec.Subcommands {
			if sub.Hidden {
				continue
			}
			info.Subcommands = append(info.Subcommands, mesvc.CommandInfo{
				Name:    sub.Name,
				Summary: sub.Summary,
				Usage:   sub.Usage,
			})
		}
		out = append(out, info)
	}
	return out
}

// Reply implements command.MeController.
func (c *controller) Reply(ctx context.Context, chatID, text string) error {
	return c.svc.Reply(ctx, chatID, text)
}

// Pause implements command.MeController.
func (c *controller) Pause(ctx context.Context, chatID, messageID string) error {
	return c.svc.Pause(ctx, chatID, messageID)
}

// Resume implements command.MeController.
func (c *controller) Resume(ctx context.Context, chatID, messageID, note string) (mesvc.OperatorDecision, error) {
	return c.svc.Resume(ctx, chatID, messageID, note)
}

// Next implements command.MeController.
func (c *controller) Next(ctx context.Context, chatID, messageID, note string) (mesvc.OperatorDecision, error) {
	return c.svc.Next(ctx, chatID, messageID, note)
}

// Stop implements command.MeController.
func (c *controller) Stop(ctx context.Context, chatID, messageID string) error {
	return c.svc.Stop(ctx, chatID, messageID)
}

// Status implements command.MeController.
func (c *controller) Status(ctx context.Context, chatID, messageID string) error {
	return c.svc.Status(ctx, chatID, messageID)
}

// DisabledReason implements command.MeController.
func (c *controller) DisabledReason() string {
	return c.svc.DisabledReason()
}
