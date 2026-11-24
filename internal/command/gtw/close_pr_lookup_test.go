package gtw

// Tests for the pull request the close looks up before it takes the
// worktree away: the workspace's PR cache answers first, the
// platform answers the miss, and the provider's CLI runs in the
// repository root so it still has a directory once the worktree is
// gone.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/prcache"
)

// prLookupCalls counts the provider lookups the close made.
func prLookupCalls(prov *fakeGitProvider) int {
	return len(prov.CallsByMethod("FindPRForBranch"))
}

// TestRunClose_PRFromCache_NoPlatformLookup pins the cache path:
// when the workspace already knows the branch's pull request, the
// close asks the platform for nothing.
func TestRunClose_PRFromCache_NoPlatformLookup(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	seedFixRemote(t, rig, prov, wt, repoRoot)
	rig.deps.PRCache = &prcache.Registry{}
	rig.deps.PRCache.WritePR(wt, &messages.PR{
		Number: 7, URL: "https://github.com/cnlangzi/nightme/pull/7", State: "merged",
	}, "fix/42-test")
	prov.SetPRLabels(7, []string{LabelReady})

	if _, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1", true); err != nil {
		t.Fatalf("RunClose: %v", err)
	}

	if n := prLookupCalls(prov); n != 0 {
		t.Errorf("FindPRForBranch calls = %d, want 0 — the cache knew the PR", n)
	}
	// The cached number is still the one that gets finalized.
	assertPRLabels(t, prov, 7)
	card := rig.rec.lastText()
	if want := "→ pr: cnlangzi/nightme#7 -" + LabelReady; !strings.Contains(card, want) {
		t.Errorf("card missing %q:\n%s", want, card)
	}
}

// TestRunClose_PRCacheMiss_FallsBackToThePlatform pins the miss
// path: the platform answers, and the answer is written back so a
// retry does not ask again.
func TestRunClose_PRCacheMiss_FallsBackToThePlatform(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	seedFixRemote(t, rig, prov, wt, repoRoot)
	reg := &prcache.Registry{}
	rig.deps.PRCache = reg
	seedPR(t, prov, "fix/42-test", 7, LabelReady)

	// The worktree is dirty, so the close aborts after the lookup —
	// the cache keeps what the platform answered.
	rig.git.statusResp = " M foo.txt\n"

	if _, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1", true); err != nil {
		t.Fatalf("RunClose: %v", err)
	}

	if n := prLookupCalls(prov); n != 1 {
		t.Errorf("FindPRForBranch calls = %d, want 1 — the cache was cold", n)
	}
	if got := reg.PRForBranch(wt, "fix/42-test"); got == nil || got.Number != 7 {
		t.Errorf("cache after the lookup = %+v, want {Number:7}", got)
	}

	// The retry finds the PR in the cache and asks the platform for
	// nothing.
	rig.git.statusResp = ""
	before := prLookupCalls(prov)
	if _, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-2", true); err != nil {
		t.Fatalf("RunClose (retry): %v", err)
	}
	if n := prLookupCalls(prov) - before; n != 0 {
		t.Errorf("FindPRForBranch calls on the retry = %d, want 0 — the cache answered", n)
	}
}

// TestRunClose_PRCacheHoldsAnotherBranch_FallsBackToThePlatform
// pins that a workspace whose cached PR belongs to another branch
// does not answer for the branch being closed.
func TestRunClose_PRCacheHoldsAnotherBranch_FallsBackToThePlatform(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	seedFixRemote(t, rig, prov, wt, repoRoot)
	rig.deps.PRCache = &prcache.Registry{}
	rig.deps.PRCache.WritePR(wt, &messages.PR{
		Number: 99, URL: "https://github.com/cnlangzi/nightme/pull/99", State: "open",
	}, "other/branch")
	seedPR(t, prov, "fix/42-test", 7, LabelReady)

	if _, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1", true); err != nil {
		t.Fatalf("RunClose: %v", err)
	}

	if n := prLookupCalls(prov); n != 1 {
		t.Errorf("FindPRForBranch calls = %d, want 1 — the cached PR was another branch's", n)
	}
	// The branch's own PR is the one that gets finalized.
	assertPRLabels(t, prov, 7)
	card := rig.rec.lastText()
	if want := "→ pr: cnlangzi/nightme#7 -" + LabelReady; !strings.Contains(card, want) {
		t.Errorf("card missing %q:\n%s", want, card)
	}
	if strings.Contains(card, "#99") {
		t.Errorf("card must not name the other branch's PR:\n%s", card)
	}
}

// TestRunClose_PRLookupFailure_WarnsAndTearsDown pins that a pull
// request the close cannot look up only warns: the local teardown
// runs to the end, and the reply says what went wrong.
func TestRunClose_PRLookupFailure_WarnsAndTearsDown(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	seedFixRemote(t, rig, prov, wt, repoRoot)
	prov.SetFindPRForBranchErr("fix/42-test", errors.New("network is unreachable"))

	if _, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1", true); err != nil {
		t.Fatalf("RunClose: %v", err)
	}

	card := rig.rec.lastText()
	want := "could not look up the PR of branch fix/42-test"
	if !strings.Contains(card, want) {
		t.Errorf("card missing %q:\n%s", want, card)
	}
	if strings.Contains(card, "→ pr:") {
		t.Errorf("card must not carry a PR row:\n%s", card)
	}

	// The teardown ran.
	if got := rig.cs.SelectedCwd(); got != repoRoot {
		t.Errorf("SelectedCwd = %q, want %q", got, repoRoot)
	}
	sawRemove, sawBranchDel := false, false
	for _, args := range rig.git.calls {
		if len(args) >= 2 && args[0] == "worktree" && args[1] == "remove" {
			sawRemove = true
		}
		if len(args) >= 3 && args[0] == "branch" && args[1] == "-D" {
			sawBranchDel = true
		}
	}
	if !sawRemove {
		t.Errorf("git worktree remove not called; calls=%v", rig.git.calls)
	}
	if !sawBranchDel {
		t.Errorf("git branch -D not called; calls=%v", rig.git.calls)
	}
}

