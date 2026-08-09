package gtw

// Integration tests that exercise the real `git` binary against
// a throwaway repo in t.TempDir(). These complement the unit
// tests in close_test.go (which use a programmableGit fake) by
// catching path-passing / arg-ordering bugs the fake can't see.
//
// Skip when git isn't on PATH (rare in practice — the unit tests
// already cover the bulk of logic).

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/chatsession"
)



// TestIntegration_FixCloseRoundTrip is the most important
// integration test: it drives the full /gtw fix → /gtw close
// happy path against a real git repo. Steps:
//
//  1. git init a temp repo with one initial commit
//  2. Pretend /gtw fix ran: create a worktree via the real
//     `git worktree add`, write .nightme/gtw.yml via
//     WriteGTWYml, store Context in a memSlot
//  3. Verify yml is on disk and parses back to the same Context
//  4. Run RunClose (which calls the REAL `git status` and
//     `git worktree remove`) and verify:
//     - worktree directory is gone
//     - ActiveCwd is back at repoRoot
//     - slot is cleared
//     - reply text mentions branch + repoRoot
func TestIntegration_FixCloseRoundTrip(t *testing.T) {
	repoRoot := initTempRepo(t)
	wt := filepath.Join(filepath.Dir(repoRoot), filepath.Base(repoRoot)+".wt-test")
	branch := "fix/42-roundtrip"

	// --- step 2: simulate /gtw fix ---
	mustGit(t, repoRoot, "worktree", "add", "-b", branch, wt, "HEAD")

	// EnsureGitignore + CommitGitignore + WriteGTWYml — exactly
	// what completeFixAndDispatch does after SetActiveCwd. The
	// commit step matters: without it, .gitignore stays untracked
	// and `git worktree remove` (which RunClose uses) refuses
	// with "contains modified or untracked files".
	if err := EnsureGitignore(wt); err != nil {
		t.Fatalf("EnsureGitignore: %v", err)
	}
	if err := CommitGitignoreIfDirty(context.Background(), wt, ExecGitRunner{}); err != nil {
		t.Fatalf("CommitGitignoreIfDirty: %v", err)
	}

	now := time.Date(2026, 8, 8, 14, 0, 0, 0, time.UTC)
	if err := WriteGTWYml(wt, Context{
		Mode:     ModeLocal,
		Issue:    -1,
		Branch:   branch,
		Worktree: wt,
		RepoRoot: repoRoot,
		State:    StateFixing,
	}, func() time.Time { return now }); err != nil {
		t.Fatalf("WriteGTWYml: %v", err)
	}

	// Sanity check: after the commit, the worktree should be
	// git-clean (no porcelain entries) so `git worktree remove`
	// won't refuse later.
	if out, _ := mustGitOut(t, wt, "status", "--porcelain"); out != "" {
		t.Fatalf("worktree dirty after /gtw fix simulation:\n%s", out)
	}

	// --- step 3: yml round-trip sanity --------------------------
	got, err := ReadGTWYml(wt)
	if err != nil {
		t.Fatalf("ReadGTWYml: %v", err)
	}
	if got.Branch != branch || got.Worktree != wt || got.RepoRoot != repoRoot {
		t.Fatalf("ReadGTWYml = %+v, want branch=%s wt=%s repoRoot=%s",
			got, branch, wt, repoRoot)
	}

	// --- step 4: actually run /gtw close -------------------------
	ch := &recordingCh{}
	cs, _ := chatsession.New("chat-int-1", "test-agent", ch)
	if err := cs.SetActiveCwd(wt); err != nil {
		t.Fatalf("SetActiveCwd wt: %v", err)
	}
	slot := &memSlot{Context{
		Mode: ModeLocal, Issue: -1, Branch: branch,
		Worktree: wt, RepoRoot: repoRoot, State: StateFixing,
		UpdatedAt: now,
	}}
	deps := HandlerDeps{
		Git: ExecGitRunner{},
		Now: func() time.Time { return now },
	}

	res, err := RunClose(context.Background(), cs, slot, deps, cs.ChatID, "msg-int-1", false /* force */)
	if err != nil {
		t.Fatalf("RunClose: %v", err)
	}
	if res == nil || !res.Consumed {
		t.Fatalf("Result = %+v, want Consumed=true", res)
	}

	// Worktree directory must be gone.
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("worktree still on disk after close: stat err=%v", err)
	}
	// yml (which lived in the worktree) must be gone too.
	if _, err := os.Stat(filepath.Join(wt, ".nightme", "gtw.yml")); !os.IsNotExist(err) {
		t.Errorf("yml still on disk after close: stat err=%v", err)
	}
	// CWD must be back at repoRoot.
	if got := cs.ActiveCwd(); got != repoRoot {
		t.Errorf("ActiveCwd = %q, want %q", got, repoRoot)
	}
	// In-memory slot must be cleared.
	if slot.Load() != (Context{}) {
		t.Errorf("slot.Load() = %+v, want zero", slot.Load())
	}
	// Reply must mention both branch and the new cwd.
	last := ch.lastText()
	if !strings.Contains(last, branch) {
		t.Errorf("reply missing branch %q:\n%s", branch, last)
	}
	if !strings.Contains(last, repoRoot) {
		t.Errorf("reply missing repoRoot %q:\n%s", repoRoot, last)
	}
}

