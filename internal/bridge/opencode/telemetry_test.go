package opencode

import (
	"context"
	"slices"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/telemetry"
)

func TestAcpStarter_DisablesOpencodeTelemetry(t *testing.T) {
	telemetry.Allow(false)

	s := NewStarter("opencode", "opencode", []string{"acp"})
	env := s.acpStarter(context.Background(), agent.StartConfig{Workspace: t.TempDir()}).Info().Env
	if !slices.Contains(env, "DO_NOT_TRACK=1") {
		t.Fatalf("spawned child env does not carry DO_NOT_TRACK=1: %v", env)
	}
}

func TestAcpStarter_AllowedTelemetryLeavesTheKnobOut(t *testing.T) {
	telemetry.Allow(true)
	t.Cleanup(func() { telemetry.Allow(false) })

	s := NewStarter("opencode", "opencode", []string{"acp"})
	env := s.acpStarter(context.Background(), agent.StartConfig{Workspace: t.TempDir()}).Info().Env
	if slices.Contains(env, "DO_NOT_TRACK=1") {
		t.Fatalf("spawned child env carries DO_NOT_TRACK=1 with agent telemetry allowed: %v", env)
	}
}
