// me_brain.go — the session that thinks for the owner.
//
// The personal agent asks the owner's thinking when it cannot work out
// what to do. The thinking is an ordinary coding agent, driven over the
// same bridge the rest of nightme uses: nightme picks the agent, never
// the model.

package runtime

import (
	"context"
	"fmt"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/me"
	"github.com/cnlangzi/nightme/internal/timeouts"
)

// brainOpener opens the session that thinks for the owner.
type brainOpener struct{}

// Open implements me.AgentOpener.
//
// The context is the session's scope: the process lives as long as the
// run does.
func (brainOpener) Open(ctx context.Context, name, workspace string) (me.AgentSession, error) {
	starter, err := agent.Builtins.Get(name)
	if err != nil {
		return nil, fmt.Errorf("unknown agent %q: %w", name, err)
	}
	if err := starter.Detect(); err != nil {
		return nil, fmt.Errorf("agent %s is not available: %w", name, err)
	}
	a, err := starter.Start(ctx, agent.StartConfig{Workspace: workspace})
	if err != nil {
		return nil, fmt.Errorf("agent %s could not start: %w", name, err)
	}
	return &brainSession{agent: a, name: name}, nil
}

// brainSession is one session of the agent that thinks for the owner.
type brainSession struct {
	// agent is the live session.
	agent *agent.Agent
	// name is the agent's name, for the failures.
	name string
}

// Ask implements me.AgentSession.
//
// The turn carries its own deadline: the session answers or it does
// not, and a session that never answers must not hold the run.
func (s *brainSession) Ask(ctx context.Context, prompt string) (me.AgentReply, error) {
	turnCtx, cancel := context.WithTimeout(ctx, timeouts.Agent)
	defer cancel()
	res, err := agent.DrainSession(turnCtx, s.agent, []agent.ContentBlock{{
		Type: agent.ContentText,
		Text: prompt,
	}}, nil)
	if err != nil {
		return me.AgentReply{}, fmt.Errorf("agent %s failed: %w", s.name, err)
	}
	return me.AgentReply{Text: res.Text, Model: res.Model}, nil
}

// Close implements me.AgentSession.
func (s *brainSession) Close() error {
	return s.agent.Close()
}
