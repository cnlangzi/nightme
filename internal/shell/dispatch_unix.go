//go:build !windows

// Unix shell execution — sh -c. POSIX guarantees /bin/sh
// exists on every Unix-like system, so we don't fall back to
// bash / dash / etc. explicitly.
//
// Env is inherited from os.Environ() (clean parent env, NOT
// whatever the agent process has mutated) so the user gets a
// shell they recognise. cwd comes from cs.SelectedCwd — no
// tilde / env-var resolution.
package shell

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"time"

	"github.com/cnlangzi/nightme/internal/proc"
)

func executeShell(ctx context.Context, cwd, cmdline string, extraEnv []string) *result {
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

	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr

	runErr := c.Run()
	dur := time.Since(start)

	r := &result{
		Consumed: true,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Cmd:      cmdline,
		Cwd:      cwd,
		Duration: dur,
	}

	if runErr != nil {
		// exec.ExitError carries the child's exit code;
		// anything else (e.g. ctx cancellation) reports -1.
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			r.ExitCode = ee.ExitCode()
		} else {
			r.ExitCode = -1
			// Surface the error in stderr so the user sees it.
			if r.Stderr != "" {
				r.Stderr += "\n"
			}
			r.Stderr += runErr.Error()
		}
	}

	r.Reply = renderSummary(r)
	return r
}
