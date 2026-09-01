package slack

import (
	"strings"
	"testing"
)

// TestManifestHasNoSlashCommands verifies the AppManifest does NOT
// register any `slash_commands` segment, since 2026-09-01 nightme
// uses `$`-prefix plain-text message matching for command invocation
// (docs/channel/slack.md §6.2.1). This is an inverted regression
// test: a future engineer who re-adds a `slash_commands` block (to
// re-enable Slack's built-in command autocomplete) will be reminded
// that the design moved away from that mechanism — see the inline
// comment in manifest.go for the rationale.
func TestManifestHasNoSlashCommands(t *testing.T) {
	m := AppManifest
	if strings.Contains(m, "slash_commands:") {
		t.Errorf("manifest must not contain a 'slash_commands:' segment (use $-prefix plain text matching instead); see docs/channel/slack.md §6.2.1")
	}
	// Also block the individual `- command: /X` entries that would
	// only appear inside a slash_commands block.
	for _, cmd := range []string{"/cwd", "/use", "/watch", "/stop", "/steer", "/queue", "/new", "/kclose", "/think", "/tools", "/review", "/gtw"} {
		needle := "- command: " + cmd
		if strings.Contains(m, needle) {
			t.Errorf("manifest must not register %q as a Slack slash command", cmd)
		}
	}
}
