package gtw

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/cnlangzi/nightme/internal/chatsession"
)

// RunBack is the entry point for `/gtw back`. The non-destructive
// counterpart to RunClose: switch CWD back to the main repo
// without removing the worktree, the branch, or the .nightme/
// gtw.yml — and then run `gtw sync` on the main repo so the next
// /gtw fix has a clean baseline.
//
// Flow (mirrors RunClose, drops the destructive + reset steps):
//
//  1. Read `<cs.SelectedCwd()>/.nightme/gtw.yml` (same source
//     of truth RunClose uses — no walk-up by design, see
//     wip/gtw.md §14.4).
//  2. Refuse if missing — there's no active fix to back out of.
//  3. SetSelectedCwd back to repoRoot (RunClose's step 6).
//     The worktree, the branch, and the yml stay on disk — user
//     can /cwd back into the worktree to resume, or run
//     /gtw fix <other-id> from the main repo.
//  4. Emit a one-line "back" card naming the repoRoot we landed
//     on (NOT a close-style teardown card — the worktree was not
//     touched). The worktree still exists; just the chat's
//     SelectedCwd has moved.
//  5. Run buildSyncReply on repoRoot and emit its card. Errors
//     surface with ❌ prefix; sync is the LAST step so the user
//     reads "back to root" first, then "synced" / "sync failed".
//
// Skipped vs RunClose (each with the why):
//
//   - Step 2.5 EvictAgentSessionsInCwd(c.Worktree): the worktree
//     is NOT being removed, so any ASes pinned to it must stay
//     alive. The user may `/cwd` back into the worktree to
//     continue.
//   - Step 3 assertWorktreeClean: back is a temporary cwd swap,
//     not a teardown. A dirty worktree is fine — the user's
//     uncommitted work is exactly the point of being able to
//     come back. (RunClose is hard-refusal because removing a
//     dirty worktree is destructive; back never removes
//     anything.)
//   - Step 4 WorktreeRemove / Step 5 branch -D: the whole
//     non-destructive premise.
//   - Step 6.5 PR cache clear: the branch is still alive;
//     caching is still valid.
//   - Step 7 slot.Store(Context{}): the fix is still in flight
//     per the yml; reaction handlers for the same worktree
//     should keep routing. (Clearing would silently break
//     reactions if the user /cwd's back into the worktree.)
//   - Step 9 /new: back is reversible — the user might resume
//     the fix in minutes. Forcing a context reset would be
//     hostile. RunClose does /new because its whole purpose is
//     "tear down the experiment, start fresh".
func RunBack(
	ctx context.Context,
	cs *chatsession.ChatSession,
	deps HandlerDeps,
	chatID, messageID string,
) (*Result, error) {
	selectedCwd := cs.SelectedCwd()

	// --- step 1+2: locate the snapshot ---------------------------
	c, err := ReadGTWYml(selectedCwd)
	if err != nil {
		if os.IsNotExist(err) {
			return reply(ctx, cs.Emitter(), chatID, messageID,
				"❌ no active fix to back out of in this chat\n"+
					"hint: /cwd into the /gtw fix worktree first (its "+
					"`.nightme/gtw.yml` is the back source of truth)."), nil
		}
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ failed to read .nightme/gtw.yml: %v", err)), nil
	}

	if c.RepoRoot == "" {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ .nightme/gtw.yml is malformed: repoRoot is empty"), nil
	}
	// Worktree may legitimately be empty for a freshly-started
	// fix that hasn't reached §5.2.④ yet — back is still safe
	// (we just SetSelectedCwd back). We surface c.Worktree in
	// the card only when set so a half-formed yml doesn't show
	// a misleading "(empty)" path.

	// --- step 3: switch CWD back to repoRoot ---------------------
	// Same pattern as RunClose's step 6, but with a different
	// failure-mode story: nothing on disk has changed, so the
	// worst case is "the chat is still pointing at the
	// worktree" — much less urgent than the close-path
	// equivalent where the worktree IS gone.
	if err := cs.SetSelectedCwd(c.RepoRoot); err != nil {
		slog.Default().Warn("gtw: SetSelectedCwd back to repoRoot failed",
			"repo_root", c.RepoRoot,
			"err", err)
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("⚠️ SetSelectedCwd(%s) failed: %v\n"+
				"worktree at %s is unchanged; run `/cwd %s` manually.",
				c.RepoRoot, err, c.Worktree, c.RepoRoot)), nil
	}

	// --- step 4: back's own success card ------------------------
	// Single-line card that mirrors the close-card format (✅
	// headline + `→` rows) but says "back to root" instead of
	// "closed X". Listing c.Worktree as "preserved" makes the
	// non-destructive intent obvious — the user shouldn't have
	// to wonder whether `back` is a synonym for `close`.
	var body string
	if c.Worktree != "" {
		body = fmt.Sprintf(
			"✅ back to `%s`\n"+
				"→ worktree: %s (preserved)\n"+
				"→ branch: %s (preserved)\n"+
				"→ .nightme/gtw.yml (preserved — `/cwd %s` to resume)",
			c.RepoRoot, c.Worktree, c.Branch, c.Worktree)
	} else {
		body = fmt.Sprintf(
			"✅ back to `%s`\n"+
				"→ worktree: (not yet recorded — fix didn't reach §5.2.④)\n"+
				"→ .nightme/gtw.yml (preserved)",
			c.RepoRoot)
	}
	// Mid-flow by design — the sync card that follows in
	// step 5 is a separate reply, matching the close-path
	// two-card shape (close + sync, back + sync).
	reply(ctx, cs.Emitter(), chatID, messageID, body)

	// --- step 5: sync main (separate card) ----------------------
	// buildSyncReply is the same helper runSync + runClose use;
	// sharing the formatter keeps the "synced" / "already up to
	// date" / "❌ sync failed" surface identical across the three
	// commands. Respect deps.SkipRefreshDefaultBranch as a
	// short-circuit (test-only).
	//
	// Single terminal step: all three branches (error / normal /
	// skip) fall through to the same return.
	syncBody, syncErr := buildSyncReply(ctx, c.RepoRoot, deps)
	if syncErr != nil {
		// Same pattern as RunClose's step 10: stamp a ❌ prefix
		// unconditionally so the user can tell the sync failure
		// apart from the back-success card above. RefreshDefaultBranch
		// returns plain fmt.Errorf values whose messages already
		// include git stderr tails + user-facing hints — the prefix
		// is the only wrapping RunClose / RunBack do.
		reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ sync failed: "+syncErr.Error())
	} else if syncBody != "" {
		reply(ctx, cs.Emitter(), chatID, messageID, syncBody)
	}
	// else: SkipRefreshDefaultBranch set (test-only); no sync
	// card. The back-success card above still stands.

	return &Result{Consumed: true}, nil
}
