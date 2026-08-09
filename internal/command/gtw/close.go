package gtw

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/cnlangzi/nightme/internal/chatsession"
)

// maxDirtyFilesReported caps how many uncommitted paths we
// surface in the "worktree is dirty" error reply. 10 is the
// sweet spot the user tested with: enough to identify the
// problem (top of the porcelain output is the most-recent
// changes), short enough that a Feishu reply doesn't get
// truncated by the channel's own message-size limits.
const maxDirtyFilesReported = 10

// RunClose is the entry point for `/gtw close`. Mirrors RunFix
// in shape (ctx, cs, slot, deps, chatID, messageID; returns
// *Result, error) so the factory can wire it through the same
// dispatcher pipeline.
//
// Flow (wip/gtw.md §14.5):
//
//  1. Look for `<cs.SelectedCwd()>/.nightme/gtw.yml` (no walk-up
//     by design — see §14.4 design rationale).
//  2. Refuse if missing — there's no active fix in this chat.
//  3. Refuse if the worktree has uncommitted changes (`git
//     status --porcelain` non-empty) — UNLESS force=true, in
//     which case the user is opting into destroying any
//     uncommitted local edits.
//  4. Run `git worktree remove [(--force)] <path>` from inside
//     the main repo (git refuses to run that command from
//     inside a worktree itself). The yml file goes away with
//     the worktree — no explicit unlink needed.
//  5. SetSelectedCwd back to repoRoot so the next agent message
//     spawns in the main repo.
//  6. Clear the in-memory Context.
//
// On any error before step 6 (yml missing / dirty / git failure)
// we leave the chat's active cwd and the in-memory Context
// untouched, so the user can retry once they fix the underlying
// problem. force=true changes step 3 (skipped) and step 4
// (passes --force to git).
func RunClose(
	ctx context.Context,
	cs *chatsession.ChatSession,
	slot ContextSlot,
	deps HandlerDeps,
	chatID, messageID string,
	force bool,
) (*Result, error) {
	selectedCwd := cs.SelectedCwd()

	// --- step 1+2: locate the snapshot ---------------------------
	c, err := ReadGTWYml(selectedCwd)
	if err != nil {
		if os.IsNotExist(err) {
			return reply(ctx, deps.Send, chatID, messageID,
				"❌ no active fix to close in this chat\n"+
					"hint: /cwd into the /gtw fix worktree first (its "+
					"`.nightme/gtw.yml` is the close source of truth)."), nil
		}
		return reply(ctx, deps.Send, chatID, messageID,
			fmt.Sprintf("❌ failed to read .nightme/gtw.yml: %v", err)), nil
	}

	if c.Worktree == "" {
		return reply(ctx, deps.Send, chatID, messageID,
			"❌ .nightme/gtw.yml is malformed: worktree is empty"), nil
	}
	if c.RepoRoot == "" {
		return reply(ctx, deps.Send, chatID, messageID,
			"❌ .nightme/gtw.yml is malformed: repoRoot is empty"), nil
	}

	// --- step 3: dirty check --------------------------------------
	// force=true bypasses — the user explicitly accepted the
	// risk of losing uncommitted local edits.
	if !force {
		if err := assertWorktreeClean(ctx, c.Worktree, deps); err != nil {
			return reply(ctx, deps.Send, chatID, messageID, err.Error()), nil
		}
	}

	// --- step 4: git worktree remove ------------------------------
	if err := WorktreeRemove(ctx, c.RepoRoot, c.Worktree, force, deps.Git); err != nil {
		// Don't roll back — the yml still reflects the (still-
		// attached) fix. User can retry once they understand the
		// git error.
		stderr := ""
		var we *WorktreeError
		if errors.As(err, &we) {
			stderr = tailLines(we.Stderr, 10)
		}
		body := fmt.Sprintf("❌ git worktree remove failed: %v", err)
		if stderr != "" {
			body += "\n[git stderr tail]\n" + stderr
		}
		return reply(ctx, deps.Send, chatID, messageID, body), nil
	}

	// --- step 5: switch CWD back to repoRoot ----------------------
	if err := cs.SetSelectedCwd(c.RepoRoot); err != nil {
		// The worktree IS removed at this point. Failing to
		// switch CWD is awkward (user is stranded in a path
		// that no longer exists) but we surface the error so
		// they can run `/cwd <repoRoot>` manually.
		slog.Default().Warn("gtw: SetSelectedCwd back to repoRoot failed",
			"repo_root", c.RepoRoot,
			"err", err)
		return reply(ctx, deps.Send, chatID, messageID,
			fmt.Sprintf("⚠️ worktree removed but SetSelectedCwd(%s) failed: %v\n"+
				"run `/cwd %s` manually.", c.RepoRoot, err, c.RepoRoot)), nil
	}

	// --- step 6: clear in-memory state ----------------------------
	slot.Store(Context{})

	// --- step 7: success reply ------------------------------------
	body := fmt.Sprintf(
		"✅ closed /gtw fix on branch %q\n"+
			"━━━━━━━━━━━━━━\n"+
			"[Torn down]\n"+
			"📁 worktree: %s (removed%s)\n"+
			"📄 .nightme/gtw.yml (removed with worktree)\n"+
			"━━━━━━━━━━━━━━\n"+
			"[Context]\n"+
			"📂 cwd:      %s\n"+
			"🌿 branch:   %s\n"+
			"━━━━━━━━━━━━━━\n"+
			"[Command result]\n"+
			"💡 next: continue working in %s, or run another /gtw fix",
		c.Branch, c.Worktree, forceNote(force), c.RepoRoot, c.Branch, c.RepoRoot,
	)
	return reply(ctx, deps.Send, chatID, messageID, body), nil
}

// forceNote renders the trailing "force" annotation for the
// success reply. Empty string for normal closes; a short
// warning when the user opted into destroying uncommitted
// edits.
func forceNote(force bool) string {
	if !force {
		return ""
	}
	return "; any uncommitted edits were force-discarded"
}

// assertWorktreeClean returns a user-friendly error if `dir` has
// any porcelain-visible changes (modified / added / deleted /
// renamed / copied / unmerged conflicts / untracked). Returns
// nil if the worktree is clean.
//
// Implemented as `git -C <dir> status --porcelain` — same
// porcelain format GitStatusSnapshot uses elsewhere in gtw, so
// the semantics are consistent across the package.
func assertWorktreeClean(ctx context.Context, dir string, deps HandlerDeps) error {
	stdout, _, err := deps.Git.Run(ctx, dir, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("❌ git status in %s failed: %w", dir, err)
	}
	if stdout == "" {
		return nil
	}

	// Split + filter in one pass. strings.Split tolerates a
	// trailing \n; we drop empties because git occasionally
	// pads with blank lines (e.g. after a section header in
	// some configs — defensive, even though --porcelain
	// shouldn't produce any).
	lines := make([]string, 0, 4)
	for _, line := range strings.Split(stdout, "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}

	preview := lines
	if len(preview) > maxDirtyFilesReported {
		preview = preview[:maxDirtyFilesReported]
	}
	body := fmt.Sprintf(
		"❌ worktree has uncommitted changes — commit or stash before closing:\n"+
			"  %s",
		filepath.Join(dir, "(worktree)"),
	)
	for _, line := range preview {
		body += "\n  " + line
	}
	if len(lines) > maxDirtyFilesReported {
		body += fmt.Sprintf("\n  ... and %d more", len(lines)-maxDirtyFilesReported)
	}
	body += "\nhint: this command does not support `--force`; clean the worktree first."
	return errors.New(body)
}