// Package main — `nightme logs` subcommand.
//
// The daemon (re)launched by `nightme start` runs detached: its
// stdout and stderr are redirected to /dev/null so the parent shell
// stays clean. The only persistent trace of daemon activity is the
// slog file configured via Logging.File (default
// $HOME/.nightme/nightme.log), so the natural way to
// watch the daemon after `start` is to tail that file.
//
// Behavior mirrors `tail -f`:
//   - Prints the last N existing lines (default 50) so the user
//     gets immediate context.
//   - Then follows new lines appended by the daemon until SIGINT
//     or SIGTERM (Ctrl-C from the terminal, or the kill signal a
//     supervisor sends). The signal breaks out of the loop and
//     the REPL / shell continues without losing the prompt.
//
// --once switches to a one-shot tail (prints the last N lines
// and exits with 0) — useful in scripts that want a snapshot
// without an open handle on the file.
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/logging"
	nmerrors "github.com/cnlangzi/nightme/internal/errors"
)

const (
	// followPollInterval is the trade-off between latency and
	// wakeups when tailing. The file's only writer is the
	// nightme daemon (single-digit log lines per minute at steady
	// state), so 200ms is more than enough to catch every
	// append without hammering stat().
	followPollInterval = 200 * time.Millisecond

	// logsDefaultLines is the count of trailing lines the command
	// prints before entering follow mode. Mirrors tail's default
	// of 10, bumped to 50 so the user sees enough startup context
	// after a fresh `start`.
	logsDefaultLines = 50
)

type logsCmdFlags struct {
	lines  int
	follow bool
}

// logsOnce is read directly by the cobra.RunE callback (rather
// than captured in the flags struct) because pflag binds each
// flag to the address passed at registration time, and binding
// --once to &f.follow would flip follow=true (NoOptDefVal="true")
// when the user types a bare --once, which is the opposite of
// what --once should mean.
//
// Cobra/pflag do not reset flag values between successive
// Execute() calls (verified empirically: a bare --once in one
// invocation leaks into the next). RunE captures the value
// locally and resets the backing variable so a subsequent REPL
// invocation sees the default unless --once is re-supplied.
var logsOnce bool

func newLogsCmd() *cobra.Command {
	f := logsCmdFlags{lines: logsDefaultLines, follow: true}
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Tail the nightme daemon log file (like tail -f)",
		Long: "logs tails the slog file the daemon writes to. By\n" +
			"default it prints the last 50 lines and then follows\n" +
			"new lines until interrupted. Use --once for a\n" +
			"one-shot snapshot.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			once := logsOnce
			// Reset for the next invocation so a single
			// --once doesn't leak into subsequent REPL
			// commands (cobra/pflag don't reset flag values
			// between Execute() calls).
			logsOnce = false
			if once {
				f.follow = false
			}
			return runLogs(cmd, f)
		},
	}
	cmd.Flags().IntVarP(&f.lines, "lines", "n", logsDefaultLines, "lines of existing log to print before following")
	cmd.Flags().BoolVarP(&f.follow, "follow", "f", true, "keep following the log file (use --follow=false / --once for a snapshot)")
	// --once is a friendlier alias for the one-shot "print and
	// exit" mode; pflag doesn't synthesize `--no-follow` from a
	// default-true bool and expecting users to type
	// `--follow=false` is awkward.
	cmd.Flags().BoolVar(&logsOnce, "once", false, "print tail and exit (equivalent to --follow=false)")
	return cmd
}

// resolveLogPath returns the absolute log path the daemon writes
// to, expanding tilde the way config.Load does.
func resolveLogPath(cfg *config.Config) (string, error) {
	p, err := logging.Path(cfg)
	if err != nil {
		return "", err
	}
	// logging.Path falls back to a non-empty default when
	// Logging.File is unset, so an empty return value here is
	// unreachable in practice. The filepath.Abs call below is
	// useful only when the user supplies a relative path in
	// Logging.File; errors from it are non-fatal.
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return p, nil
}

