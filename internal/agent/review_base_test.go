package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/proc"
)

// gitRun runs a git command in dir and fails the test on error.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	c := proc.New(t.Context(), "git", append([]string{"-C", dir}, args...)...)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// gitRepoWithOrigin creates a working repository whose origin is a
// local bare repository holding one commit on main. The working copy
// is checked out from origin/main, so the review has a base to diff
// against — and no refs/remotes/origin/HEAD, because `git fetch` never
// creates one.
func gitRepoWithOrigin(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	bare := filepath.Join(base, "remote.git")
	gitRun(t, base, "init", "--bare", bare)
	gitRun(t, bare, "symbolic-ref", "HEAD", "refs/heads/main")

	seed := filepath.Join(base, "seed")
	gitRun(t, seed, "init")
	gitRun(t, seed, "config", "user.email", "test@test.local")
	gitRun(t, seed, "config", "user.name", "test")
	gitWriteFile(t, seed, "a.go", "package a\n")
	gitRun(t, seed, "add", "a.go")
	gitRun(t, seed, "commit", "-q", "-m", "init")
	gitRun(t, seed, "remote", "add", "origin", bare)
	gitRun(t, seed, "push", "-q", "origin", "HEAD:refs/heads/main")

	work := filepath.Join(base, "work")
	gitRun(t, work, "init")
	gitRun(t, work, "config", "user.email", "test@test.local")
	gitRun(t, work, "config", "user.name", "test")
	gitRun(t, work, "remote", "add", "origin", bare)
	gitRun(t, work, "fetch", "origin")
	removeOriginHead(t, work)
	gitRun(t, work, "checkout", "-B", "main", "origin/main")
	return work
}

// removeOriginHead deletes refs/remotes/origin/HEAD in whichever form
// it exists — symbolic or direct. Some git versions create it on
// fetch; the repair path under test needs it absent.
func removeOriginHead(t *testing.T, repo string) {
	t.Helper()
	c := proc.New(t.Context(), "git", "-C", repo, "symbolic-ref", "-d", "refs/remotes/origin/HEAD")
	_, _ = c.CombinedOutput()
	c = proc.New(t.Context(), "git", "-C", repo, "update-ref", "--no-deref", "-d", "refs/remotes/origin/HEAD")
	_, _ = c.CombinedOutput()
}

// TestReview_BaseUnknown_ReturnsErrBaseUnknown covers the failure the
// old two-tier probe produced: a repository with an origin remote
// whose base branch cannot be determined. The review must fail
// distinctly — "no diff to review" would claim there was nothing to
// review, while the truth is that nothing was examined — and no agent
// turn may be spawned.
func TestReview_BaseUnknown_ReturnsErrBaseUnknown(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	gitRun(t, dir, "remote", "add", "origin", filepath.Join(dir, "does-not-exist.git"))

	for _, tc := range []struct {
		name string
		run  func(context.Context, Starter, StartConfig) (RunResult, error)
	}{
		{"prompt path", func(ctx context.Context, s Starter, cfg StartConfig) (RunResult, error) {
			return ReviewWithPrompt(ctx, s, cfg)
		}},
		{"ocr path", func(ctx context.Context, s Starter, cfg StartConfig) (RunResult, error) {
			return ReviewWithOcr(ctx, s, cfg)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var turns int
			var mu sync.Mutex
			fs := &sessionFakeStarter{
				name: "test-agent",
				newDriver: func(StartConfig) *sessionFakeDriver {
					return newSessionFakeDriver(func([]ContentBlock) []AgentEvent {
						mu.Lock()
						turns++
						mu.Unlock()
						return groupReply("ok")
					})
				},
			}
			_, err := tc.run(context.Background(), fs, StartConfig{Workspace: dir})
			if !errors.Is(err, ErrBaseUnknown) {
				t.Fatalf("err = %v, want ErrBaseUnknown", err)
			}
			if errors.Is(err, ErrNoDiff) {
				t.Error("err satisfies ErrNoDiff — the base failure must be distinct")
			}
			mu.Lock()
			defer mu.Unlock()
			if turns != 0 {
				t.Errorf("%d agent turns were spawned, want 0", turns)
			}
		})
	}
}

