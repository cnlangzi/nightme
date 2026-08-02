// Package main — nightme config (interactive configuration menu).
//
// v1.2: replaces any pure-args config command. Subcommands are
// minimal and explicit; for non-trivial choices (e.g. "which agent
// should be primary?"), interactive mode is the recommended path.
//
// Current submenu: only "Agents" (per Devin 2026-08-02). Other
// sections (feishu / session / logging / paths) deferred to F-XX.
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/config"
)

func newConfigCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "config",
		Short: "Interactive configuration (currently: pick primary agent)",
		Long: "Enter an interactive menu for nightme configuration.\n" +
			"Currently the only submenu is Agents, which lets you pick\n" +
			"the primary agent (global default) from the merged list of\n" +
			"built-in and user-configured agents.",
		RunE: runConfig,
	}
}

func runConfig(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadDefault()
	if err != nil {
		return err
	}
	return configInteractive(cfg, os.Stdin, os.Stdout)
}

// configInteractive drives the top-level menu loop. Extracted for
// testability: callers can pass any io.Reader/Writer.
func configInteractive(cfg *config.Config, in io.Reader, out io.Writer) error {
	fmt.Fprintln(out, "nightme config — interactive")
	fmt.Fprintln(out, "===========================")

	for {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Main menu:")
		fmt.Fprintln(out, "  [1] Agents")
		fmt.Fprintln(out, "  [q] Quit")
		fmt.Fprint(out, "> ")

		choice := readLine(in)
		switch strings.TrimSpace(choice) {
		case "1":
			if err := configAgentsMenu(cfg, in, out); err != nil {
				fmt.Fprintf(out, "Error: %v\n", err)
			}
		case "q", "":
			fmt.Fprintln(out, "Bye.")
			return nil
		default:
			fmt.Fprintln(out, "Unknown choice; try [1] or [q].")
		}
	}
}

// AgentChoice is one row in the merged agent list (builtin ∪ cfg).
// Source distinguishes the two for display.
type AgentChoice struct {
	Name    string
	Bridge  string
	Command string
	Source  string // "builtin" | "config"
}

// MergeAgents combines built-in agents with user-configured agents
// from cfg.Agents. User config takes priority on name collision.
//
// Stable order: builtins first (alphabetical), then cfg additions
// (order preserved from YAML).
func MergeAgents(cfg *config.Config) []AgentChoice {
	seen := map[string]int{}
	out := []AgentChoice{}

	// 1. Builtins (sorted by name for stability).
	builtins := agent.Builtins.List()
	sort.Slice(builtins, func(i, j int) bool { return builtins[i].Name() < builtins[j].Name() })
	for _, a := range builtins {
		choice := AgentChoice{
			Name:    a.Name(),
			Bridge:  a.Mode().String(),
			Command: strings.TrimSpace(a.Command() + " " + strings.Join(a.Args(), " ")),
			Source:  "builtin",
		}
		seen[choice.Name] = len(out)
		out = append(out, choice)
	}

	// 2. User config (in YAML order; overrides builtins on collision).
	for _, entry := range cfg.Agents {
		choice := AgentChoice{
			Name:    entry.Name,
			Bridge:  entry.Bridge,
			Command: entry.Command,
			Source:  "config",
		}
		if idx, ok := seen[choice.Name]; ok {
			out[idx] = choice // override builtin
		} else {
			seen[choice.Name] = len(out)
			out = append(out, choice)
		}
	}

	return out
}

// configAgentsMenu shows the merged list and lets the user pick
// one as the new primary agent. Saves to the config file on
// successful selection.
func configAgentsMenu(cfg *config.Config, in io.Reader, out io.Writer) error {
	merged := MergeAgents(cfg)

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Agents (merged: builtins + your config):")
	for i, a := range merged {
		marker := "  "
		if a.Name == cfg.Primary {
			marker = "* "
		}
		bridge := a.Bridge
		if bridge == "" {
			bridge = "?"
		}
		fmt.Fprintf(out, "  %s[%d] %-10s  %-8s  %s\n", marker, i+1, a.Name, "("+bridge+")", a.Command)
	}

	fmt.Fprintf(out, "\nCurrent primary: %s\n", cfg.Primary)
	fmt.Fprintf(out, "Enter number to set as primary [1-%d], q to cancel: ", len(merged))

	choice := readLine(in)
	choice = strings.TrimSpace(choice)
	if choice == "q" || choice == "" {
		fmt.Fprintln(out, "Cancelled (no changes saved).")
		return nil
	}

	n, err := strconv.Atoi(choice)
	if err != nil || n < 1 || n > len(merged) {
		return fmt.Errorf("invalid choice %q", choice)
	}

	picked := merged[n-1]
	cfg.Primary = picked.Name
	fmt.Fprintf(out, "✓ Primary set to %q (%s)\n", picked.Name, picked.Source)

	if err := config.SaveDefault(cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	fmt.Fprintf(out, "✓ Saved to %s\n", config.DefaultPath())
	return nil
}

// readLine reads a single line from in. Trims trailing newline.
// Returns "" on EOF or scanner error.
func readLine(in io.Reader) string {
	scanner := bufio.NewScanner(in)
	if scanner.Scan() {
		return scanner.Text()
	}
	return ""
}

// ensureParentDir is a small helper for code paths that write files
// outside the standard config.Save path. Exported for test use.
func ensureParentDir(path string) error {
	dir := filepath.Dir(path)
	return os.MkdirAll(dir, 0o700)
}