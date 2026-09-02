//go:build windows

// Windows shell executor — runs the user command via `cmd /c`.
// `cmd.exe` is on every Windows install since NT 4.0 so no shell
// detection chain is needed.
//
// Encoding gotcha: cmd.exe writes stdout/stderr in CP_ACP. On a
// zh-CN Windows (CP936 = GBK) we decode via GB18030; otherwise
// passthrough. On decode error the rest of the stream passes
// through raw (once-decoded-forever-raw).
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

const cpACPChinese = 936

// executeShell runs cmdline in cwd via "cmd /c". Failure modes
// (non-zero exit / ctx cancel / drainer panic) collapse into
// ExitCode=-1 + Stderr so the dispatcher footer renders ❌.
// See dispatch_unix.go for the full table.
func executeShell(ctx context.Context, cwd, cmdline string, extraEnv []string, onChunk func(string) error) (ret *result) {
	start := time.Now()
	c := proc.New(ctx, "cmd", "/c", cmdline)
	c.Dir = cwd
	c.Env = append(os.Environ(), extraEnv...)

	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	c.Stdout, c.Stderr = outW, errW

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
		_ = drainWithFallback(&outDecodedOK, outRdr, func() io.Reader { return outR },
			&stdoutBuf, onChunk, false)
	}()
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				panicCh <- r
			}
		}()
		_ = drainWithFallback(&errDecodedOK, errRdr, func() io.Reader { return errR },
			&stderrBuf, onChunk, true)
	}()

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

// decoderFor returns a GB18030 decoder for CP936 (Simplified
// Chinese) or nil (identity — raw bytes) for everything else.
func decoderFor(acp uint32) *encoding.Decoder {
	if acp == cpACPChinese {
		return simplifiedchinese.GB18030.NewDecoder()
	}
	return nil
}

func wrapDecoder(r io.Reader, dec *encoding.Decoder) io.Reader {
	if dec == nil {
		return r
	}
	return transform.NewReader(r, dec)
}

// drainWithFallback: coalesceLines reads from curRdr; on first
// decode error, flip *decodedOK to false and continue with raw.
// Some bytes buffered in the transform reader are lost at the
// boundary — acceptable trade-off for avoiding a stream-wide mix
// of decoded text and mojibake.
func drainWithFallback(
	decodedOK *bool, curRdr io.Reader, fallback func() io.Reader,
	sink *bytes.Buffer, onChunk func(string) error, isStderr bool,
) error {
	err := coalesceLines(curRdr, sink, onChunk, isStderr)
	if err != nil && *decodedOK {
		*decodedOK = false
		return coalesceLines(fallback(), sink, onChunk, isStderr)
	}
	return err
}
