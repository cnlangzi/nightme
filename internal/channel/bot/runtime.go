package bot

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/wfe"
)

// botRuntime implements wfe.Runtime. Each method is a "channel"
// from wfe to a specific resource:
//
//   - RunShell:   direct os/exec (shell channel)
//   - SendPrompt: push msg into bot.Incoming(), block on r.reply
//     (the nightme channel — agent reply flows back via bot.Send
//     which delivers to r.reply)
//   - RunAction:  bot's ActionRegistry (action channels by name)
//   - Now:        time.Now
type botRuntime struct {
	bot *Bot
	run *botRun
}

func (r *botRuntime) Now() time.Time { return time.Now() }

func (r *botRuntime) RunShell(_ context.Context, spec wfe.ShellSpec) (*wfe.ShellResult, error) {
	cwd := spec.Cwd
	if cwd == "" {
		cwd = r.run.workspace
	}
	if cwd == "" {
		return nil, errors.New("bot: no CWD for run")
	}

	shellBin, shellArg := pickShell(spec.Shell)
	cmd := exec.Command(shellBin, shellArg, spec.Command)
	cmd.Dir = cwd
	cmd.Env = envToUnix(spec.Env)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	res := &wfe.ShellResult{
		Stdout:  stdout.String(),
		Stderr:  stderr.String(),
		Outputs: parseKVOutput(stdout.String()),
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		res.ExitCode = exitErr.ExitCode()
		return res, err
	}
	res.ExitCode = 0
	return res, nil
}

func (r *botRuntime) SendPrompt(ctx context.Context, spec wfe.PromptSpec) (*wfe.Reply, error) {
	// 1. push the prompt into bot.Incoming() — gateway dispatches
	msg := messages.InboundMessage{
		ChatID: spec.ChatID,
		Text:   spec.Prompt,
		Time:   time.Now(),
	}
	select {
	case r.bot.in <- msg:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// 2. block on the per-run reply channel (delivered by bot.Send)
	timeout := spec.Timeout
	if timeout == 0 {
		timeout = 30 * time.Minute
	}
	select {
	case text := <-r.run.reply:
		return &wfe.Reply{Text: text}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(timeout):
		return nil, wfe.ErrStepFailed
	}
}

func (r *botRuntime) RunAction(ctx context.Context, spec wfe.ActionSpec) (*wfe.ActionResult, error) {
	return r.bot.actions.Run(ctx, spec)
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
