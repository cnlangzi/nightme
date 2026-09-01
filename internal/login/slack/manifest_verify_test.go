package slack

import (
	"strings"
	"testing"
)

// TestManifestHasSlashCommands verifies the AppManifest registers
// every IM command under internal/command/*/cmd.go so Slack's
// client-side composer does not intercept them as "not a valid
// command". See docs/channel/slack.md §9 known-issue A.
func TestManifestHasSlashCommands(t *testing.T) {
	m := AppManifest
	required := []string{
		"slash_commands:",
		"- command: /cwd",
		"- command: /use",
		"- command: /watch",
		"- command: /stop",
		"- command: /steer",
		"- command: /queue",
		"- command: /new",
		// /close is reserved by Slack (built-in channel close). The
		// manifest registers /kclose instead; handleSlashCommand in
		// internal/channel/slack/adapter.go translates it back to
		// /close for the engine's command parser.
		"- command: /kclose",
		"- command: /think",
		"- command: /tools",
		"- command: /review",
		"- command: /gtw",
	}
	for _, want := range required {
		if !strings.Contains(m, want) {
			t.Errorf("manifest missing %q", want)
		}
	}
	if strings.Contains(m, "- command: /close") {
		t.Errorf("manifest still has /close (reserved by Slack; use /kclose)")
	}
	lines := strings.Split(m, "\n")
	inSlash := false
	count := 0
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "slash_commands:") {
			inSlash = true
			continue
		}
		if !inSlash {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "oauth_config:") {
			break
		}
		if strings.HasPrefix(strings.TrimSpace(line), "- command: /") {
			count++
		}
	}
	if count != 12 {
		t.Errorf("expected 12 slash command entries, got %d", count)
	}
}
