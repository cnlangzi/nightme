package claudecode

import (
	"os"
	"slices"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/telemetry"
)

func TestChildEnviron_DisablesClaudeCodeTelemetry(t *testing.T) {
	telemetry.Allow(false)

	env := childEnviron(agent.StartConfig{})
	if !slices.Contains(env, "DISABLE_TELEMETRY=1") {
		t.Fatalf("child env does not carry DISABLE_TELEMETRY=1: %v", env)
	}
}

func TestChildEnviron_AllowedTelemetryLeavesTheKnobOut(t *testing.T) {
	telemetry.Allow(true)
	t.Cleanup(func() { telemetry.Allow(false) })

	env := childEnviron(agent.StartConfig{})
	if slices.Contains(env, "DISABLE_TELEMETRY=1") {
		t.Fatalf("child env carries DISABLE_TELEMETRY=1 with agent telemetry allowed: %v", env)
	}
}

func TestChildEnviron_LeavesTheDaemonEnvironmentAlone(t *testing.T) {
	telemetry.Allow(false)
	t.Setenv("DISABLE_TELEMETRY", "")

	env := childEnviron(agent.StartConfig{})
	if !slices.Contains(env, "DISABLE_TELEMETRY=1") {
		t.Fatalf("child env does not carry DISABLE_TELEMETRY=1: %v", env)
	}
	if got := os.Getenv("DISABLE_TELEMETRY"); got != "" {
		t.Fatalf("daemon env DISABLE_TELEMETRY = %q, want empty", got)
	}
}
