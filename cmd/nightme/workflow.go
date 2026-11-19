// Package main — `nightme workflow` subcommands.
//
//	nightme workflow list              # list configured workflows
//	nightme workflow show <name>       # show one workflow's parsed details
//	nightme workflow run <name>        # dry-run a workflow (prints run plan)
//	nightme workflow validate [path...] # check the workflows and report what is wrong
//
// Workflows are read from the workflows dir (default
// `~/.nightme/workflows/`). A workflow is a bundle: a directory under
// the workflows dir that holds a `workflow.yml`, and the bundle's path
// under that dir is the workflow's name. The CLI is self-contained —
// it does not require the daemon to be running, and it does not
// actually trigger an agent (the daemon does that through
// internal/me).
//
// `nightme workflow run` is a dry-run: it parses the workflow,
// builds a plan (what the steps are, in what order) and prints it.
// A workflow is started by a person, with `/me start <name>` in a
// chat — the CLI is a planning/inspection tool.
//
// `nightme workflow validate` is a linter: it reports every problem
// a workflow file has, where the problem is, and exits with a code a
// CI job can act on.
//
// A name that carries no owner is the project's own namespace:
// "coder" is the workflow "nightme/coder".

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/config"
	nmerrors "github.com/cnlangzi/nightme/internal/errors"
	"github.com/cnlangzi/nightme/internal/runtime"
	"github.com/cnlangzi/nightme/internal/wfname"
	"github.com/cnlangzi/nightme/pkg/wfe"
)

// workflowsDir returns the directory where workflow YAML files
// live: the workflows dir under the configured data dir — the same
// directory the daemon reads its workflows from.
//
// $NIGHTME_WORKFLOWS_DIR overrides it, for testing.
func workflowsDir() (string, error) {
	if d := os.Getenv("NIGHTME_WORKFLOWS_DIR"); d != "" {
		return d, nil
	}
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return "", err
	}
	return runtime.WorkflowsDir(cfg)
}

// loadWorkflows loads all workflows from the workflows dir.
//
// The workflows are read on every command: the CLI needs no running
// daemon. Returns (workflows, failures, dir).
func loadWorkflows() ([]*wfe.Workflow, []wfe.Failure, string, error) {
	dir, err := workflowsDir()
	if err != nil {
		return nil, nil, "", err
	}
	wfs, failures := wfe.LoadDir(dir)
	return wfs, failures, dir, nil
}

// newWorkflowCmd builds the `nightme workflow` parent command and
// attaches list/show/run/validate as subcommands.
func newWorkflowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workflow",
		Short: "Workflow automation commands",
		Long: "workflow inspects the YAML workflows the personal agent runs. " +
			"Subcommands are read-only or planning tools — a workflow is " +
			"started by a person, with /me start <name> in a chat.",
	}
	cmd.AddCommand(newWorkflowListCmd())
	cmd.AddCommand(newWorkflowShowCmd())
	cmd.AddCommand(newWorkflowRunCmd())
	cmd.AddCommand(newWorkflowValidateCmd())
	return cmd
}

// newWorkflowListCmd implements `nightme workflow list`.
func newWorkflowListCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List configured workflows",
		Long: "list reads every bundle under the workflows dir and prints a " +
			"summary table. A workflow is a bundle: a directory under the " +
			"workflows dir that holds a workflow.yml. A workflow's name is " +
			"the path of its bundle under the workflows dir: fix/workflow.yml " +
			"is the workflow fix, nightme/coder/workflow.yml is the workflow " +
			"nightme/coder. The files that did not load are reported below the " +
			"table. The --json flag swaps the table for a JSON array (one " +
			"element per workflow); the failures go to stderr, so the " +
			"output stays machine-readable.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			wfs, failures, dir, err := loadWorkflows()
			if err != nil {
				return err
			}
			if err := printWorkflowList(cmd.OutOrStdout(), wfs, failures, dir, jsonOut); err != nil {
				return err
			}
			if jsonOut {
				return printWorkflowFailures(cmd.ErrOrStderr(), failures)
			}
			return printWorkflowFailures(cmd.OutOrStdout(), failures)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON array")
	return cmd
}