// TestIntegration_CloseRejectsDirty verifies that a real
// uncommitted change inside the worktree causes RunClose to
// fail without touching the worktree on disk.
func TestIntegration_CloseRejectsDirty(t *testing.T) {
	repoRoot := initTempRepo(t)
	wt := filepath.Join(filepath.Dir(repoRoot), filepath.Base(repoRoot)+".wt-dirty")
	branch := "fix/dirty"

	mustGit(t, repoRoot, "worktree", "add", "-b", branch, wt, "HEAD")

	// Make the worktree dirty.
	if err := os.WriteFile(filepath.Join(wt, "sentinel.txt"), []byte("dirty"), 0o644); err != nil {
		t.Fatalf("write dirty sentinel: %v", err)
	}

	if err := WriteGTWYml(wt, Context{
		Mode:     ModeLocal,
		Issue:    -1,
		Branch:   branch,
		Worktree: wt,
		RepoRoot: repoRoot,
		State:    StateFixing,
	}, func() time.Time { return time.Now() }); err != nil {
		t.Fatalf("WriteGTWYml: %v", err)
	}

	ch := &recordingCh{}
	cs, _ := chatsession.New("chat-int-2", "test-agent", ch)
	_ = cs.SetActiveCwd(wt)
	slot := &memSlot{Context{Mode: ModeLocal, Branch: branch, Worktree: wt, RepoRoot: repoRoot, State: StateFixing}}
	deps := HandlerDeps{
		Git: ExecGitRunner{},
		Now: time.Now,
	}

	if _, err := RunClose(context.Background(), cs, slot, deps, cs.ChatID, "msg", false /* force */); err != nil {
		t.Fatalf("RunClose: %v", err)
	}

	// Worktree must still be on disk.
	if _, err := os.Stat(wt); err != nil {
		t.Errorf("worktree removed despite dirty: stat err=%v", err)
	}
	// Sentinel must still be on disk.
	if _, err := os.Stat(filepath.Join(wt, "sentinel.txt")); err != nil {
		t.Errorf("dirty sentinel removed: %v", err)
	}
	// Reply must list the dirty sentinel and not contain "closed".
	last := ch.lastText()
	if !strings.Contains(last, "sentinel.txt") {
		t.Errorf("reply missing dirty file:\n%s", last)
	}
	if strings.Contains(last, "closed /gtw fix") {
		t.Errorf("reply looks like success but should be dirty rejection:\n%s", last)
	}
}

