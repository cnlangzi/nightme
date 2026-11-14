package me

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"

	"github.com/cnlangzi/nightme/internal/proc"
	"github.com/cnlangzi/nightme/internal/wfe"
)

// ShellRunner runs the shell command of a workflow step.
type ShellRunner interface {
	// RunShell runs the command in the workspace and reports what it
	// produced.
	RunShell(ctx context.Context, spec wfe.ShellSpec) (*wfe.ShellResult, error)
}

// ExecShell runs a step's shell command with os/exec.
//
// The command is bound to ctx: a cancelled run (or a daemon that is
// shutting down) stops the child instead of leaving it behind. The
// step's env is layered on top of the daemon's own environment, so
// the shell finds PATH, HOME and the git/ssh configuration a real
// command needs.
type ExecShell struct{}

// RunShell implements ShellRunner.
func (ExecShell) RunShell(ctx context.Context, spec wfe.ShellSpec) (*wfe.ShellResult, error) {
	cwd := spec.Cwd
	if cwd == "" {
		cwd = spec.Env["WORKSPACE_CWD"]
	}
	if cwd == "" {
		return nil, errors.New("me: no workspace for the shell step")
	}

	shellBin, shellArg := pickShell(spec.Shell)
	cmd := proc.New(ctx, shellBin, shellArg, spec.Command)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), envToUnix(spec.Env)...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	res := &wfe.ShellResult{
		Stdout:  stdout.String(),
		Stderr:  stderr.String(),
		Outputs: parseKVOutput(stdout.String()),
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
			return res, err
		}
		// The command never ran — a bad working directory, a
		// missing shell. That is a failed step, not a successful
		// one with empty output.
		res.ExitCode = -1
		return res, err
	}
	res.ExitCode = 0
	return res, nil
}

// pickShell returns the binary + arg to invoke. If shell is empty
// it defaults to "sh -c"; "bash"/"zsh"/"pwsh" are recognized.
func pickShell(s string) (string, string) {
	switch s {
	case "":
		return "sh", "-c"
	case "sh":
		return "sh", "-c"
	case "bash":
		return "bash", "-c"
	case "zsh":
		return "zsh", "-c"
	case "pwsh", "powershell":
		return "pwsh", "-Command"
	}
	// Unknown → use as-is with -c flag.
	return s, "-c"
}

// envToUnix converts a map[string]string to "KEY=VAL" entries for
// exec.Cmd.Env. The os/exec package requires this format.
func envToUnix(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

// parseKVOutput extracts "key=value" lines from stdout. Both key
// and value must be non-empty. Lines that don't match are ignored.
// Used to populate ShellResult.Outputs for downstream step
// references like ${{ steps.x.outputs.data.y }}.
func parseKVOutput(stdout string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		idx := strings.IndexByte(line, '=')
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		if key == "" || val == "" {
			continue
		}
		out[key] = val
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
