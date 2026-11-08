package codex

import (
	"slices"
	"testing"

	"github.com/cnlangzi/nightme/internal/bridge/telemetry"
)

func TestAppServerArgs_DisablesCodexAnalytics(t *testing.T) {
	telemetry.Allow(false)

	args := appServerArgs(sessionConfig{})
	if !slices.Contains(args, "analytics.enabled=false") {
		t.Fatalf("spawned child argv does not carry analytics.enabled=false: %v", args)
	}
}

func TestAppServerArgs_AllowedTelemetryLeavesTheKnobOut(t *testing.T) {
	telemetry.Allow(true)
	t.Cleanup(func() { telemetry.Allow(false) })

	args := appServerArgs(sessionConfig{})
	if slices.Contains(args, "analytics.enabled=false") {
		t.Fatalf("spawned child argv carries analytics.enabled=false with agent telemetry allowed: %v", args)
	}
}
