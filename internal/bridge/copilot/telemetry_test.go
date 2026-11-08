package copilot

import (
	"slices"
	"testing"

	"github.com/cnlangzi/nightme/internal/bridge/telemetry"
)

func TestAcpStarter_DisablesCopilotTelemetry(t *testing.T) {
	telemetry.Allow(false)

	s := NewStarter("copilot", "copilot", DefaultACPArgs)
	args := s.acpStarter().Info().Args
	if !slices.Contains(args, "--disable-client-telemetry") {
		t.Fatalf("spawned child argv does not carry --disable-client-telemetry: %v", args)
	}
}

func TestAcpStarter_TelemetryFlagPrecedesTheTransportFlags(t *testing.T) {
	telemetry.Allow(false)

	s := NewStarter("copilot", "copilot", DefaultACPArgs)
	args := s.acpStarter().Info().Args
	telemetryAt := slices.Index(args, "--disable-client-telemetry")
	transportAt := slices.Index(args, "--acp")
	if telemetryAt < 0 || transportAt < 0 {
		t.Fatalf("argv is missing the telemetry or transport flag: %v", args)
	}
	if telemetryAt > transportAt {
		t.Fatalf("telemetry flag at %d follows the transport flag at %d: %v", telemetryAt, transportAt, args)
	}
}

func TestAcpStarter_AllowedTelemetryLeavesTheKnobOut(t *testing.T) {
	telemetry.Allow(true)
	t.Cleanup(func() { telemetry.Allow(false) })

	s := NewStarter("copilot", "copilot", DefaultACPArgs)
	args := s.acpStarter().Info().Args
	if slices.Contains(args, "--disable-client-telemetry") {
		t.Fatalf("spawned child argv carries --disable-client-telemetry with agent telemetry allowed: %v", args)
	}
}
