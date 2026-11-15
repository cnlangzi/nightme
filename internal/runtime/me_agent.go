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
	"log/slog"
	"path/filepath"
	"time"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/command/gtw"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/llm"
	_ "github.com/cnlangzi/nightme/internal/llm/anthropic" // registers the Anthropic chat provider
	_ "github.com/cnlangzi/nightme/internal/llm/decision"  // registers the decision protocols
	_ "github.com/cnlangzi/nightme/internal/llm/openai"    // registers the OpenAI-compatible chat provider
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
	dir, err := workflowsDir(cfg)
	if err != nil {
		logger.Warn("run: the personal agent has no workflows dir", "err", err)
		return nil
	}

	provider := buildChatModel(cfg, logger)
	decider := buildDecider(cfg, logger)

	svc := me.New(me.Config{
		WorkflowsDir:  dir,
		StateDir:      filepath.Join(dir, "runs"),
		Provider:      provider,
		Decider:       decider,
		MinConfidence: cfg.Me.Decider.MinConfidence,
		Agent:         cfg.Me.LLM.Agent,
		AgentOpener:   brainOpener{},
		Operator:      op,
		Shell:         me.ExecShell{},
		Git:           gtw.ExecGitRunner{},
		Cwd:           chatCwd{},
	})
	svc.SetLogger(logger)
	return svc
}

// buildChatModel builds the chat model (System 2).
//
// A missing configuration is not an error: the personal agent runs
// with its fast path alone.
func buildChatModel(cfg *config.Config, logger *slog.Logger) llm.Provider {
	if cfg.Me.LLM.Provider == "" {
		return nil
	}
	llmCfg := llm.Config{
		Provider:  cfg.Me.LLM.Provider,
		BaseURL:   cfg.Me.LLM.BaseURL,
		Model:     cfg.Me.LLM.Model,
		APIKey:    cfg.Me.LLM.APIKey,
		APIKeyEnv: cfg.Me.LLM.APIKeyEnv,
		MaxTokens: cfg.Me.LLM.MaxTokens,
	}
	warnDualCredential(logger, "me.llm", llmCfg)
	provider, err := llm.New(llmCfg)
	if err != nil {
		logger.Warn("run: the chat model could not be built", "err", err)
		return nil
	}
	return provider
}

// buildDecider builds the decision model (System 1).
//
// A missing configuration is not an error: the personal agent runs
// with its tool loop alone.
func buildDecider(cfg *config.Config, logger *slog.Logger) llm.Decider {
	if cfg.Me.Decider.Protocol == "" {
		return nil
	}
	llmCfg := llm.Config{
		Protocol:  cfg.Me.Decider.Protocol,
		BaseURL:   cfg.Me.Decider.BaseURL,
		Model:     cfg.Me.Decider.Model,
		APIKey:    cfg.Me.Decider.APIKey,
		APIKeyEnv: cfg.Me.Decider.APIKeyEnv,
	}
	warnDualCredential(logger, "me.decider", llmCfg)
	decider, err := llm.NewDecider(llmCfg)
	if err != nil {
		logger.Warn("run: the decision model could not be built", "err", err)
		return nil
	}
	return decider
}

// warnDualCredential reports a capability that names its key twice.
//
// The inline key wins, so the configuration is unambiguous — but the
// operator should know that one of the two is dead weight. The warning
// carries neither of the credentials.
func warnDualCredential(logger *slog.Logger, capability string, cfg llm.Config) {
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
