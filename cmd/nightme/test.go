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
//   - Stdin → session.SendBlocks(text); session.Events → stdout.
//     There is no formatting or aggregation in v0.1 — bytes flow
//     through verbatim.
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
	"sort"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentregistry"
	"github.com/cnlangzi/nightme/internal/chatsession"
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

	agentReg := agentregistry.Build(cfg, f.agentName)

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

// configuredAgentEnv flattens a map into a deterministic sorted
// slice of KEY=VALUE strings for use as an agent env. The shape
// matches what `agent -pty` expects; the runtime does not
// currently inject any per-call env, but this helper is the
// seam where future per-agent overrides land.
//
// Note: buildAgentRegistry / buildRunAgentRegistry used to live
// here too; they moved to internal/agentregistry so the runtime
// (nightme run) and the CLI subcommands (nightme test / agents)
// share one implementation. See internal/agentregistry.
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
	signal.Notify(sigCh, shutdownSignals()...)
	defer signal.Stop(sigCh)

	stdinDone := make(chan struct{})
	go func() {
		defer close(stdinDone)
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if err := as.SendBlocks(as.OpContext(), []agent.ContentBlock{
				{Type: agent.ContentText, Text: line},
			}); err != nil {
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
