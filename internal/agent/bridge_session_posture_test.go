package agent_test

// Session spawn posture for the bridges whose session path carries
// spawn-time approval flags.
//
// /review runs on the agent's session path with no interactive user,
// so every bridge must spawn its session in the same posture its
// print-mode one-shot uses: never ask for approval, grant full
// access. A bridge that differs would wedge the review turn on an
// unanswered approval.
//
// The bridges without a spawn-time flag are pinned elsewhere:
//
//   - codex — the app-server argv (approval_policy="never",
//     sandbox_mode="danger-full-access") is pinned by
//     internal/bridge/codex/session_unix_test.go.
//   - dsh — the shared host's permission mode defaults to
//     danger-full-access; pinned by internal/bridge/dsh/handshake_test.go.
//   - opencode / acp — no spawn flag exists; approvals arrive over the
//     ACP protocol and the shared session drain answers them with the
//     first option (internal/agent/run_session.go).

import (
	"testing"

	"github.com/cnlangzi/nightme/internal/bridge/claudecode"
	"github.com/cnlangzi/nightme/internal/bridge/copilot"
	"github.com/cnlangzi/nightme/internal/bridge/cursor"
	"github.com/cnlangzi/nightme/internal/bridge/omp"
	"github.com/cnlangzi/nightme/internal/bridge/pi"
)

// TestSessionSpawnPosture_NeverAskFullAccess pins the approval
// posture of each bridge's session spawn args.
func TestSessionSpawnPosture_NeverAskFullAccess(t *testing.T) {
	cases := []struct {
		bridge string
		args   []string
		want   []string
	}{
		{"claudecode", claudecode.DefaultArgs, []string{"--permission-mode", "bypassPermissions"}},
		{"pi", pi.DefaultArgs, []string{"--approve"}},
		{"omp", omp.DefaultArgs, []string{"--approval-mode", "yolo"}},
		{"copilot", copilot.DefaultACPArgs, []string{"--allow-all-tools"}},
		{"cursor", cursor.DefaultACPArgs, []string{"--force", "--trust", "--sandbox", "disabled"}},
	}
	for _, c := range cases {
		t.Run(c.bridge, func(t *testing.T) {
			for _, w := range c.want {
				if !containsArg(c.args, w) {
					t.Errorf("%s session args %v missing %q", c.bridge, c.args, w)
				}
			}
		})
	}
}

// containsArg reports whether args holds want as an element.
func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}
