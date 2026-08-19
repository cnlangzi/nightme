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
// v1.3+ multi-channel: the CLI shell fills in Deps (channel
// registry auto-resolves via NewChannels) and calls Runner.Run.
// The legacy --channel flag is removed.
//
// All construction seams live in Deps; every helper that needs to
// be testable directly (NewEventHandler, WireRuntimeCallbacksAndRestore,
// MarkPromptDone, ShutdownRun, NewMessageDispatcher) is exported.
package runtime

import (
	"github.com/cnlangzi/nightme/internal/chatstore"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentregistry"
	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/registry"
)

// Deps holds the construction seams for the daemon: every
// dependency is injectable for deterministic tests. Nil fields
// fall back to the production defaults applied by Runner.Run.
//
// v1.3+ multi-channel: NewChannels returns every channel with
// valid credentials; the runtime auto-starts them all. Adding a
// new channel is an OCP extension — implement channel.Channel
// and call channel.Register from the adapter's init().
type Deps struct {
	LoadConfig        func() (*config.Config, error)
	OpenChatSessions  func(*config.Config) (*chatstore.Store, error)
	OpenAgentSessions func(*config.Config) (*registry.AgentSessionFile, error)
	BuildAgents       func(*config.Config) *agent.Registry
	NewChannels       func(*config.Config) ([]channel.Channel, error)
	Signals           <-chan os.Signal
	OnReady           func()

	// RegisterHealth, if non-nil, is called once per channel
	// (v1.3+ multi-channel: one snapshot per attached Channel,
	// not a single shared one). The closure receives the
	// channel's HealthSnapshot function.
	RegisterHealth func(snapshot func() (string, json.RawMessage, error))
}

// DefaultDeps returns the production Deps: real config loader,
// real registry stores under cfg.Paths.DataDir, real agent
// registry, and channel.BuildAll (the registry-driven
// multi-channel builder). CLI/test callers override individual
// hooks (echo for smoke tests, temp-dir stores for harness).
func DefaultDeps() Deps {
	return Deps{
		LoadConfig:        config.LoadDefault,
		OpenChatSessions:  defaultOpenChatSessions,
		OpenAgentSessions: defaultOpenAgentSessions,
		BuildAgents:       defaultBuildAgents,
		NewChannels:       channel.BuildAll,
	}
}

// defaultOpenChatSessions opens chat_sessions.json relative to
// cfg.Paths.DataDir.
func defaultOpenChatSessions(cfg *config.Config) (*chatstore.Store, error) {
	path, err := ChatSessionsPath(cfg)
	if err != nil {
		return nil, err
	}
	return chatstore.New(path)
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
