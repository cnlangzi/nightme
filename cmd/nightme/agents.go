// Package main — `nightme agents` subcommand.
//
// `nightme agents` enumerates every agent the daemon would dispatch
// `/run` to, drawn from the same config-driven registry used by
// `nightme test` and `nightme run`. The point is to answer the
// question "/run with what name?" without booting the daemon.
//
// Default text format:
//
//	  NAME     COMMAND          ARGS
//	  claude   claude
//	  codex    codex-acp
//	  opencode opencode         acp
//
//	  (default: claude)
//
// `--json` swaps the table for a JSON array (one element per agent).
//
// Design notes:
//   - Header + rows are column-aligned to a fixed width; long args
//     are truncated so the output stays readable on a 120-col terminal.
//   - The "(default: X)" footer comes from cfg.Agent.Default, falling
//     back to the v0.1 hard-coded "claude" if unset.
//   - Registry build reuses buildRunAgentRegistry so the CLI view
//     matches what the daemon would actually see at startup.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/config"
)

// agentsCmdFlags captures every flag the agents subcommand accepts.
type agentsCmdFlags struct {
	jsonOutput bool
}

// agentRow is the JSON wire format for one agent. Field order matches
// the table column order so consumers can grep either view.
type agentRow struct {
	Name    string   `json:"name"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

func newAgentsCmd() *cobra.Command {
	var f agentsCmdFlags

	cmd := &cobra.Command{
		Use:   "agents",
		Short: "List registered agents",
		Long: "List every agent registered in the on-disk config, one\n" +
			"row per agent. Pass --json to get a machine-readable array.\n" +
			"The same registry drives /run inside the daemon, so the names\n" +
			"here are exactly what `/run <name>` accepts.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAgents(cmd, f)
		},
	}

	cmd.Flags().BoolVar(&f.jsonOutput, "json", false, "output as JSON")
	return cmd
}

// runAgents loads the config, builds the same agent registry the
// daemon would use, and prints every entry as a table (default) or
// JSON array (--json).
func runAgents(cmd *cobra.Command, f agentsCmdFlags) error {
	cfg, err := config.LoadDefault()
	if err != nil {
		return fmt.Errorf("agents: load config: %w", err)
	}

	reg := buildRunAgentRegistry(cfg)
	rows := collectAgents(reg)
	defaultName := cfg.Agent.Default
	if defaultName == "" {
		defaultName = "claude"
	}

	if f.jsonOutput {
		return printAgentsJSON(cmd.OutOrStdout(), rows)
	}
	printAgentsTable(cmd.OutOrStdout(), rows, defaultName)
	return nil
}

// collectAgents projects the registry into the JSON-friendly row
// shape. The order is the registry's order (unspecified) — both the
// table and JSON consumers get the same slice.
func collectAgents(reg *agent.Registry) []agentRow {
	agents := reg.List()
	rows := make([]agentRow, 0, len(agents))
	for _, a := range agents {
		if a == nil {
			continue
		}
		rows = append(rows, agentRow{
			Name:    a.Name(),
			Command: a.Command(),
			Args:    a.Args(),
		})
	}
	return rows
}

// agentsColumn widths match the format spec decided with Devin: NAME,
// COMMAND, ARGS. Long values are truncated to keep the right-hand
// columns aligned.
const (
	colAgentName    = 10
	colAgentCommand = 18
	colAgentArgs    = 36
)

// printAgentsTable writes the human-readable table to w. The header
// is always emitted so users see an unambiguous "registry is empty"
// instead of "did the command run?". The "(default: X)" footer only
// prints when there is at least one row.
func printAgentsTable(w io.Writer, rows []agentRow, defaultName string) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tCOMMAND\tARGS")
	if len(rows) == 0 {
		tw.Flush()
		return
	}
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\n",
			truncate(r.Name, colAgentName),
			truncate(r.Command, colAgentCommand),
			truncate(quoteArgs(r.Args), colAgentArgs),
		)
	}
	tw.Flush()
	if defaultName != "" {
		fmt.Fprintf(w, "\n(default: %s)\n", defaultName)
	}
}

// quoteArgs joins an arg slice into a single space-separated string.
// The result is truncated by the caller for the table view; JSON
// consumers receive the original slice unchanged.
func quoteArgs(args []string) string {
	if len(args) == 0 {
		return ""
	}
	out := args[0]
	for _, a := range args[1:] {
		out += " " + a
	}
	return out
}

// printAgentsJSON serializes rows to w. The output is the raw slice
// (not wrapped in an envelope) so `jq '.[]'` works directly.
func printAgentsJSON(w io.Writer, rows []agentRow) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
}