// printWorkflowList writes a human-readable (or JSON) table of
// workflows to w.
func printWorkflowList(w io.Writer, wfs []*wfe.Workflow, failures []wfe.Failure, dir string, asJSON bool) error {
	if asJSON {
		type item struct {
			Name       string   `json:"name"`
			Title      string   `json:"title"`
			Workspaces []string `json:"workspaces"`
			Triggers   []string `json:"triggers"`
			Jobs       []string `json:"jobs"`
		}
		out := make([]item, 0, len(wfs))
		for _, wf := range wfs {
			out = append(out, item{
				Name:       wf.Name,
				Title:      wf.Title,
				Workspaces: wf.Workspaces,
				Triggers:   triggerSummary(wf),
				Jobs:       jobNames(wf),
			})
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tTITLE\tWORKSPACES\tTRIGGERS\tJOBS")
	for _, wf := range wfs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			wf.Name,
			truncate(wf.Title, 40),
			truncate(strings.Join(wf.Workspaces, ", "), 30),
			truncate(strings.Join(triggerSummary(wf), ", "), 30),
			strings.Join(jobNames(wf), ", "),
		)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(wfs) == 0 && len(failures) == 0 {
		fmt.Fprintf(w, "\nno workflows found in %s\n", dir)
		fmt.Fprintf(w, "hint: create a <name>/workflow.yml bundle there, or set $NIGHTME_WORKFLOWS_DIR\n")
	}
	return nil
}

// printWorkflowFailures writes the files that did not load to w.
//
// A file that did not load does not take the workflows beside it down
// with it: the failures are reported next to the ones that loaded. A
// failure carries every problem the file has, one per line, and every
// line of it is indented.
func printWorkflowFailures(w io.Writer, failures []wfe.Failure) error {
	if len(failures) == 0 {
		return nil
	}
	fmt.Fprintln(w, "\nthe files that did not load:")
	for _, f := range failures {
		for _, line := range strings.Split(f.Error(), "\n") {
			fmt.Fprintf(w, "  %s\n", line)
		}
	}
	return nil
}

// triggerSummary returns a short, human-readable list of trigger
// kinds configured on the workflow.
func triggerSummary(wf *wfe.Workflow) []string {
	var out []string
	if len(wf.On.Schedule) > 0 {
		out = append(out, fmt.Sprintf("schedule[%d]", len(wf.On.Schedule)))
	}
	if wf.On.PullRequest != nil {
		out = append(out, "pull_request")
	}
	if wf.On.Branch != nil {
		out = append(out, "branch")
	}
	if wf.On.Issue != nil {
		out = append(out, "issue")
	}
	if wf.On.Mention != nil {
		out = append(out, "mention")
	}
	if len(out) == 0 {
		out = append(out, "(none)")
	}
	return out
}

func jobNames(wf *wfe.Workflow) []string {
	names := make([]string, 0, len(wf.Jobs))
	for n := range wf.Jobs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// newWorkflowShowCmd implements `nightme workflow show <name>`.
func newWorkflowShowCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Show one workflow's parsed details",
		Long: "show prints the full workflow details: triggers, jobs, steps, " +
			"and effective agent resolution. The name is the path of the " +
			"workflow's bundle under the workflows dir: the directory that " +
			"holds its workflow.yml. A name that carries no owner is " +
			"the project's own: \"coder\" is \"nightme/coder\". " +
			"The --json flag emits the raw parsed " +
			"Workflow struct.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			wfs, failures, _, err := loadWorkflows()
			if err != nil {
				return err
			}
			wf := findWorkflow(wfs, args[0])
			if wf == nil {
				return notFound(args[0], failures)
			}
			return printWorkflowShow(cmd.OutOrStdout(), wf, jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit raw workflow JSON")
	return cmd
}

// notFound reports that a workflow is not there.
//
// A file that did not load carries its own reason: the operator asked
// for that name, and the file is on disk.
func notFound(name string, failures []wfe.Failure) error {
	for _, candidate := range wfname.Names(name) {
		for _, f := range failures {
			if f.Name == candidate {
				return f.Err
			}
		}
	}
	return fmt.Errorf("workflow %q not found", name)
}

// findWorkflow returns the workflow the name stands for.
//
// A name that carries no owner is the workflow of that name first, and
// the project's own namespace second: "coder" is the workflow "coder"
// when there is one, and the workflow "nightme/coder" otherwise.
func findWorkflow(wfs []*wfe.Workflow, name string) *wfe.Workflow {
	for _, candidate := range wfname.Names(name) {
		for _, wf := range wfs {
			if wf.Name == candidate {
				return wf
			}
		}
	}
	return nil
}

func printWorkflowShow(w io.Writer, wf *wfe.Workflow, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(wf)
	}
	fmt.Fprintf(w, "name:       %s\n", wf.Name)
	if wf.Title != "" {
		fmt.Fprintf(w, "title:      %s\n", wf.Title)
	}
	fmt.Fprintf(w, "workspaces: %s\n", workspacesLabel(wf))
	if len(wf.Args) > 0 {
		fmt.Fprintf(w, "args:       %s\n", strings.Join(wf.ArgNames(), ", "))
	}
	fmt.Fprintln(w, "\ntriggers:")
	for _, s := range wf.On.Schedule {
		fmt.Fprintf(w, "  - schedule: cron=%q\n", s.Cron)
	}
	if wf.On.PullRequest != nil {
		fmt.Fprintf(w, "  - pull_request: branches=%v events=%v\n",
			wf.On.PullRequest.Branches, wf.On.PullRequest.Events)
	}
	if wf.On.Branch != nil {
		fmt.Fprintf(w, "  - branch: patterns=%v events=%v\n",
			wf.On.Branch.Patterns, wf.On.Branch.Events)
	}
	if wf.On.Issue != nil {
		fmt.Fprintf(w, "  - issue: events=%v\n", wf.On.Issue.Events)
	}
	if wf.On.Mention != nil {
		fmt.Fprintf(w, "  - mention: commands=%v\n", wf.On.Mention.Commands)
	}
	fmt.Fprintln(w, "\njobs:")
	for _, name := range jobNames(wf) {
		job := wf.Jobs[name]
		fmt.Fprintf(w, "  - %s (steps: %d)\n", name, len(job.Steps))
		if len(job.Needs) > 0 {
			fmt.Fprintf(w, "    needs: %s\n", strings.Join(job.Needs, ", "))
		}
		if rounds := job.Rounds(); rounds == 0 {
			fmt.Fprintf(w, "    iterations: until the run ends\n")
		} else if rounds > 1 {
			fmt.Fprintf(w, "    iterations: %d\n", rounds)
		}
		for i, step := range job.Steps {
			fmt.Fprintf(w, "    [%d] %s\n", i, describeStep(step))
		}
	}
	return nil
}

func describeStep(s wfe.Step) string {
	switch s.Kind() {
	case wfe.StepKindMessage:
		return fmt.Sprintf("message: %q (id=%s)", truncate(s.Message, 60), s.ID)
	case wfe.StepKindShell:
		return fmt.Sprintf("shell: %q (id=%s)", truncate(s.Shell, 60), s.ID)
	case wfe.StepKindUse:
		return fmt.Sprintf("use: %s (id=%s, with=%d args)",
			s.Use, s.ID, len(s.With))
	case wfe.StepKindWait:
		return fmt.Sprintf("wait: %s (id=%s)", s.Wait, s.ID)
	}
	return fmt.Sprintf("? (id=%s)", s.ID)
}

// inheritedWorkspace is what a workflow without workspaces says: the
// run works where the chat works.
const inheritedWorkspace = "(the workspace of the chat)"

// workspacesLabel renders the workflow's workspaces.
func workspacesLabel(wf *wfe.Workflow) string {
	if len(wf.Workspaces) == 0 {
		return inheritedWorkspace
	}
	return strings.Join(wf.Workspaces, ", ")
}

// newWorkflowRunCmd implements `nightme workflow run <name>`.
// It is a dry-run: it prints the run plan (what the steps are, in
// what order) without executing anything. A workflow is started by a
// person, with `/me start <name>` in a chat.
func newWorkflowRunCmd() *cobra.Command {
	var workspace string
	cmd := &cobra.Command{
		Use:   "run <name>",
		Short: "Dry-run a workflow (prints what would be executed)",
		Long: "run parses the workflow and prints a step-by-step execution " +
			"plan. It does NOT actually execute anything — a workflow is " +
			"started by a person, with /me start <name> in a chat. Use " +
			"this to verify the step ordering before relying on the real run.\n\n" +
			"The name is the path of the workflow's bundle under the " +
			"workflows dir: the directory that holds its workflow.yml. A " +
			"name that carries no owner is the project's own: \"coder\" " +
			"is \"nightme/coder\".\n\n" +
			"--workspace overrides one of the workflow's workspaces " +
			"(defaults to the first one). A workflow that declares no " +
			"workspace works where the chat works.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			wfs, failures, _, err := loadWorkflows()
			if err != nil {
				return err
			}
			wf := findWorkflow(wfs, args[0])
			if wf == nil {
				return notFound(args[0], failures)
			}
			ws := workspace
			if ws == "" && len(wf.Workspaces) > 0 {
				ws = wf.Workspaces[0]
			}
			return printWorkflowRun(cmd.OutOrStdout(), wf, ws)
		},
	}
	cmd.Flags().StringVar(&workspace, "workspace", "",
		"override workspace (defaults to workflow.workspaces[0])")
	return cmd
}