// TestRunClose_PRCacheHoldsANumberlessPR_FallsBackToThePlatform
// pins that a cached PR without a number does not answer: the
// labels of PR #0 are nothing, so the close asks the platform for
// the real number.
func TestRunClose_PRCacheHoldsANumberlessPR_FallsBackToThePlatform(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	seedFixRemote(t, rig, prov, wt, repoRoot)
	rig.deps.PRCache = &prcache.Registry{}
	rig.deps.PRCache.WritePR(wt, &messages.PR{
		URL: "https://github.com/cnlangzi/nightme/pull/unknown", State: "open",
	}, "fix/42-test")
	seedPR(t, prov, "fix/42-test", 7, LabelReady)

	if _, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1", true); err != nil {
		t.Fatalf("RunClose: %v", err)
	}

	if n := prLookupCalls(prov); n != 1 {
		t.Errorf("FindPRForBranch calls = %d, want 1 — the cached PR carried no number", n)
	}
	assertPRLabels(t, prov, 7)
	card := rig.rec.lastText()
	if want := "→ pr: cnlangzi/nightme#7 -" + LabelReady; !strings.Contains(card, want) {
		t.Errorf("card missing %q:\n%s", want, card)
	}
}

// TestRunClose_PRCacheHoldsAnotherReposPR_FallsBackToThePlatform
// pins that a cached pull request from another repository does not
// answer: the close applies its own repository to the number, and
// the same number in that repository is a different pull request.
func TestRunClose_PRCacheHoldsAnotherReposPR_FallsBackToThePlatform(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	seedFixRemote(t, rig, prov, wt, repoRoot)
	rig.deps.PRCache = &prcache.Registry{}
	rig.deps.PRCache.WritePR(wt, &messages.PR{
		Number: 7, URL: "https://github.com/someone-else/nightme-fork/pull/7", State: "open",
	}, "fix/42-test")
	seedPR(t, prov, "fix/42-test", 7, LabelReady)

	if _, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1", true); err != nil {
		t.Fatalf("RunClose: %v", err)
	}

	if n := prLookupCalls(prov); n != 1 {
		t.Errorf("FindPRForBranch calls = %d, want 1 — the cached PR came from another repository", n)
	}
	// The pull request that got finalized is the platform's own.
	assertPRLabels(t, prov, 7)
	card := rig.rec.lastText()
	if want := "→ pr: cnlangzi/nightme#7 -" + LabelReady; !strings.Contains(card, want) {
		t.Errorf("card missing %q:\n%s", want, card)
	}
}

// TestRunClose_ProviderCLIRunsInTheRepoRoot pins the directory the
// provider's CLI runs in: the repository root, which outlives the
// worktree the close removes.
//
// A CLI bound to the worktree cannot start once the worktree is
// gone, and the spawn failure names the binary — which reads as a
// missing installation.
func TestRunClose_ProviderCLIRunsInTheRepoRoot(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	seedFixRemote(t, rig, prov, wt, repoRoot)

	var cliDir string
	rig.deps.Detect = func(_ context.Context, _ string, _ HTTPProber, dir string) (GitProvider, error) {
		cliDir = dir
		return prov, nil
	}

	if _, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1", true); err != nil {
		t.Fatalf("RunClose: %v", err)
	}

	if cliDir != repoRoot {
		t.Errorf("provider CLI dir = %q, want the repo root %q", cliDir, repoRoot)
	}
}

// TestResolveClosePlatform_BindsTheRepoRoot pins the same thing on
// the provider itself: the instance the close works with is bound to
// the repository root, so every gh / glab call it makes runs there.
//
// The yml-cached path is the one that builds a provider directly,
// so it shows the binding without a detection round-trip.
func TestResolveClosePlatform_BindsTheRepoRoot(t *testing.T) {
	repoRoot := t.TempDir()
	worktree := t.TempDir()

	plat := resolveClosePlatform(context.Background(), Context{
		RepoRoot: repoRoot,
		Worktree: worktree,
		Repo:     "cnlangzi/nightme",
		Provider: "github",
	}, HandlerDeps{})
	if plat.err != nil {
		t.Fatalf("resolveClosePlatform: %v", plat.err)
	}
	gh, ok := plat.prov.(*GitHubProvider)
	if !ok {
		t.Fatalf("provider = %T, want *GitHubProvider", plat.prov)
	}
	if gh.CLIDir != repoRoot {
		t.Errorf("provider Worktree = %q, want the repo root %q", gh.CLIDir, repoRoot)
	}
	if plat.owner != "cnlangzi" || plat.repo != "nightme" {
		t.Errorf("owner/repo = %q/%q, want cnlangzi/nightme", plat.owner, plat.repo)
	}
}
