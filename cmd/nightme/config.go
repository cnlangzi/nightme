// Package main — nightme config (interactive configuration menu).
//
// v1.2: replaces any pure-args config command. Subcommands are
// minimal and explicit; for non-trivial choices (e.g. "which agent
// should be primary?"), interactive mode is the recommended path.
//
// Current submenus: Name (show/set instance name) and Agents
// (manage primary + per-built-in path overrides). Other sections
// (feishu / session / logging / paths) deferred to a separate
// pass.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentregistry"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/daemoncontrol"
)

func newConfigCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "config",
		Short: "Interactive configuration (instance name, primary agent)",
		Long: "Enter an interactive menu for nightme configuration.\n" +
			"Currently the submenus are Name (show/set the instance\n" +
			"name) and Agents (pick the primary agent and configure\n" +
			"per-built-in binary paths; only the seven built-in\n" +
			"agents in `nightme agents` are configurable).",
		RunE: runConfig,
	}
}

func runConfig(cmd *cobra.Command, args []string) error {
	path := config.DefaultPath()
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	return configInteractive(cfg, path, bufio.NewReader(cmd.InOrStdin()), cmd.OutOrStdout())
}

// configInteractive drives the top-level menu loop. Extracted for
// testability: callers can pass any *bufio.Reader. path is the
// config path the menu was loaded from, threaded down to submenus
// that need to round-trip a Save back to the same location — without
// this, a caller using $NIGHTME_CONFIG=/some/other.yaml would see
// the menu save to the default path instead.
func configInteractive(cfg *config.Config, path string, in *bufio.Reader, out io.Writer) error {
	fmt.Fprintln(out, "nightme config — interactive")
	fmt.Fprintln(out, "===========================")

	for {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Main menu:")
		fmt.Fprintln(out, "  [1] Name    show/set instance name")
		fmt.Fprintln(out, "  [2] Agents")
		fmt.Fprintln(out, "  [q] Quit")
		fmt.Fprint(out, "> ")

		choice := readLine(in)
		switch strings.TrimSpace(choice) {
		case "1":
			if err := configNameMenu(cfg, path, in, out); err != nil {
				fmt.Fprintf(out, "Error: %v\n", err)
			}
		case "2":
			if err := configAgentsMenu(cfg, path, in, out); err != nil {
				fmt.Fprintf(out, "Error: %v\n", err)
			}
		case "q", "":
			fmt.Fprintln(out, "Bye.")
			return nil
		default:
			fmt.Fprintln(out, "Unknown choice; try [1], [2] or [q].")
		}
	}
}

// configAgentsMenu opens the Agents submenu. Renders the same
// table the now-removed `nightme agents` subcommand produced, so
// operators see per-built-in detection status + resolved path +
// args at a glance, then picks a row to manage (set as primary,
// configure path, or clear an existing override).
//
// Detection is re-run on each menu render so the table stays in
// sync with disk as the user edits paths in the sub-menu.
//
// path is the config file the menu was loaded from and saves back
// to — see configInteractive.
func configAgentsMenu(cfg *config.Config, path string, in *bufio.Reader, out io.Writer) error {
	builtins := agent.Builtins.List()

	for {
		fmt.Fprintln(out)
		renderAgentsTable(out, cfg)
		fmt.Fprintf(out, "\nCurrent primary: %s\n", cfg.Primary)
		fmt.Fprintln(out, "Enter number to manage, q to cancel:")
		fmt.Fprint(out, "> ")

		choice := strings.TrimSpace(readLine(in))
		if choice == "" || strings.EqualFold(choice, "q") {
			return nil
		}
		n, err := strconv.Atoi(choice)
		if err != nil || n < 1 || n > len(builtins) {
			fmt.Fprintf(out, "invalid choice %q\n", choice)
			continue
		}
		picked := builtins[n-1]
		if err := manageAgent(cfg, path, picked.Info().Name, in, out); err != nil {
			fmt.Fprintf(out, "Error: %v\n", err)
		}
	}
}

// cfgPathMap flattens cfg.Agents into a name → path map. The
// Command string is taken verbatim after TrimSpace — the
// schema is "absolute path to one binary", not "command line".
// Splitting on whitespace would mangle Windows paths with
// spaces (`C:\Program Files\claude\claude.exe` → `C:\Program`).
func cfgPathMap(cfg *config.Config) map[string]string {
	out := make(map[string]string, len(cfg.Agents))
	for _, e := range cfg.Agents {
		if e.Name == "" || e.Command == "" {
			continue
		}
		if path := strings.TrimSpace(e.Command); path != "" {
			out[e.Name] = path
		}
	}
	return out
}

// manageStatus renders the one-line header inside the manageAgent
// sub-menu. The table at the top of the Agents submenu uses
// ✓/blank for brevity; the sub-menu header spells out the cfg
// vs PATH distinction so the user sees which source the agent
// is currently coming from.
func manageStatus(name string, cfgPath map[string]string, reg *agent.Registry) string {
	s, err := reg.Get(name)
	if err != nil {
		return "?"
	}
	if err := s.Detect(); err == nil {
		if p, ok := cfgPath[name]; ok {
			return "cfg: " + p
		}
		return "PATH"
	}
	if p, ok := cfgPath[name]; ok {
		return "cfg (broken): " + p
	}
	return "not found"
}

