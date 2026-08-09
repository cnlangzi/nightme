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
//   - The agent registry is populated from cfg.Agents. Agents
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
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/pty"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/config"
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
	// v1.2 chatsession does not need the legacy registry.File —
	// the spawner is enough to bring up a child and forward
	// bytes. We retain the loadConfig call so the future
	// persistence wiring (csFile / asFile) can be added here
	// without changing the test's public surface.

	agentReg := buildAgentRegistry(cfg, f.agentName)

	spawner := chatsession.NewRegistrySpawner(agentReg)
	mgr := chatsession.NewManager().WithSpawner(spawner)

	cs, err := mgr.GetOrCreate("test:"+f.agentName, f.agentName)
	if err != nil {
		return fmt.Errorf("test: get or create chat session: %w", err)
	}
	if err := cs.SetSelectedCwd(f.workspace); err != nil {
		return fmt.Errorf("test: set cwd: %w", err)
	}
	if err := cs.SetSelectedAgent(f.agentName); err != nil {
		return fmt.Errorf("test: set agent: %w", err)
	}
	as, err := cs.LookupSelectedAgentSession()
	if err != nil {
		return fmt.Errorf("test: spawn: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(),
		"[nightme] session %s started in %s (agent=%s, args=%v)\n",
		as.ID, f.workspace, f.agentName, f.args)

	return pumpIO(cmd, as)
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

// buildAgentRegistry seeds an agent.Registry for nightme run / nightme
// test / nightme agents. The dispatch is:
//
//  1. Start with agent.Builtins (claude is the only v0.2.x built-in
//     — each agent package registers itself via init()).
//  2. Layer cfg.Agents on top. A name matching a built-in
//     replaces the built-in (custom binary path); an unknown name
//     becomes a PTY agent (the safe default for user-supplied CLIs).
//  3. If --agent /some/binary was passed, auto-register that bare
//     path so a typo surfaces as "agent not found" instead of a
//     confusing exec error.
func buildAgentRegistry(cfg *config.Config, requested string) *agent.Registry {
	reg := agent.New()
	for _, a := range agent.Builtins.List() {
		reg.Register(a)
	}
	if cfg != nil && len(cfg.Agents) > 0 {
		for _, entry := range cfg.Agents {
			if entry.Name == "" || entry.Command == "" {
				continue
			}
			// v1.2 schema: Command is the full command line (binary + args),
			// e.g. "claude --dangerously-skip-permissions". Split with
			// strings.Fields; first token is the binary.
			fields := strings.Fields(entry.Command)
			if len(fields) == 0 {
				continue
			}
			a := pty.NewAgent(entry.Name, fields[0], fields[1:], nil)
			a.Cols = cfg.Session.DefaultPtyCols
			a.Rows = cfg.Session.DefaultPtyRows
			reg.LegacyRegister(a)
		}
	}
	if _, err := reg.Get(requested); err != nil {
		// Auto-register a bare-path agent when the user passed
		// `--agent /some/binary`. Only do this if the file exists
		// so a typo surfaces as "agent not found" instead of a
		// confusing exec error.
		if requested != "" {
			if _, statErr := os.Stat(requested); statErr == nil {
				reg.LegacyRegister(pty.NewAgent(requested, filepath.Base(requested), nil, nil))
			}
		}
	}
	return reg
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

// pumpIO bridges stdin ↔ agent and stdout. It returns when the
// agent session terminates naturally or a signal forces shutdown.
func pumpIO(cmd *cobra.Command, as *chatsession.AgentSession) error {
	out := cmd.OutOrStdout()

	stop := make(chan struct{})
	go func() {
		defer close(stop)
		events := as.Events()
		if events == nil {
			return
		}
		for ev := range events {
			// ev is now chatsession.EnrichedEvent (CS-AS 边界重构 Phase 1).
			// Bridge events arrive as KindAgentEvent wrapping the original
			// agent.AgentEvent.
			if ev.Kind != chatsession.KindAgentEvent || ev.AgentEvent == nil {
				continue
			}
			switch ev.AgentEvent.Kind {
			case agent.EventAgentText:
				_, _ = io.WriteString(out, ev.AgentEvent.Text)
			case agent.EventAgentDone:
				fmt.Fprintln(out, "\n[nightme] session ended")
				return
			case agent.EventAgentError:
				fmt.Fprintf(out, "\n[nightme] session error: %v\n", ev.AgentEvent.Err)
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
			if err := as.SendText(line); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "send: %v\n", err)
				return
			}
		}
	}()

	select {
	case sig := <-sigCh:
		fmt.Fprintf(cmd.ErrOrStderr(), "[nightme] received %s\n", sig)
		_ = as.Close()
	case <-stop:
		// Session ended naturally; the events goroutine has
		// drained and the channel closed.
	}

	// Wait for any in-flight I/O to flush before exiting. A
	// second signal force-exits.
	select {
	case <-stop:
	case <-stdinDone:
	case <-sigCh:
		os.Exit(130)
	}
	return nil
}
