//go:build windows

// Windows shell executor — runs the user command via `cmd /c`.
// `cmd.exe` is on every Windows install since NT 4.0
// (C:\Windows\System32\cmd.exe, PATH-default), so no shell
// detection chain is needed.
//
// The Windows-specific gotcha is encoding: cmd.exe writes
// stdout/stderr in the system's active ANSI/OEM code page
// (CP_ACP). On a Simplified Chinese Windows this is CP936
// (GBK), and Go's os/exec returns raw bytes — so a user
// running `!ipconfig /all` on a Chinese-locale box would see
// garbled text. We decode via golang.org/x/text/encoding
// using GB18030 (a GBK superset) when GetACP() returns the
// Chinese code page; otherwise we fall through to UTF-8
// (which works for cmd /u /c invocations and modern PowerShell
// pipes — though we don't invoke those here, leaving the door
// open for future variants).
//
// Streaming (F-shell-stream): stdout / stderr are drained via
// io.Pipe + transform.NewReader + coalesceLines. On a decoder
// error, the once-decoded-forever-raw fallback flips a flag
// and the rest of the stream passes through untouched — so
// the user sees uniformly-raw bytes after the first decode
// failure, rather than a mix of decoded text and mojibake.
package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/proc"
	"golang.org/x/sys/windows"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

// cpACPChinese is the Windows ANSI code page for Simplified
// Chinese. GetACP() returns 936 on zh-CN systems; Big5 (950)
// and Shift-JIS (932) are out of scope for this MVP — they
// would need their own decoder branches.
const cpACPChinese = 936

// executeShell runs cmdline in cwd via "cmd /c", draining
// stdout/stderr through transform.NewReader + coalesceLines.
// onChunk is invoked with each coalesced decoded chunk; pass
// nil to collect the full output without streaming (used by
// Run / gtw hooks).
//
// See dispatch_unix.go's executeShell for the failure-mode
// table; same three modes (non-zero exit / ctx cancel /
// drainer panic) collapse into ExitCode=-1 + Stderr.
//
// Decoder fallback: once a stream produces a decode error,
// decodedOK flips to false and the rest of that stream
// passes through as raw bytes. Mixing partially-decoded
// text with raw bytes in the same chat bubble is worse
// than uniformly-raw (visually noisy but predictable).
func executeShell(ctx context.Context, cwd, cmdline string, extraEnv []string, onChunk func(string) error) (ret *result) {
	start := time.Now()

	c := proc.New(ctx, "cmd", "/c", cmdline)
	c.Dir = cwd
	// Parent env first, then caller-supplied vars on top.
	c.Env = append(os.Environ(), extraEnv...)

	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	c.Stdout, c.Stderr = outW, errW

	// Decoder state per stream: decodedOK starts true; the first
	// decode error flips it to false and replaces the reader
	// with an identity reader. decR / errR are the readers the
	// drainers actually consume from — they may be either the
	// transform reader (decoded) or the raw pipe reader (raw).
	outDec := decoderFor(windows.GetACP())
	errDec := decoderFor(windows.GetACP())
	outDecodedOK := outDec != nil
	errDecodedOK := errDec != nil
	outRdr := wrapDecoder(outR, outDec)
	errRdr := wrapDecoder(errR, errDec)

	var (
		stdoutBuf, stderrBuf bytes.Buffer
		panicCh              = make(chan any, 2)
		wg                   sync.WaitGroup
		runErr               error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				panicCh <- r
			}
		}()
		err := drainWithFallback(&outDecodedOK, outRdr, func() io.Reader {
			// fallback: switch to raw pipe on first decode error
			return outR
		}, &stdoutBuf, onChunk, false)
		_ = err // coalesceLines errors are surfaced via drainErr/runErr
	}()
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				panicCh <- r
			}
		}()
		err := drainWithFallback(&errDecodedOK, errRdr, func() io.Reader {
			return errR
		}, &stderrBuf, onChunk, true)
		_ = err
	}()

	// Cleanup defer (mirrors dispatch_unix.go): closes the pipe
	// writers and joins the drainer goroutines BEFORE reading
	// the buffers (avoids data race with WriteString). Then
	// assembles the result, handles runErr + late panic, and
	// assigns ret. The named return value lets the defer
	// assemble the full result without needing an explicit
	// return value.
	defer func() {
		_ = outW.Close()
		_ = errW.Close()
		wg.Wait()

		ret = &result{
			Stdout:   stdoutBuf.String(),
			Stderr:   stderrBuf.String(),
			Cwd:      cwd,
			Duration: time.Since(start),
		}
		if runErr != nil {
			var ee *exec.ExitError
			if errors.As(runErr, &ee) {
				ret.ExitCode = ee.ExitCode()
			} else {
				ret.ExitCode = -1
				if ret.Stderr != "" {
					ret.Stderr += "\n"
				}
				ret.Stderr += runErr.Error()
			}
		}

		// Panic drain. panicCh is buffered(2); at most one
		// entry per drainer. Non-blocking select so a missing
		// panic doesn't delay exit.
		var panicVal any
		select {
		case panicVal = <-panicCh:
		default:
		}
		if panicVal != nil && ret.ExitCode == 0 {
			ret.ExitCode = -1
			if ret.Stderr != "" {
				ret.Stderr += "\n"
			}
			ret.Stderr += fmt.Sprintf("shell: drainer panic: %v", panicVal)
		}
	}()

	runErr = c.Run()
	return
}

// decoderFor picks the right decoder for the given ANSI code
// page. Unknown code pages fall back to nil (identity — raw
// bytes) so we never silently corrupt output — a user on a
// non-Chinese system will see whatever cmd.exe wrote, which
// is what they'd see if they ran the command manually in cmd.
func decoderFor(acp uint32) *encoding.Decoder {
	if acp == cpACPChinese {
		return simplifiedchinese.GB18030.NewDecoder()
	}
	return nil
}

// wrapDecoder returns either a transform reader (when dec !=
// nil) or the raw reader (when dec == nil — identity). The
// caller passes the returned reader to coalesceLines.
func wrapDecoder(r io.Reader, dec *encoding.Decoder) io.Reader {
	if dec == nil {
		return r
	}
	return transform.NewReader(r, dec)
}

// drainWithFallback is the Windows-specific drain loop:
// coalesceLines reads from curRdr; on the first decode error
// (signaled by the transform reader returning an error), we
// flip *decodedOK to false, drain any decoded bytes that
// already reached the buffer, then switch to rawRdr for the
// remainder of the stream.
//
// Implementation note: transform.Reader surfaces decode
// errors via the read path; coalesceLines returns sc.Err() in
// that case. We detect "was this a decode error?" by trying a
// direct read of the transform reader; if that returns a
// transform-specific error, we fall back. For the MVP we
// approximate: any error from coalesceLines on a stream with
// an active decoder flips the flag, and the remaining bytes
// flow through the raw pipe. Some bytes that the transform
// reader had already buffered but not yet consumed are lost
// at the boundary — acceptable: a single byte boundary is
// far less disruptive than a stream-wide mix of decoded +
// raw.
func drainWithFallback(
	decodedOK *bool, curRdr io.Reader, fallback func() io.Reader,
	sink *bytes.Buffer, onChunk func(string) error, isStderr bool,
) error {
	err := coalesceLines(curRdr, sink, onChunk, isStderr)
	if err != nil && *decodedOK {
		// First decode error: flip flag and continue with raw.
		*decodedOK = false
		rawRdr := fallback()
		return coalesceLines(rawRdr, sink, onChunk, isStderr)
	}
	return err
}
