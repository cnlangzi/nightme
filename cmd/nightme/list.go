// Package main — `nightme list` subcommand.
//
// `nightme list` reads the v1.2 persistence stores (chat_sessions.json
// + agent_sessions.json) and prints every AgentSession that is
// currently alive — i.e. StatusRunning or StatusDetached. StatusExited
// entries are auto-removed from agent_sessions.json on each list, so
// the on-disk file does not accumulate dead processes.
//
//	v1.3 text format:
//
//	  SID         CHAT       AGENT   WORKSPACE                          PID     STATUS     RESUME             STARTED
//	  as_01HF8XXX  oc_x1     claude  /home/devin/code/bailing           12345   running    abc-123            10:30:00
//	  as_01HF9XXX  oc_x2     codex   /home/devin/code/nightme           -       detached   -                  10:35:12
//
//	  `--json` swaps the table for a JSON array (one element per row).
//	  `--all` includes StatusExited entries (no GC).
//	  `--keep-exited` skips the auto-GC step even when --all is not set.
//
// Design notes:
//   - Header + rows are column-aligned via tabwriter. No field is
//     truncated: every value (chat id, agent name, workspace path,
//     agent session id, resume id) is printed verbatim so operators
//     can copy-paste any id directly into follow-up commands.
//   - The chat column resolves AgentSessionEntry.ChatSessionID to
//     ChatSessionEntry.ChatID; if the chat has been deleted (orphan),
//     the column renders `(orphan)` and the row is still shown so
//     operators can clean up by hand.
//   - The resume column is the agent's own session id (e.g. Claude
//     Code's `system/init.session_id`); captured by the runtime's
//     EventHandler on EventAgentReady and persisted so a follow-up spawn
//     can replay `--resume <id>`.
package main

import (
	"github.com/cnlangzi/nightme/internal/chatstore"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/registry"
	"github.com/cnlangzi/nightme/internal/runtime"
)

// listCmdFlags captures every flag the list subcommand accepts.
type listCmdFlags struct {
	jsonOutput bool
	all        bool
	keepExited bool
}

// listRow is the flattened view of one AgentSessionEntry joined with
// its owning ChatSessionEntry. JSON-serializable (json tags match the
// JSON-friendly camelCase surface used by the registry).
type listRow struct {
	AgentSessionID string          `json:"agentSessionId"`
	ChatID         string          `json:"chatId"`
	Agent          string          `json:"agent"`
	Cwd            string          `json:"cwd"`
	PID            int             `json:"pid"`
	Status         registry.Status `json:"status"`
	SessionID      string          `json:"resumeId,omitempty"`
	ExitCode       *int            `json:"exitCode,omitempty"`
	CreatedAt      time.Time       `json:"createdAt"`
	LastRunAt      time.Time       `json:"lastRunAt"`
}

func newListCmd() *cobra.Command {
	var f listCmdFlags

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List persisted agent sessions",
		Long: "List every agent session currently persisted in the v1.2\n" +
			"stores (chat_sessions.json + agent_sessions.json), one row\n" +
			"per AgentSession. By default, only running + detached\n" +
			"sessions are shown and exited sessions are removed from\n" +
			"disk. Pass --all to also see exited sessions, or\n" +
			"--keep-exited to skip the auto-cleanup. Pass --json to get\n" +
			"a machine-readable array.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runList(cmd, f)
		},
	}

	cmd.Flags().BoolVar(&f.jsonOutput, "json", false, "output as JSON")
	cmd.Flags().BoolVar(&f.all, "all", false, "include exited (status=exited) sessions; skip auto-GC")
	cmd.Flags().BoolVar(&f.keepExited, "keep-exited", false, "skip auto-GC of exited sessions")
	return cmd
}

// runList loads the config, opens the v1.2 stores, joins them, filters
// entries, auto-GCs exited entries, and prints the result as a table
// or JSON array.
func runList(cmd *cobra.Command, f listCmdFlags) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	csFile, asFile, err := openV12Stores(cfg, cmd.ErrOrStderr())
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}

	rows, gced, err := loadListRows(csFile, asFile, f.all, f.keepExited)
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}

	if gced > 0 {
		if es := cmd.ErrOrStderr(); es != nil {
			fmt.Fprintf(es, "list: GC removed %d exited session(s)\n", gced)
		}
	}

	if f.jsonOutput {
		return printListJSON(cmd.OutOrStdout(), rows)
	}
	printListTable(cmd.OutOrStdout(), rows)
	return nil
}

// loadConfig returns the merged Config from DefaultPath(). A missing
// file is not an error (defaults are returned) — the store paths are
// well-defined either way.
func loadConfig() (*config.Config, error) {
	cfg, err := config.LoadDefault()
	if err != nil {
		return nil, fmt.Errorf("list: load config: %w", err)
	}
	return cfg, nil
}

