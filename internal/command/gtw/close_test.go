package gtw

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/chatsession"
)



// closeTestRig bundles the dependencies RunClose needs. Lives
// here rather than in a shared test helper because no other test
// file needs this exact setup.
type closeTestRig struct {
	cs        *chatsession.ChatSession
	slot      *memSlot
	deps      HandlerDeps
	sentTexts []string
	git       *programmableGit
}

// memSlot is an in-memory ContextSlot for tests. Real production
// uses Manager.states[chatID] via managerContextSlot; tests just
// need Load/Store round-trips.
type memSlot struct{ c Context }

func (m *memSlot) Load() Context    { return m.c }
func (m *memSlot) Store(c Context)  { m.c = c }

// programmableGit is a fakeGit whose response per (subcommand)
// the test pre-records. Lets us simulate "worktree remove"
// success / failure and "status" clean / dirty from a single
// fake without scattering switch-cases across files.
type programmableGit struct {
	// statusResp is returned for any `status --porcelain` call.
	// Empty string = clean worktree.
	statusResp string

	// worktreeRemoveErr is the error (and stderr) returned for
	// any `worktree remove` call. nil = success.
	worktreeRemoveErr error
	worktreeRemoveStderr string

	// calls records every (args) the fake saw, for assertions
	// about which commands RunClose issued.
	calls [][]string
}

func (p *programmableGit) Run(_ context.Context, dir string, args ...string) (string, string, error) {
	p.calls = append(p.calls, append([]string(nil), args...))

	switch {
	case len(args) >= 1 && args[0] == "status":
		return p.statusResp, "", nil
	case len(args) >= 2 && args[0] == "worktree" && args[1] == "remove":
		return "", p.worktreeRemoveStderr, p.worktreeRemoveErr
	}
	return "", "", nil
}

func newCloseRig(t *testing.T) *closeTestRig {
	t.Helper()

	rig := &closeTestRig{
		slot: &memSlot{},
		git:  &programmableGit{},
	}
	rig.deps = HandlerDeps{
		Git: rig.git,
		Now: func() time.Time { return time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC) },
		Send: func(_ context.Context, m OutMsg) error {
			rig.sentTexts = append(rig.sentTexts, m.Text)
			return nil
		},
	}
	// Use a per-test ChatSession via the existing helper. Each
	// test gets its own chatID so they don't bleed state.
	cs, _ := chatsession.New("chat-close-" + t.Name(), "test-agent", newTestChannel())
	_ = cs.SetActiveCwd("/tmp/start") // neutral starting cwd; tests overwrite
	rig.cs = cs
	return rig
}

// seedFix writes a complete fix snapshot into
// `<wt>/.nightme/gtw.yml` (the way /gtw fix does) and points
// the ChatSession at the worktree, mimicking the post-fix state
// that RunClose expects to find.
func seedFix(t *testing.T, rig *closeTestRig, wt, repoRoot string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(wt, nightmeDirName), 0o755); err != nil {
		t.Fatalf("mkdir .nightme: %v", err)
	}
	if err := WriteGTWYml(wt, Context{
		Mode:     ModeLocal,
		Issue:    -1,
		Branch:   "fix/42-test",
		Worktree: wt,
		RepoRoot: repoRoot,
		State:    StateFixing,
	}, rig.deps.Now); err != nil {
		t.Fatalf("seed WriteGTWYml: %v", err)
	}
	if err := rig.cs.SetActiveCwd(wt); err != nil {
		t.Fatalf("seed SetActiveCwd: %v", err)
	}
	rig.slot.Store(Context{
		Mode: ModeLocal, Issue: -1, Branch: "fix/42-test",
		Worktree: wt, RepoRoot: repoRoot, State: StateFixing,
		UpdatedAt: rig.deps.Now(),
	})
}

// --- tests ---

// TestRunClose_CleanWorktree_Success is the happy path: yml
// exists, status is clean, git worktree remove succeeds, CWD
// switches back, in-memory Context is cleared.
func TestRunClose_CleanWorktree_Success(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	seedFix(t, rig, wt, repoRoot)

	res, err := RunClose(context.Background(), rig.cs, rig.slot, rig.deps, rig.cs.ChatID, "msg-1", false /* force */)
	if err != nil {
		t.Fatalf("RunClose: %v", err)
	}
	if res == nil || !res.Consumed {
		t.Fatalf("Result = %+v, want Consumed=true", res)
	}

	// CWD must be back at repoRoot.
	if got := rig.cs.ActiveCwd(); got != repoRoot {
		t.Errorf("ActiveCwd after close = %q, want %q", got, repoRoot)
	}
	// In-memory context must be cleared.
	if got := rig.slot.Load(); got != (Context{}) {
		t.Errorf("slot.Load() after close = %+v, want zero Context", got)
	}
	// git worktree remove must have been called from inside
	// repoRoot, with the worktree path as argument.
	sawRemove := false
	for _, args := range rig.git.calls {
		if len(args) >= 3 && args[0] == "worktree" && args[1] == "remove" {
			sawRemove = true
			if filepath.Clean(args[2]) != filepath.Clean(wt) {
				t.Errorf("worktree remove target = %q, want %q", args[2], wt)
			}
		}
	}
	if !sawRemove {
		t.Errorf("git worktree remove not called; calls=%v", rig.git.calls)
	}
	// Success reply must mention the branch + worktree.
	got := rig.sentTexts[len(rig.sentTexts)-1]
	if !strings.Contains(got, "fix/42-test") {
		t.Errorf("success reply missing branch:\n%s", got)
	}
	if !strings.Contains(got, "worktree") {
		t.Errorf("success reply missing worktree label:\n%s", got)
	}
}

