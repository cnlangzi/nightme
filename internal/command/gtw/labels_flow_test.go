package gtw

// Flow-level tests for the label-catalog check: the /gtw commands
// that talk to the tracker verify that the repository's catalog
// holds every label in AllLabels, and the replies say what the
// check found.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/chatsession"
)

// catalogMissingAssigned is the catalog of a repository that was
// bootstrapped before `nightme/assigned` joined AllLabels — the
// state the check has to repair.
func catalogMissingAssigned() []string {
	return []string{
		LabelWIP, LabelReady, LabelReviewing, LabelRevise, LabelDone, LabelStuck,
	}
}

// TestFixAuto_CreatesMissingAssignedLabelBeforePick pins the
// deadlock this feature removes: a repository whose catalog predates
// `nightme/assigned` can carry no queue marker, so the pick would
// report an empty queue forever. The check creates the label first,
// and the reply says the queue is waiting for a human to attach it.
func TestFixAuto_CreatesMissingAssignedLabelBeforePick(t *testing.T) {
	rig := newFixRemoteRig(t)
	rig.prov.SetListLabels(catalogMissingAssigned())
	rig.prov.SetListIssues([]*Issue{
		{ID: 43, Title: "feat: unassigned", State: "open"},
	})

	if _, err := rig.driveAuto(t); err != nil {
		t.Fatalf("RunFix: %v", err)
	}

	// The missing label was created — and only that one.
	calls := rig.prov.CallsByMethod("CreateLabel")
	if len(calls) != 1 || calls[0].Label != LabelAssigned {
		t.Fatalf("CreateLabel calls = %+v, want exactly one for %q", calls, LabelAssigned)
	}
	// The catalog is read before the queue is consulted.
	recorded := rig.prov.Calls()
	listLabelsAt, listIssuesAt := -1, -1
	for i, c := range recorded {
		switch c.Method {
		case "ListLabels":
			listLabelsAt = i
		case "ListIssues":
			listIssuesAt = i
		}
	}
	if listLabelsAt < 0 || listIssuesAt < 0 || listLabelsAt > listIssuesAt {
		t.Errorf("ListLabels at %d, ListIssues at %d; the catalog check must come first: %+v",
			listLabelsAt, listIssuesAt, recorded)
	}
	// The reply says why the queue is empty and what fills it.
	last := rig.rec.lastText()
	if !strings.Contains(last, LabelAssigned) {
		t.Errorf("reply must name the queue label:\n%s", last)
	}
	if !strings.Contains(last, "just created") {
		t.Errorf("reply must say the label was just created:\n%s", last)
	}
	if !strings.Contains(last, "run `/gtw assign") {
		t.Errorf("reply must say how the queue is filled:\n%s", last)
	}
}

// TestFixAuto_SkippedCandidate_DropsTheQueueNote pins that the
// queue-marker note belongs to a queue that holds no candidate at
// all. A candidate that was passed over has its own reason on the
// card, and "attach the label to an issue" would not change that.
func TestFixAuto_SkippedCandidate_DropsTheQueueNote(t *testing.T) {
	rig := newFixRemoteRig(t)
	rig.prov.SetListLabels(catalogMissingAssigned())
	blocked := &Issue{
		ID:        41,
		Title:     "feat: blocked one",
		State:     "open",
		Labels:    []string{LabelAssigned},
		CreatedAt: time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC),
	}
	rig.prov.SetListIssues([]*Issue{blocked})
	rig.prov.SetBlockedBy(41, []*Issue{{ID: 30, State: "open"}})

	if _, err := rig.driveAuto(t); err != nil {
		t.Fatalf("RunFix: %v", err)
	}
	last := rig.rec.lastText()
	if !strings.Contains(last, "blocked") {
		t.Errorf("reply must name the reason the candidate was passed over:\n%s", last)
	}
	if strings.Contains(last, "run `/gtw assign") {
		t.Errorf("reply must not tell the user to attach the label — the queue holds a candidate:\n%s", last)
	}
}

// TestFixRemote_LabelCheckFailure_IsFatal pins that a failing check
// stops /gtw fix before it creates anything: the provider's own
// error is echoed, and no worktree and no branch are left behind.
func TestFixRemote_LabelCheckFailure_IsFatal(t *testing.T) {
	rig := newFixRemoteRig(t)
	rig.prov.SetIssue(42, &Issue{
		ID:    42,
		Title: "Login state expiration",
		State: "open",
		URL:   "https://github.com/cnlangzi/nightme/issues/42",
	})
	rig.prov.SetListLabelsErr(errors.New("403 Forbidden: missing Labels read scope"))

	if _, err := rig.drive(t, "42"); err != nil {
		t.Fatalf("RunFix: %v", err)
	}

	last := rig.rec.lastText()
	if !strings.Contains(last, "Could not ensure gtw labels on cnlangzi/nightme") {
		t.Errorf("reply must name the failed check:\n%s", last)
	}
	if !strings.Contains(last, "403 Forbidden: missing Labels read scope") {
		t.Errorf("reply must echo the provider error verbatim:\n%s", last)
	}
	if n := len(rig.prov.CallsByMethod("AddIssueLabel")); n != 0 {
		t.Errorf("AddIssueLabel calls = %d, want 0 (the check failed first)", n)
	}
	wtOut, _ := mustGitOut(t, rig.repoRoot, "worktree", "list", "--porcelain")
	if c := strings.Count(wtOut, "worktree "); c != 1 {
		t.Errorf("worktree count = %d, want 1 (the check fails before the worktree exists):\n%s", c, wtOut)
	}
	branchOut, _ := mustGitOut(t, rig.repoRoot, "branch", "--list", "fix/*")
	if strings.TrimSpace(branchOut) != "" {
		t.Errorf("expected no fix/* branches after the failed check, got: %q", branchOut)
	}
}

