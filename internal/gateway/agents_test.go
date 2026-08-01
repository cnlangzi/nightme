package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestRenderAgents_Empty confirms the empty-registry case returns a
// graceful fallback instead of crashing or printing an empty bullet
// list.
func TestRenderAgents_Empty(t *testing.T) {
	reg := agent.New()
	got := renderAgents(reg)
	if got != "no agents registered" {
		t.Errorf("renderAgents(empty) = %q, want %q", got, "no agents registered")
	}
}

// TestRenderAgents_Nil covers the defensive nil-registry branch —
// tests sometimes wire a handlerContext without populating agents.
func TestRenderAgents_Nil(t *testing.T) {
	if got := renderAgents(nil); got != "no agents registered" {
		t.Errorf("renderAgents(nil) = %q, want %q", got, "no agents registered")
	}
}

// TestRenderAgents_Multiple verifies the IM-friendly layout: bullet
// per agent, name/command/args visible, footer hints at /run.
func TestRenderAgents_Multiple(t *testing.T) {
	reg := agent.New()
	reg.Register(agentForTest("claude", "claude", nil))
	reg.Register(agentForTest("codex", "codex-acp", nil))
	reg.Register(agentForTest("opencode", "opencode", []string{"acp"}))

	got := renderAgents(reg)
	for _, want := range []string{
		"Registered agents:",
		"claude",
		"codex-acp",
		"opencode",
		"acp",
		"/run",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("renderAgents missing %q: %q", want, got)
		}
	}
}

// TestAgentsHandler drives the cobra wiring: a /agents message lands
// in the registry and produces the same renderAgents output via the
// responder.
func TestAgentsHandler(t *testing.T) {
	gw, _, resp := newTestStack(t)

	if err := gw.Handle(WithGateway(context.Background(), gw), &Message{
		ChatID: "oc_chat",
		Text:   "/agents",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	reply := resp.last()
	if !strings.Contains(reply, "claude") {
		t.Errorf("agents reply missing claude: %q", reply)
	}
	if !strings.Contains(reply, "/run") {
		t.Errorf("agents reply missing /run hint: %q", reply)
	}
}

// agentForTest is a tiny in-package helper to register a single
// Agent interface entry. It avoids importing the bridge packages
// just for the gateway test surface.
func agentForTest(name, command string, args []string) agent.Agent {
	return testAgent{name: name, command: command, args: args}
}

// testAgent is the smallest possible agent.Agent for gateway tests.
// It returns ModePTY so it matches the registry helpers used by the
// other handler tests.
type testAgent struct {
	name    string
	command string
	args    []string
}

func (t testAgent) Name() string    { return t.name }
func (t testAgent) Mode() agent.Mode { return agent.ModePTY }
func (t testAgent) Command() string { return t.command }
func (t testAgent) Args() []string  { return append([]string(nil), t.args...) }
func (t testAgent) Detect() error   { return nil }
func (t testAgent) Start(context.Context, agent.StartConfig) (agent.AgentSession, error) {
	return nil, nil
}