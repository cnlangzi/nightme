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
//	/me continue [note]                       — hand the control back to the run
//	/me stop                                  — stop the run
//	/me status                                — report where the run stands
//
// The `-i` flag of `/me start` sets how many rounds every job of the
// workflow runs in that run. The workflow file keeps its own counts.
package me

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	mesvc "github.com/cnlangzi/nightme/internal/me"
	"github.com/cnlangzi/nightme/internal/messages"
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
		Usage:    "/me start <workflow> [args...] [-i <rounds>]\n/me reply <text>\n/me pause\n/me continue [note]\n/me show\n/me stop\n/me status",
		Category: "agent",
		Subcommands: []command.SubcommandSpec{
			{Name: "start", Summary: "Start a workflow in this chat. -i sets the rounds."},
			{Name: "reply", Summary: "Answer the question the run asked."},
			{Name: "pause", Summary: "Stop the run after the current step."},
			{Name: "continue", Summary: "Hand the control back to the run."},
			{Name: "show", Summary: "Show the run's state again."},
			{Name: "stop", Summary: "Stop the run of this chat."},
			{Name: "status", Summary: "Report where the run stands."},
		},
	}
}

// startCmdSpec declares /me start's argv grammar. The shared lexer
// (command.ParseCmdArgs) enforces the "unknown flag" / arity contract,
// and "--" hands the rest of the command line to the workflow.
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
	case "continue":
		return f.continueRun(ctx, rt, input)
	case "show":
		return f.show(ctx, rt, input)
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

	name := parsed.Arg(0)
	runID, err := f.ctrl.StartWorkflow(ctx, command.MeRequest{
		Name:       name,
		Args:       parsed.Args[1:],
		Iterations: iterations,
		ChatID:     input.ChatID,
		Cwd:        cwd,
		Owner:      input.UserID,
		Commands:   commandInfos(command.Default().SpecsVisible()),
		Agent:      cs.SelectedAgent(),
	})
	if err != nil {
		chatsession.ReportOutcome(ctx, command.FailedOutcome("error", "the workflow could not be started"))
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}
	data := map[string]string{"workflow": name, "run_id": runID}
	reply := "▶ " + name + " started (" + runID + ")"
	if iterations != nil {
		data["iterations"] = strconv.Itoa(*iterations)
		reply += " · rounds: " + roundsText(*iterations)
	}
	chatsession.ReportOutcome(ctx, messages.Outcome{
		Status:  messages.OutcomeOK,
		Summary: "started " + name,
		Data:    data,
	})
	return command.Reply(ctx, rt, reply), nil
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

// roundsText renders the round count a run was given.
//
// Zero is not "no rounds": it is "until the run ends", the same
// meaning `iterations: 0` has in the file.
func roundsText(rounds int) string {
	if rounds == 0 {
		return "until the run ends"
	}
	return strconv.Itoa(rounds)
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
	if err := f.ctrl.Pause(ctx, input.ChatID); err != nil {
		chatsession.ReportOutcome(ctx, command.FailedOutcome("error", "the run could not be paused"))
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}
	chatsession.ReportOutcome(ctx, messages.Outcome{
		Status:  messages.OutcomeOK,
		Summary: "the run pauses after the current step",
	})
	return command.Reply(ctx, rt, "⏸ The run pauses after the current step."), nil
}

// continueRun hands the control back to a paused run.
func (f *Factory) continueRun(ctx context.Context, rt command.RuntimeServices, input command.SlashInput) (*command.SlashOutput, error) {
	note := continueNote(input.Text)
	if err := f.ctrl.Continue(ctx, input.ChatID, note); err != nil {
		chatsession.ReportOutcome(ctx, command.FailedOutcome("error", "the run could not be continued"))
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}
	chatsession.ReportOutcome(ctx, messages.Outcome{
		Status:  messages.OutcomeOK,
		Summary: "the run goes on",
	})
	return command.Reply(ctx, rt, "▶ The run goes on."), nil
}

// show puts the run's state back in front of the operator.
func (f *Factory) show(ctx context.Context, rt command.RuntimeServices, input command.SlashInput) (*command.SlashOutput, error) {
	if err := f.ctrl.Show(ctx, input.ChatID); err != nil {
		chatsession.ReportOutcome(ctx, command.FailedOutcome("error", "the run could not be shown"))
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}
	chatsession.ReportOutcome(ctx, messages.Outcome{
		Status:  messages.OutcomeOK,
		Summary: "the run's state is shown again",
	})
	return command.Reply(ctx, rt, "📋 The run's state is shown again."), nil
}

