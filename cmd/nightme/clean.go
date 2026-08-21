// Package main — `nightme clean` subcommand.
//
// The default clean command clears volatile runtime data without losing
// durable state. With --all, it becomes a full local reset: it removes the
// config, session stores, corruption backups, daemon lifecycle files, update
// cache, known atomic-write leftovers, and the entire $HOME/.nightme
// directory.
//
// The inbox path is fixed at $HOME/.nightme/inbox to match
// internal/channel/feishu.defaultInboxBaseDir, which is what actually
// downloads attachments — it does not honour Paths.DataDir (see
// feishu.InboxBaseDir). If a future change moves the inbox under DataDir, both
// sites must move together; this comment is the tripwire.
//
// Neither mode has a dry-run or confirmation prompt. The default mode only
// truncates logs and removes inbox contents, preserving configuration and
// session state. --all deletes durable state and lifecycle files, so stop the
// daemon before using it.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/logging"
	"github.com/cnlangzi/nightme/internal/pathutil"
)

func newCleanCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "clean",
		Short: "Truncate logs and delete downloaded attachments",
		Long: "clean zeroes out nightme.log and daemon-stderr.log, and\n" +
			"removes every entry under $HOME/.nightme/inbox (the per-session\n" +
			"directory of attachments downloaded from inbound messages).\n" +
			"The default preserves configuration, session state, lock files, the\n" +
			"daemon socket, and corruption backups.\n\n" +
			"--all performs a full local reset. In addition to the default cleanup,\n" +
			"it removes the config, chat/agent session stores, daemon lifecycle\n" +
			"files, update and version-check caches, daemon-stderr.log.1, known\n" +
			"atomic-write temp files, and the entire $HOME/.nightme directory.\n" +
			"Worktree .nightme/attachments are left untouched. Stop the daemon\n" +
			"before using --all; the default mode only truncates logs and removes\n" +
			"inbox contents.\n" +
			"Runs as a destructive operation — no dry-run and no confirmation.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCleanWithOptions(cmd.OutOrStdout(), all)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false,
		"also remove durable state, config, lifecycle files, and caches")
	return cmd
}

// runClean keeps the original no-flag behavior for callers and tests.
func runClean(out io.Writer) error {
	return runCleanWithOptions(out, false)
}

// runCleanWithOptions resolves the three baseline paths and then applies the
// selected cleanup mode. A missing baseline path is reported as a no-op; other
// errors abort.
func runCleanWithOptions(out io.Writer, all bool) error {
	cfg, err := config.LoadDefault()
	if err != nil {
		if !all {
			return fmt.Errorf("clean: load config: %w", err)
		}
		// --all must still be useful when config.yaml is corrupt. Fall back
		// to the shipped defaults, then continue using the default paths.
		cfg, err = config.Load("")
		if err != nil {
			return fmt.Errorf("clean: load default config after corrupt config: %w", err)
		}
	}

	logPath, err := logging.Path(cfg)
	if err != nil {
		return fmt.Errorf("clean: resolve log path: %w", err)
	}
	// F-PATHUTIL-001 §13.3.1: NormalizeForOS covers the
	// Abs-equivalent AND the forward-slash → backslash fixup for
	// Windows. cfg-supplied log paths in YAML commonly come in
	// forward-slash form on Windows; without the Normalize, the
	// daemon's log-rotation OpenFile rejects "F:/nightme/
	// nightme.log" as a mixed-separator path.
	if abs, err := pathutil.NormalizeForOS(logPath); err == nil {
		logPath = abs
	}
	stderrPath, err := daemonStderrPath(cfg)
	if err != nil {
		return fmt.Errorf("clean: resolve stderr path: %w", err)
	}

	// Inbox lives at $HOME/.nightme/inbox, NOT <DataDir>/inbox. The
	// Feishu adapter pins the download inbox to the home directory via
	// defaultInboxBaseDir, regardless of Paths.DataDir; clean must target
	// the same path or it will silently miss the real inbox when DataDir is
	// customised.
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("clean: resolve home: %w", err)
	}
	// F-PATHUTIL-001 §13.3.1: pathutil.Join for the inbox path.
	inboxDir := pathutil.Join(home, ".nightme", "inbox")

	if all {
		return cleanAll(out, cfg, logPath, stderrPath, inboxDir)
	}

	if err := truncateLog(out, logPath); err != nil {
		return err
	}
	if err := truncateLog(out, stderrPath); err != nil {
		return err
	}
	if err := removeInbox(out, inboxDir); err != nil {
		return err
	}
	return nil
}