// tailLines returns up to n trailing lines from path. It reads the
// file in chunks from the end so a multi-megabyte log doesn't get
// slurped into memory just to grab the last 50 lines.
//
// The implementation is a small reverse reader: read the trailing
// chunk large enough to capture n newlines (capped at 1 MiB to
// bound memory for pathological cases), scan backwards for
// newlines, and slice off the prefix after the (n-th-from-the-end)
// separator.
func tailLines(path string, n int) ([][]byte, error) {
	if n <= 0 {
		return nil, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	const maxChunk = 1 << 20 // 1 MiB
	size := info.Size()
	if size == 0 {
		return nil, nil
	}
	readSize := size
	if readSize > maxChunk {
		readSize = maxChunk
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(size-readSize, io.SeekStart); err != nil {
		return nil, err
	}
	buf := make([]byte, readSize)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, err
	}
	// Trim any partial leading line: if we didn't read from the
	// start of the file, the first line of buf is not a complete
	// log record, so drop it to avoid emitting a half-line that
	// JSON-formatters can't parse.
	if readSize < size {
		if idx := bytes.IndexByte(buf, '\n'); idx >= 0 {
			buf = buf[idx+1:]
		} else {
			// No newline at all inside the chunk — the file is
			// one giant unterminated line, rare for slog JSON.
			// Bail out with what we have rather than emit garbage.
			return nil, nil
		}
	}
	lines := bytes.Split(bytes.TrimRight(buf, "\n"), []byte{'\n'})
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

// runLogs is the body of `nightme logs`. It loads the config to
// find the log path, prints the tail, and — when follow is true —
// loops waiting for new bytes appended to the file. Ctrl-C /
// SIGTERM unblocks the follow loop via a signal-bound context.
func runLogs(cmd *cobra.Command, f logsCmdFlags) error {
	if f.lines < 0 {
		return nmerrors.New(nmerrors.CodeValidationError, "logs: --lines must be non-negative")
	}
	cfg, err := config.LoadDefault()
	if err != nil {
		return fmt.Errorf("logs: load config: %w", err)
	}
	path, err := resolveLogPath(cfg)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	tail, err := tailLines(path, f.lines)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(out, "logs: %s does not exist yet (is the daemon running?)\n", path)
			if !f.follow {
				return nil
			}
		} else {
			return fmt.Errorf("logs: read %s: %w", path, err)
		}
	} else {
		for _, line := range tail {
			if _, err := out.Write(append(line, '\n')); err != nil {
				return err
			}
		}
	}
	if !f.follow {
		return nil
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return followLog(ctx, out, path)
}

// followLog opens path and prints every newly-appended chunk of
// bytes. It exits cleanly when ctx is cancelled (Ctrl-C / SIGTERM)
// and resumes from offset 0 if the file is rotated or truncated
// under us (size shrinks past our last offset).
//
// Polling is used instead of fsnotify/inotify so the implementation
// stays portable (the rest of the daemon already depends on Go's
// build tag for unix-only paths; tail -f should not add another
// platform matrix).
func followLog(ctx context.Context, out io.Writer, path string) error {
	f, err := os.Open(path)
	if err != nil {
		// Common case at startup: file doesn't exist yet. Wait
		// for it to appear rather than failing loudly.
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("logs: open %s: %w", path, err)
		}
		for {
			f, err = os.Open(path)
			if err == nil {
				break
			}
			if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("logs: open %s: %w", path, err)
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(followPollInterval):
			}
		}
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("logs: stat %s: %w", path, err)
	}
	offset := info.Size()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return fmt.Errorf("logs: seek %s: %w", path, err)
	}

	reader := bufio.NewReader(f)
	ticker := time.NewTicker(followPollInterval)
	defer ticker.Stop()
	// Flush partial lines on exit so the trailing record (which may
	// arrive a few ms before SIGINT) is not lost mid-JSON.
	var pending bytes.Buffer
	flush := func() {
		if pending.Len() == 0 {
			return
		}
		// pending.Bytes() returns a slice into the buffer that
		// remains valid until the next pending mutation; out.Write
		// does not retain it, so a copy is unnecessary.
		_, _ = out.Write(pending.Bytes())
		pending.Reset()
	}

	for {
		select {
		case <-ctx.Done():
			flush()
			return nil
		case <-ticker.C:
		}
		// Detect rotation/truncation: size went backwards.
		info, err := f.Stat()
		if err != nil {
			return fmt.Errorf("logs: stat %s: %w", path, err)
		}
		if info.Size() < offset {
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				return fmt.Errorf("logs: rewind %s: %w", path, err)
			}
			reader = bufio.NewReader(f)
			offset = 0
		}
		for {
			chunk, err := reader.ReadSlice('\n')
			if err == nil {
				if _, werr := out.Write(chunk); werr != nil {
					return werr
				}
				offset += int64(len(chunk))
				continue
			}
			if errors.Is(err, io.EOF) {
				// No complete line yet — stash the partial
				// bytes so the next iteration can prepend
				// them when the line closes.
				pending.Write(chunk)
				break
			}
			if errors.Is(err, bufio.ErrBufferFull) {
				// Pathological: line longer than the reader
				// buffer (default 64 KiB). Emit what we have
				// and continue accumulating.
				if _, werr := out.Write(chunk); werr != nil {
					return werr
				}
				offset += int64(len(chunk))
				continue
			}
			return fmt.Errorf("logs: read %s: %w", path, err)
		}
	}
}