// openV12Stores opens chat_sessions.json + agent_sessions.json under
// cfg.Paths.DataDir. A missing parent directory is created (matching
// the daemon's behavior under cmd/nightme/run.go). It also archives
// the obsolete v0.1 registry.json to .v1.bak (best-effort).
//
// warn is the destination for non-fatal diagnostic output. Callers
// pass cmd.ErrOrStderr() so output respects the cobra context (in
// particular, --json output to stdout is not interleaved).
func openV12Stores(cfg *config.Config, warn io.Writer) (*chatstore.Store, *registry.AgentSessionFile, error) {
	dir := cfg.Paths.DataDir
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create data dir %s: %w", dir, err)
	}
	// (legacy registry cleanup removed in v1.3+; v0.1 file no
	// longer exists in this codebase)

	csPath, err := runtime.ChatSessionsPath(cfg)
	if err != nil {
		return nil, nil, err
	}
	asPath, err := runtime.AgentSessionsPath(cfg)
	if err != nil {
		return nil, nil, err
	}

	csFile, err := chatstore.New(csPath)
	if err != nil {
		return nil, nil, fmt.Errorf("open chat_sessions.json: %w", err)
	}
	asFile, err := registry.OpenAgentSessionFile(asPath)
	if err != nil {
		return nil, nil, fmt.Errorf("open agent_sessions.json: %w", err)
	}
	return csFile, asFile, nil
}

// loadListRows joins ChatSession with AgentSession entries, filters
// out StatusExited (unless all=true), and removes the latter from the
// agent_sessions.json store unless keepExited=true. Returns the
// joined rows (sorted by LastRunAt desc) and the number of exited
// entries deleted. The GC is performed in a single batched
// DeleteMany call so N exited entries cost one file rewrite, not N.
func loadListRows(
	csFile *chatstore.Store,
	asFile *registry.AgentSessionFile,
	all, keepExited bool,
) ([]listRow, int, error) {
	// Build chatByID so we can resolve AgentSessionEntry.ChatSessionID
	// to its ChatSessionEntry.ChatID for display.
	chatByID := make(map[string]*registry.ChatSessionEntry)
	for _, e := range csFile.List() {
		if e != nil {
			chatByID[e.ID] = e
		}
	}

	// First pass: collect the rows to display + the ids to GC.
	// Capacity hint: we may emit at most one row per persisted
	// agent session.
	allAS := asFile.List()
	rows := make([]listRow, 0, len(allAS))
	var toGC []string
	for _, as := range allAS {
		if as == nil {
			continue
		}
		if as.Status == registry.StatusExited {
			// Preserve the entry when it still carries a resume id
			// (e.g. Claude Code's `system/init.session_id`). The
			// next respawn of the same (chat, agent, cwd) tuple
			// uses this id to replay `--resume <id>`. Deleting it
			// here would lose the id permanently — list must not
			// destroy state the runtime needs.
			canGC := as.SessionID == ""
			if !all && !keepExited && canGC {
				toGC = append(toGC, as.ID)
			}
			if !all {
				continue
			}
		} else if as.Status != registry.StatusRunning && as.Status != registry.StatusDetached {
			// Unknown status — be conservative: keep in display only
			// when --all, otherwise leave alone (don't GC unknown).
			if !all {
				continue
			}
		}

		chatID := "(orphan)"
		if cs, ok := chatByID[as.ChatSessionID]; ok && cs != nil {
			chatID = cs.ChatID
		}
		rows = append(rows, listRow{
			AgentSessionID: as.ID,
			ChatID:         chatID,
			Agent:          as.Agent,
			Cwd:            as.Cwd,
			PID:            as.PID,
			Status:         as.Status,
			SessionID:      as.SessionID,
			ExitCode:       as.ExitCode,
			CreatedAt:      as.CreatedAt,
			LastRunAt:      as.LastRunAt,
		})
	}

	// Second pass: single batched GC write.
	if len(toGC) > 0 {
		if err := asFile.DeleteMany(toGC); err != nil {
			return nil, 0, fmt.Errorf("gc batch (%d): %w", len(toGC), err)
		}
	}

	sort.Slice(rows, func(i, j int) bool {
		return rows[i].LastRunAt.After(rows[j].LastRunAt)
	})
	return rows, len(toGC), nil
}

// listColumn widths hint tabwriter's minimum column padding. None
// of the fields are truncated — every value is printed verbatim
// so operators can copy-paste any id (chat, cwd, agent session id,
// resume id) directly into follow-up commands.
const (
	colChat      = 24
	colAgent     = 10
	colPID       = 8
	colStatus    = 10
	colWorkspace = 48
	colSID       = 32
	colResume    = 36
)

// printListTable writes the human-readable table to w. The header is
// always emitted even when there are no rows, so users see an
// unambiguous "registry is empty" instead of "did the command run?".
func printListTable(w io.Writer, rows []listRow) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CHAT\tAGENT\tPID\tSTATUS\tWORKSPACE\tSTARTED\tSID\tRESUME")
	if len(rows) == 0 {
		tw.Flush()
		return
	}
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.ChatID,
			r.Agent,
			pidCell(r.PID),
			statusCell(r.Status, r.ExitCode),
			r.Cwd,
			startCell(r.LastRunAt),
			r.AgentSessionID,
			resumeCell(r.SessionID),
		)
	}
	tw.Flush()
}

// pidCell returns the PID column value. Exited/detached entries may
// have PID=0, which we render as "-" to match v0.x conventions.
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

// resumeCell renders the agent's resume id, or "-" when the
// agent has no resume semantics (ACP / Pi / PTY) or the id has
// not yet been captured. We intentionally do NOT truncate the
// resume id — operators copy-paste it into `claude --resume <id>`.
func resumeCell(id string) string {
	if id == "" {
		return "-"
	}
	return id
}

// startCell formats a timestamp as HH:MM:SS (24h, local time). We keep
// the date out of the default view — `nightme list` is for "what's
// running right now".
func startCell(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("15:04:05")
}

// printListJSON serializes rows to w. The output is the raw array
// (not wrapped in an envelope) so `jq '.[]'` works directly.
func printListJSON(w io.Writer, rows []listRow) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
}
