// Package main — `nightme clean` subcommand.
//
// Clears volatile runtime data so the operator can recover disk
// space without losing durable state. Scope is intentionally narrow:
// the two log files (nightme.log + daemon-stderr.log) and the
// per-session attachment inbox under $HOME/.nightme/inbox.
// Anything that holds session state or is required by the daemon
// lifecycle (agent_sessions.json, chat_sessions.json, *.lock,
// *.sock) is left alone.
//
// The inbox path is fixed at $HOME/.nightme/inbox to match
// internal/channel/feishu.defaultInboxBaseDir, which is what
// actually downloads attachments — it does not honour Paths.DataDir
// (see feishu.InboxBaseDir). If a future change moves the inbox
// under DataDir, both sites must move together; this comment is the
// tripwire.
//
// Behaviour is deliberately destructive: no flags, no dry-run, no
// confirmation prompt. The user is expected to know what they are
// asking for. Side-effects on a running daemon:
//
//   - nightme.log is held open with O_APPEND by the daemon's
//     slog writer. Truncating the file in place leaves the
//     writer's per-fd offset untouched, so subsequent writes
//     write into a sparse file at the previous offset. The next
//     daemon restart reopens the file from offset 0 and the
//     log returns to normal. Acceptable for a manual cleanup;
//     operators who want a clean handoff should `nightme stop`
//     first.
//   - daemon-stderr.log is only written by the daemon at panic
//     time (rare), so the same caveat is even less likely to
//     bite.
//   - inbox entries are per-session attachment directories. If
//     an agent is still mid-turn when clean runs, the agent's
//     reference to LocalPath will point to a now-deleted file.
//     The next inbound message from the same chat recreates the
//     directory, so the worst case is one failed prompt rather
//     than a crash.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/logging"
)

func newCleanCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "clean",
		Short: "Truncate log files and delete downloaded attachments",
		Long: "clean zeroes out nightme.log and daemon-stderr.log, and\n" +
			"removes every entry under $HOME/.nightme/inbox (the per-session\n" +
			"directory of attachments downloaded from inbound messages).\n" +
			"The inbox path is fixed at $HOME/.nightme/inbox regardless of\n" +
			"Paths.DataDir — it tracks the Feishu attachment inbox, which\n" +
			"is also pinned to the home directory.\n" +
			"Session JSON files, lock files, and the daemon socket are\n" +
			"left untouched. Runs as a destructive operation — no flags,\n" +
			"no dry-run, no confirmation.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runClean(cmd.OutOrStdout())
		},
	}
	return cmd
}

// runClean resolves the three target paths and clears each in
// turn. Each step is best-effort: a missing log file is reported
// as "missing" and treated as a no-op, so a fresh install doesn't
// fail. A missing inbox is the same. Other errors abort.
func runClean(out io.Writer) error {
	cfg, err := config.LoadDefault()
	if err != nil {
		return fmt.Errorf("clean: load config: %w", err)
	}
	logPath, err := logging.Path(cfg)
	if err != nil {
		return fmt.Errorf("clean: resolve log path: %w", err)
	}
	if abs, err := filepath.Abs(logPath); err == nil {
		logPath = abs
	}
	stderrPath, err := daemonStderrPath(cfg)
	if err != nil {
		return fmt.Errorf("clean: resolve stderr path: %w", err)
	}
	// Inbox lives at $HOME/.nightme/inbox, NOT <DataDir>/inbox. The
	// Feishu adapter pins the download inbox to the home directory
	// via defaultInboxBaseDir, regardless of Paths.DataDir; clean
	// must target the same path or it will silently miss the real
	// inbox when DataDir is customised.
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("clean: resolve home: %w", err)
	}
	inboxDir := filepath.Join(home, ".nightme", "inbox")

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

// truncateLog empties path in place. A missing file is reported
// explicitly and treated as a no-op so a fresh install doesn't
// fail. Other errors propagate.
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

// removeInbox deletes every entry directly under dir. The
// directory itself is preserved so the next download can recreate
// files without a MkdirAll race. Subdirectories are removed
// recursively because each session's attachments typically live in
// their own subdir.
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
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return fmt.Errorf("clean: remove %s (after %d/%d entries): %w", e.Name(), removed, len(entries), err)
		}
		removed++
	}
	fmt.Fprintf(out, "✓ %s  removed %d entries\n", dir, removed)
	return nil
}
