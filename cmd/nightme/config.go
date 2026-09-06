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

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentregistry"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/pathutil"
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
	return configInteractive(cfg, path, bufio.NewReader(os.Stdin), os.Stdout)
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
			if err := configAgentsMenu(cfg, in, out); err != nil {
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

// configAgentsMenu lists every built-in agent with its current
// detection status and lets the user pick one to manage (set as
// primary, configure path, or clear an existing override). Only
// the seven built-ins are listed — cfg.Agents no longer accepts
// non-whitelist names (see internal/agentregistry).
//
// Detection is re-run on each menu render so the status column
// stays in sync with disk as the user edits paths.
func configAgentsMenu(cfg *config.Config, in *bufio.Reader, out io.Writer) error {
	builtins := agent.Builtins.List()

	for {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Agents:")
		cfgPath := cfgPathMap(cfg)
		reg := agentregistry.Build(cfg, "")
		for i, s := range builtins {
			name := s.Info().Name
			marker := "  "
			if name == cfg.Primary {
				marker = "* "
			}
			fmt.Fprintf(out, "  %s[%d] %-9s %s\n", marker, i+1, name, agentStatus(name, cfgPath, reg))
		}
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
		if err := manageAgent(cfg, picked.Info().Name, in, out); err != nil {
			fmt.Fprintf(out, "Error: %v\n", err)
		}
	}
}

// cfgPathMap flattens cfg.Agents into a name → path map. Strips
// any trailing args from the command string so the path column
// always shows the executable only.
func cfgPathMap(cfg *config.Config) map[string]string {
	out := make(map[string]string, len(cfg.Agents))
	for _, e := range cfg.Agents {
		if e.Name == "" || e.Command == "" {
			continue
		}
		fields := strings.Fields(e.Command)
		if len(fields) == 0 {
			continue
		}
		out[e.Name] = fields[0]
	}
	return out
}

// agentStatus renders the status column for the menu. The
// underlying registry has already been built with cfg.Agents
// applied, so Detect reflects the configured path when one
// exists.
func agentStatus(name string, cfgPath map[string]string, reg *agent.Registry) string {
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
func manageAgent(cfg *config.Config, name string, in *bufio.Reader, out io.Writer) error {
	for {
		cfgPath := cfgPathMap(cfg)
		reg := agentregistry.Build(cfg, "")
		detected := false
		if s, err := reg.Get(name); err == nil {
			detected = s.Detect() == nil
		}

		fmt.Fprintln(out)
		fmt.Fprintf(out, "%s — %s\n", name, agentStatus(name, cfgPath, reg))
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
			cfg.Primary = name
			return saveOrError(cfg, out)
		case "2":
			path, err := promptAgentPath(name, in, out)
			if err != nil {
				if errors.Is(err, errPromptAborted) {
					return nil
				}
				return err
			}
			applyAgentOverride(cfg, name, path)
			return saveOrError(cfg, out)
		case "3":
			if _, ok := cfgPath[name]; !ok {
				fmt.Fprintln(out, "no path override to remove")
				continue
			}
			removeAgentOverride(cfg, name)
			return saveOrError(cfg, out)
		case "b", "q", "":
			return nil
		default:
			fmt.Fprintln(out, "unknown choice")
		}
	}
}

func saveOrError(cfg *config.Config, out io.Writer) error {
	if err := config.SaveDefault(cfg); err != nil {
		return fmt.Errorf("save: %w", err)
	}
	fmt.Fprintf(out, "✓ saved to %s\n", config.DefaultPath())
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

	cfg.Name = value
	if err := config.Save(cfg, path); err != nil {
		return fmt.Errorf("config name: save config: %w", err)
	}

	fmt.Fprintf(out, "✓ Name set to %q.\n", value)
	fmt.Fprintf(out, "  Saved to: %s\n", path)
	return nil
}

// readLine reads a single line from br. Trims trailing newline.
// Returns "" on EOF or read error.
//
// The *bufio.Reader is required (rather than io.Reader) because
// bufio.Scanner and ad-hoc bufio.NewReader wrappers both read
// ahead from the underlying io.Reader — a new Scanner or
// bufio.Reader over the same underlying source loses bytes
// between calls. Tests wrap their synthetic input in
// bufio.NewReader; production wraps os.Stdin in
// cmd/nightme/run.go.
func readLine(br *bufio.Reader) string {
	line, err := br.ReadString('\n')
	if err != nil && line == "" {
		return ""
	}
	return strings.TrimRight(line, "\r\n")
}

// ensureParentDir is a small helper for code paths that write files
// outside the standard config.Save path. Exported for test use.
func ensureParentDir(path string) error {
	dir := pathutil.Dir(path)
	return os.MkdirAll(dir, 0o700)
}
