// Package shell: line-buffered streaming coalescer used by the
// streaming executeShell path. Lives in its own file so the
// dispatcher stays focused on the OutReply protocol; coalesceLines
// is pure data-in / data-out with no platform or emitter
// dependencies and can be unit-tested in isolation.
package shell

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"time"
)

const (
	// chunkBytes is the soft cap on a single coalesced chunk
	// before coalesceLines flushes it via onChunk. 4 KiB matches
	// roughly one Markdown div in the Feishu card, which keeps
	// each PATCH visually coherent.
	chunkBytes = 4 * 1024

	// flushInterval caps the send rate to ~1/250ms = 4 chunks/sec
	// regardless of how fast the child writes. Without it, fast
	// commands (`seq 1 1000` in 100ms) flood the channel with
	// 6+ PATCHes in a tiny window — most get throttled by the
	// channel anyway, and the user sees only the final state.
	//
	// F-shell-stream design: shell-side debouncing rather than
	// channel-side throttling. No channel can keep up with raw
	// shell output (multi-MB/s bursts); capping the rate at the
	// producer lets channels stay simple (no per-stream rate
	// limits in the adapter).
	//
	// The ticker check between scanner reads is best-effort: when
	// the scanner is blocked on Read (child is slow), the ticker
	// can't fire mid-read, but it will fire on the next iteration.
	// For continuous output the size threshold dominates (we hit
	// chunkBytes long before 250ms elapses); for slow / line-
	// by-line output the time threshold batches lines into 250ms
	// windows.
	flushInterval = 250 * time.Millisecond

	// maxLineBytes caps a single line that bufio.Scanner will
	// accept. Lines longer than this surface as bufio.ErrTooLong,
	// which coalesceLines propagates; the caller (executeShell)
	// cancels the child via CommandContext. 1 MiB is generous
	// (no realistic shell output line is longer) without
	// permitting unbounded memory growth if a child misbehaves.
	maxLineBytes = 1 << 20
)

// coalesceLines reads r line by line, always writes each line to
// sink, and — when onChunk != nil — flushes accumulated text to
// onChunk on a chunkBytes / EOF trigger. isStderr=true prefixes
// every line with "stderr: " so the rendered card visually separates
// the two streams.
//
// Two buffers with different lifetimes:
//   - sink: receives EVERY line read, regardless of onChunk
//     success. Used by Run() to collect the full stdout/stderr
//     after the command exits. Never reset.
//   - b: coalesces lines between onChunk invocations. Reset on
//     each successful flush. Holds the partial chunk currently
//     being assembled for onChunk.
//
// This separation lets gtw/hooks.go (which uses Run with
// onChunk=nil) get the complete stdout via sink without paying the
// streaming cost, while the dispatcher (onChunk != nil) gets
// real-time chunks AND a complete stdout via sink in r.Stdout.
//
// Returns the first non-nil onChunk error so the caller can abort
// the child (the dispatcher's onChunk returns the emitter error,
// runShell cancels its ctx on drainErr, CommandContext kills the
// child).
func coalesceLines(
	r io.Reader, sink *bytes.Buffer,
	onChunk func(string) error, isStderr bool,
) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), maxLineBytes)

	var (
		b          strings.Builder
		n          int
		err        error
		lastFlush  = time.Now()
	)

	flush := func() bool {
		if b.Len() == 0 {
			return true
		}
		if onChunk != nil {
			if e := onChunk(b.String()); e != nil {
				err = e
				return false
			}
		}
		b.Reset()
		n = 0
		lastFlush = time.Now()
		return true
	}
	appendLine := func(line string) {
		if isStderr {
			line = "stderr: " + line
		}
		line += "\n"
		sink.WriteString(line)
		b.WriteString(line)
		n += len(line)
		if n >= chunkBytes {
			_ = flush() // err captured into outer err
		}
	}

	// Time-based debouncing: the loop checks time.Since(lastFlush)
// on every iteration. We don't use a ticker because the scanner
// is the source of truth for "ready to process" — between line
// reads, we ask "has flushInterval elapsed?" and flush if so.
// This catches up regardless of how many ticks would have fired
// during a slow sc.Scan, and avoids the channel-buffer races
// that come with ticker.C select inside a tight loop.

	for sc.Scan() {
		// Time-based flush: if the interval has elapsed since the
		// last flush AND there's pending content, flush now.
		if time.Since(lastFlush) >= flushInterval && b.Len() > 0 {
			if !flush() {
				break
			}
		}
		appendLine(sc.Text())
		if err != nil {
			break
		}
	}
	// Trailing flush: skip if onChunk already errored (otherwise
	// we'd re-invoke onChunk with the same buffered content and
	// overwrite err with the same value). Also skip on scanner
	// error — the buffered bytes are an incomplete record and
	// emitting them mid-error would mislead the user; the caller
	// will cancel the child and surface the diagnostic separately.
	if err == nil && sc.Err() == nil {
		flush()
	}
	if err != nil {
		return err
	}
	return sc.Err()
}