// stop stops the run of this chat.
//
// The summary of the session is on its way: the run posts it once it
// has unwound.
func (f *Factory) stop(ctx context.Context, rt command.RuntimeServices, input command.SlashInput) (*command.SlashOutput, error) {
	if err := f.ctrl.Stop(ctx, input.ChatID); err != nil {
		chatsession.ReportOutcome(ctx, command.FailedOutcome("error", "the run could not be stopped"))
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}
	chatsession.ReportOutcome(ctx, messages.Outcome{
		Status:  messages.OutcomeOK,
		Summary: "the run was stopped",
	})
	return command.Reply(ctx, rt, "⏹ The run was stopped — the summary is on its way."), nil
}

// status reports where the run stands.
func (f *Factory) status(ctx context.Context, rt command.RuntimeServices, input command.SlashInput) (*command.SlashOutput, error) {
	st := f.ctrl.Status(input.ChatID)
	if st.Phase == "" || st.Phase == "idle" {
		if st.Error != "" {
			chatsession.ReportOutcome(ctx, command.FailedOutcome("no_agent", st.Error))
			return command.Reply(ctx, rt, "❌ "+st.Error), nil
		}
		chatsession.ReportOutcome(ctx, command.NoopOutcome("this chat has no run in it", nil))
		return command.Reply(ctx, rt, "This chat has no run in it."), nil
	}
	chatsession.ReportOutcome(ctx, messages.Outcome{
		Status:  messages.OutcomeOK,
		Summary: "the run is " + st.Phase,
		Data:    map[string]string{"run_id": st.RunID, "phase": st.Phase},
	})
	return command.Reply(ctx, rt, renderStatus(st)), nil
}

// renderStatus renders the state of a run.
func renderStatus(st command.MeStatus) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s · %s\n", st.Phase, st.RunID)
	if st.Step != "" {
		fmt.Fprintf(&b, "step: %s\n", st.Step)
	}
	if st.Execution != "" {
		fmt.Fprintf(&b, "execution: %s\n", st.Execution)
	}
	if st.Progress != "" {
		fmt.Fprintf(&b, "progress: %s\n", st.Progress)
	}
	if st.Paused {
		fmt.Fprintf(&b, "paused: %s\n", st.PauseReason)
	}
	fmt.Fprintf(&b, "actions: %d taken\n", st.Actions)
	fmt.Fprintf(&b, "system-1: %d decisions, %d tokens\n", st.System1Decisions, st.System1Tokens)
	fmt.Fprintf(&b, "system-2: %d decisions, %d tokens\n", st.System2Decisions, st.System2Tokens)
	if st.Question != "" {
		fmt.Fprintf(&b, "question: %s\n", st.Question)
	}
	if st.Error != "" {
		fmt.Fprintf(&b, "error: %s\n", st.Error)
	}
	return strings.TrimRight(b.String(), "\n")
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

// continueNote returns the note of a /me continue: everything after
// the "continue" token.
func continueNote(text string) string {
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
func (c *controller) StartWorkflow(ctx context.Context, req command.MeRequest) (string, error) {
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
func (c *controller) Pause(ctx context.Context, chatID string) error {
	return c.svc.Pause(ctx, chatID)
}

// Continue implements command.MeController.
func (c *controller) Continue(ctx context.Context, chatID, note string) error {
	return c.svc.Continue(ctx, chatID, note)
}

// Show implements command.MeController.
func (c *controller) Show(ctx context.Context, chatID string) error {
	return c.svc.Show(ctx, chatID)
}

// Stop implements command.MeController.
func (c *controller) Stop(ctx context.Context, chatID string) error {
	return c.svc.Stop(ctx, chatID)
}

// Status implements command.MeController.
func (c *controller) Status(chatID string) command.MeStatus {
	st := c.svc.Status(chatID)
	out := command.MeStatus{
		Phase:            string(st.Phase),
		RunID:            st.RunID,
		Step:             st.Step,
		Execution:        string(st.Execution),
		Actions:          st.Actions,
		System1Decisions: st.System1Decisions,
		System2Decisions: st.System2Decisions,
		System1Tokens:    st.System1Usage.Total(),
		System2Tokens:    st.System2Usage.Total(),
		Question:         st.Question,
		Paused:           st.Paused,
		PauseReason:      st.PauseReason,
		Error:            st.Error,
	}
	if st.Progress.LastBeatAt.IsZero() {
		out.Progress = fmt.Sprintf("%d tools", st.Progress.ToolCount)
	} else {
		out.Progress = fmt.Sprintf("%d tools, last beat %s ago",
			st.Progress.ToolCount, roundDuration(time.Since(st.Progress.LastBeatAt)))
	}
	return out
}

// roundDuration renders a duration in whole seconds.
func roundDuration(d time.Duration) string {
	return fmt.Sprintf("%ds", int(d.Seconds()))
}
