// Package main — `nightme workflow` subcommands.
//
// v0 surface (per Phase 3 design):
//
//	nightme workflow list              # list configured workflows
//	nightme workflow show <name>       # show one workflow's parsed details
//	nightme workflow run <name>        # dry-run a workflow (prints run plan)
//
// Workflows are read from the bot's workflows dir (default
// `~/.nightme/workflows/`). The CLI is self-contained — it does
// not require the daemon to be running, and it does not actually
// trigger an agent (the daemon does that via the channel pipeline).
//
// `nightme workflow run` for v0 is a dry-run: it parses the
// workflow, builds a dry-run plan (which trigger would fire, what
// steps would run, in what order), and prints it. Actual agent
// execution is the daemon's job — the CLI is a planning/inspection
// tool.

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

	"github.com/cnlangzi/nightme/internal/channel/bot"
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
// Returns (workflows, dir, err).
func loadWorkflows() ([]*wfe.Workflow, string, error) {
	dir, err := workflowsDir()
	if err != nil {
		return nil, "", err
	}
	wfs, err := wfe.LoadDir(dir)
	if err != nil {
		return nil, dir, err
	}
	return wfs, dir, nil
}

// newWorkflowCmd builds the `nightme workflow` parent command and
// attaches list/show/run as subcommands.
func newWorkflowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workflow",
		Short: "Workflow automation commands",
		Long: "workflow manages YAML-defined automations that bot runs " +
			"on cron / git events. Subcommands are read-only or planning " +
			"tools — actual execution is the daemon's job via the " +
			"channel pipeline.",
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
		Long: "list reads every *.yaml in the workflows dir and prints " +
			"a summary table. The --json flag swaps the table for a " +
			"JSON array (one element per workflow).",
		RunE: func(cmd *cobra.Command, _ []string) error {
			wfs, dir, err := loadWorkflows()
			if err != nil {
				return err
			}
			return printWorkflowList(cmd.OutOrStdout(), wfs, dir, jsonOut)
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
			Workspaces []string `json:"workspaces"`
			Triggers   []string `json:"triggers"`
			Jobs       []string `json:"jobs"`
		}
		out := make([]item, 0, len(wfs))
		for _, wf := range wfs {
			out = append(out, item{
				Name:       wf.Name,
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
	fmt.Fprintln(tw, "NAME\tWORKSPACES\tTRIGGERS\tJOBS")
	for _, wf := range wfs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			wf.Name,
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
		fmt.Fprintf(w, "hint: create a *.yaml file there, or set $NIGHTME_WORKFLOWS_DIR\n")
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
			"and effective agent resolution. The --json flag emits the " +
			"raw parsed Workflow struct.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			wfs, _, err := loadWorkflows()
			if err != nil {
				return err
			}
			wf := findWorkflow(wfs, args[0])
			if wf == nil {
				return fmt.Errorf("workflow %q not found", args[0])
			}
			return printWorkflowShow(cmd.OutOrStdout(), wf, jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit raw workflow JSON")
	return cmd
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
	fmt.Fprintf(w, "workspaces: %s\n", strings.Join(wf.Workspaces, ", "))
	if wf.Agent != "" {
		fmt.Fprintf(w, "agent:      %s (workflow default)\n", wf.Agent)
	} else {
		fmt.Fprintf(w, "agent:      (inherits nightme primary)\n")
	}
	fmt.Fprintf(w, "worker:     %d\n", wf.Worker)
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
		for i, step := range job.Steps {
			fmt.Fprintf(w, "    [%d] %s\n", i, describeStep(step))
		}
	}
	return nil
}

func describeStep(s wfe.Step) string {
	switch s.Kind() {
	case wfe.StepKindRun:
		return fmt.Sprintf("run: %q (id=%s)", truncate(s.Run, 60), s.ID)
	case wfe.StepKindPrompt:
		return fmt.Sprintf("prompt: %q (id=%s, agent=%q)",
			truncate(s.Prompt, 40), s.ID, s.Agent)
	case wfe.StepKindUse:
		return fmt.Sprintf("use: %s (id=%s, with=%d args)",
			s.Use, s.ID, len(s.With))
	}
	return fmt.Sprintf("? (id=%s)", s.ID)
}

// newWorkflowRunCmd implements `nightme workflow run <name>`.
// For v0 this is a dry-run: it prints the run plan (what would be
// triggered, what steps would execute, in what order) without
// actually executing. Actual execution is the daemon's job.
func newWorkflowRunCmd() *cobra.Command {
	var workspace string
	cmd := &cobra.Command{
		Use:   "run <name>",
		Short: "Dry-run a workflow (prints what would be executed)",
		Long: "run parses the workflow and prints a step-by-step execution " +
			"plan. It does NOT actually execute anything — the daemon is " +
			"responsible for live execution via the channel pipeline. Use " +
			"this to verify trigger matching and step ordering before " +
			"relying on the real run.\n\n" +
			"--workspace overrides one of the workflow's workspaces " +
			"(defaults to the first one).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			wfs, _, err := loadWorkflows()
			if err != nil {
				return err
			}
			wf := findWorkflow(wfs, args[0])
			if wf == nil {
				return fmt.Errorf("workflow %q not found", args[0])
			}
			ws := workspace
			if ws == "" {
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
	fmt.Fprintf(w, "workspace:  %s\n", workspace)
	if wf.Agent != "" {
		fmt.Fprintf(w, "agent:      %s\n", wf.Agent)
	}
	fmt.Fprintln(w, "\ntrigger sources (workflow will fire on any):")
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

// ensure bot package is referenced (its init() registers the
// channel). Importing here keeps the link active even if a
// refactor stops anyone else from importing bot.
var _ = bot.New
