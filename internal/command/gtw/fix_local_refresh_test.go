package gtw

// The local mode of /gtw fix — `/gtw fix --name <branch>` —
// refreshes the default branch before it creates the worktree, the
// same way the remote mode does. These tests pin that contract:
// the refresh runs first, a failed refresh ends the command without
// creating anything, and the success card names the base the
// worktree branched off.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/messages"
)

// localFixGit is a scripted GitRunner: it answers the git calls the
// local /gtw fix flow makes and records every argv, so a test can
// assert the order the calls ran in. The refresh has to happen
// before `git worktree add` — otherwise the worktree branches off
// whatever the primary checkout's HEAD happened to be.
type localFixGit struct {
	mu    sync.Mutex
	calls [][]string

	// repoRoot is what `git rev-parse --show-toplevel` prints.
	repoRoot string
	// head is what `git rev-parse HEAD` prints after the pull.
	head string
	// noOrigin makes the origin lookup fail — the "no origin
	// remote" refusal.
	noOrigin bool
	// linkedWorktree makes the primary-checkout check fail — the
	// "is a linked worktree" refusal.
	linkedWorktree bool
	// dirtyStatus is what `git status --porcelain` prints in the
	// primary checkout. Non-empty makes the refresh refuse.
	dirtyStatus string
}

