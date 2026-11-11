package workflow

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/proc"
	"github.com/cnlangzi/nightme/internal/wfe"
)

// runRuntime implements wfe.Runtime for one workflow run. Each
// method is a channel from the workflow engine to a specific
// resource:
//
//   - RunShell:   direct os/exec (shell channel)
//   - SendPrompt: the loopback client — the prompt goes into the
//     daemon as a message and the answer comes back as the settled
//     execution
//   - RunAction:  the service's ActionRegistry (action channels by
//     name)
//   - Now:        time.Now
type runRuntime struct {
	svc *Service
	run *run
}

func (r *runRuntime) Now() time.Time { return time.Now() }

// RunShell runs a step's shell command in the run's workspace.
//
// The command is bound to ctx: a cancelled run (or a daemon that is
// shutting down) stops the child instead of leaving it behind. The
// step's env is layered on top of the daemon's own environment, so
// the shell finds PATH, HOME and the git/ssh configuration a real
// command needs.
func (r *runRuntime) RunShell(ctx context.Context, spec wfe.ShellSpec) (*wfe.ShellResult, error) {
	cwd := spec.Cwd
	if cwd == "" {
		cwd = r.run.workspace
	}
	if cwd == "" {
		return nil, errors.New("me: no CWD for run")
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

// SendPrompt sends the prompt into the chat and waits for the
// daemon to settle it.
//
// The reply is the final text of the execution — the answer the
// agent produced, not the first thing the daemon said. A command
// that never settles ends the wait with a timeout snapshot, which
// fails the step.
func (r *runRuntime) SendPrompt(ctx context.Context, spec wfe.PromptSpec) (*wfe.Reply, error) {
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = defaultPromptTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ex, err := r.svc.client.Do(ctx, spec.ChatID, spec.Prompt)
	if err != nil {
		return nil, err
	}
	return &wfe.Reply{Text: ex.Final()}, nil
}

func (r *runRuntime) RunAction(ctx context.Context, spec wfe.ActionSpec) (*wfe.ActionResult, error) {
	return r.svc.actions.Run(ctx, spec)
}

// defaultPromptTimeout bounds a prompt step when the workflow does
// not say otherwise.
const defaultPromptTimeout = 30 * time.Minute

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
// references like ${{ steps.x.outputs.y }}.
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
