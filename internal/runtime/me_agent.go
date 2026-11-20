// me_agent.go — the personal agent the daemon runs alongside its
// channels.
//
// The personal agent is the user half of nightme: it runs a workflow
// in the chat the run started in, decides what comes next, and shows
// the run's overview there.
//
// Its models are the user's choice. me.llm is the chat model (System
// 2), me.decider the decision model (System 1); either, both or
// neither may be configured. A model that is configured but cannot be
// built is reported and left out — the daemon still comes up, and /me
// says why it cannot run.

package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/command/gtw"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/llm"
	_ "github.com/cnlangzi/nightme/internal/llm/chat"     // registers the chat protocols
	_ "github.com/cnlangzi/nightme/internal/llm/decision" // registers the decision protocols
	"github.com/cnlangzi/nightme/internal/me"
)

// buildPersonalAgent constructs the personal agent.
//
// It returns nil when the daemon has no channel to work in: the agent
// drives the daemon through a chat, so without one there is nothing
// to drive.
func buildPersonalAgent(cfg *config.Config, chs []channel.Channel, op *operator, logger *slog.Logger) *me.Service {
	if len(chs) == 0 {
		logger.Info("run: no channel; the personal agent stays off")
		return nil
	}
	dir, err := WorkflowsDir(cfg)
	if err != nil {
		logger.Warn("run: the personal agent has no workflows dir", "err", err)
		return nil
	}

	provider, chatIssue := buildChatModel(cfg, logger)
	decider, deciderIssue := buildDecider(cfg, logger)

	issues := make([]string, 0, 2)
	if chatIssue != "" {
		issues = append(issues, chatIssue)
		logger.Error("run: the chat model did not start: " + chatIssue)
	}
	if deciderIssue != "" {
		issues = append(issues, deciderIssue)
		logger.Error("run: the decision model did not start: " + deciderIssue)
	}

	svc := me.New(personalAgentConfig(cfg, dir, op, provider, decider, issues))
	svc.SetLogger(logger)
	if len(issues) == 0 {
		// No model was asked for at all: that is a state, not an
		// error. The agent goes on without one.
		if reason := svc.ModelsReason(); reason != "" {
			logger.Info("run: " + reason)
		}
	}
	return svc
}

// personalAgentConfig maps the configuration onto the personal agent's
// own configuration.
//
// The agent that thinks for the owner comes from the me section: it
// names a coding agent, not a model.
func personalAgentConfig(cfg *config.Config, dir string, op *operator, provider llm.Provider, decider llm.Decider, issues []string) me.Config {
	return me.Config{
		WorkflowsDir:  dir,
		StateDir:      filepath.Join(dir, "runs"),
		Provider:      provider,
		Decider:       decider,
		ModelIssues:   issues,
		MinConfidence: cfg.Me.Decider.MinConfidence,
		MaxJudgments:  cfg.Me.MaxJudgments,
		Agent:         cfg.Me.Agent,
		AgentOpener:   brainOpener{},
		Operator:      op,
		Shell:         me.ExecShell{},
		Git:           gtw.ExecGitRunner{},
		Cwd:           chatCwd{},
		AgentSwitcher: chatAgentSwitcher{},
	}
}

// ChatModelConfig maps the chat-model configuration onto the
// configuration of the LLM client.
//
// The daemon and the `nightme me` wizard build the chat model from
// the same block; they must build the same one.
func ChatModelConfig(c config.MeLLMConfig) llm.ChatConfig {
	return llm.ChatConfig{
		Protocol:  c.Protocol,
		BaseURL:   c.BaseURL,
		Model:     c.Model,
		APIKey:    c.APIKey,
		APIKeyEnv: c.APIKeyEnv,
		MaxTokens: c.MaxTokens,
		Timeout:   c.Timeout,
		Headers:   c.Headers,
	}
}

