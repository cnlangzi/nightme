package statusbar

import (
	"context"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentsession"
	"github.com/cnlangzi/nightme/internal/messages"
)

// Build assembles a StatusBar from a single AgentSession + usage
// snapshot. Pure: no I/O of its own beyond the delegated git
// status call (3s deadline to keep a hung git from blocking the
// outbound pipeline).
//
// Always returns a non-nil StatusBar when s is non-nil: the
// GitBar is populated unconditionally (so the channel can show
// "git status unknown" honestly when collection timed out), and
// AgentBar / UsageBar are added when their backing data is
// present. Pre-move this was `cmd/nightme/run.go buildStatusBar`
// with a flat `Returns nil when there's nothing to render` gate;
// that gate was dropped during the F-58 rename because GitBar is
// always populated when an AS exists.
//
// Workspace resolution: AS.Cwd is the source of truth when an AS
// exists; the caller is responsible for passing the correct AS.
// The status-bar fallback (no AS) lives in NewRuntimeSource, not
// here — this function is AS-centric.
//
// PR / MR lookup (F-49): per-AgentSession, via deps.LookupPR
// (synchronous read of the runtime's prcache) and deps.RefreshPR
// (asynchronous background refresh). The read is strictly
// synchronous (returns the cached value, possibly nil on the
// first ever call) — no I/O blocks the stamp path. RefreshPR
// fires a background refresh; the next stamp picks up the
// refreshed value.
//
// Nil-safe on every dep: a hand-wired debug build that skips
// git / refresh / PR-lookup construction still gets a
// fully-functional stamp path with PR / GitStatus fields left
// nil.
func Build(s *agentsession.AgentSession, usage *agent.UsageInfo, deps Deps) *messages.StatusBar {
	cwd := s.Cwd

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	var gitSnap *messages.GitStatusSnapshot
	if deps.CollectGit != nil {
		gitSnap, _ = deps.CollectGit(ctx, cwd)
	}
	cancel()

	var prRef *messages.PR
	if deps.RefreshPR != nil {
		deps.RefreshPR(s.ID, cwd)
	}
	if deps.LookupPR != nil {
		prRef = deps.LookupPR(s.ID)
	}

	// Snapshot Model() and SessionID() once each — both take
	// asMu.RLock() internally; calling them twice (here + in
	// the literal below) would double the lock acquisitions
	// per stamp for no functional gain.
	model := s.Model()
	sessionID := s.SessionID()

	// Always populate GitBar when an AS exists — even when
	// git collection timed out and GitStatus is nil. The
	// renderer decides whether to show the git line based on
	// GitStatus; producing a GitBar with nil GitStatus is the
	// honest "git status unknown" state.
	gitBar := &messages.GitStatusBar{
		Workspace:   cwd,
		GitStatus:   gitSnap,
		PullRequest: prRef,
	}

	sb := &messages.StatusBar{
		GitBar: gitBar,
	}

	// AgentBar: every segment is omitted independently when
	// empty (the renderer follows the same rule). Build it
	// unconditionally — if all three are empty, AgentBar
	// still carries as a typed marker that the AS exists.
	if s.Agent != "" || model != "" || sessionID != "" {
		sb.AgentBar = &messages.AgentStatusBar{
			Agent:     s.Agent,
			Model:     model,
			SessionID: sessionID,
		}
	}

	if usage != nil {
		sb.UsageBar = &messages.UsageStatusBar{UsageInfo: usage}
	}

	return sb
}