func printWorkflowRun(w io.Writer, wf *wfe.Workflow, workspace string) error {
	fmt.Fprintf(w, "workflow:   %s\n", wf.Name)
	if wf.Title != "" {
		fmt.Fprintf(w, "title:      %s\n", wf.Title)
	}
	if workspace != "" {
		fmt.Fprintf(w, "workspace:  %s\n", workspace)
	} else {
		fmt.Fprintf(w, "workspace:  %s\n", inheritedWorkspace)
	}
	fmt.Fprintln(w, "\ntriggers (parsed and displayed; nothing fires them):")
	if len(wf.On.Schedule) > 0 {
		for _, s := range wf.On.Schedule {
			fmt.Fprintf(w, "  - cron: %s\n", s.Cron)
		}
	}
	if wf.On.PullRequest != nil {
		fmt.Fprintf(w, "  - pull_request: branches=%v events=%v\n",
			wf.On.PullRequest.Branches, wf.On.PullRequest.Events)
	}
	if wf.On.Branch != nil {
		fmt.Fprintf(w, "  - branch: patterns=%v events=%v\n",
			wf.On.Branch.Patterns, wf.On.Branch.Events)
	}
	if wf.On.Issue != nil {
		fmt.Fprintf(w, "  - issue: events=%v\n", wf.On.Issue.Events)
	}
	if wf.On.Mention != nil {
		fmt.Fprintf(w, "  - mention: commands=%v\n", wf.On.Mention.Commands)
	}
	fmt.Fprintln(w, "\nexecution plan (topological order):")
	for _, name := range jobNames(wf) {
		job := wf.Jobs[name]
		fmt.Fprintf(w, "\n  job: %s (steps: %d)\n", name, len(job.Steps))
		if len(job.Needs) > 0 {
			fmt.Fprintf(w, "    needs: %s\n", strings.Join(job.Needs, ", "))
		}
		if rounds := job.Rounds(); rounds == 0 {
			fmt.Fprintf(w, "    iterations: until the run ends\n")
		} else if rounds > 1 {
			fmt.Fprintf(w, "    iterations: %d\n", rounds)
		}
		for i, step := range job.Steps {
			fmt.Fprintf(w, "    [%d] %s\n", i, describeStep(step))
		}
	}
	fmt.Fprintln(w, "\n(this is a dry-run; no agent is actually invoked)")
	return nil
}

