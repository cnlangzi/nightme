package gtw

// loadDispatchContext resolves the per-chat workspace for
// /gtw commit, /gtw push, and /gtw pr. It returns either a
// populated Context (with one of two fill strategies) or a
// ready-to-send *Result whose reply has already been emitted via
// cs.Emitter().
//
// Two fill strategies:
//
//  1. yml-present (worktree mode): `/gtw fix` (any mode) wrote
//     `.nightme/gtw.yml` into the worktree. ReadGTWYml parses
//     Worktree / Branch / RepoRoot / Repo / Provider / Issue / Mode
//     straight from the yml. This is the canonical path for
//     /gtw fix-created worktrees.
//
//  2. yml-absent (non-worktree mode): the user is on a plain
//     `git checkout -b <branch>` branch and never ran /gtw fix.
//     Derive Worktree from cs.SelectedCwd(), RepoRoot from
//     `git rev-parse --show-toplevel`, Branch from
//     `git rev-parse --abbrev-ref HEAD`. Issue = -1 (no remote
//     issue), Mode = ModeLocal, Repo = "" (provider resolution
//     will use the Detect fallback).
//
// Why chat-session CWD, not system pwd: nightme is a long-running
// daemon. The user's chat knows where they `cd`'d to via /cwd
// <path>; the daemon's process CWD is whatever it was when
// launched. Reading system pwd (the old `pushCwd()` shell-out)
// silently returns the daemon's startup dir, which is rarely
// what the user wants — the chat's SelectedCwd is the
// authoritative "where am I right now" signal.

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/cnlangzi/nightme/internal/chatsession"
)

// loadDispatchContext returns the Context for /gtw commit,
// /gtw push, or /gtw pr, or an already-sent *Result on
// early-return errors. The caller must check `res != nil`
// first; if non-nil, propagate the Result verbatim and don't
// touch the (zero-value) Context.
func loadDispatchContext(
	ctx context.Context,
	cs *chatsession.ChatSession,
	deps HandlerDeps,
	chatID, messageID string,
) (Context, *Result) {
	cwd := cs.SelectedCwd()
	if cwd == "" {
		return Context{}, reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ no active workspace. Send /cwd <path> first.")
	}

	c, err := ReadGTWYml(cwd)
	if err == nil {
		// yml-present: validate and return.
		if c.Worktree == "" || c.Branch == "" || c.RepoRoot == "" {
			return Context{}, reply(ctx, cs.Emitter(), chatID, messageID,
				"❌ .nightme/gtw.yml is malformed (worktree/branch/repoRoot required)")
		}
		return c, nil
	}
	if !os.IsNotExist(err) {
		return Context{}, reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ failed to read .nightme/gtw.yml: %v", err))
	}

	// yml-absent: derive from git. This branch lets /gtw commit,
	// /gtw push, and /gtw pr work on manually-created branches (no
	// /gtw fix pre-amble). The cwd must be inside a git repo;
	// we use --show-toplevel + rev-parse --abbrev-ref HEAD for
	// the two pieces of state we can't otherwise guess.
	repoRootOut, _, err := deps.Git.Run(ctx, cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return Context{}, reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ %s is not inside a git repository", cwd))
	}
	branchOut, _, err := deps.Git.Run(ctx, cwd, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return Context{}, reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ cannot determine current branch in %s: %v", cwd, err))
	}
	branch := strings.TrimSpace(branchOut)
	if branch == "" || branch == "HEAD" {
		// detached HEAD — refuse rather than guess a PR target.
		return Context{}, reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ %s is on a detached HEAD; checkout a named branch first", cwd))
	}
	return Context{
		Mode:     ModeLocal,
		Issue:    -1,
		Branch:   branch,
		Worktree: cwd,
		RepoRoot: strings.TrimSpace(repoRootOut),
	}, nil
}