// Package main — `nightme workflow` subcommands.
//
//	nightme workflow list              # list configured workflows
//	nightme workflow show <name>       # show one workflow's parsed details
//	nightme workflow run <name>        # dry-run a workflow (prints run plan)
//
// Workflows are read from the workflows dir (default
// `~/.nightme/workflows/`). The CLI is self-contained — it does
// not require the daemon to be running, and it does not actually
// trigger an agent (the daemon does that through internal/me).
//
// `nightme workflow run` is a dry-run: it parses the workflow,
// builds a plan (what the steps are, in what order) and prints it.
// A workflow is started by a person, with `/me start <name>` in a
// chat — the CLI is a planning/inspection tool.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/wfe"
)

// workflowsDir returns the directory where workflow YAML files
// live. Default: `$HOME/.nightme/workflows/`. Overridable via
// $NIGHTME_WORKFLOWS_DIR for testing.
func workflowsDir() (string, error) {
	if d := os.Getenv("NIGHTME_WORKFLOWS_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home: %w", err)
	}
	return filepath.Join(home, ".nightme", "workflows"), nil
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
// attaches list/show/run as subcommands.
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
	return cmd
}

// newWorkflowListCmd implements `nightme workflow list`.
func newWorkflowListCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List configured workflows",
		Long: "list reads every *.yaml and *.yml in the workflows dir and " +
			"prints a summary table. A workflow's name is the base name of " +
			"its file. The files that did not load are reported below the " +
			"table. The --json flag swaps the table for a JSON array (one " +
			"element per workflow); the failures go to stderr, so the " +
			"output stays machine-readable.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			wfs, failures, dir, err := loadWorkflows()
			if err != nil {
				return err
			}
			if err := printWorkflowList(cmd.OutOrStdout(), wfs, dir, jsonOut); err != nil {
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
func printWorkflowList(w io.Writer, wfs []*wfe.Workflow, dir string, asJSON bool) error {
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
	if len(wfs) == 0 {
		fmt.Fprintf(w, "\nno workflows found in %s\n", dir)
		fmt.Fprintf(w, "hint: create a *.yaml or *.yml file there, or set $NIGHTME_WORKFLOWS_DIR\n")
	}
	return nil
}

// printWorkflowFailures writes the files that did not load to w.
//
// A file that did not load does not take the workflows beside it down
// with it: the failures are reported next to the ones that loaded.
func printWorkflowFailures(w io.Writer, failures []wfe.Failure) error {
	if len(failures) == 0 {
		return nil
	}
	fmt.Fprintln(w, "\nthe files that did not load:")
	for _, f := range failures {
		fmt.Fprintf(w, "  %s\n", f.Error())
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
			"and effective agent resolution. The name is the base name of " +
			"the workflow's file. The --json flag emits the raw parsed " +
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
	for _, f := range failures {
		if f.Name == name {
			return f.Err
		}
	}
	return fmt.Errorf("workflow %q not found", name)
}

func findWorkflow(wfs []*wfe.Workflow, name string) *wfe.Workflow {
	for _, wf := range wfs {
		if wf.Name == name {
			return wf
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
// run takes the workspace of the chat that starts it.
const inheritedWorkspace = "(the workspace of the chat that starts it)"

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
			"--workspace overrides one of the workflow's workspaces " +
			"(defaults to the first one). A workflow that declares no " +
			"workspace takes the one of the chat that starts it.",
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

// jsonNewEncoder is unused (kept for future expansion). Kept
// here to avoid an unused-import warning if someone re-enables
// the wrapper path.
var _ = json.NewEncoder