// buildChatModel builds the chat model (System 2).
//
// A missing configuration is not an error: the personal agent runs
// with its fast path alone. A half-written one is an issue — the
// operator learns which key is missing, or why the model does not
// build.
func buildChatModel(cfg *config.Config, logger *slog.Logger) (llm.Provider, string) {
	c := cfg.Me.LLM
	switch {
	case c.Protocol == "" && c.Model == "":
		return nil, ""
	case c.Protocol == "":
		return nil, fmt.Sprintf("me.llm.protocol is not set (known: %s)", protocolNames(llm.ChatProtocols()))
	case c.Model == "":
		return nil, "me.llm.model is not set"
	}
	llmCfg := ChatModelConfig(c)
	warnDualCredential(logger, "me.llm", llmCfg)
	provider, err := llm.New(llmCfg)
	if err != nil {
		return nil, "the chat model could not be built: " + err.Error()
	}
	return provider, ""
}

// buildDecider builds the decision model (System 1).
//
// A missing configuration is not an error: the personal agent runs
// with its tool loop alone. A half-written one is an issue — the
// operator learns which key is missing, or why the model does not
// build.
func buildDecider(cfg *config.Config, logger *slog.Logger) (llm.Decider, string) {
	c := cfg.Me.Decider
	switch {
	case c.Protocol == "" && c.Model == "":
		return nil, ""
	case c.Protocol == "":
		return nil, fmt.Sprintf("me.decider.protocol is not set (known: %s)", protocolNames(llm.DecisionProtocols()))
	case c.Model == "":
		return nil, "me.decider.model is not set"
	}
	llmCfg := llm.DeciderConfig{
		Protocol:  c.Protocol,
		BaseURL:   c.BaseURL,
		Model:     c.Model,
		APIKey:    c.APIKey,
		APIKeyEnv: c.APIKeyEnv,
		Timeout:   c.Timeout,
		Headers:   c.Headers,
	}
	warnDualCredential(logger, "me.decider", llmCfg)
	decider, err := llm.NewDecider(llmCfg)
	if err != nil {
		return nil, "the decision model could not be built: " + err.Error()
	}
	return decider, ""
}

// protocolNames renders the names of the registered protocols for an
// error message.
func protocolNames(protocols []llm.Protocol) string {
	names := make([]string, 0, len(protocols))
	for _, p := range protocols {
		names = append(names, p.Name)
	}
	return strings.Join(names, ", ")
}

// personalAgentStartLine renders how the personal agent came up: with
// models to decide with, or without them.
//
// The reason is the one the agent itself gives, so the log and the
// chat say the same thing.
func personalAgentStartLine(reason string) string {
	if reason != "" {
		return "run: the personal agent has no models: " + reason
	}
	return "run: personal agent started"
}

// warnDualCredential reports a capability that names its key twice.
//
// The inline key wins, so the configuration is unambiguous — but the
// operator should know that one of the two is dead weight. The warning
// carries neither of the credentials.
func warnDualCredential(logger *slog.Logger, capability string, cfg llm.Credentialed) {
	if !llm.DualCredential(cfg) {
		return
	}
	logger.Warn("run: both api_key and api_key_env are set; api_key wins", "capability", capability)
}

// stopPersonalAgent stops the personal agent. It is a no-op for a
// daemon that never started one.
//
// The stop is bounded: a run that is still going is cancelled and the
// daemon does not wait for it forever.
func stopPersonalAgent(svc *me.Service, logger *slog.Logger) {
	if svc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := svc.Shutdown(ctx); err != nil {
		logger.Warn("run: the personal agent did not stop cleanly", "err", err)
	}
}

// chatCwd reports the directory a chat currently works in.
//
// The directory moves when a command moves it — `/gtw fix` moves the
// chat into the worktree — so a shell step of a workflow runs where
// the chat stands.
type chatCwd struct{}

// Cwd implements me.CwdProvider.
func (chatCwd) Cwd(chatID string) string {
	cs := findChatSession(chatID)
	if cs == nil {
		return ""
	}
	return cs.SelectedCwd()
}

// chatAgentSwitcher switches the agent a chat works with, for the
// length of one attempt.
type chatAgentSwitcher struct{}

// SwitchAgent implements me.AgentSwitcher.
func (chatAgentSwitcher) SwitchAgent(chatID, agent string) (func(), error) {
	cs := findChatSession(chatID)
	if cs == nil {
		return nil, fmt.Errorf("no chat session owns %q", chatID)
	}
	return cs.SwitchAgentTransient(agent), nil
}
