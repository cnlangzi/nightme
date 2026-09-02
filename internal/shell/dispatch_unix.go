//go:build !windows

// Unix shell execution — sh -c.
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
)

// executeShell runs cmdline in cwd via "sh -c", draining stdout
// and stderr through coalesceLines. onChunk is invoked per chunk;
// pass nil to collect full output without streaming (Run / gtw hooks).
//
// Failure modes collapse into ExitCode=-1 + Stderr so the dispatcher
// footer renders ❌:
//   - non-zero exit (exec.ExitError carries the code)
//   - ctx cancelled (CommandContext killed child)
//   - drainer panic (recovered via panicCh)
func executeShell(ctx context.Context, cwd, cmdline string, extraEnv []string, onChunk func(string) error) (ret *result) {
	start := time.Now()
	c := proc.New(ctx, "sh", "-c", cmdline)
	c.Dir = cwd
	c.Env = append(os.Environ(), extraEnv...)

	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	c.Stdout, c.Stderr = outW, errW

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
		_ = coalesceLines(outR, &stdoutBuf, onChunk, false)
	}()
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				panicCh <- r
			}
		}()
		_ = coalesceLines(errR, &stderrBuf, onChunk, true)
	}()

	// Defer closes pipes + joins drainers BEFORE we read stdoutBuf
	// / stderrBuf (reading concurrently with WriteString is a data
	// race). Runs on normal return AND on c.Run / coalesceLines panic.
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