// TestRunClose_DirtyWorktree_Rejected verifies the v1 "no
// --force" rule. Any porcelain output from `git status` makes
// the close fail with a per-file preview and the in-memory
// Context must NOT be touched.
func TestRunClose_DirtyWorktree_Rejected(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	seedFix(t, rig, wt, repoRoot)

	rig.git.statusResp = " M foo.txt\n?? untracked.go\n"

	res, err := RunClose(context.Background(), rig.cs, rig.slot, rig.deps, rig.cs.ChatID, "msg-1", false /* force */)
	if err != nil {
		t.Fatalf("RunClose: %v", err)
	}
	if res == nil || !res.Consumed {
		t.Fatalf("Result = %+v, want Consumed=true", res)
	}

	// CWD must still be the worktree.
	if got := rig.cs.ActiveCwd(); got != wt {
		t.Errorf("ActiveCwd after dirty close = %q, want %q (unchanged)", got, wt)
	}
	// In-memory context must be unchanged.
	gotCtx := rig.slot.Load()
	if gotCtx.Branch != "fix/42-test" || gotCtx.Worktree != wt {
		t.Errorf("slot.Load() after dirty close = %+v, want fix/42-test / %q", gotCtx, wt)
	}
	// git worktree remove must NOT have been called.
	for _, args := range rig.git.calls {
		if len(args) >= 2 && args[0] == "worktree" && args[1] == "remove" {
			t.Errorf("git worktree remove called despite dirty worktree: %v", args)
		}
	}
	// Reply must list the dirty files.
	reply := rig.sentTexts[len(rig.sentTexts)-1]
	if !strings.Contains(reply, "foo.txt") {
		t.Errorf("dirty reply missing foo.txt:\n%s", reply)
	}
	if !strings.Contains(reply, "untracked.go") {
		t.Errorf("dirty reply missing untracked.go:\n%s", reply)
	}
	if !strings.Contains(reply, "commit or stash") {
		t.Errorf("dirty reply missing commit hint:\n%s", reply)
	}
}

// TestRunClose_NoYml verifies the "no active fix" path. CWD
// points somewhere with no .nightme/gtw.yml — reply explains
// and no git commands run.
func TestRunClose_NoYml(t *testing.T) {
	wt := t.TempDir()

	rig := newCloseRig(t)
	if err := rig.cs.SetActiveCwd(wt); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}

	res, err := RunClose(context.Background(), rig.cs, rig.slot, rig.deps, rig.cs.ChatID, "msg-1", false /* force */)
	if err != nil {
		t.Fatalf("RunClose: %v", err)
	}
	if res == nil || !res.Consumed {
		t.Fatalf("Result = %+v, want Consumed=true", res)
	}

	if len(rig.git.calls) != 0 {
		t.Errorf("git called despite missing yml: %v", rig.git.calls)
	}
	reply := rig.sentTexts[len(rig.sentTexts)-1]
	if !strings.Contains(reply, "no active fix") {
		t.Errorf("reply = %q, want 'no active fix'", reply)
	}
	if !strings.Contains(reply, "/cwd") {
		t.Errorf("reply missing /cwd hint:\n%s", reply)
	}
}

// TestRunClose_GitRemoveFails verifies a failed `git worktree
// remove` produces an error reply with the stderr tail, and
// state (CWD + slot) is left intact for the user to retry.
func TestRunClose_GitRemoveFails(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	seedFix(t, rig, wt, repoRoot)

	rig.git.worktreeRemoveErr = &fakeExitError{code: 128, msg: "worktree remove failed"}
	rig.git.worktreeRemoveStderr = "fatal: could not remove worktree"

	res, err := RunClose(context.Background(), rig.cs, rig.slot, rig.deps, rig.cs.ChatID, "msg-1", false /* force */)
	if err != nil {
		t.Fatalf("RunClose: %v", err)
	}
	if !res.Consumed {
		t.Fatalf("Result.Consumed = false")
	}

	// State must be untouched so user can retry.
	if got := rig.cs.ActiveCwd(); got != wt {
		t.Errorf("ActiveCwd after git failure = %q, want %q (unchanged)", got, wt)
	}
	if got := rig.slot.Load(); got.Branch != "fix/42-test" {
		t.Errorf("slot cleared despite git failure: %+v", got)
	}
	// Reply must surface the git stderr tail.
	reply := rig.sentTexts[len(rig.sentTexts)-1]
	if !strings.Contains(reply, "git worktree remove failed") {
		t.Errorf("reply missing git error:\n%s", reply)
	}
	if !strings.Contains(reply, "git stderr tail") {
		t.Errorf("reply missing stderr-tail label:\n%s", reply)
	}
}

// fakeExitError stands in for exec.ExitError without dragging in
// os/exec. The real one is a struct with embedded ProcessState;
// RunClose's WorktreeError.Unwrap → Err chain is what matters.
type fakeExitError struct {
	code int
	msg  string
}

func (e *fakeExitError) Error() string { return e.msg }

// (rigSetCwd removed; we don't need it — RunClose takes the
// ChatSession's ActiveCwd directly.)