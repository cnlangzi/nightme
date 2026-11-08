package copilot

import (
	"flag"
	"os"
	"slices"
	"testing"

	"github.com/cnlangzi/nightme/internal/bridge/telemetry"
)

// standInTelemetryFlag registers a flag whose name matches the
// Copilot CLI's telemetry knob, so this test binary's own `--help`
// output — which the probe reads — carries the flag the way the real
// CLI's help does.
var standInTelemetryFlag = flag.String("disable-client-telemetry", "",
	"stand-in for the Copilot CLI's telemetry flag")

func TestAcpStarter_AllowedTelemetryLeavesTheKnobOut(t *testing.T) {
	telemetry.Allow(true)
	t.Cleanup(func() { telemetry.Allow(false) })

	s := NewStarter("copilot", "copilot", DefaultACPArgs)
	args := s.acpStarter().Info().Args
	if slices.Contains(args, "--disable-client-telemetry") {
		t.Fatalf("spawned child argv carries --disable-client-telemetry with agent telemetry allowed: %v", args)
	}
}

// TestChildArgs_AddsTheKnobWhenTheBinarySupportsIt verifies the
// probe finds the flag in the binary's own help output and the bridge
// adds it ahead of the transport flags.
func TestChildArgs_AddsTheKnobWhenTheBinarySupportsIt(t *testing.T) {
	telemetry.Allow(false)

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test binary: %v", err)
	}
	args := childArgs([]string{"--acp", "--stdio"}, self)
	if !slices.Contains(args, "--disable-client-telemetry") {
		t.Fatalf("spawned child argv does not carry --disable-client-telemetry: %v", args)
	}
	telemetryAt := slices.Index(args, "--disable-client-telemetry")
	transportAt := slices.Index(args, "--acp")
	if telemetryAt > transportAt {
		t.Fatalf("telemetry flag at %d follows the transport flag at %d: %v", telemetryAt, transportAt, args)
	}
}

// TestChildArgs_MissingBinaryKeepsTheKnobOut covers the contract
// that matters most: a copilot build that cannot answer the probe
// must not abort the spawn. The CLI rejects an unknown flag outright
// ("error: unexpected argument"), so a build that predates
// --disable-client-telemetry has to start without the knob.
func TestChildArgs_MissingBinaryKeepsTheKnobOut(t *testing.T) {
	telemetry.Allow(false)

	args := childArgs([]string{"--acp", "--stdio"}, "copilot-does-not-exist-xyz-12345")
	if slices.Contains(args, "--disable-client-telemetry") {
		t.Fatalf("spawned child argv carries --disable-client-telemetry for a binary that cannot run: %v", args)
	}
	if !slices.Contains(args, "--acp") {
		t.Fatalf("spawned child argv lost its transport flags: %v", args)
	}
}