// Run answers the git calls the local fix flow makes and records the
// argv. Anything it does not recognise succeeds silently — the flow
// under test does not depend on it.
func (g *localFixGit) Run(_ context.Context, _ string, args ...string) (string, string, error) {
	g.mu.Lock()
	g.calls = append(g.calls, append([]string(nil), args...))
	g.mu.Unlock()

	switch {
	case argsAre(args, "rev-parse", "--show-toplevel"):
		return g.repoRoot, "", nil
	case len(args) > 0 && args[0] == "show-ref":
		// The branch does not exist yet: a miss is a non-zero exit
		// with an empty stderr (see BranchExists).
		return "", "", errors.New("exit status 1")
	case argsAre(args, "worktree", "list", "--porcelain"):
		return "", "", nil
	case argsAre(args, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"):
		if g.noOrigin {
			return "", "", errors.New("exit status 1")
		}
		return "origin/main", "", nil
	case argsAre(args, "remote", "get-url", "origin"):
		if g.noOrigin {
			return "", "", errors.New("exit status 1")
		}
		return "https://example.com/owner/repo.git", "", nil
	case argsAre(args, "rev-parse", "--git-dir"):
		if g.linkedWorktree {
			return ".git/worktrees/other", "", nil
		}
		return ".git", "", nil
	case argsAre(args, "rev-parse", "--git-common-dir"):
		return ".git", "", nil
	case argsAre(args, "rev-parse", "HEAD"):
		return g.head, "", nil
	case argsAre(args, "checkout", "main"):
		return "", "", nil
	case argsAre(args, "pull", "--rebase", "origin", "main"):
		return "", "", nil
	case len(args) > 1 && gitSubcommand(args) == "status":
		return g.dirtyStatus, "", nil
	case len(args) > 1 && args[0] == "worktree" && args[1] == "add":
		return "", "", nil
	}
	return "", "", nil
}

// callIndex returns the position of the first recorded call whose
// argv starts with prefix, or -1 when there is none.
func (g *localFixGit) callIndex(prefix ...string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, c := range g.calls {
		if len(c) < len(prefix) {
			continue
		}
		match := true
		for j, p := range prefix {
			if c[j] != p {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// argsAre reports whether argv equals want exactly.
func argsAre(args []string, want ...string) bool {
	if len(args) != len(want) {
		return false
	}
	for i := range want {
		if args[i] != want[i] {
			return false
		}
	}
	return true
}

// newLocalFixProbe builds a chat session pointed at repoRoot, with
// the git double the local fix flow talks to.
func newLocalFixProbe(t *testing.T, repoRoot string, git *localFixGit) (*outcomeProbe, HandlerDeps) {
	t.Helper()
	p := newOutcomeProbe(t)
	if err := p.cs.SetSelectedCwd(repoRoot); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	return p, HandlerDeps{
		Git: git,
		Now: func() time.Time { return time.Date(2026, 8, 8, 14, 0, 0, 0, time.UTC) },
	}
}

// TestFixLocal_RefreshRunsBeforeWorktreeAdd pins the ordering the
// local mode owes the remote mode: the default branch is synced
// first, and only then does the worktree branch off it. A local
// worktree created from a stale HEAD is the bug this guards.
func TestFixLocal_RefreshRunsBeforeWorktreeAdd(t *testing.T) {
	repoRoot := t.TempDir()
	head := strings.Repeat("a1b2c3d4e5f6", 3) + "abcd" // 40 chars
	git := &localFixGit{repoRoot: repoRoot, head: head}
	p, deps := newLocalFixProbe(t, repoRoot, git)

	if _, err := RunFix(p.ctx, ModeLocal, p.cs, deps, p.cs.ChatID, "msg-1",
		[]string{"login-bug"}, false, false); err != nil {
		t.Fatalf("RunFix: %v", err)
	}

	checkout := git.callIndex("checkout", "main")
	pull := git.callIndex("pull", "--rebase", "origin", "main")
	add := git.callIndex("worktree", "add")
	if checkout < 0 || pull < 0 || add < 0 {
		t.Fatalf("missing git calls: checkout=%d pull=%d worktree add=%d; calls=%v",
			checkout, pull, add, git.calls)
	}
	if !(checkout < pull && pull < add) {
		t.Errorf("git calls out of order: checkout=%d pull=%d worktree add=%d; want checkout < pull < worktree add; calls=%v",
			checkout, pull, add, git.calls)
	}

	// The worktree is still created from HEAD — the refresh left the
	// primary checkout ON the default branch, so HEAD is the
	// refreshed base.
	addArgs := git.calls[add]
	if addArgs[len(addArgs)-1] != "HEAD" {
		t.Errorf("worktree add argv = %v, want it to end with HEAD", addArgs)
	}

	// The card names the base the worktree branched off.
	card := p.ch.lastText()
	if want := "→ base:     " + shortSHA(head); !strings.Contains(card, want) {
		t.Errorf("card missing base line %q:\n%s", want, card)
	}
}

// TestFixLocal_RefreshRefusalEndsCommand covers every refusal the
// refresh makes: the local mode surfaces the refresh's own message
// (so the remedy is obvious), reports a failure, and creates
// nothing — no worktree, no cwd move, no yml.
func TestFixLocal_RefreshRefusalEndsCommand(t *testing.T) {
	cases := []struct {
		name string
		// setup arranges the precondition the refresh refuses on.
		setup func(t *testing.T, g *localFixGit)
		// wantMsg is a fragment of the refresh's message the reply
		// has to carry unchanged.
		wantMsg func(repoRoot string) string
	}{
		{
			name: "no origin remote",
			setup: func(_ *testing.T, g *localFixGit) {
				g.noOrigin = true
			},
			wantMsg: func(string) string {
				return "could not determine default branch: no origin remote configured " +
					"(run `git remote add origin <url>` then `git fetch origin`)"
			},
		},
		{
			name: "cwd inside a linked worktree",
			setup: func(_ *testing.T, g *localFixGit) {
				g.linkedWorktree = true
			},
			wantMsg: func(repoRoot string) string {
				return fmt.Sprintf("❌ %s is a linked worktree; default-branch refresh must run "+
					"from the primary checkout. /cwd into the main repo first", repoRoot)
			},
		},
		{
			name: "unfinished rebase",
			setup: func(t *testing.T, g *localFixGit) {
				if err := os.MkdirAll(filepath.Join(g.repoRoot, ".git", "rebase-merge"), 0o755); err != nil {
					t.Fatalf("mkdir rebase-merge: %v", err)
				}
			},
			wantMsg: func(repoRoot string) string {
				return fmt.Sprintf("❌ %s is in the middle of a rebase (rebase-merge) — "+
					"run `git rebase --abort` to clear it before refreshing", repoRoot)
			},
		},
		{
			name: "uncommitted changes in the primary checkout",
			setup: func(_ *testing.T, g *localFixGit) {
				g.dirtyStatus = " M README.md"
			},
			wantMsg: func(repoRoot string) string {
				return fmt.Sprintf("❌ main repo %s has uncommitted changes — commit or stash first:\n M README.md", repoRoot)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repoRoot := t.TempDir()
			git := &localFixGit{repoRoot: repoRoot, head: strings.Repeat("a", 40)}
			tc.setup(t, git)
			p, deps := newLocalFixProbe(t, repoRoot, git)

			if _, err := RunFix(p.ctx, ModeLocal, p.cs, deps, p.cs.ChatID, "msg-1",
				[]string{"login-bug"}, false, false); err != nil {
				t.Fatalf("RunFix: %v", err)
			}

			reply := p.ch.lastText()
			if want := tc.wantMsg(repoRoot); !strings.Contains(reply, want) {
				t.Errorf("reply does not carry the refresh's message %q:\n%s", want, reply)
			}

			// The outcome is a failure.
			got := p.settle(t)
			assertOutcome(t, got, messages.OutcomeFailed, map[string]string{"reason": "error"})

			// No worktree was created.
			if i := git.callIndex("worktree", "add"); i >= 0 {
				t.Errorf("git worktree add ran (call %d) despite the failed refresh; calls=%v", i, git.calls)
			}
			wt := WorktreePath(repoRoot, "login-bug")
			if _, err := os.Stat(wt); !os.IsNotExist(err) {
				t.Errorf("worktree %s exists despite the failed refresh: %v", wt, err)
			}

			// The chat's cwd did not move and no yml was written.
			if got := p.cs.SelectedCwd(); got != repoRoot {
				t.Errorf("SelectedCwd = %q, want %q (unchanged)", got, repoRoot)
			}
			if _, err := os.Stat(filepath.Join(wt, ".nightme", "gtw.yml")); !os.IsNotExist(err) {
				t.Errorf("gtw.yml exists despite the failed refresh: %v", err)
			}
		})
	}
}

// TestFixLocal_SkipRefreshDefaultBranch pins the test seam: with the
// flag set, the local flow performs no refresh at all and the card
// carries no base line.
func TestFixLocal_SkipRefreshDefaultBranch(t *testing.T) {
	repoRoot := t.TempDir()
	git := &localFixGit{repoRoot: repoRoot, head: strings.Repeat("a", 40)}
	p, deps := newLocalFixProbe(t, repoRoot, git)
	deps.SkipRefreshDefaultBranch = true

	if _, err := RunFix(p.ctx, ModeLocal, p.cs, deps, p.cs.ChatID, "msg-1",
		[]string{"login-bug"}, false, false); err != nil {
		t.Fatalf("RunFix: %v", err)
	}

	if i := git.callIndex("checkout", "main"); i >= 0 {
		t.Errorf("git checkout ran (call %d) with SkipRefreshDefaultBranch set; calls=%v", i, git.calls)
	}
	if i := git.callIndex("pull", "--rebase", "origin", "main"); i >= 0 {
		t.Errorf("git pull ran (call %d) with SkipRefreshDefaultBranch set; calls=%v", i, git.calls)
	}
	if i := git.callIndex("worktree", "add"); i < 0 {
		t.Errorf("git worktree add never ran; calls=%v", git.calls)
	}
	card := p.ch.lastText()
	if strings.Contains(card, "→ base:") {
		t.Errorf("card must not carry a base line when the refresh was skipped:\n%s", card)
	}
}

// TestFixLocal_RefreshBranchesOffUpstreamTip drives the local fix
// against a real git repo whose origin has moved ahead. The worktree
// must branch off the upstream tip — a worktree created from the
// stale local main is the bug this guards.
func TestFixLocal_RefreshBranchesOffUpstreamTip(t *testing.T) {
	repoRoot := initTempRepo(t)
	bare := addLocalRemote(t, repoRoot)

	// Upstream moves ahead of the local main.
	pushWork := t.TempDir()
	mustGit(t, pushWork, "clone", "-q", bare, pushWork)
	mustGit(t, pushWork, "config", "user.email", "upstream@example.com")
	mustGit(t, pushWork, "config", "user.name", "Upstream")
	if err := writeFile(filepath.Join(pushWork, "new.txt"), "upstream\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustGit(t, pushWork, "add", "new.txt")
	mustGit(t, pushWork, "commit", "-q", "-m", "upstream commit")
	mustGit(t, pushWork, "push", "-q", "origin", "main")

	staleHead, _, err := runGit(t, repoRoot, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD before: %v", err)
	}
	upstreamHead, _, err := runGit(t, bare, "rev-parse", "main")
	if err != nil {
		t.Fatalf("rev-parse upstream main: %v", err)
	}
	if staleHead == upstreamHead {
		t.Fatal("precondition: local HEAD should differ from upstream before the fix")
	}

	p, deps := newLocalFixProbe(t, repoRoot, &localFixGit{repoRoot: repoRoot})
	deps.Git = ExecGitRunner{}

	if _, err := RunFix(p.ctx, ModeLocal, p.cs, deps, p.cs.ChatID, "msg-1",
		[]string{"login-bug"}, false, false); err != nil {
		t.Fatalf("RunFix: %v", err)
	}

	// The card names the refreshed base.
	card := p.ch.lastText()
	if want := "→ base:     " + shortSHA(upstreamHead); !strings.Contains(card, want) {
		t.Errorf("card missing base line %q:\n%s", want, card)
	}

	// The primary checkout now sits on the synced default branch.
	headAfter, _, err := runGit(t, repoRoot, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD after: %v", err)
	}
	if headAfter != upstreamHead {
		t.Errorf("primary checkout HEAD = %s, want the upstream tip %s", headAfter, upstreamHead)
	}

	// The worktree carries the upstream commit — it branched off the
	// refreshed base, not off the stale local main.
	wt := WorktreePath(repoRoot, "login-bug")
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree %s not created: %v", wt, err)
	}
	if _, _, err := runGit(t, wt, "merge-base", "--is-ancestor", upstreamHead, "HEAD"); err != nil {
		t.Errorf("worktree HEAD does not descend from the upstream tip %s: %v", upstreamHead, err)
	}
}