// TestReview_UntrackedOnlyWorkspace_ReturnsErrNoDiff covers a workspace
// whose only changes are new files: they have no baseline, so no diff
// exists for them — the review can only name them, never judge them.
// They are out of scope, and a workspace holding nothing else has no
// diff to review at all.
func TestReview_UntrackedOnlyWorkspace_ReturnsErrNoDiff(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	gitCommitEmpty(t, dir)
	gitWriteFile(t, dir, "new.go", "package new\n")

	var mu sync.Mutex
	var turns int
	fs := &sessionFakeStarter{
		name: "test-agent",
		newDriver: func(StartConfig) *sessionFakeDriver {
			return newSessionFakeDriver(func([]ContentBlock) []AgentEvent {
				mu.Lock()
				turns++
				mu.Unlock()
				return groupReply("ok")
			})
		},
	}

	_, err := ReviewWithOcr(context.Background(), fs, StartConfig{Workspace: dir})
	if !errors.Is(err, ErrNoDiff) {
		t.Fatalf("review of an untracked-only workspace err = %v, want ErrNoDiff", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if turns != 0 {
		t.Errorf("%d agent turns were spawned, want 0", turns)
	}
}

// TestReview_DocsOnlyChanges_SkipsSimplifyGroup covers a changeset with
// no code file: the builtin group still reviews it, but the simplify
// lens has nothing to act on — no simplify group is created, so no
// session is spawned for it and the plan does not name it.
func TestReview_DocsOnlyChanges_SkipsSimplifyGroup(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(context.Context, Starter, StartConfig, ...RunOnceOption) (RunResult, error)
		// builtin says whether the single group is the builtin one.
		// The ocr path answers with ocr's own groups when the ocr CLI
		// is installed, so only the prompt path has a builtin group.
		builtin bool
	}{
		{name: "prompt path", run: ReviewWithPrompt, builtin: true},
		{name: "ocr path", run: ReviewWithOcr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			gitInit(t, dir)
			gitCommitEmpty(t, dir)
			gitStageFile(t, dir, "README.md", "# docs\n")

			var mu sync.Mutex
			var plan []ReviewPlanGroup
			fs := &sessionFakeStarter{
				name: "test-agent",
				newDriver: func(StartConfig) *sessionFakeDriver {
					return newSessionFakeDriver(func([]ContentBlock) []AgentEvent {
						return groupReply("ok")
					})
				},
			}

			_, err := tc.run(context.Background(), fs, StartConfig{Workspace: dir},
				WithReviewPlanObserver(func(p []ReviewPlanGroup) {
					mu.Lock()
					plan = p
					mu.Unlock()
				}))
			if err != nil {
				t.Fatalf("review failed: %v", err)
			}

			mu.Lock()
			defer mu.Unlock()
			if len(plan) != 1 {
				t.Fatalf("plan carries %d groups, want 1: %+v", len(plan), plan)
			}
			if plan[0].Pattern == PatternSimplify {
				t.Error("the plan names a simplify group although the changeset has no code file")
			}
			if !tc.builtin {
				return
			}
			if plan[0].Pattern != PatternBuiltin {
				t.Errorf("plan group pattern = %q, want %q", plan[0].Pattern, PatternBuiltin)
			}
			if len(plan[0].Files) != 1 || plan[0].Files[0] != "README.md" {
				t.Errorf("builtin group files = %v, want [README.md] (its scope is unchanged)", plan[0].Files)
			}
		})
	}
}

// TestReview_PromptOmitsUntrackedFiles covers a changeset that holds a
// tracked change next to an untracked file: no prompt names the
// untracked file, and no prompt carries a "new file additions" section
// — an untracked file has no diff, so a review could only name it.
func TestReview_PromptOmitsUntrackedFiles(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	gitCommitEmpty(t, dir)
	gitStageFile(t, dir, "a.go", "package a\n")
	gitWriteFile(t, dir, "orphan.go", "package orphan\n")

	var mu sync.Mutex
	var prompts []string
	fs := &sessionFakeStarter{
		name: "test-agent",
		newDriver: func(StartConfig) *sessionFakeDriver {
			return newSessionFakeDriver(func(blocks []ContentBlock) []AgentEvent {
				mu.Lock()
				if len(blocks) > 0 {
					prompts = append(prompts, blocks[0].Text)
				}
				mu.Unlock()
				return groupReply("ok")
			})
		},
	}

	if _, err := ReviewWithPrompt(context.Background(), fs, StartConfig{Workspace: dir}); err != nil {
		t.Fatalf("review failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(prompts) == 0 {
		t.Fatal("no agent turn ran")
	}
	for _, p := range prompts {
		if strings.Contains(p, "New file additions") {
			t.Errorf("prompt carries a new-file-additions section:\n%s", p)
		}
		if strings.Contains(p, "orphan.go") {
			t.Errorf("prompt names the untracked file:\n%s", p)
		}
		if !strings.Contains(p, "a.go") {
			t.Errorf("prompt does not name the tracked file:\n%s", p)
		}
	}
}

// TestReview_RepairIsAnnounced covers the repair notice: a repository
// that had no refs/remotes/origin/HEAD gets one, and the review says
// so — the run touched the user's repository.
func TestReview_RepairIsAnnounced(t *testing.T) {
	dir := gitRepoWithOrigin(t)
	gitCommitFile(t, dir, "a.go", "package a\n+changed\n", "change")

	fs := &sessionFakeStarter{
		name: "test-agent",
		newDriver: func(StartConfig) *sessionFakeDriver {
			return newSessionFakeDriver(func([]ContentBlock) []AgentEvent {
				return groupReply("ok")
			})
		},
	}

	res, err := ReviewWithPrompt(context.Background(), fs, StartConfig{Workspace: dir})
	if err != nil {
		t.Fatalf("review failed: %v", err)
	}
	if len(res.Notes) != 1 {
		t.Fatalf("notes = %v, want one repair notice", res.Notes)
	}
	want := "repaired missing refs/remotes/origin/HEAD → origin/main"
	if res.Notes[0] != want {
		t.Errorf("note = %q, want %q", res.Notes[0], want)
	}

	formatted := FormatReviewMessage(dir, "test-agent", res.Text, res.Notes...)
	if !strings.Contains(formatted, want) {
		t.Errorf("review message does not announce the repair:\n%s", formatted)
	}
}

// TestFormatReviewMessage_NoNotes pins the preamble shape when the run
// reported nothing: the notes section is absent.
func TestFormatReviewMessage_NoNotes(t *testing.T) {
	got := FormatReviewMessage("/ws", "agent-x", "the findings")
	want := "## Code review of /ws (run by \"agent-x\")\n\n" +
		"(current branch vs default branch; run via /review)\n\n" +
		"the findings"
	if got != want {
		t.Errorf("FormatReviewMessage =\n%q\nwant\n%q", got, want)
	}
}
