package pi

import (
	"os"
	"slices"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/telemetry"
)

func TestChildEnviron_DisablesPiTelemetry(t *testing.T) {
	telemetry.Allow(false)

	env := childEnviron(agent.StartConfig{})
	if !slices.Contains(env, "PI_TELEMETRY=0") {
		t.Fatalf("child env does not carry PI_TELEMETRY=0: %v", env)
	}
}

func TestChildEnviron_AllowedTelemetryLeavesTheKnobOut(t *testing.T) {
	telemetry.Allow(true)
	t.Cleanup(func() { telemetry.Allow(false) })

	env := childEnviron(agent.StartConfig{})
	if slices.Contains(env, "PI_TELEMETRY=0") {
		t.Fatalf("child env carries PI_TELEMETRY=0 with agent telemetry allowed: %v", env)
	}
}

func TestChildEnviron_LeavesTheDaemonEnvironmentAlone(t *testing.T) {
	telemetry.Allow(false)
	t.Setenv("PI_TELEMETRY", "")

	env := childEnviron(agent.StartConfig{})
	if !slices.Contains(env, "PI_TELEMETRY=0") {
		t.Fatalf("child env does not carry PI_TELEMETRY=0: %v", env)
	}
	if got := os.Getenv("PI_TELEMETRY"); got != "" {
		t.Fatalf("daemon env PI_TELEMETRY = %q, want empty", got)
	}
}
