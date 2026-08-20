// F-237: tests for CollectReadinessForDispatch and
// verifyUpstreamOnOrigin — the upstream-refresh probe that
// catches the "stale cached remote-tracking ref" hole in the
// F-57 readiness gate.
//
// The dispatch path goes through CollectReadinessForDispatch
// (not the bare CollectReadiness), so we test the wrapper.
// The wrapper's contract:
//   - HasUpstream=false / detached-HEAD → no extra git call
//     (zero-cost for the common cases)
//   - HasUpstream=true + ls-remote says empty → snap flipped
//     to HasUpstream=false with AheadOfRemote/BehindRemote
//     zeroed (the cached counts were derived against the
//     cached SHA and are now meaningless)
//   - HasUpstream=true + ls-remote says non-empty → snap
//     unchanged (upstream really exists on origin)
//   - HasUpstream=true + ls-remote errors → snap unchanged
//     (graceful fallback; defense-in-depth in pr.go catches
//     the actual gh rejection if the cached ref really was
//     stale and we just couldn't tell)
package gtw

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/messages"
)

// probeGit is a minimal GitRunner that captures every
// Run call and returns pre-canned responses. Used by the F-237
// probe tests where we want to inspect argv (to confirm the
// probe was / wasn't issued) without standing up the full
// pushGit mock machinery.
//
// responses[argv-key] = (stdout, stderr, err). Missing key →
// defaultErr (or nil if defaultErr is nil).
type probeGit struct {
	responses  map[string]probeResp
	calls      [][]string
	defaultErr error
}

type probeResp struct {
	stdout string
	stderr string
	err    error
}

func (r *probeGit) on(args []string, stdout, stderr string, err error) {
	if r.responses == nil {
		r.responses = make(map[string]probeResp)
	}
	r.responses[joinArgs(args)] = probeResp{stdout, stderr, err}
}

func (r *probeGit) Run(_ context.Context, _ string, args ...string) (string, string, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	key := joinArgs(args)
	if r.responses != nil {
		if resp, ok := r.responses[key]; ok {
			return resp.stdout, resp.stderr, resp.err
		}
	}
	if r.defaultErr != nil {
		return "", "", r.defaultErr
	}
	return "", "", errors.New("probeGit: unmocked " + strings.Join(args, " "))
}

func TestCollectReadinessForDispatch_UpstreamMissingNoProbe(t *testing.T) {
	// When porcelain says no upstream (the most common case),
	// the probe must NOT issue a `git ls-remote` call. Issuing
	// it would add a network round-trip to every /gtw pr / push
	// / commit invocation even for branches that have never
	// been pushed — wasteful.
	g := &probeGit{}
	g.on([]string{"status", "--porcelain", "--branch", "--untracked-files=normal"},
		"## feature/no-upstream\n", "", nil)

	snap, err := CollectReadinessForDispatch(context.Background(), "/w", g)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if snap == nil {
		t.Fatal("expected non-nil snap")
	}
	if snap.Branch != "feature/no-upstream" {
		t.Fatalf("Branch: got %q, want %q", snap.Branch, "feature/no-upstream")
	}
	if snap.HasUpstream {
		t.Fatal("HasUpstream should be false")
	}
	// Invariant: only `git status` was called.
	if len(g.calls) != 1 {
		t.Fatalf("expected 1 git call, got %d: %v", len(g.calls), g.calls)
	}
}

func TestCollectReadinessForDispatch_DetachedHeadNoProbe(t *testing.T) {
	// Detached HEAD (snap.Branch=="") should not probe the
	// remote. There's no branch name to verify against, and
	// the detached branch is already flagged via the porcelain
	// header (no upstream possible).
	g := &probeGit{}
	g.on([]string{"status", "--porcelain", "--branch", "--untracked-files=normal"},
		"## HEAD (no branch)\n", "", nil)

	snap, err := CollectReadinessForDispatch(context.Background(), "/w", g)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if snap == nil {
		t.Fatal("expected non-nil snap")
	}
	if snap.Branch != "" {
		t.Fatalf("Branch: got %q, want \"\" (detached)", snap.Branch)
	}
	if snap.HasUpstream {
		t.Fatal("HasUpstream should be false")
	}
	if len(g.calls) != 1 {
		t.Fatalf("expected 1 git call, got %d: %v", len(g.calls), g.calls)
	}
}