// cleanAll removes the baseline artifacts and every known durable/runtime
// artifact owned by nightme. It removes the entire home .nightme directory at
// the end, while keeping custom DataDir files outside the home directory
// unless they match a known nightme artifact.
func cleanAll(
	out io.Writer,
	cfg *config.Config,
	logPath string,
	stderrPath string,
	inboxDir string,
) error {
	if cfg == nil {
		return fmt.Errorf("clean: nil config")
	}

	// Delete configured/current log paths rather than truncating them: --all
	// means remove the artifacts entirely.
	if err := removePath(out, logPath); err != nil {
		return err
	}
	if err := removePath(out, stderrPath); err != nil {
		return err
	}
	if err := removePath(out, stderrPath+".1"); err != nil {
		return err
	}
	if err := removeInbox(out, inboxDir); err != nil {
		return err
	}

	// F-PATHUTIL-001: cfg.Paths.DataDir is user-supplied YAML
	// and on Windows is commonly written with forward slashes
	// (Git Bash / WSL habits). filepath.Abs converts a relative
	// path to absolute; pathutil.NormalizeForOS then ensures
	// Windows uses backslashes — otherwise dataPaths below
	// produces mixed-separator entries that os.RemoveAll
	// rejects.
	dataDir := cfg.Paths.DataDir
	if abs, err := filepath.Abs(dataDir); err == nil {
		dataDir = abs
	}
	if n, err := pathutil.NormalizeForOS(dataDir); err == nil {
		dataDir = n
	}
	dataPaths := []string{
		pathutil.Join(dataDir, "chat_sessions.json"),
		pathutil.Join(dataDir, "agent_sessions.json"),
		pathutil.Join(dataDir, "chat_sessions.json.bak"),
		pathutil.Join(dataDir, "agent_sessions.json.bak"),
		pathutil.Join(dataDir, "telegram_state.json"),
		pathutil.Join(dataDir, "registry.json"),
		pathutil.Join(dataDir, "registry.json.v1.bak"),
		pathutil.Join(dataDir, "version-check.json"),
		pathutil.Join(dataDir, "daemon.sock"),
		pathutil.Join(dataDir, "daemon.lock"),
		pathutil.Join(dataDir, "lifecycle.lock"),
		pathutil.Join(dataDir, "updates"),
	}
	for _, path := range dataPaths {
		if err := removePath(out, path); err != nil {
			return err
		}
	}

	// These are the atomic-write temp patterns used by config, chat store,
	// agent session store, and Telegram state. Only match patterns owned by
	// nightme.
	for _, pattern := range []string{
		".config-*.yaml.tmp",
		".chat_sessions-*.tmp",
		".agent_sessions-*.tmp",
		".telegram-state-*.tmp",
	} {
		matches, err := filepath.Glob(pathutil.Join(dataDir, pattern))
		if err != nil {
			return fmt.Errorf("clean: glob %s: %w", pattern, err)
		}
		for _, path := range matches {
			if err := removePath(out, path); err != nil {
				return err
			}
		}
	}

	// The config is not necessarily under DataDir: NIGHTME_CONFIG may point
	// elsewhere. It is still a known nightme-owned artifact, so --all removes
	// it at its effective config path.
	if configPath := config.DefaultPath(); configPath != "" {
		if err := removePath(out, configPath); err != nil {
			return err
		}
	}

	// The inbox is always under the home .nightme directory. --all removes
	// the directory itself, not just its contents.
	if err := removePath(out, pathutil.Dir(inboxDir)); err != nil {
		return err
	}
	return nil
}

// removePath removes either a file or a directory and reports the result.
// A missing path is an explicit no-op, matching truncateLog and removeInbox.
func removePath(out io.Writer, path string) error {
	if _, err := os.Lstat(path); err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(out, "✓ %s  (missing)\n", path)
			return nil
		}
		return fmt.Errorf("clean: stat %s: %w", path, err)
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("clean: remove %s: %w", path, err)
	}
	fmt.Fprintf(out, "✓ %s  removed\n", path)
	return nil
}

// truncateLog empties path in place. A missing file is reported
// explicitly and treated as a no-op so a fresh install doesn't fail. Other
// errors propagate.
func truncateLog(out io.Writer, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(out, "✓ %s  (missing)\n", path)
			return nil
		}
		return fmt.Errorf("clean: stat %s: %w", path, err)
	}
	if info.Size() == 0 {
		fmt.Fprintf(out, "✓ %s  (already empty)\n", path)
		return nil
	}
	if err := os.Truncate(path, 0); err != nil {
		return fmt.Errorf("clean: truncate %s: %w", path, err)
	}
	fmt.Fprintf(out, "✓ %s  truncated (%d bytes removed)\n", path, info.Size())
	return nil
}

// removeInbox deletes every entry directly under dir. The directory itself is
// preserved so the next download can recreate files without a MkdirAll race.
// Subdirectories are removed recursively because each session's attachments
// typically live in their own subdir.
func removeInbox(out io.Writer, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(out, "✓ %s  (no inbox)\n", dir)
			return nil
		}
		return fmt.Errorf("clean: read %s: %w", dir, err)
	}
	if len(entries) == 0 {
		fmt.Fprintf(out, "✓ %s  (already empty)\n", dir)
		return nil
	}
	removed := 0
	for _, e := range entries {
		if err := os.RemoveAll(pathutil.Join(dir, e.Name())); err != nil {
			return fmt.Errorf("clean: remove %s (after %d/%d entries): %w", e.Name(), removed, len(entries), err)
		}
		removed++
	}
	fmt.Fprintf(out, "✓ %s  removed %d entries\n", dir, removed)
	return nil
}
