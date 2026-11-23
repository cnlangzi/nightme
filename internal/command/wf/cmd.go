package wf

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/pkg/outcome"
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
		Summary:  "Install or remove a published workflow by its address.",
		Usage:    wfUsage,
		Category: "agent",
		Subcommands: []command.SubcommandSpec{
			{
				Name:    "install",
				Summary: "Install a published workflow by its address. A name without an owner is the project's own: coder is nightme/coder.",
				Usage:   installCmdSpec.Usage,
			},
			{
				Name:    "uninstall",
				Summary: "Remove an installed workflow by its address. A name without an owner is the project's own: coder is nightme/coder.",
				Usage:   uninstallCmdSpec.Usage,
			},
		},
	}
}

// The subcommands of /wf.
const (
	subInstall   = "install"
	subUninstall = "uninstall"
)

// installCmdSpec declares /wf install's argv grammar.
//
// The shared lexer enforces the contract: the subcommand is required,
// the address is one argument, and only the force flag is known.
var installCmdSpec = command.CmdSpec{
	Name:       "/wf install",
	Usage:      "/wf install [-f] <owner>/<name> | <name>",
	Subcommand: subInstall,
	Flags: map[string]command.FlagSpec{
		"-f":      {Name: "force"},
		"--force": {Name: "force"},
	},
	MinArgs: 1,
	MaxArgs: 1,
}

// uninstallCmdSpec declares /wf uninstall's argv grammar.
//
// The uninstall takes no flags: it removes one address and nothing
// else.
var uninstallCmdSpec = command.CmdSpec{
	Name:       "/wf uninstall",
	Usage:      "/wf uninstall <owner>/<name> | <name>",
	Subcommand: subUninstall,
	MinArgs:    1,
	MaxArgs:    1,
}

// wfUsage is the usage line of the /wf command: the two subcommands
// on a line each.
var wfUsage = installCmdSpec.Usage + "\n" + uninstallCmdSpec.Usage

// Handle implements command.SlashCommandFactory.
//
// The result reaches the operator through the checklist alone: the
// command's SlashOutput carries no reply text.
func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices,
	mgr *chatsession.Manager, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {

	// The command name is the first token; the rest is the command
	// line the lexer reads.
	argv := input.Args
	if len(argv) > 0 {
		argv = argv[1:]
	}
	spec, err := specFor(argv)
	if err != nil {
		return usageError(ctx, err), nil
	}
	parsed, err := command.ParseCmdArgs(argv, spec)
	if err != nil {
		return usageError(ctx, err), nil
	}

	if f.dir == "" {
		chatsession.ReportOutcome(ctx, command.FailedOutcome(http.StatusNotFound,
			"the daemon has no workflows directory"))
		return &command.SlashOutput{
			Reply:    "❌ This daemon has no workflows directory.",
			Consumed: true,
		}, nil
	}

	if spec.Subcommand == subUninstall {
		return f.runUninstall(ctx, cs, input, parsed.Arg(0))
	}
	return f.runInstall(ctx, cs, input, parsed.Arg(0), parsed.Bool("force"))
}

// specFor returns the grammar of the subcommand the command line
// names.
//
// The subcommand is matched without regard to case, like the shared
// lexer does.
func specFor(argv []string) (command.CmdSpec, error) {
	if len(argv) == 0 {
		return command.CmdSpec{}, fmt.Errorf("missing subcommand: want %q or %q (Usage: %s)",
			subInstall, subUninstall, wfUsage)
	}
	switch {
	case strings.EqualFold(argv[0], subInstall):
		return installCmdSpec, nil
	case strings.EqualFold(argv[0], subUninstall):
		return uninstallCmdSpec, nil
	}
	return command.CmdSpec{}, fmt.Errorf("unexpected subcommand %q: want %q or %q (Usage: %s)",
		argv[0], subInstall, subUninstall, wfUsage)
}

// usageError answers a command line that does not fit.
func usageError(ctx context.Context, err error) *command.SlashOutput {
	chatsession.ReportOutcome(ctx, command.FailedOutcome(http.StatusBadRequest, "the command line is not valid"))
	return &command.SlashOutput{
		Reply:    fmt.Sprintf("❌ %v", err),
		Consumed: true,
	}
}

// runInstall installs the workflow of the given address.
func (f *Factory) runInstall(ctx context.Context, cs *chatsession.ChatSession,
	input command.SlashInput, address string, force bool) (*command.SlashOutput, error) {
	addr, err := ParseAddress(address)
	if err != nil {
		// The address does not fit the hub's shape: nothing is
		// fetched and nothing is written.
		list := newChecklist("download "+address, f.emitter(ctx, cs, input.ChatID, input.MessageID))
		list.set(rowDownload, agent.TaskFailed, err.Error())
		list.set(rowValidate, agent.TaskSkipped, "")
		list.set(rowInstall, agent.TaskSkipped, "")
		_ = list.send()
		chatsession.ReportOutcome(ctx, command.FailedOutcomeData(http.StatusBadRequest,
			err.Error(),
			map[string]string{KeyAddress: address}))
		return &command.SlashOutput{Consumed: true}, nil
	}

	list := newChecklist("download "+addr.String(), f.emitter(ctx, cs, input.ChatID, input.MessageID))
	result := f.install(ctx, list, addr, force)
	f.report(ctx, addr, result)
	return &command.SlashOutput{Consumed: true}, nil
}