// manageAgent is the per-agent sub-menu. Validates that "set as
// primary" only runs when Detect passes; lets the user re-enter
// the path until Detect resolves; writes cfg on success.
//
// path is the config file the change is written to.
func manageAgent(cfg *config.Config, path, name string, in *bufio.Reader, out io.Writer) error {
	for {
		cfgPath := cfgPathMap(cfg)
		reg := agentregistry.Build(cfg, "")
		detected := false
		if s, err := reg.Get(name); err == nil {
			detected = s.Detect() == nil
		}

		fmt.Fprintln(out)
		fmt.Fprintf(out, "%s — %s\n", name, manageStatus(name, cfgPath, reg))
		var opts []string
		if detected {
			opts = append(opts, "  [1] Set as primary")
		}
		if _, hasOverride := cfgPath[name]; hasOverride {
			opts = append(opts, "  [2] Change path", "  [3] Remove path override")
		} else {
			opts = append(opts, "  [2] Configure path")
		}
		opts = append(opts, "  [b] Back")
		for _, o := range opts {
			fmt.Fprintln(out, o)
		}
		fmt.Fprint(out, "> ")

		switch strings.TrimSpace(readLine(in)) {
		case "1":
			if !detected {
				fmt.Fprintln(out, "agent is not detected; configure its path first")
				continue
			}
			return saveOrError(cfg, path, out, func(c *config.Config) {
				c.Primary = name
			})
		case "2":
			agentPath, err := promptAgentPath(name, in, out)
			if err != nil {
				if errors.Is(err, errPromptAborted) {
					return nil
				}
				return err
			}
			return saveOrError(cfg, path, out, func(c *config.Config) {
				applyAgentOverride(c, name, agentPath)
			})
		case "3":
			if _, ok := cfgPath[name]; !ok {
				fmt.Fprintln(out, "no path override to remove")
				continue
			}
			return saveOrError(cfg, path, out, func(c *config.Config) {
				removeAgentOverride(c, name)
			})
		case "b", "q", "":
			return nil
		default:
			fmt.Fprintln(out, "unknown choice")
		}
	}
}

// saveOrError applies change to the config file and reports the
// result.
//
// The change is applied to the file as it is at save time, so a block
// another writer changed while the menu was open — the me.llm block
// `nightme me` writes, say — survives the save. The in-memory config
// the menu renders is changed as well, so the display stays in sync.
func saveOrError(cfg *config.Config, path string, out io.Writer, change func(*config.Config)) error {
	if err := config.Update(path, func(c *config.Config) error {
		change(c)
		return nil
	}); err != nil {
		return fmt.Errorf("save: %w", err)
	}
	change(cfg)
	fmt.Fprintf(out, "✓ saved to %s\n", path)
	// If a daemon is running, its in-memory cfg.Primary and
	// Builtins singletons (cfg.Agents overrides) are now stale.
	// The user must restart it for these changes to apply.
	paths, _ := daemoncontrol.ResolvePaths(cfg.Paths.DataDir)
	if running, _ := daemoncontrol.Ping(paths.Socket, 2*time.Second); running {
		fmt.Fprintln(out, "⚠ daemon is running — restart it for these changes to take effect")
	}
	return nil
}

// configNameMenu shows the current instance name and lets the user
// set a new one. Empty input keeps the current name unchanged.
//
// path is where the config was loaded from and where the new name
// will be saved. Callers should pass that path explicitly (see
// configInteractive) rather than letting this function resolve it
// from config.DefaultPath() — the latter would silently diverge
// when the menu was loaded from a non-default location (e.g. one
// pointed at by $NIGHTME_CONFIG).
func configNameMenu(cfg *config.Config, path string, in *bufio.Reader, out io.Writer) error {
	current := config.EffectiveName(cfg)

	fmt.Fprintf(out, "\nCurrent name: %s\n", current)
	fmt.Fprintln(out, "Enter new name (empty to keep current):")
	fmt.Fprint(out, "> ")

	value := readLine(in)
	value = strings.TrimSpace(value)

	if value == "" {
		fmt.Fprintln(out, "No changes.")
		return nil
	}

	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf(
			"config name: no config file at %q — run `nightme login` first",
			path,
		)
	} else if err != nil {
		return fmt.Errorf("config name: stat %s: %w", path, err)
	}

	if err := config.Update(path, func(c *config.Config) error {
		c.Name = value
		return nil
	}); err != nil {
		return fmt.Errorf("config name: save config: %w", err)
	}
	cfg.Name = value

	fmt.Fprintf(out, "✓ Name set to %q.\n", value)
	fmt.Fprintf(out, "  Saved to: %s\n", path)
	return nil
}

// readLine reads a single line from br. Trims the surrounding
// whitespace. Returns "" on EOF or read error.
//
// The *bufio.Reader is required (rather than io.Reader) because
// bufio.Scanner and ad-hoc bufio.NewReader wrappers both read
// ahead from the underlying io.Reader — a new Scanner or
// bufio.Reader over the same underlying source loses bytes
// between calls. Tests wrap their synthetic input in
// bufio.NewReader; production wraps os.Stdin in
// cmd/nightme/run.go.
func readLine(br *bufio.Reader) string {
	line, _ := readAnswer(br)
	return line
}
