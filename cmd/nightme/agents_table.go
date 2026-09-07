// Package main — shared agent-table renderer for `nightme config`.
//
// renderAgentsTable prints a tab-aligned table of the seven
// built-in agents. The leading "✓" shows whether Detect passes
// (PATH lookup or cfg.Agents override resolved); the trailing
// row count lets the configAgentsMenu prompt accept the
// displayed index 1:1. The current primary is named in the
// "(default: …)" line below the table, not marked on a row.
package main

import (
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/cnlangzi/nightme/internal/agentregistry"
	"github.com/cnlangzi/nightme/internal/config"
)

// renderAgentsTable prints the agent table to w. The first
// column is the row number (1-based, matching the picker's
// expected input); the second is ✓ when Detect passes, blank
// otherwise.
func renderAgentsTable(w io.Writer, cfg *config.Config) {
	reg := agentregistry.Build(cfg, "")
	specs := reg.List()

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "\t#\tNAME\tBRIDGE\tCOMMAND\tARGS")
	for i, s := range specs {
		if s == nil {
			continue
		}
		info := s.Info()
		detectErr := s.Detect()
		mark := ""
		if detectErr == nil {
			mark = "✓"
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\n",
			mark,
			i+1,
			info.Name,
			info.Mode.String(),
			resolveAgentCommand(info.Command, cfg, info.Name, detectErr),
			quoteArgs(info.Args),
		)
	}
	tw.Flush()
}

// resolveAgentCommand renders the COMMAND cell for one row. cfg
// overrides win when present (verbatim, after trim); otherwise
// absolute configured paths render verbatim and relative names go
// through LookPath so the user sees exactly what Detect would use.
func resolveAgentCommand(configured string, cfg *config.Config, name string, detectErr error) string {
	for _, e := range cfg.Agents {
		if e.Name != name || e.Command == "" {
			continue
		}
		if path := strings.TrimSpace(e.Command); path != "" {
			return path
		}
	}
	if filepath.IsAbs(configured) {
		return configured
	}
	if resolved, err := exec.LookPath(configured); err == nil {
		return resolved
	}
	return configured
}

// quoteArgs joins an arg slice into a single space-separated
// string for table display.
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