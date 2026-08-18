// Package runtime — `nightme run` daemon core.
//
// The runtime package owns every piece of the long-running Feishu
// daemon that is NOT cobra plumbing:
//
//   - chatsession.Manager (per-chat ChatSession table)
//   - chatsession.NewRegistrySpawner (lazy fork via agent.Registry)
//   - chatsession.InputBuffer FSM (commit 9; ownership moved to ChatSession)
//   - gateway.RegisterChatSessionCommands (/cwd /use /kill slash commands)
//   - EventCallback: each AgentSession.Events() is consumed by a
//     per-active-AS readPump goroutine that translates AgentEvent →
//     OutboundMessage → channel.Send, AND drives the InputBuffer FSM
//     (non-terminal events → SetBusy; EventAgentDone / Error → SetIdle +
//     OnTurnEnded → flush via the runtime-installed FlushHook).
//
// The cmd/nightme/run.go file is now a thin cobra adapter that
// parses --channel, fills in Deps, and calls Runner.Run.
//
// All construction seams live in Deps; every helper that needs to
// be testable directly (NewEventHandler, WireRuntimeCallbacksAndRestore,
// MarkPromptDone, ShutdownRun, NewMessageDispatcher) is exported.
package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentregistry"
	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/channel/bot"
	"github.com/cnlangzi/nightme/internal/channel/echo"
	"github.com/cnlangzi/nightme/internal/channel/feishu"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/registry"
)

// Deps holds the construction seams for the daemon: every
// dependency is injectable for deterministic tests. Nil fields
// fall back to the production defaults applied by Runner.Run
// (matching the v0.x cmd/nightme/run.go behaviour).
type Deps struct {
	LoadConfig        func() (*config.Config, error)
	OpenChatSessions  func(*config.Config) (*registry.ChatSessionFile, error)
	OpenAgentSessions func(*config.Config) (*registry.AgentSessionFile, error)
	BuildAgents       func(*config.Config) *agent.Registry
	NewChannel        func(*config.Config) (channel.Channel, error)
	NewBot            func(*config.Config) (channel.Channel, error)
	Signals           <-chan os.Signal
	SkipFeishuLogin   bool
	OnReady           func()

	// RegisterHealth, if non-nil, is called after the channel is
	// constructed and started. The closure receives the channel's
	// HealthSnapshot function directly (Phase 2.1: every Channel
	// implements HealthSnapshot itself — no feishu type assertion
	// needed at the runtime layer).
	RegisterHealth func(snapshot func() (string, json.RawMessage, error))
}

// DefaultDeps returns the production Deps: real config loader,
// real registry stores under cfg.Paths.DataDir, real agent
// registry, and feishu.NewAdapter as the channel. CLI/test
// callers override individual hooks (echo for smoke tests,
// temp-dir stores for harness, …).
func DefaultDeps() Deps {
	return Deps{
		LoadConfig:        config.LoadDefault,
		OpenChatSessions:  defaultOpenChatSessions,
		OpenAgentSessions: defaultOpenAgentSessions,
		BuildAgents:       defaultBuildAgents,
		NewChannel: func(cfg *config.Config) (channel.Channel, error) {
			return feishu.NewAdapter(cfg)
		},
	}
}

// WithChannel selects the channel implementation (feishu | echo).
// Unknown channel names return an error so the CLI shell can
// surface a friendly message and exit non-zero. The echo
// selection sets SkipFeishuLogin so the runtime doesn't error
// out on missing Feishu credentials.
func WithChannel(deps Deps, channelName string) (Deps, error) {
	switch channelName {
	case "feishu", "":
		// default — feishu.NewAdapter (already wired by DefaultDeps)
	case "echo":
		deps.SkipFeishuLogin = true
		deps.NewChannel = func(*config.Config) (channel.Channel, error) {
			return echo.New("echo", os.Stdout), nil
		}
	default:
		return deps, fmt.Errorf("runtime: unknown channel %q (want feishu or echo)", channelName)
	}
	return deps, nil
}

// WithBot enables the bot channel. bot is a workflow automation
// driver — it pushes synthesized messages into its own Incoming(),
// where the gateway's pumpInbound reads them, and it captures
// agent replies via Send(). When NewBot is non-nil, runtime
// attaches BOTH the primary channel (feishu/echo) and bot to
// the gateway, so messages from both sources are dispatched
// through the same inbound chain.
func WithBot(deps Deps, workflowsDir string) Deps {
	deps.NewBot = func(_ *config.Config) (channel.Channel, error) {
		return bot.New(bot.Config{
			WorkflowsDir: workflowsDir,
			ActionsDir:   filepath.Join(workflowsDir, "actions"),
		}), nil
	}
	return deps
}

// defaultOpenChatSessions opens chat_sessions.json relative to
// cfg.Paths.DataDir.
func defaultOpenChatSessions(cfg *config.Config) (*registry.ChatSessionFile, error) {
	path, err := ChatSessionsPath(cfg)
	if err != nil {
		return nil, err
	}
	return registry.OpenChatSessionFile(path)
}

// defaultOpenAgentSessions opens agent_sessions.json relative to
// cfg.Paths.DataDir.
func defaultOpenAgentSessions(cfg *config.Config) (*registry.AgentSessionFile, error) {
	path, err := AgentSessionsPath(cfg)
	if err != nil {
		return nil, err
	}
	return registry.OpenAgentSessionFile(path)
}

// defaultBuildAgents is the default Deps.BuildAgents: every
// built-in agent + cfg.Agents, no bare-path auto-register
// (cfg.Primary selects from the registered set). Delegates to
// internal/agentregistry so the runtime doesn't have to import
// bridge/pty directly (cycle: bridge/pty → agent → ... → pty).
func defaultBuildAgents(cfg *config.Config) *agent.Registry {
	return agentregistry.Build(cfg, "")
}

// RemoveLegacyRegistryFile is best-effort cleanup of the v0.1
// registry.json (the v1.2 daemon no longer reads it). Exported
// so the CLI's `list` command can call it directly without
// having its own copy.
func RemoveLegacyRegistryFile(cfg *config.Config) error {
	path, err := legacyRegistryPath(cfg)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	bak := path + ".v1.bak"
	if _, err := os.Stat(bak); err == nil {
		// Backup already exists — leave both files alone.
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(path, bak); err != nil {
		return err
	}
	return nil
}

// ChatSessionsPath returns the absolute path to chat_sessions.json
// under cfg.Paths.DataDir. Exported so the CLI's `list` command
// can resolve the same file the daemon writes.
func ChatSessionsPath(cfg *config.Config) (string, error) {
	base, err := filepath.Abs(cfg.Paths.DataDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "chat_sessions.json"), nil
}

// AgentSessionsPath returns the absolute path to agent_sessions.json
// under cfg.Paths.DataDir. Exported so the CLI's `list` command
// can resolve the same file the daemon writes.
func AgentSessionsPath(cfg *config.Config) (string, error) {
	base, err := filepath.Abs(cfg.Paths.DataDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "agent_sessions.json"), nil
}

// legacyRegistryPath returns the absolute path to the v0.1
// registry.json that the v1.2 daemon no longer writes.
// Unexported — only RemoveLegacyRegistryFile uses it.
func legacyRegistryPath(cfg *config.Config) (string, error) {
	base, err := filepath.Abs(cfg.Paths.DataDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "registry.json"), nil
}