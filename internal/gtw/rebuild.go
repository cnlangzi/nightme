package gtw

import (
	"context"
	"strings"
)

// RebuildContext is the daemon-recovery entry point. When a /gtw
// command is invoked after the daemon restarts (gtwContext was
// in-memory and is now zero), we reconstruct it from:
//
//  1. The git worktree list — if the cwd is a worktree holding a
//     branch named `fix/<id>-*`, we have a strong signal.
//  2. The platform label lookup — if the branch matches an issue
//     with nightme/wip, we have the issue number.
//  3. The cwd's path layout — confirms the worktree ↔ repo mapping.
//
// Returns the zero Context when the cwd is not part of any /gtw fix;
// the caller should treat that as "no rebuild needed" and proceed
// with the normal /gtw fix flow.
//
// The runtime calls RebuildContext from the preflight step of any
// /gtw command (§5.7) when gtwContext is the zero value.
func RebuildContext(
	ctx context.Context,
	cs Sender,
	git GitRunner,
	newPlatform func(PlatformKind) (PlatformClient, error),
) Context {
	cwd := cs.ActiveCwd()
	if cwd == "" {
		return Context{}
	}
	// Step 1: is cwd a worktree?
	branch, err := CurrentBranch(ctx, cwd, git)
	if err != nil || branch == "" {
		return Context{}
	}
	if !strings.HasPrefix(branch, IssueIDPrefix+"/") {
		return Context{}
	}
	// Step 2: parse the issue id out of the branch name.
	rest := strings.TrimPrefix(branch, IssueIDPrefix+"/")
	// rest = "<id>-<slug>" or just "<id>"
	idPart, _, hasDash := strings.Cut(rest, "-")
	if !hasDash {
		idPart = rest
	}
	var issueID int
	for _, r := range idPart {
		if r < '0' || r > '9' {
			return Context{}
		}
		issueID = issueID*10 + int(r-'0')
	}
	if issueID == 0 {
		return Context{}
	}
	// Step 3: query the platform for the issue's current label.
	// We need the remote URL to know which platform / repo to call.
	// `git remote get-url origin` works from inside a worktree
	// (it reads the shared .git config) so we don't need to walk
	// up to the main repo root.
	remoteURL, err := RemoteOriginURL(ctx, cwd, git)
	if err != nil || remoteURL == "" {
		return Context{}
	}
	kind, err := DetectPlatform(remoteURL)
	if err != nil {
		return Context{}
	}
	owner, repo, err := ParseRepoOwner(remoteURL)
	if err != nil {
		return Context{}
	}
	plat, err := newPlatform(kind)
	if err != nil {
		return Context{}
	}
	issue, err := plat.GetIssue(ctx, owner, repo, issueID)
	now := timeNow()
	if err != nil {
		// Issue not found (deleted / moved) — keep the local
		// state but tag it as "fixing" anyway. The next /gtw
		// command will surface the issue-status mismatch.
		return Context{
			Issue:     issueID,
			Branch:    branch,
			Worktree:  cwd,
			State:     StateFixing,
			UpdatedAt: now,
		}
	}
	state := StateFixing
	for _, lbl := range issue.Labels {
		switch lbl {
		case LabelReady:
			state = StateReady
		case LabelWIP, LabelReviewing, LabelRevise, LabelDone, LabelStuck:
			// All other gtw states are treated as "fixing" for
			// v1 — the FSM evolution is F-46+.
		}
	}
	return Context{
		Issue:     issueID,
		Branch:    branch,
		Worktree:  cwd,
		State:     state,
		UpdatedAt: now,
	}
}