// newWorkflowValidateCmd implements `nightme workflow validate
// [path...]`.
//
// It is a linter: it reports every problem a workflow file has and
// exits with a code a CI job can act on.
func newWorkflowValidateCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "validate [path...]",
		Short: "Validate workflow files",
		Long: "validate checks the workflow files and reports every problem " +
			"it found: a missing title, a job without steps, a step that " +
			"names two kinds, a key the schema does not have. Each problem is " +
			"one line — \"path:line:column: message\" — and a run where every " +
			"workflow is valid prints nothing.\n\n" +
			"With no argument it validates the workflows dir. With arguments " +
			"it validates the files and the directories it is given; a " +
			"directory contributes the workflow.yml of every bundle under " +
			"it, and a file that is named is checked for its content. An " +
			"argument " +
			"that is not a path on disk is looked up as a workflow name, so " +
			"`nightme workflow validate nightme/coder` validates the file " +
			"of that name. A name that carries no owner is the project's " +
			"own: \"coder\" is \"nightme/coder\". The exit code " +
			"is 0 when every workflow is valid, 8 when a workflow does not " +
			"validate, and 9 when a path does not exist or cannot be read.\n\n" +
			"--json swaps the lines for a JSON array.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkflowValidate(cmd.OutOrStdout(), cmd.ErrOrStderr(), args, jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit a JSON array of diagnostics")
	return cmd
}

