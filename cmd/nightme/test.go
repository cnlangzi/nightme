// Package main — `nightme test` subcommand.
//
// `nightme test` is the M1 Local Bridge smoke test: it spawns one
// AI Coding CLI in a PTY, pumps bytes between the user's terminal
// and the child, and shuts down cleanly on SIGINT (default detach
// policy — the CLI process survives).
//
// Usage:
//
//	nightme test --workspace /home/devin/code/bailing --agent claude
//	nightme test --workspace /tmp/foo --agent /bin/echo --args hello
//
// Design notes (docs/feat/F-19-cli-bridge.md, F-10 §1):
//   - The agent registry is populated from cfg.Agent.Agents. Agents
//     whose command resolves on PATH are registered as PTY. Agents
//     not in the registry are auto-registered when their command
//     resolves to an existing file (handy for `--agent /bin/echo`).
//   - Stdin → session.SendText; session.Events → stdout. There is no
//     formatting or aggregation in v0.1 — bytes flow through verbatim.
//   - SIGINT triggers session.Kill(); the CLI stays alive per the
//     default detach policy (SPEC §3). A second signal force-exits.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agent/acpagent"
	"github.com/cnlangzi/nightme/internal/agent/ptyagent"
	"github.com/cnlangzi/nightme/internal/bridge/claudecode"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/session"
)

// testCmdFlags captures every flag the test subcommand accepts.
type testCmdFlags struct {
	workspace string
	agentName string
	args      []string
}

func newTestCmd() *cobra.Command {
	var f testCmdFlags

	cmd := &cobra.Command{
		Use:   "test",
		Short: "Spawn an AI Coding CLI in a PTY (Local Bridge smoke test)",
		Long: "test spawns one AI Coding CLI in a PTY and pumps bytes\n" +
			"between your terminal and the child. It is the M1 Local\n" +
			"Bridge smoke test — there is no IM channel, no slash\n" +
			"command router. Send SIGINT to detach (default) or SIGTERM\n" +
			"to kill the CLI.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTest(cmd, f)
		},
	}

	cmd.Flags().StringVar(&f.workspace, "workspace", "", "workspace directory the agent operates in (required)")
	cmd.Flags().StringVar(&f.agentName, "agent", "", "agent name from config, or path to a binary (required)")
	cmd.Flags().StringSliceVar(&f.args, "args", nil, "extra arguments to append after the agent's defaults")

	_ = cmd.MarkFlagRequired("workspace")
	_ = cmd.MarkFlagRequired("agent")

	return cmd
}