// TestIntegration_EnsureGitignoreOnRealWorktree verifies that
// EnsureGitignore + CommitGitignoreIfDirty keep an existing
// .gitignore that the user authored intact, commit the change,
// and end up with a clean worktree (so later `git worktree
// remove` won't refuse).
func TestIntegration_EnsureGitignoreOnRealWorktree(t *testing.T) {
	repoRoot := initTempRepo(t)
	wt := filepath.Join(filepath.Dir(repoRoot), filepath.Base(repoRoot)+".wt-gin")
	mustGit(t, repoRoot, "worktree", "add", "-b", "fix/gin", wt, "HEAD")

	// Seed a user-authored .gitignore (and commit it so we have
	// something to compare against in the diff later).
	userGI := "*.log\nbuild/\n# a comment\n"
	if err := os.WriteFile(filepath.Join(wt, ".gitignore"), []byte(userGI), 0o644); err != nil {
		t.Fatalf("seed .gitignore: %v", err)
	}
	mustGit(t, wt, "add", ".gitignore")
	mustGit(t, wt, "commit", "-q", "-m", "user: initial gitignore")

	if err := EnsureGitignore(wt); err != nil {
		t.Fatalf("EnsureGitignore: %v", err)
	}
	if err := CommitGitignoreIfDirty(context.Background(), wt, ExecGitRunner{}); err != nil {
		t.Fatalf("CommitGitignoreIfDirty: %v", err)
	}

	// File on disk must end with the user's content + our entry.
	got, err := os.ReadFile(filepath.Join(wt, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	want := userGI + ".nightme/\n"
	if string(got) != want {
		t.Fatalf(".gitignore after EnsureGitignore:\n--- got ---\n%s--- want ---\n%s",
			got, want)
	}

	// Worktree must be clean (no porcelain output).
	if out, _ := mustGitOut(t, wt, "status", "--porcelain"); out != "" {
		t.Fatalf("worktree dirty after EnsureGitignore+Commit:\n%s", out)
	}

	// The commit log on the branch must include exactly one
	// gtw-tagged commit (the one we just made).
	logOut, _ := mustGitOut(t, wt, "log", "--oneline")
	if !strings.Contains(logOut, "gtw: ignore .nightme/") {
		t.Fatalf("branch log missing gtw commit:\n%s", logOut)
	}

	// Idempotency: a second Ensure+Commit cycle must NOT make a
	// second gtw commit (or any commit at all).
	if err := EnsureGitignore(wt); err != nil {
		t.Fatalf("second EnsureGitignore: %v", err)
	}
	if err := CommitGitignoreIfDirty(context.Background(), wt, ExecGitRunner{}); err != nil {
		t.Fatalf("second CommitGitignoreIfDirty: %v", err)
	}
	logOut2, _ := mustGitOut(t, wt, "log", "--oneline", "HEAD..HEAD")
	// `git log HEAD..HEAD` is always empty — what we really
	// want is the count of "gtw: ignore" lines in the log.
	fullLog, _ := mustGitOut(t, wt, "log", "--oneline")
	if c := strings.Count(fullLog, "gtw: ignore"); c != 1 {
		t.Fatalf("expected exactly 1 gtw commit, got %d:\n%s", c, fullLog)
	}
	_ = logOut2 // silence unused
}

// --- helpers ---

// initTempRepo creates a throwaway git repo in t.TempDir() with
// one initial commit so `git worktree add` has a valid base.
// Returns the repo root (absolute) path.
func initTempRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; skipping real-git integration test")
	}

	dir := t.TempDir()
	mustGit(t, dir, "init", "-q", "-b", "main")
	mustGit(t, dir, "config", "user.email", "test@example.com")
	mustGit(t, dir, "config", "user.name", "Test")
	// Disable GPG signing for the test repo: gpg would
	// otherwise block on a passphrase prompt in CI/sandboxed
	// environments, hanging the test.
	mustGit(t, dir, "config", "commit.gpgsign", "false")
	mustGit(t, dir, "config", "tag.gpgsign", "false")
	// No credential helper — git fetch against a fake remote
	// would otherwise block waiting for credentials.
	mustGit(t, dir, "config", "credential.helper", "")

	// Seed one commit so the worktree has something to branch off.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	mustGit(t, dir, "add", "README.md")
	mustGit(t, dir, "commit", "-q", "-m", "init")
	return dir
}

// addLocalRemote points repoRoot's `origin` at a fresh bare
// repo inside a t.TempDir, pushes main, and pins
// refs/remotes/origin/HEAD to refs/remotes/origin/main.
// All offline — no network involved. Returns the bare repo
// path so tests can manipulate it (push additional commits
// upstream, etc).
//
// Assumes repoRoot has NO origin remote yet — adds one. Tests
// that pre-add a github.com-style origin URL (so ParseRepoOwner
// works) should call setOriginBare instead, which uses
// `git remote set-url` to swap an existing remote.
//
// RefreshDefaultBranch reads refs/remotes/origin/HEAD to
// discover the default branch; without this set, the helper
// errors with "no origin remote" before any network happens.
func addLocalRemote(t *testing.T, repoRoot string) string {
	t.Helper()
	bare := initBareRepo(t)
	mustGit(t, repoRoot, "remote", "add", "origin", bare)
	mustGit(t, repoRoot, "push", "-q", "origin", "main")
	mustGit(t, repoRoot, "symbolic-ref",
		"refs/remotes/origin/HEAD",
		"refs/remotes/origin/main")
	return bare
}

// initBareRepo creates a fresh bare repo in t.TempDir(). The
// returned path is suitable for use as an `origin` remote
// URL. RefreshDefaultBranch / GitHub-style test fixtures
// share this helper.
func initBareRepo(t *testing.T) string {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "bare.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatalf("mkdir bare parent: %v", err)
	}
	mustGit(t, filepath.Dir(bare), "init", "--bare", "-q", "-b", "main", filepath.Base(bare))
	return bare
}

// mustGit runs a git command in dir, t.Fatal-ing on non-zero
// exit. Stdout/stderr are joined into the failure message for
// easier debugging.
func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// mustGitOut runs a git command in dir and returns its combined
// output, t.Fatal-ing on non-zero exit. Used when the test wants
// to inspect stdout (mustGit swallows it).
func mustGitOut(t *testing.T, dir string, args ...string) (string, string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s failed: %v\nstdout: %s\nstderr: %s",
			strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	return stdout.String(), stderr.String()
}

