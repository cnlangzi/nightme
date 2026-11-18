package wf

import (
	"context"
	"fmt"
	"net/http"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
)

// Factory implements command.SlashCommandFactory for /wf.
type Factory struct {
	// dir is the workflows directory the install writes into.
	dir string
	// client asks the tiers for the workflow file.
	client *http.Client
	// sources are the tiers the file is fetched from, in order.
	sources []Source
}

// init self-registers the /wf builder. SetDeps (called once by the
// orchestrator) supplies the workflows directory.
func init() {
	command.RegisterBuilder(func(d command.Deps) command.SlashCommandFactory {
		return NewFactory(d.WorkflowsDir)
	})
}

// NewFactory constructs a Factory over the given workflows directory.
func NewFactory(dir string) *Factory {
	return &Factory{
		dir:     dir,
		client:  httpClient(),
		sources: Sources(),
	}
}

// Spec implements command.SlashCommandFactory.
func (f *Factory) Spec() command.Spec {
	return command.Spec{
		Name:     "wf",
		Summary:  "Install a published workflow: /wf install <owner>/<name>",
		Usage:    "/wf install <owner>/<name>",
		Category: "agent",
		Subcommands: []command.SubcommandSpec{
			{
				Name:    "install",
				Summary: "Install a published workflow by its address.",
				Usage:   "/wf install <owner>/<name>",
			},
		},
	}
}

// installCmdSpec declares /wf install's argv grammar.
//
// The shared lexer enforces the contract: the subcommand is required,
// the address is one argument, and a flag is an error.
var installCmdSpec = command.CmdSpec{
	Name:       "/wf install",
	Usage:      "/wf install <owner>/<name>",
	Subcommand: "install",
	MinArgs:    1,
	MaxArgs:    1,
}

// Handle implements command.SlashCommandFactory.
//
// The install's result reaches the operator through the checklist
// alone: the command's SlashOutput carries no reply text.
func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices,
	mgr *chatsession.Manager, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {

	// The command name is the first token; the rest is the command
	// line the lexer reads.
	argv := input.Args
	if len(argv) > 0 {
		argv = argv[1:]
	}
	parsed, err := command.ParseCmdArgs(argv, installCmdSpec)
	if err != nil {
		chatsession.ReportOutcome(ctx, command.FailedOutcome("usage", "the command line is not valid"))
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ %v", err),
			Consumed: true,
		}, nil
	}

	if f.dir == "" {
		chatsession.ReportOutcome(ctx, command.FailedOutcome("no_workflows_dir",
			"the daemon has no workflows directory to install into"))
		return &command.SlashOutput{
			Reply:    "❌ This daemon has no workflows directory to install into.",
			Consumed: true,
		}, nil
	}

	address := parsed.Arg(0)
	addr, err := ParseAddress(address)
	if err != nil {
		// The address does not fit the hub's shape: nothing is
		// fetched and nothing is written.
		list := newChecklist("download "+address, f.emitter(ctx, cs, input.ChatID, input.MessageID))
		list.set(rowDownload, agent.TaskFailed, err.Error())
		list.set(rowValidate, agent.TaskSkipped, "")
		list.set(rowInstall, agent.TaskSkipped, "")
		_ = list.send()
		chatsession.ReportOutcome(ctx, command.FailedOutcomeData(ReasonInvalidAddress,
			"the address is not valid",
			map[string]string{"address": address}, err.Error()))
		return &command.SlashOutput{Consumed: true}, nil
	}

	list := newChecklist("download "+addr.String(), f.emitter(ctx, cs, input.ChatID, input.MessageID))
	result := f.install(ctx, list, addr)
	f.report(ctx, addr, result)
	return &command.SlashOutput{Consumed: true}, nil
}

// report publishes the outcome of an install.
//
// The reason is the verdict: a program reacts to it without parsing
// anything.
func (f *Factory) report(ctx context.Context, addr Address, result Result) {
	data := map[string]string{"reason": result.Reason}
	if result.Path != "" {
		data["path"] = result.Path
	}
	if result.Source != "" {
		data["source"] = result.Source
	}
	if result.SHA256 != "" {
		data["sha256"] = result.SHA256
	}
	switch result.Reason {
	case ReasonInstalled, ReasonUpdated:
		chatsession.ReportOutcome(ctx, messages.Outcome{
			Status:  messages.OutcomeOK,
			Summary: "the workflow " + addr.String() + " is " + result.Reason,
			Data:    data,
		})
	case ReasonUnchanged:
		// The workflow was already current: the request was valid and
		// there was nothing to write.
		chatsession.ReportOutcome(ctx, command.NoopOutcome(
			"the workflow "+addr.String()+" is already current", data))
	default:
		chatsession.ReportOutcome(ctx, command.FailedOutcomeData(result.Reason,
			"the workflow "+addr.String()+" was not installed",
			data, result.Detail))
	}
}

// emitter publishes the checklist in the chat.
//
// The checklist goes out through the chat session's emitter: a
// command's SlashOutput is returned once, at the end of the handle,
// and cannot carry a mid-flight update.
//
// Every snapshot is anchored to the message the operator sent: the
// channel that can patch a card in place updates the one card the
// message opened, instead of posting a card per snapshot.
func (f *Factory) emitter(ctx context.Context, cs *chatsession.ChatSession, chatID, replyTo string) func([]agent.AgentTaskItem) error {
	first := true
	return func(items []agent.AgentTaskItem) error {
		if cs == nil {
			return nil
		}
		emitter := cs.Emitter()
		if emitter == nil {
			return nil
		}
		kind := messages.OutTaskUpdate
		if first {
			kind = messages.OutTaskCreate
			first = false
		}
		return emitter.Send(ctx, messages.OutboundMessage{
			ChatID:   chatID,
			ReplyTo:  replyTo,
			Kind:     kind,
			TaskList: &agent.AgentTaskListEvent{Items: items},
		})
	}
}
