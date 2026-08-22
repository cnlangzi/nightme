// Package main — `nightme agents` subcommand.
//
// `nightme agents` enumerates every agent the daemon would dispatch
// `/run` to, drawn from the same config-driven registry used by
// `nightme test` and `nightme run`. The point is to answer the
// question "/run with what name?" without booting the daemon.
//
// Default text format:
//
//	NAME     COMMAND          ARGS
//	claude   claude
//	codex    codex             (app-server)
//	opencode opencode         acp
//
//	(default: claude)
//
// `--json` swaps the table for a JSON array (one element per agent).
//
// Design notes:
//   - Header + rows are column-aligned via tabwriter. No field is
//     truncated: every value (NAME, COMMAND, ARGS) is printed
//     verbatim so operators can copy-paste the command line.
//   - The "(default: X)" footer comes from cfg.Primary. When
//     cfg.Primary is empty (no auto-detectable builtin, no user
//     config) the footer is omitted — see
//     docs/primary-agent-detection.md for the resolution chain.
//   - Registry build reuses agentregistry.Build so the CLI view
//     matches what the daemon would actually see at startup.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentregistry"
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
//
// cfg.Primary already reflects auto-detection by the time we read
// it — config.LoadDefault runs the probe-and-persist step on empty
// Primary — so the footer below just forwards what the daemon
// would itself bind to a new ChatSession.
func runAgents(cmd *cobra.Command, f agentsCmdFlags) error {
	cfg, err := config.LoadDefault()
	if err != nil {
		return fmt.Errorf("agents: load config: %w", err)
	}

	reg := agentregistry.Build(cfg, "")
	specs := reg.List()
	rows := collectAgents(specs) //nolint:staticcheck

	if f.jsonOutput {
		return printAgentsJSON(cmd.OutOrStdout(), rows)
	}
	printAgentsTable(cmd.OutOrStdout(), rows, cfg.Primary)
	return nil
}

// collectAgents projects the spec-only view into the JSON-friendly
// row shape. The argument is `[]agent.Starter`, so the loop body
// reads s.Info() to get the static metadata without touching
// the live-half methods.
func collectAgents(specs []agent.Starter) []agentRow {
	rows := make([]agentRow, 0, len(specs))
	for _, s := range specs {
		if s == nil {
			continue
		}
		rows = append(rows, agentRow{
			Name:    s.Info().Name,
			Command: s.Info().Command,
			Args:    s.Info().Args,
		})
	}
	return rows
}

// agentsColumn widths hint tabwriter's minimum column padding. None
// of the fields are truncated — every value (NAME, COMMAND, ARGS)
// is printed verbatim so operators can copy-paste the command line.
const (
	colAgentName    = 10
	colAgentCommand = 18
	colAgentArgs    = 36
)

// printAgentsTable writes the human-readable table to w. The header
// is always emitted so users see an unambiguous "registry is empty"
// instead of "did the command run?". The "(default: X)" footer only
// prints when there is a row to attach it to AND a primary is set —
// a bare "(default: )" footer when no builtin is detectable would
// just confuse operators.
func printAgentsTable(w io.Writer, rows []agentRow, defaultName string) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tCOMMAND\tARGS")
	if len(rows) == 0 {
		tw.Flush()
		return
	}
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\n",
			r.Name,
			r.Command,
			quoteArgs(r.Args),
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
