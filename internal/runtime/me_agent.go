// me_agent.go — the personal agent the daemon runs alongside its
// channels.
//
// The personal agent is the user half of nightme: it runs a
// playbook through the loopback channel, decides what comes next, and
// reports back to the chat that started the run.
//
// Its models are the user's choice. me.llm is the chat model (System
// 2), me.decider the decision model (System 1); either, both or
// neither may be configured. A model that is configured but cannot be
// built is reported and left out — the daemon still comes up, and /me
// says why it cannot run.

package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/channel/loopback"
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
// It returns nil when the daemon has no loopback channel: the agent
// drives the daemon through that channel, so without it there is no
// chat to drive.
func buildPersonalAgent(cfg *config.Config, chs []channel.Channel, logger *slog.Logger) *me.Service {
	lb := findLoopback(chs)
	if lb == nil {
		logger.Info("run: no loopback channel; the personal agent stays off")
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
		Loopback:      loopback.NewClient(lb),
		Sender:        NewChatSender(),
		Answerer:      chatAnswerer{},
		Git:           gtw.ExecGitRunner{},
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
	provider, err := llm.New(llm.Config{
		Provider:  cfg.Me.LLM.Provider,
		BaseURL:   cfg.Me.LLM.BaseURL,
		Model:     cfg.Me.LLM.Model,
		APIKeyEnv: cfg.Me.LLM.APIKeyEnv,
		MaxTokens: cfg.Me.LLM.MaxTokens,
	})
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
	decider, err := llm.NewDecider(llm.Config{
		Protocol:  cfg.Me.Decider.Protocol,
		BaseURL:   cfg.Me.Decider.BaseURL,
		Model:     cfg.Me.Decider.Model,
		APIKeyEnv: cfg.Me.Decider.APIKeyEnv,
	})
	if err != nil {
		logger.Warn("run: the decision model could not be built", "err", err)
		return nil
	}
	return decider
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

// chatAnswerer answers a pending prompt of a work chat.
//
// The daemon owns the answer: the prompt is waiting inside the agent
// session the work chat drives, and the agent's handle is what takes
// the answer.
type chatAnswerer struct{}

// Answer implements me.ChoiceAnswerer.
func (chatAnswerer) Answer(chatID, requestID, optionID string) error {
	cs := findChatSession(chatID)
	if cs == nil {
		return fmt.Errorf("runtime: no chat session for %q", chatID)
	}
	as := cs.SelectedAgentSession()
	if as == nil {
		return errors.New("runtime: the chat has no selected agent")
	}
	h := as.Handle()
	if h == nil {
		return errors.New("runtime: the agent is not running")
	}
	return h.SendPermission(optionID)
}
