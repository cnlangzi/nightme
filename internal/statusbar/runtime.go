package statusbar

import (
	"context"
	"time"

	"github.com/cnlangzi/nightme/internal/agentsession"
	"github.com/cnlangzi/nightme/internal/messages"
)

// ChatInfo is the minimal chat state NewRuntimeSource needs to
// produce a StatusBar. The runtime extracts this from its
// ChatSession lookup and passes it in. ChatInfo is a value type
// (not a pointer) so callers can return the zero value when the
// chat is unknown — the Source treats that as "skip the status
// bar this turn".
type ChatInfo struct {
	// Cwd is the chat's selected workspace. Empty when the
	// chat has no workspace selected (then the Source returns
	// nil — chat is unusable for any work).
	Cwd string
	// AS is the chat's selected AgentSession (or nil if the
	// chat hasn't picked one yet). When non-nil, the Source
	// produces a full StatusBar via Build. When nil, falls
	// back to a git-only bar using Cwd.
	AS *agentsession.AgentSession
}

// ChatLookupFunc is the per-chat lookup the runtime injects into
// NewRuntimeSource. Implementations return the zero ChatInfo
// (or a value with empty Cwd and nil AS) when the chat is
// unknown — Source treats that as "skip". A nil func pointer is
// also treated as "always skip".
type ChatLookupFunc func(chatID string) ChatInfo

// NewRuntimeSource returns the runtime's per-chat StatusBar
// producer — looks up the chat via lookup, falls back to a
// git-only bar when no AS is selected, returns nil when the
// chat has no workspace at all.
//
// Wired into outbound.Emitter at construction time so the stamp
// happens uniformly: runtime pump, slash-command replies, gtw
// handlers, and MessageState subscribers all converge on the same
// stamping path.
//
// Returning nil signals "skip the status bar this turn"; the
// channel render path treats nil StatusBar as "omit the status
// bar entirely" rather than rendering an empty one.
//
// GitBar fallback (the "git status always present" rule):
// when the chat has no selected AgentSession but does have a
// selected workspace (info.Cwd != ""), the source produces a
// StatusBar with only GitBar populated — git status is still
// attached because the chat user should always see what
// worktree they're talking about, even for /gtw replies or
// pre-spawn placeholders. AgentBar is skipped (no AS → no agent
// identity to surface). UsageBar is nil at this layer
// (AttachIfMissing copies msg.Usage across when present).
//
// Pre-move this was `cmd/nightme/run.go newRuntimeStatusBar`.
// Renamed to NewRuntimeSource because "Stamper" described the
// verb but not the typed payload being produced — the function
// returns the runtime's Source.
//
// PullRequest lookup (F-49) is plumbed through deps.LookupPR /
// deps.RefreshPR the same way as Build — both stamp sites share
// the per-AS prcache registry so a /gtw pr Invalidate surfaces
// on the very next outbound (not after the 60s TTL).
func NewRuntimeSource(lookup ChatLookupFunc, deps Deps) Source {
	return func(chatID string) *messages.StatusBar {
		if lookup == nil {
			return nil
		}
		info := lookup(chatID)
		if info.AS != nil {
			// Happy path: AS selected → full StatusBar
			// (GitBar + AgentBar + optional UsageBar).
			return Build(info.AS, nil, deps)
		}
		// GitBar fallback: no AS, but chat still has a
		// workspace. Produce a git-only StatusBar so the chat
		// user always sees what worktree they're on.
		cwd := info.Cwd
		if cwd == "" {
			// No workspace at all — chat is unusable for any
			// work; skip the status bar entirely.
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		var gitSnap *messages.GitStatusSnapshot
		if deps.CollectGit != nil {
			gitSnap, _ = deps.CollectGit(ctx, cwd)
		}
		cancel()
		return &messages.StatusBar{
			GitBar: &messages.GitStatusBar{
				Workspace: cwd,
				GitStatus: gitSnap,
				// PullRequest: nil — PR lookup is per-AS.
			},
		}
	}
}