// TestIntegration_ShortFlagNForLocalFix drives the FULL /gtw
// fix local-mode flow through the short `-n` form, end to
// end with a real git repo. Pairs with the parser-level
// TestParseFixArgs_ShortFlagN — confirms the short form
// isn't just syntactically accepted but also drives every
// downstream step (RepoRoot / BranchExists / Preflight /
// WorktreeAdd / EnsureGitignore+Commit / WriteGTWYml /
// SetActiveCwd / dispatch-skip / slot.Store).
func TestIntegration_ShortFlagNForLocalFix(t *testing.T) {
	repoRoot := initTempRepo(t)

	ch := &recordingCh{}
	cs, _ := chatsession.New("chat-int-shortN", "test-agent", ch)
	if err := cs.SetActiveCwd(repoRoot); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}
	slot := &memSlot{}
	drafts := newMemDrafts()
	deps := HandlerDeps{
		Git: ExecGitRunner{},
		Now: func() time.Time { return time.Date(2026, 8, 8, 14, 0, 0, 0, time.UTC) },
	}

	// parseFixArgs with `-n foo` — what /gtw fix -n foo would
	// produce after the commander strips "/gtw" + "fix".
	args, err := parseFixArgs([]string{"-n", "login-bug"})
	if err != nil {
		t.Fatalf("parseFixArgs: %v", err)
	}
	if args.Mode != ModeLocal || args.RawArg != "login-bug" {
		t.Fatalf("parsed args wrong: %+v", args)
	}

	// Drive RunFix end-to-end (the same path the factory
	// uses in production).
	res, err := RunFix(
		context.Background(), args.Mode, cs, slot, drafts, deps,
		cs.ChatID, "msg-int-shortN", []string{args.RawArg}, args.Force,
	)
	if err != nil {
		t.Fatalf("RunFix: %v", err)
	}
	if !res.Consumed {
		t.Errorf("Result.Consumed = false")
	}

	// Local mode: branch = slugified rawArg, no dispatch.
	wantBranch := "login-bug"
	wt := WorktreePath(repoRoot, wantBranch)

	// Worktree must exist on disk.
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree %s not created: %v", wt, err)
	}
	// ActiveCwd must point at the new worktree.
	if got := cs.ActiveCwd(); got != wt {
		t.Errorf("ActiveCwd = %q, want %q", got, wt)
	}
	// yml must round-trip with the right fields.
	parsed, err := ReadGTWYml(wt)
	if err != nil {
		t.Fatalf("ReadGTWYml: %v", err)
	}
	if parsed.Mode != ModeLocal || parsed.Branch != wantBranch || parsed.RepoRoot != repoRoot {
		t.Errorf("yml = %+v, want mode=local branch=%s repoRoot=%s", parsed, wantBranch, repoRoot)
	}
	// In-memory slot must mirror yml.
	got := slot.Load()
	if got.Mode != ModeLocal || got.Branch != wantBranch {
		t.Errorf("slot = %+v, want mode=local branch=%s", got, wantBranch)
	}
	// No dispatch in local mode → queue must be empty.
	if n := cs.QueueLen(); n != 0 {
		t.Errorf("queue len = %d, want 0 (local mode no dispatch)", n)
	}
	// Reply is the local-mode success card.
	last := ch.lastText()
	if !strings.Contains(last, "Local worktree") {
		t.Errorf("reply missing local-mode marker:\n%s", last)
	}

	// Now /gtw close should cleanly tear down.
	closeRes, err := RunClose(context.Background(), cs, slot, deps, cs.ChatID, "msg-close", false /* force */)
	if err != nil {
		t.Fatalf("RunClose: %v", err)
	}
	if !closeRes.Consumed {
		t.Errorf("Result.Consumed = false")
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("worktree still on disk after close: %v", err)
	}
}
// recordingCh captures every Send / SendCard / Patch call's
// payload for assertion. Used by integration tests after the
// cs.Channel() migration; previous deps.Send mock is no longer
// the actual path. Field-by-field copy of OutboundMessage.
type recordingCh struct {
	mu    sync.Mutex
	sends []chatsession.OutboundMessage
}

func (r *recordingCh) Send(_ context.Context, m chatsession.OutboundMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sends = append(r.sends, m)
	return nil
}

func (r *recordingCh) SendCard(_ context.Context, m chatsession.OutboundMessage) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sends = append(r.sends, m)
	return "rec-card-id", nil
}

func (r *recordingCh) Patch(_ context.Context, m chatsession.OutboundMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sends = append(r.sends, m)
	return nil
}

func (r *recordingCh) lastText() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.sends) == 0 {
		return ""
	}
	return r.sends[len(r.sends)-1].Text
}