// TestFixRemote_AddIssueLabelFailure_NamesStaleCatalog pins the
// remedy for a label that cannot be attached even though the catalog
// was verified: the catalog may have changed since.
func TestFixRemote_AddIssueLabelFailure_NamesStaleCatalog(t *testing.T) {
	rig := newFixRemoteRig(t)
	rig.prov.SetListLabels(AllLabels)
	rig.prov.SetIssue(42, &Issue{
		ID:    42,
		Title: "Login state expiration",
		State: "open",
		URL:   "https://github.com/cnlangzi/nightme/issues/42",
	})
	rig.prov.SetAddIssueLabelErr(errors.New("'nightme/wip' not found"))

	if _, err := rig.drive(t, "42"); err != nil {
		t.Fatalf("RunFix: %v", err)
	}

	last := rig.rec.lastText()
	for _, want := range []string{
		LabelWIP,
		"#42",
		"'nightme/wip' not found",
		"was verified earlier in this session",
		"restart the nightme daemon",
		"by hand",
	} {
		if !strings.Contains(last, want) {
			t.Errorf("reply missing %q:\n%s", want, last)
		}
	}
}

// TestRunClose_UnreadableCatalog_ClosesWithWarning pins that a
// failing catalog check does not stop the close: the check warns,
// the labels are finalised anyway, and the warning rides along on
// the close's card.
func TestRunClose_UnreadableCatalog_ClosesWithWarning(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	prov.SetListLabelsErr(errors.New("403 Forbidden: missing Labels read scope"))
	seedFixRemote(t, rig, prov, wt, repoRoot)

	if _, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1", false); err != nil {
		t.Fatalf("RunClose: %v", err)
	}

	// The close itself ran to completion.
	last := rig.rec.lastText()
	if !strings.Contains(last, "closed") {
		t.Errorf("reply must report the close:\n%s", last)
	}
	// The warning names the failed check.
	if !strings.Contains(last, "⚠️ could not ensure gtw labels on cnlangzi/nightme") {
		t.Errorf("reply must carry the warning line:\n%s", last)
	}
	// The catalog was read — the close attaches a label of its own.
	if n := len(prov.CallsByMethod("ListLabels")); n != 1 {
		t.Errorf("ListLabels calls = %d, want 1 (the close checks the catalog)", n)
	}
	// The issue was finalised despite the failed check.
	got := prov.IssueLabels(42)
	if !slices.Contains(got, LabelDone) {
		t.Errorf("issue labels = %v, want %q to be present", got, LabelDone)
	}
}

// TestDispatchPR_LabelCheckFailure_WarnsAndContinues pins that a
// failing check does not stop /gtw pr: the pull request is opened
// and the reply carries the warning.
func TestDispatchPR_LabelCheckFailure_WarnsAndContinues(t *testing.T) {
	rig := newPRTestRig(t)
	setupPRWorktree(t, rig, Context{Branch: "wt"})
	setupPRGit(rig, "wt", 0)
	rig.git.on("remote", "git@github.com:octocat/hello.git", "", nil)
	rig.prov.SetListLabelsErr(errors.New("403 Forbidden: missing Labels read scope"))
	rig.prov.SetCreatePRResp("https://github.com/x/y/pull/1")
	rig.installDeps()

	cs := rig.cs
	s := captureCh(t, cs)
	if _, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg", prArgs{}, Config{}); err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "https://github.com/x/y/pull/1") {
		t.Fatalf("expected the pull request to be opened, got:\n%s", r)
	}
	if !strings.Contains(r, "⚠️ could not ensure gtw labels on") {
		t.Errorf("reply must carry the warning line:\n%s", r)
	}
}

// TestDispatchPR_LabelCheckFailure_WarnsOnFailureToo pins that the
// warning survives the command's failure paths: the user learns
// about the broken catalog whichever way /gtw pr ends.
func TestDispatchPR_LabelCheckFailure_WarnsOnFailureToo(t *testing.T) {
	rig := newPRTestRig(t)
	setupPRWorktree(t, rig, Context{Branch: "wt"})
	setupPRGit(rig, "wt", 0)
	rig.git.on("remote", "git@github.com:octocat/hello.git", "", nil)
	rig.prov.SetListLabelsErr(errors.New("403 Forbidden: missing Labels read scope"))
	rig.prov.SetFindOpenPRResp(&PR{Number: 42, URL: "https://github.com/octocat/hello/pull/42", State: "open"})
	rig.installDeps()

	cs := rig.cs
	s := captureCh(t, cs)
	if _, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg", prArgs{}, Config{}); err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "already has an open PR") {
		t.Fatalf("expected the existing-PR reply, got:\n%s", r)
	}
	if !strings.Contains(r, "⚠️ could not ensure gtw labels on") {
		t.Errorf("reply must carry the warning line:\n%s", r)
	}
}

