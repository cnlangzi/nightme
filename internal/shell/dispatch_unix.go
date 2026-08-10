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
	"os"
	"os/exec"
	"time"
)

func executeShell(ctx context.Context, cwd, cmd string) (*Result, error) {
	start := time.Now()
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	c.Dir = cwd
	c.Env = os.Environ()

	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr

	runErr := c.Run()
	dur := time.Since(start)

	r := &Result{
		Consumed: true,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Cmd:      cmd,
		Cwd:      cwd,
		Duration: dur,
	}

	if runErr != nil {
		// exec.ExitError carries the child's exit code;
		// anything else (e.g. ctx cancellation) reports -1.
		if ee, ok := runErr.(*exec.ExitError); ok {
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
	return r, nil
}
