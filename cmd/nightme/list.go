// Package main — `nightme list` subcommand.
//
// `nightme list` reads the on-disk process registry and prints every
// persisted session. It does NOT need a running nightme daemon — the
// registry is a plain JSON file in $data_dir/registry.json.
//
//	v0.1 text format:
//
//	  SID         AGENT   WORKSPACE                          PID     STATUS     STARTED
//	  s_01HF8XXX  claude  /home/devin/code/bailing           12345   running    10:30:00
//	  s_01HF9XXX  claude  /home/devin/code/nightme           -       exited(0)  10:35:12
//
//	`--json` swaps the table for a JSON array (one element per entry).
//
// Design notes (per docs/feat/F-10-session-list-cmd.md):
//   - Header + rows are column-aligned to a fixed width; long values
//     are truncated so the output stays readable on a 120-col terminal.
//   - Workspace ellipsis keeps the right-hand columns aligned; the
//     full path is recoverable from `--json`.
//   - PID is "-" for exited entries (PID was cleared at Kill time).
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/registry"
)

// listCmdFlags captures every flag the list subcommand accepts.
type listCmdFlags struct {
	jsonOutput bool
}

func newListCmd() *cobra.Command {
	var f listCmdFlags

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List persisted sessions",
		Long: "List every session recorded in the registry, one row per\n" +
			"session. Pass --json to get a machine-readable array.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runList(cmd, f)
		},
	}

	cmd.Flags().BoolVar(&f.jsonOutput, "json", false, "output as JSON")
	return cmd
}

// runList loads the config, opens the registry, and prints every
// entry as a table (default) or JSON array (--json).
func runList(cmd *cobra.Command, f listCmdFlags) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	reg, err := openRegistry(cfg)
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}

	entries := reg.List()
	if f.jsonOutput {
		return printListJSON(cmd.OutOrStdout(), entries)
	}
	printListTable(cmd.OutOrStdout(), entries)
	return nil
}

// loadConfig returns the merged Config from DefaultPath(). A missing
// file is not an error (defaults are returned) — the registry path
// is well-defined either way.
func loadConfig() (*config.Config, error) {
	cfg, err := config.LoadDefault()
	if err != nil {
		return nil, fmt.Errorf("list: load config: %w", err)
	}
	return cfg, nil
}

// openRegistry loads the registry.File from cfg. The registry path is
// resolved relative to cfg.Paths.DataDir if it is not absolute (this
// matches the spec'd behavior in F-05 §2).
func openRegistry(cfg *config.Config) (*registry.File, error) {
	path, err := registryPath(cfg)
	if err != nil {
		return nil, err
	}
	return registry.Open(path)
}

// registryPath resolves cfg.Paths.RegistryFile relative to
// cfg.Paths.DataDir when it is not absolute.
func registryPath(cfg *config.Config) (string, error) {
	name := cfg.Paths.RegistryFile
	if name == "" {
		name = "registry.json"
	}
	if filepath.IsAbs(name) {
		return name, nil
	}
	dir := cfg.Paths.DataDir
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("list: create data dir %s: %w", dir, err)
	}
	return filepath.Join(dir, name), nil
}

// listColumn widths match the format spec in F-10 §3. They are
// package-level constants so both printListTable and any future
// tests share a single source of truth.
const (
	colSID       = 16
	colAgent     = 8
	colWorkspace = 36
	colPID       = 8
	colStatus    = 14
)

// printListTable writes the human-readable table to w. The header is
// always emitted even when there are no entries, so users see an
// unambiguous "registry is empty" instead of "did the command run?".
func printListTable(w io.Writer, entries []registry.Entry) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SID\tAGENT\tWORKSPACE\tPID\tSTATUS\tSTARTED")
	if len(entries) == 0 {
		tw.Flush()
		return
	}
	for _, e := range entries {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			truncate(e.SessionID, colSID),
			truncate(e.Agent, colAgent),
			truncate(e.Workspace, colWorkspace),
			pidCell(e.PID),
			statusCell(e.Status, e.ExitCode),
			startCell(e.StartedAt),
		)
	}
	tw.Flush()
}

// pidCell returns the PID column value. Exited entries have PID=0 in
// the registry, which we render as "-" to match F-10 §3.
func pidCell(pid int) string {
	if pid <= 0 {
		return "-"
	}
	return fmt.Sprintf("%d", pid)
}

// statusCell formats a registry status with its exit code when
// available: "exited(0)", "exited(2)", etc.
func statusCell(s registry.Status, code *int) string {
	if s == registry.StatusExited && code != nil {
		return fmt.Sprintf("%s(%d)", s, *code)
	}
	return string(s)
}

// startCell formats a timestamp as HH:MM:SS (24h, local time). We keep
// the date out of the default view — `nightme list` is for "what's
// running right now"; a separate `--since` filter can land later.
func startCell(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("15:04:05")
}

// truncate shortens s to at most n runes, appending an ellipsis when
// it does not fit. The result is intended for fixed-width columns so
// no escaping is performed.
func truncate(s string, n int) string {
	if n <= 1 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-1]) + "…"
}

// printListJSON serializes entries to w. The output is the raw
// entries (not wrapped in an envelope) so `jq '.[]'` works directly.
func printListJSON(w io.Writer, entries []registry.Entry) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(entries)
}