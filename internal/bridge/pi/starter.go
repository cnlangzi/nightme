// starter.go — the spawn recipe for the pi bridge.
//
// After the Agent → Info/Starter/Agent/driver refactor
// (wip/agent.md), the static metadata (name/command/args) lives
// on Starter and is exposed via Info(). The runtime state (cmd,
// pipes, RPC client, goroutines) lives on driver and is exposed
// via the unexported driver interface. External callers never
// see *Starter or *driver directly — they interact via
// *agent.Agent, which Starter.Start returns.
package pi

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/cnlangzi/nightme/internal/agent"
)

// Starter is the pi spawn recipe. Held in agent.Builtins as a
// singleton per agent name.
type Starter struct {
	name    string
	command string
	args    []string
}

// NewStarter constructs the pi spawn recipe. Entry point used at
// registration time (cmd/nightme/agents.go calls it from init()).
func NewStarter(name, command string, args []string) *Starter {
	return &Starter{
		name:    name,
		command: command,
		args:    append([]string(nil), args...),
	}
}

// Info returns the fixed metadata for this starter. Observable
// at any time; used by `nightme agents` and any other spec-only
// consumer.
func (s *Starter) Info() agent.Info {
	return agent.NewInfo(s.name, agent.ModeJSONIO, s.command, s.args, nil)
}

// Detect verifies the `pi` binary resolves on PATH. Called by
// Spawner before Start; an error aborts session creation with a
// clear "pi not installed" message.
func (s *Starter) Detect() error {
	_, err := exec.LookPath(s.command)
	return err
}

// Start spawns Pi in RPC mode and returns a live *agent.Agent
// that streams events on its Events channel. The Starter is
// unchanged (reusable across many sessions).
//
// cfg.Workspace is the child process's cwd. cfg.Args are appended
// after the agent's defaults. cfg.Env is appended to os.Environ()
// for the child. cfg.SessionID is not used by pi (no resume
// semantics).
func (s *Starter) Start(ctx context.Context, cfg agent.StartConfig) (*agent.Agent, error) {
	if cfg.Workspace == "" {
		return nil, fmt.Errorf("pi: workspace is required")
	}
	d, err := newDriver(ctx, s, cfg)
	if err != nil {
		return nil, err
	}
	return agent.NewAgent(s.Info(), d.pid, d.events, d), nil
}

// RunOnce is the one-shot counterpart to Start. Drives the
// configured agent through a single prompt and returns the
// final text.
//
// As of F-PI-PRINT-001 (2026-08-13), RunOnce routes through
// the print-mode spawn (--mode json -p) rather than the long-
// lived RPC session that Start uses. Rationale:
//
//   - One-shot invocations (/gtw commit, buildAgentPrompt)
//     don't need a multi-turn session; they spawn, do the
//     work, and exit.
//   - The RPC path was observed to flake in production — pi's
//     stdout pipe closed 2-5 s after the prompt RPC ack with
//     no events ever delivered, even though the same RunOnce
//     flow worked in `go test` smoke tests. Root cause is still
//     under investigation but the print-mode path sidesteps
//     it: there is no long-lived pipe, no response-correlation
//     map, and process exit is the natural turn-end signal.
//
// The print-mode spawn lives in print_unix.go / print_windows.go
// and reuses the shared JSON-event translator (translate.go)
// because print-mode emits the same event format as RPC.
//
// Start (above) is unchanged: it still opens an RPC session
// for the chat session's long-lived use case where multiple
// turns ride one bridge. RunOnce and Start share the same
// Starter; only the spawn path differs.
func (s *Starter) RunOnce(ctx context.Context, cfg agent.StartConfig, blocks []agent.ContentBlock) (agent.RunResult, error) {
	prompt := blocksToPrompt(blocks)
	result, err := runPrintMode(ctx, s.command, prompt, cfg.Workspace, cfg.Env)
	if err != nil {
		return agent.RunResult{}, fmt.Errorf("agent %s: %w", s.Info().Name, err)
	}
	return result, nil
}

// blocksToPrompt joins multiple ContentText blocks with "\n"
// and degrades ContentImage / ContentFile blocks to compact
// "[file: ...]" / "[image: ...]" suffixes on the message. The
// output is the single string passed to `pi -p`. Images are
// not yet threaded through the print-mode argv (pi's `-p`
// flag accepts only one positional prompt); when /gtw commit
// starts carrying image attachments this is where the
// base64-encoding or placeholder strategy will land.
func blocksToPrompt(blocks []agent.ContentBlock) string {
	var sb strings.Builder
	for _, b := range blocks {
		switch b.Type {
		case agent.ContentText:
			if b.Text == "" {
				continue
			}
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(b.Text)
		case agent.ContentImage:
			if b.Path == "" {
				continue
			}
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			fmt.Fprintf(&sb, "[image: %s (%s)]", b.Path, b.MediaType)
		case agent.ContentFile:
			if b.Path == "" {
				continue
			}
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			fmt.Fprintf(&sb, "[file: %s]", b.Path)
		}
	}
	return sb.String()
}
