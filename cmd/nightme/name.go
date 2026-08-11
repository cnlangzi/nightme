// Package main — `nightme name` subcommand.
//
// v1.4: introduces an instance-level identifier so multiple machines
// running the nightme daemon can be told apart in logs / IM message
// headers / gateway registrations (the latter two are not wired up
// yet — this command only stores the value).
//
// Behavior:
//
//	nightme name                  # print the effective name
//	nightme name <value>          # set name (writes to config.yaml)
//	nightme name ""               # same as `nightme name` (empty arg
//	                             # after trim is treated as no arg)
//
// Clearing the stored name (so the effective name falls back to the
// hostname) is intentionally NOT exposed as a CLI verb — there is no
// `--reset` flag, and empty args are read (not write-with-blank).
// Users who want to clear it edit config.yaml directly. Adding a
// one-shot "clear" command would invent a verb pattern this codebase
// doesn't otherwise use; the explicit-file-edit path is consistent
// with how every other section of config.yaml is managed.
//
// Set requires an existing config file at DefaultPath() — we do NOT
// silently create a fresh one, because that would also stamp default
// values for Feishu / Agents / etc. and obscure what the user hasn't
// configured yet. Show works without a config file: it just reports
// the hostname fallback.
//
// The hostname fallback is intentional and never persisted — see the
// note on Config.Name / EffectiveName in internal/config/config.go.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/config"
)

// newNameCmd returns the top-level `nightme name` command.
func newNameCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "name [value]",
		Short: "Show or set this nightme instance's name",
		Long: "name prints or sets the name used to identify this\n" +
			"nightme instance. With no arguments it prints the\n" +
			"effective name (config value, falling back to the\n" +
			"hostname). With one non-empty argument it writes the\n" +
			"value to config.yaml. An empty or whitespace-only\n" +
			"argument is treated as no argument (a read, not a\n" +
			"write). To clear the stored name, edit config.yaml\n" +
			"directly.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runName(cmd, args)
		},
	}
	return cmd
}

// runName is the testable core. Loading / saving are factored out
// via config.LoadDefault / config.SaveDefault so NIGHTME_CONFIG
// redirection in tests works the same way as in production.
//
// Contract:
//   - 0 args  → read (show effective name)
//   - 1 arg   → trim then dispatch: empty after trim is treated as
//     0 args (read), non-empty writes to config.yaml
//
// Trimming empty args down to "no arg" is deliberate: it matches
// the natural shell idiom where `cmd ""` is just shorthand for
// `cmd` with an explicit (empty) argument, and avoids forcing users
// who quoted an unset shell variable into an error path.
func runName(cmd *cobra.Command, args []string) error {
	out := cmd.OutOrStdout()

	switch len(args) {
	case 0:
		return showName(out)
	case 1:
		value := strings.TrimSpace(args[0])
		if value == "" {
			// Empty / whitespace-only → same as no argument.
			return showName(out)
		}
		return writeName(cmd, value)
	default:
		// Unreachable: cobra.MaximumNArgs(1) rejects this upstream,
		// but we keep the branch so the switch is exhaustive under
		// direct test calls.
		return fmt.Errorf("name: too many arguments (got %d)", len(args))
	}
}

// showName loads the config (or falls back to defaults) and prints
// the effective name. Works even when config.yaml is missing — the
// EffectiveName helper handles that case.
func showName(out io.Writer) error {
	cfg, err := config.LoadDefault()
	if err != nil {
		return fmt.Errorf("name: load config: %w", err)
	}
	fmt.Fprintln(out, config.EffectiveName(cfg))
	return nil
}

// writeName sets cfg.Name to value and persists via SaveDefault.
//
// Errors when config.yaml doesn't exist rather than silently
// creating one — see the package doc above for the rationale.
func writeName(cmd *cobra.Command, value string) error {
	path := config.DefaultPath()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf(
			"name: no config file at %q — run `nightme login` (or another setup command) first, then retry",
			path,
		)
	} else if err != nil {
		return fmt.Errorf("name: stat %s: %w", path, err)
	}

	cfg, err := config.LoadDefault()
	if err != nil {
		return fmt.Errorf("name: load config: %w", err)
	}
	cfg.Name = value

	if err := config.SaveDefault(cfg); err != nil {
		return fmt.Errorf("name: save config: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "✓ Name set to %q.\n", value)
	fmt.Fprintf(cmd.OutOrStdout(), "  Saved to: %s\n", path)
	return nil
}