// TestRunNewCreate_LabelCheckFailure_WarnsAndContinues pins that a
// failing check does not stop /gtw new -y: the issue is created and
// the reply carries the warning.
func TestRunNewCreate_LabelCheckFailure_WarnsAndContinues(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	repoRoot := initTempRepo(t)
	mustGit(t, repoRoot, "remote", "add", "origin", "https://github.com/cnlangzi/nightme.git")

	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	prov.SetListLabelsErr(errors.New("403 Forbidden: missing Labels read scope"))
	prov.SetCreateIssueResp(&Issue{ID: 42, Title: "feat(gtw): archive chats", URL: "https://github.com/cnlangzi/nightme/issues/42"})

	cs, _ := chatsession.New("chat-new-labels", "test-agent")
	ch := &recordingCh{}
	cs.WithEmitter(ch)
	if err := cs.SetSelectedCwd(repoRoot); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	deps := HandlerDeps{
		Git:    ExecGitRunner{},
		Detect: fakeDetect(prov),
	}

	// A draft for the chat to turn into an issue.
	draftDir, err := issueDraftDir(cs.ChatID)
	if err != nil {
		t.Fatalf("issueDraftDir: %v", err)
	}
	if err := os.MkdirAll(draftDir, 0o700); err != nil {
		t.Fatalf("mkdir draft dir: %v", err)
	}
	draft := filepath.Join(draftDir, "01.md")
	if err := os.WriteFile(draft, []byte("# feat(gtw): archive chats\n\n## Requirements\n- R1\n"), 0o600); err != nil {
		t.Fatalf("write draft: %v", err)
	}

	if _, err := RunNewCreate(context.Background(), cs, deps, cs.ChatID, "msg-new"); err != nil {
		t.Fatalf("RunNewCreate: %v", err)
	}

	if n := len(prov.CallsByMethod("CreateIssue")); n != 1 {
		t.Fatalf("CreateIssue calls = %d, want 1 (the warning does not stop the creation)", n)
	}
	last := ch.lastText()
	if !strings.Contains(last, "Issue #42 opened") {
		t.Errorf("reply must report the opened issue:\n%s", last)
	}
	if !strings.Contains(last, "⚠️ could not ensure gtw labels on cnlangzi/nightme") {
		t.Errorf("reply must carry the warning line:\n%s", last)
	}
}

// TestCollectPR_DoesNotTouchTheLabelCatalog pins that the check
// never runs as part of provider detection: the workspace footer
// resolves a provider on every message refresh, and a check there
// would turn a read into a write to the user's repository.
func TestCollectPR_DoesNotTouchTheLabelCatalog(t *testing.T) {
	rig := newFixRemoteRig(t)

	if _, err := CollectPR(context.Background(), rig.repoRoot, "fix/42-example", rig.deps); err != nil {
		t.Fatalf("CollectPR: %v", err)
	}
	for _, method := range []string{"ListLabels", "CreateLabel"} {
		if calls := rig.prov.CallsByMethod(method); len(calls) != 0 {
			t.Errorf("%s unexpectedly called: %+v", method, calls)
		}
	}
}

// TestFixRemote_CheckRunsOncePerRepository pins that the check is
// cached per repository: a second /gtw fix against the same
// repository makes no remote call at all.
func TestFixRemote_CheckRunsOncePerRepository(t *testing.T) {
	rig := newFixRemoteRig(t)
	rig.deps.LabelCache = &LabelCache{}
	rig.prov.SetListLabels(AllLabels)
	rig.prov.SetIssue(42, &Issue{
		ID:    42,
		Title: "Login state expiration",
		State: "open",
		URL:   "https://github.com/cnlangzi/nightme/issues/42",
	})

	if _, err := rig.drive(t, "42"); err != nil {
		t.Fatalf("first RunFix: %v", err)
	}
	before := len(rig.prov.CallsByMethod("ListLabels"))
	if before != 1 {
		t.Fatalf("ListLabels calls = %d, want 1", before)
	}

	// The second run finds the branch from the first one and
	// fails on it — the point of the assertion is the catalog
	// check, which must not run again.
	_, _ = rig.drive(t, "42")
	if n := len(rig.prov.CallsByMethod("ListLabels")); n != 1 {
		t.Errorf("ListLabels calls = %d, want 1 (the repository is cached)", n)
	}
	if n := len(rig.prov.CallsByMethod("CreateLabel")); n != 0 {
		t.Errorf("CreateLabel calls = %d, want 0 (the catalog is complete)", n)
	}
}