// runWorkflowValidate validates the workflows and reports what is
// wrong with them.
//
// The exit code says what happened: 0 when every workflow is valid,
// 8 when a workflow does not validate, 9 when a path does not exist
// or cannot be read. A path that cannot be read does not stop the
// other paths: they are validated anyway, and the path that could
// not be read wins the exit code — the command could not check what
// it was asked to check.
//
// An argument that is not a path on disk is looked up as a workflow
// name: the workflows directory holds the workflows, and a name is
// the path of a bundle under it. A name that carries no owner is the
// project's own namespace: "coder" is "nightme/coder". An argument
// that names no workflow is reported by the name it stands for.
func runWorkflowValidate(out io.Writer, errOut io.Writer, args []string, asJSON bool) error {
	dir, dirErr := workflowsDir()
	paths := args
	if len(paths) == 0 {
		if dirErr != nil {
			return dirErr
		}
		paths = []string{dir}
	}
	var diags []wfe.Diagnostic
	unreadable := false
	for _, path := range paths {
		isDir, err := readablePath(path)
		if err == nil {
			if isDir {
				diags = append(diags, wfe.ValidateDir(path)...)
				continue
			}
			diags = append(diags, wfe.ValidateFile(path)...)
			continue
		}
		if !os.IsNotExist(err) {
			// The path is there but cannot be read: that is a path the
			// command could not check, not a workflow name.
			fmt.Fprintf(errOut, "%s: %v\n", path, err)
			unreadable = true
			continue
		}
		// The argument is not a path on disk: it may be the name of a
		// workflow in the workflows directory.
		matched, resolved, lookupErr := workflowPaths(dir, path)
		if lookupErr != nil {
			fmt.Fprintf(errOut, "%s: %v\n", path, lookupErr)
			unreadable = true
			continue
		}
		if len(matched) == 0 {
			// The name answers to no workflow: the command could not
			// check what it was asked to check.
			fmt.Fprintf(errOut, "no workflow named %q\n", path)
			unreadable = true
			continue
		}
		for _, match := range matched {
			diags = append(diags, wfe.ValidateFile(match)...)
		}
		if len(matched) > 1 {
			// Two files claim the name: it is ambiguous, and neither
			// file is the workflow of that name.
			diags = append(diags, wfe.Diagnostic{
				Path: matched[1],
				Message: fmt.Sprintf("duplicate workflow name %q in %s and %s",
					resolved, matched[0], matched[1]),
			})
		}
	}
	if asJSON {
		if diags == nil {
			diags = []wfe.Diagnostic{}
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(diags); err != nil {
			return err
		}
	} else {
		for _, d := range diags {
			fmt.Fprintln(out, d.Error())
		}
	}
	if unreadable {
		return nmerrors.New(nmerrors.CodeNotFound, "workflow: a path does not exist or cannot be read")
	}
	if len(diags) > 0 {
		return nmerrors.New(nmerrors.CodeValidationError, "workflow: a workflow does not validate")
	}
	return nil
}

// readablePath reports whether the path can be read, and whether it is
// a directory.
//
// A path that cannot be read is a command error — the exit code says
// so — and not a problem of a workflow. The library reports it as a
// diagnostic like any other; the command looks for it here, because
// the exit code tells the two kinds of failure apart.
func readablePath(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	file.Close()
	return info.IsDir(), nil
}

// workflowPaths returns the files of the workflow the name stands for,
// and the name it is known by.
//
// A name that carries no owner is the workflow of that name first, and
// the project's own namespace second: "coder" is the workflow "coder"
// when there is one, and the workflow "nightme/coder" otherwise.
func workflowPaths(dir, name string) ([]string, string, error) {
	for _, candidate := range wfname.Names(name) {
		paths, err := wfe.WorkflowPathsByName(dir, candidate)
		if err != nil {
			return nil, candidate, err
		}
		if len(paths) > 0 {
			return paths, candidate, nil
		}
	}
	return nil, name, nil
}

// jsonNewEncoder is unused (kept for future expansion). Kept
// here to avoid an unused-import warning if someone re-enables
// the wrapper path.
var _ = json.NewEncoder