func TestCollectReadinessForDispatch_UpstreamConfirmed(t *testing.T) {
	// Happy path: porcelain claims upstream, ls-remote confirms.
	// Snap should be unchanged.
	g := &probeGit{}
	g.on([]string{"status", "--porcelain", "--branch", "--untracked-files=normal"},
		"## fix-install...origin/fix-install [ahead 2]\n", "", nil)
	g.on([]string{"ls-remote", "--heads", "origin", "fix-install"},
		"e43d2665eeae2ff8a9b763cc2c5a3e9f50611997\trefs/heads/fix-install\n", "", nil)

	snap, err := CollectReadinessForDispatch(context.Background(), "/w", g)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if snap == nil {
		t.Fatal("expected non-nil snap")
	}
	if !snap.HasUpstreamBranch() {
		t.Fatalf("HasUpstreamBranch() should be true; got snap=%+v", snap)
	}
	if snap.AheadOfRemote != 2 {
		t.Fatalf("AheadOfRemote: got %d, want 2", snap.AheadOfRemote)
	}
	if len(g.calls) != 2 {
		t.Fatalf("expected 2 git calls (status + ls-remote), got %d: %v", len(g.calls), g.calls)
	}
	// Confirm the second call was the probe (full argv shape).
	want := []string{"ls-remote", "--heads", "origin", "fix-install"}
	if !equalSlice(g.calls[1], want) {
		t.Fatalf("probe argv: got %v, want %v", g.calls[1], want)
	}
}

func TestCollectReadinessForDispatch_StaleCachedUpstreamFlipped(t *testing.T) {
	// F-237 repro: porcelain claims upstream, ls-remote returns
	// empty. The snap must flip HasUpstream=false AND zero out
	// AheadOfRemote / BehindRemote (they were derived against
	// the cached SHA and are now meaningless — keeping them
	// would let the gate fall through to "ahead=0 / behind=0
	// / clean" → ready, which is exactly what the bug
	// exploits).
	g := &probeGit{}
	g.on([]string{"status", "--porcelain", "--branch", "--untracked-files=normal"},
		"## fix-install...origin/fix-install\n", "", nil)
	g.on([]string{"ls-remote", "--heads", "origin", "fix-install"},
		"", "", nil) // empty stdout = branch NOT on origin

	snap, err := CollectReadinessForDispatch(context.Background(), "/w", g)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if snap == nil {
		t.Fatal("expected non-nil snap")
	}
	if snap.Branch != "fix-install" {
		t.Fatalf("Branch: got %q, want %q", snap.Branch, "fix-install")
	}
	if snap.HasUpstream {
		t.Fatal("HasUpstream should be false (cached ref was stale)")
	}
	if snap.HasUpstreamBranch() {
		t.Fatal("HasUpstreamBranch() should be false")
	}
	if snap.AheadOfRemote != 0 {
		t.Fatalf("AheadOfRemote: got %d, want 0 (was derived against cached SHA, now meaningless)", snap.AheadOfRemote)
	}
	if snap.BehindRemote != 0 {
		t.Fatalf("BehindRemote: got %d, want 0 (same reason)", snap.BehindRemote)
	}
	// Critical invariant: PRBlockReason picks the documented
	// "no upstream" case now that HasUpstream is false.
	reason := snap.PRBlockReason()
	if reason == "" {
		t.Fatal("expected PRBlockReason to fire (snap should be unread for PR)")
	}
	if !strings.Contains(reason, "branch has no upstream on origin") {
		t.Fatalf("expected no-upstream reply, got:\n%s", reason)
	}
	if !strings.Contains(reason, "/gtw push first to publish the branch") {
		t.Fatalf("expected /gtw push hint, got:\n%s", reason)
	}
}