// runUninstall removes the installed workflow of the given address.
func (f *Factory) runUninstall(ctx context.Context, cs *chatsession.ChatSession,
	input command.SlashInput, address string) (*command.SlashOutput, error) {
	addr, err := ParseAddress(address)
	if err != nil {
		// The address does not fit the hub's shape: nothing is
		// removed.
		list := newRemovalChecklist("remove "+address, f.emitter(ctx, cs, input.ChatID, input.MessageID))
		list.set(rowRemove, agent.TaskFailed, err.Error())
		_ = list.send()
		chatsession.ReportOutcome(ctx, command.FailedOutcomeData(http.StatusBadRequest,
			err.Error(),
			map[string]string{KeyAddress: address}))
		return &command.SlashOutput{Consumed: true}, nil
	}

	list := newRemovalChecklist("remove "+addr.String(), f.emitter(ctx, cs, input.ChatID, input.MessageID))
	result := f.uninstall(list, addr)
	f.reportUninstall(ctx, addr, result)
	return &command.SlashOutput{Consumed: true}, nil
}

// report publishes the outcome of an install.
//
// The reason is the verdict: a program reacts to it without parsing
// anything.
func (f *Factory) report(ctx context.Context, addr Address, result Result) {
	data := outcomeData(result)
	if result.Source != "" {
		data["source"] = result.Source
	}
	if result.SHA256 != "" {
		data["sha256"] = result.SHA256
	}
	switch result.Reason {
	case ReasonInstalled, ReasonUpdated:
		chatsession.ReportOutcome(ctx, outcome.Outcome{
			Success:    true,
			Status:     http.StatusOK,
			StatusText: "the workflow " + addr.String() + " is " + result.Reason,
			Data:       data,
		})
	case ReasonSkipped:
		// The workflow was already there: the request was valid and
		// there was nothing to fetch.
		chatsession.ReportOutcome(ctx, command.SuccessOutcome(
			"the workflow "+addr.String()+" is already installed — /wf install -f overwrites it", data))
	case ReasonUnchanged:
		// The workflow was already current: the request was valid and
		// there was nothing to write.
		chatsession.ReportOutcome(ctx, command.SuccessOutcome(
			"the workflow "+addr.String()+" is already current", data))
	default:
		chatsession.ReportOutcome(ctx, command.FailedOutcomeData(installStatus(result.Reason),
			"the workflow "+addr.String()+" was not installed: "+result.Detail,
			data))
	}
}

// reportUninstall publishes the outcome of an uninstall.
//
// The reason is the verdict: a program reacts to it without parsing
// anything.
func (f *Factory) reportUninstall(ctx context.Context, addr Address, result Result) {
	data := outcomeData(result)
	switch result.Reason {
	case ReasonUninstalled:
		chatsession.ReportOutcome(ctx, outcome.Outcome{
			Success:    true,
			Status:     http.StatusOK,
			StatusText: "the workflow " + addr.String() + " is uninstalled",
			Data:       data,
		})
	case ReasonNotInstalled:
		// There was nothing to remove: the request was valid and the
		// workflow was already gone.
		chatsession.ReportOutcome(ctx, command.SuccessOutcome(
			"the workflow "+addr.String()+" is not installed", data))
	default:
		chatsession.ReportOutcome(ctx, command.FailedOutcomeData(uninstallStatus(result.Reason),
			"the workflow "+addr.String()+" was not uninstalled: "+result.Detail,
			data))
	}
}

// installStatus maps the reason an install came to onto the status of
// the outcome that reports it.
//
// The workflow is not there (404), the address or the file does not
// fit (400), no tier could serve it (503), or it could not be
// written (500).
func installStatus(reason string) int {
	switch reason {
	case ReasonNotFound:
		return http.StatusNotFound
	case ReasonInvalidAddress, ReasonInvalidContent:
		return http.StatusBadRequest
	case ReasonUnreachable:
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

// uninstallStatus maps the reason an uninstall came to onto the status
// of the outcome that reports it.
func uninstallStatus(reason string) int {
	if reason == ReasonNotFound {
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

// outcomeData builds the machine-readable facts of an outcome.
//
// The path is where the workflow is.
func outcomeData(result Result) map[string]string {
	data := map[string]string{}
	if result.Path != "" {
		data["path"] = result.Path
	}
	return data
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
		_, err := emitter.Send(ctx, messages.OutboundMessage{
			ChatID:   chatID,
			ReplyTo:  replyTo,
			Kind:     kind,
			TaskList: &agent.AgentTaskListEvent{Items: items},
		})
		return err
	}
}
