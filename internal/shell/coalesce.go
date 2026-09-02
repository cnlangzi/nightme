// Package shell: line-buffered coalescer for the streaming
// executeShell path. Time-based debouncing lives in dispatch.go
// (chunkDebouncer); this file only does size-based chunking.
package shell

import (
	"bufio"
	"bytes"
	"io"
	"strings"
)

const (
	chunkBytes   = 4 * 1024
	maxLineBytes = 1 << 20
)

// coalesceLines reads r line by line, always writes each line to
// sink, and — when onChunk != nil — flushes accumulated text to
// onChunk on a chunkBytes / EOF trigger. isStderr=true prefixes
// every line with "stderr: ".
func coalesceLines(
	r io.Reader, sink *bytes.Buffer,
	onChunk func(string) error, isStderr bool,
) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), maxLineBytes)

	var (
		b   strings.Builder
		n   int
		err error
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
			_ = flush()
		}
	}

	for sc.Scan() {
		appendLine(sc.Text())
		if err != nil {
			break
		}
	}
	if err == nil && sc.Err() == nil {
		flush()
	}
	if err != nil {
		return err
	}
	return sc.Err()
}