func TestCollectReadinessForDispatch_LSRemoteErrorFallback(t *testing.T) {
	// Network blip / no origin remote: ls-remote errors. We
	// must NOT flip HasUpstream to false — that would turn a
	// passing gate into a failing one on a transient outage.
	// The defense-in-depth in pr.go is the safety net if the
	// cached ref really was stale and we got past the gate on
	// a fluke.
	g := &probeGit{}
	g.on([]string{"status", "--porcelain", "--branch", "--untracked-files=normal"},
		"## feature/x...origin/feature/x [ahead 1]\n", "", nil)
	g.on([]string{"ls-remote", "--heads", "origin", "feature/x"},
		"", "fatal: unable to access 'origin'", errors.New("exit 128"))

	snap, err := CollectReadinessForDispatch(context.Background(), "/w", g)
	if err != nil {
		t.Fatalf("unexpected err (probe error must not propagate): %v", err)
	}
	if snap == nil {
		t.Fatal("expected non-nil snap")
	}
	if !snap.HasUpstreamBranch() {
		t.Fatal("HasUpstreamBranch() should still be true (graceful fallback)")
	}
	if snap.AheadOfRemote != 1 {
		t.Fatalf("AheadOfRemote: got %d, want 1 (porcelain truth preserved)", snap.AheadOfRemote)
	}
}

func TestCollectReadinessForDispatch_StaleWithAheadCountsZeroed(t *testing.T) {
	// Defensive: even when porcelain reports ahead=3 / behind=2
	// against the cached SHA, the ls-remote empty result must
	// zero BOTH. Reasoning: the cached SHA points to a ref
	// that doesn't exist on origin, so the "ahead/behind"
	// counts are computed against a ghost.
	g := &probeGit{}
	g.on([]string{"status", "--porcelain", "--branch", "--untracked-files=normal"},
		"## stale-branch...origin/stale-branch [ahead 3, behind 2]\n", "", nil)
	g.on([]string{"ls-remote", "--heads", "origin", "stale-branch"},
		"", "", nil)

	snap, err := CollectReadinessForDispatch(context.Background(), "/w", g)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if snap.HasUpstream {
		t.Fatal("HasUpstream should be false")
	}
	if snap.AheadOfRemote != 0 || snap.BehindRemote != 0 {
		t.Fatalf("ahead/behind not zeroed: ahead=%d behind=%d", snap.AheadOfRemote, snap.BehindRemote)
	}
}

func TestCollectReadiness_BareDoesNotProbe(t *testing.T) {
	// Sanity: the runtime footer path still uses the bare
	// CollectReadiness (NOT the dispatch wrapper). Confirm
	// it does NOT issue ls-remote — the 3s ChatSession.GitStatus
	// budget would pay for a network round-trip per stamp
	// otherwise, which is exactly what we want to avoid.
	g := &probeGit{}
	g.on([]string{"status", "--porcelain", "--branch", "--untracked-files=normal"},
		"## fix-install...origin/fix-install\n", "", nil)

	snap, err := CollectReadiness(context.Background(), "/w", g)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if snap == nil {
		t.Fatal("expected non-nil snap")
	}
	if !snap.HasUpstream {
		t.Fatal("bare CollectReadiness must preserve porcelain truth (HasUpstream=true)")
	}
	if len(g.calls) != 1 {
		t.Fatalf("expected 1 git call, got %d: %v", len(g.calls), g.calls)
	}
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Reference: messages.GitStatusSnapshot is the type aliases
// the production code uses; the test imports it explicitly to
// avoid a transitive dep on internal/command/gtw from the test
// file itself.
var _ = messages.GitStatusSnapshot{}