// runTest is the body of the `nightme test` command.
func runTest(cmd *cobra.Command, f testCmdFlags) error {
	if err := validateTestRequest(f); err != nil {
		return err
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	reg, err := openRegistry(cfg)
	if err != nil {
		return fmt.Errorf("test: open registry: %w", err)
	}

	agentReg := buildAgentRegistry(cfg, f.agentName)

	mgr := session.NewMemoryManager(agentReg, reg, nil)
	if err := mgr.Restore(context.Background()); err != nil {
		return fmt.Errorf("test: restore: %w", err)
	}

	sess, err := mgr.Create(context.Background(), session.CreateRequest{
		ChatID:    "cli:test",
		Workspace: f.workspace,
		Agent:     f.agentName,
		Args:      f.args,
	})
	if err != nil {
		return fmt.Errorf("test: create: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(),
		"[nightme] session %s started in %s (agent=%s, args=%v)\n",
		sess.ID, f.workspace, f.agentName, f.args)

	return pumpIO(cmd, mgr, sess)
}

// validateTestRequest catches workspace existence up front so the
// user gets a clear error rather than a PTY spawn failure.
func validateTestRequest(f testCmdFlags) error {
	if f.workspace == "" {
		return errors.New("test: --workspace is required")
	}
	if f.agentName == "" {
		return errors.New("test: --agent is required")
	}
	info, err := os.Stat(f.workspace)
	if err != nil {
		return fmt.Errorf("test: workspace %s: %w", f.workspace, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("test: workspace %s is not a directory", f.workspace)
	}
	return nil
}

// defaultAgentEntries is the fallback registry used when the user's
// config has no agents (empty map or nil). The three shipped entries
// match the dispatch in configuredAgent() so /run claude/codex/opencode
// work out of the box for a fresh install.
//
// Keeping the set narrow (no extras) means a user who genuinely wants
// an empty registry can still achieve it by deleting the default
// agents from this file — but the more common "I haven't configured
// anything yet" path now boots successfully.
func defaultAgentEntries() map[string]config.AgentEntry {
	return map[string]config.AgentEntry{
		"claude": {
			Command: "claude",
		},
		"codex": {
			Command: "codex-acp",
		},
		"opencode": {
			Command: "opencode",
			Args:    []string{"acp"},
		},
	}
}

// buildAgentRegistry seeds an agent.Registry from cfg.Agent.Agents. The
// built-in agents use their v0.2 protocol adapters: Claude uses the SDK
// adapter, while Codex and OpenCode use ACP. Unknown configured names remain
// PTY agents for backwards compatibility. If the requested agent is not in
// the registry, an existing bare path is still registered as PTY.
//
// When cfg.Agent.Agents is empty (the common cold-start case after
// `nightme auth login feishu`), buildAgentRegistry falls back to
// defaultAgentEntries() so /run <name> works without manual config
// editing.
func buildAgentRegistry(cfg *config.Config, requested string) *agent.Registry {
	reg := agent.New()
	if cfg == nil {
		return reg
	}
	entries := cfg.Agent.Agents
	if len(entries) == 0 {
		entries = defaultAgentEntries()
	}
	for name, entry := range entries {
		if entry.Command == "" {
			continue
		}
		configured := configuredAgent(name, entry)
		if pty, ok := configured.(*ptyagent.Agent); ok {
			pty.Cols = cfg.Session.DefaultPtyCols
			pty.Rows = cfg.Session.DefaultPtyRows
		}
		reg.Register(configured)
	}
	if _, err := reg.Get(requested); err != nil {
		// Auto-register a bare-path agent when the user passed
		// `--agent /some/binary`. Only do this if the file exists
		// so a typo surfaces as "agent not found" instead of a
		// confusing exec error.
		if requested != "" {
			if _, statErr := os.Stat(requested); statErr == nil {
				reg.Register(ptyagent.New(requested, filepath.Base(requested), nil, nil))
			}
		}
	}
	return reg
}

func configuredAgent(name string, entry config.AgentEntry) agent.Agent {
	args := append([]string(nil), entry.Args...)
	switch name {
	case "claude":
		// v0.2: Claude Code uses the dedicated JSON-IO bridge instead
		// of the SDK sentinel. See docs/feat/F-24-claudecode-bridge.md.
		return claudecode.New(name, entry.Command, args)
	case "codex", "opencode":
		if name == "opencode" && len(args) == 0 {
			args = []string{"acp"}
		}
		return acpagent.New(name, entry.Command, args)
	default:
		return ptyagent.New(name, entry.Command, args, configuredAgentEnv(entry.Env))
	}
}

func configuredAgentEnv(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+env[key])
	}
	return out
}

// buildRunAgentRegistry registers every configured agent with its selected
// v0.2 mode. User-defined names still use PTY as the safe fallback.
func buildRunAgentRegistry(cfg *config.Config) *agent.Registry {
	return buildAgentRegistry(cfg, "")
}

// pumpIO bridges stdin ↔ session and stdout. It returns when the
// session terminates naturally or a signal forces shutdown.
func pumpIO(cmd *cobra.Command, mgr session.Manager, sess *session.Session) error {
	out := cmd.OutOrStdout()

	stop := make(chan struct{})
	go func() {
		defer close(stop)
		for ev := range sess.Events() {
			switch ev.Kind {
			case agent.EventText:
				_, _ = io.WriteString(out, ev.Text)
			case agent.EventDone:
				fmt.Fprintln(out, "\n[nightme] session ended")
				return
			case agent.EventError:
				fmt.Fprintf(out, "\n[nightme] session error: %v\n", ev.Error)
				return
			}
		}
	}()

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	stdinDone := make(chan struct{})
	go func() {
		defer close(stdinDone)
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text() + "\n"
			if err := sess.SendText(line); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "send: %v\n", err)
				return
			}
		}
	}()

	select {
	case sig := <-sigCh:
		fmt.Fprintf(cmd.ErrOrStderr(), "[nightme] received %s\n", sig)
		if err := mgr.Kill(sess.ID); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "kill: %v\n", err)
		}
	case <-stop:
		// Session ended naturally; the events goroutine has
		// drained and the channel closed.
	}

	// Wait for any in-flight I/O to flush before persisting. A
	// second signal force-exits.
	select {
	case <-stop:
	case <-stdinDone:
	case <-sigCh:
		os.Exit(130)
	}
	_ = mgr.Persist()
	return nil
}
