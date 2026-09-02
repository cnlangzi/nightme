//go:build !windows

// Unix shell execution — sh -c. POSIX guarantees /bin/sh
// exists on every Unix-like system, so we don't fall back to
// bash / dash / etc. explicitly.
//
// Env is inherited from os.Environ() (clean parent env, NOT
// whatever the agent process has mutated) so the user gets a
// shell they recognise. cwd comes from cs.SelectedCwd — no
// tilde / env-var resolution.
//
// Streaming (F-shell-stream): stdout / stderr are drained via
// io.Pipe + coalesceLines so each chunk can be PATCHed onto the
// Feishu / Slack placeholder card in real time. The drainers run
// in goroutines; their panics are recovered onto panicCh and
// surfaced as ExitCode=-1 + a Stderr message so the dispatcher
// renders ❌ on the footer instead of crashing the daemon.
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
// and stderr through coalesceLines. onChunk is invoked with each
// coalesced chunk as it accumulates; pass nil to collect the
// full output without streaming (used by Run / gtw hooks).
//
// Three failure modes collapse into ExitCode=-1 + a Stderr
// message so the dispatcher footer renders ❌ and the user sees
// why:
//   1. child exited non-zero (exec.ExitError carries the code)
//   2. ctx cancelled (CommandContext killed child — drainers
//      see EOF, coalesceLines returns nil; runErr surfaces the
//      cancellation reason in runErr.Error())
//   3. one of the drainers panicked (recovered via panicCh)
//
// The cancellable ctx is supplied by runShell; on drainErr
// (onChunk returned an error), runShell cancels its ctx so the
// child is killed via CommandContext rather than left running
// against a wedged pipe.
//
// Cleanup contract: the deferred cleanup closes the pipe
// writers and joins the drainer goroutines on EVERY exit path
// (normal, panic in c.Run, panic in coalesceLines). Without
// this, a c.Run() panic would leave drainers blocked forever
// on never-closed pipes — a goroutine leak that the daemon
// would have to be restarted to clear.
func executeShell(ctx context.Context, cwd, cmdline string, extraEnv []string, onChunk func(string) error) (ret *result) {
	start := time.Now()
	// Route through proc.New so the platform-specific
	// SysProcAttr lives in one place: Setsid on Unix (so the
	// shell child becomes the leader of its own session and
	// process group; same reasoning as the agent bridges —
	// /dev/tty inheritance can't wedge the shell event loop),
	// CREATE_NO_WINDOW on Windows (handled by dispatch_windows.go).
	c := proc.New(ctx, "sh", "-c", cmdline)
	c.Dir = cwd
	// Parent env first, then caller-supplied vars on top. A
	// duplicate key in extraEnv intentionally overrides the
	// parent's value (e.g. PATH injection by the caller wins).
	c.Env = append(os.Environ(), extraEnv...)

	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	c.Stdout, c.Stderr = outW, errW

	var (
		stdoutBuf, stderrBuf bytes.Buffer
		// panicCh buffers up to 2 panics (one per drainer). After
		// wg.Wait() the senders are gone; non-blocking drain reads
		// at most one entry (in practice one or both drainers
		// panicked; we report the first).
		panicCh = make(chan any, 2)
		wg      sync.WaitGroup
		runErr  error
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

	// Cleanup defer. Runs on normal return AND on any panic
	// (c.Run panic, coalesceLines panic propagated through the
	// recover above, etc). Closes pipe writers to unblock the
	// drainers; wg.Wait joins them BEFORE we read stdoutBuf /
	// stderrBuf — reading those buffers concurrently with
	// WriteString would be a data race. Then assembles the
	// result, handles runErr + late panic, and assigns ret.
	// The named return value lets the defer assemble the full
	// result without needing an explicit return value.
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
