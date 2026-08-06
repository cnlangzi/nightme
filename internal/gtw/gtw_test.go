package gtw

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- slug / branch derivation -------------------------------------

func TestDeriveBranch_Typical(t *testing.T) {
	got := DeriveBranch(42, "Login state expiration")
	want := "fix/42-login-state-expiration"
	if got != want {
		t.Fatalf("DeriveBranch(42, %q) = %q, want %q", "Login state expiration", got, want)
	}
}

func TestDeriveBranch_EmptyTitle(t *testing.T) {
	got := DeriveBranch(42, "")
	want := "fix/42"
	if got != want {
		t.Fatalf("DeriveBranch(42, \"\") = %q, want %q", got, want)
	}
}

func TestDeriveBranch_AllSymbols(t *testing.T) {
	// Title that's all non-ASCII / punctuation → no slug.
	got := DeriveBranch(7, "中文标题 🚀 !@#")
	// Allowed: digits + "-" only. With no usable letters, the
	// slug drops to the bare issue id.
	want := "fix/7"
	if got != want {
		t.Fatalf("DeriveBranch(7, weird) = %q, want %q", got, want)
	}
}

func TestDeriveBranch_LongTitleTruncated(t *testing.T) {
	// 50 chars in slug; budget after "<id>-" = SlugMaxLen - 3 = 27.
	title := "the quick brown fox jumps over the lazy dog again and again"
	got := DeriveBranch(3, title)
	if len(got) > len("fix/3-")+SlugMaxLen {
		t.Fatalf("DeriveBranch length %d > %d: %q", len(got),
			len("fix/3-")+SlugMaxLen, got)
	}
	// Should NOT have a trailing dash.
	if strings.HasSuffix(got, "-") {
		t.Fatalf("trailing dash: %q", got)
	}
	// Sanity: starts with the right prefix.
	if !strings.HasPrefix(got, "fix/3-") {
		t.Fatalf("missing prefix: %q", got)
	}
}

func TestDeriveBranch_MultipleSeparatorsCollapsed(t *testing.T) {
	// "foo  bar -- baz" should slugify to "foo-bar-baz", not
	// "foo---bar----baz".
	got := DeriveBranch(1, "foo  bar -- baz")
	want := "fix/1-foo-bar-baz"
	if got != want {
		t.Fatalf("DeriveBranch(1, foo  bar -- baz) = %q, want %q", got, want)
	}
}

func TestDeriveBranch_UnderscoresAndDotsTreatedAsSeparators(t *testing.T) {
	got := DeriveBranch(1, "fix_login.bug")
	want := "fix/1-fix-login-bug"
	if got != want {
		t.Fatalf("DeriveBranch(1, fix_login.bug) = %q, want %q", got, want)
	}
}

func TestDeriveSlug_BareID(t *testing.T) {
	got := DeriveSlug(99, "   ")
	want := "99"
	if got != want {
		t.Fatalf("DeriveSlug(99, whitespace) = %q, want %q", got, want)
	}
}

// ---- worktree path -------------------------------------------------

func TestWorktreePath_StandardLayout(t *testing.T) {
	got := WorktreePath("/home/dev/code/nightme", "42-login-state-expiration")
	want := "/home/dev/code/nightme.nightme/42-login-state-expiration"
	if got != want {
		t.Fatalf("WorktreePath() = %q, want %q", got, want)
	}
}

func TestWorktreePath_NoTrailingSlash(t *testing.T) {
	got := WorktreePath("/code/nightme/", "42-foo")
	want := "/code/nightme.nightme/42-foo"
	if got != want {
		t.Fatalf("WorktreePath() = %q, want %q", got, want)
	}
}

func TestWorktreePath_DeepRepoPath(t *testing.T) {
	got := WorktreePath("/work/group/sub/nightme", "1-foo")
	want := "/work/group/sub/nightme.nightme/1-foo"
	if got != want {
		t.Fatalf("WorktreePath() = %q, want %q", got, want)
	}
}

func TestBranchVariant(t *testing.T) {
	cases := []struct {
		branch string
		n      int
		want   string
	}{
		{"fix/42-foo", 2, "fix/42-foo-v2"},
		{"fix/42-foo", 3, "fix/42-foo-v3"},
		{"fix/42-foo", 10, "fix/42-foo-v10"},
		// n < 2 is treated as the original (sanity guard).
		{"fix/42-foo", 1, "fix/42-foo"},
		{"fix/42-foo", 0, "fix/42-foo"},
	}
	for _, c := range cases {
		got := BranchVariant(c.branch, c.n)
		if got != c.want {
			t.Errorf("BranchVariant(%q, %d) = %q, want %q", c.branch, c.n, got, c.want)
		}
	}
}

// ---- parseIssueID --------------------------------------------------

func TestParseIssueID(t *testing.T) {
	cases := []struct {
		in     string
		want   int
		errMsg string
	}{
		{"42", 42, ""},
		{"#42", 42, ""},
		{"  42  ", 42, ""},
		{"1", 1, ""},
		{"0", 0, "issue id cannot be 0"},
		{"", 0, "empty issue id"},
		{"abc", 0, "invalid issue id"},
		{"42abc", 0, "invalid issue id"},
		{"-1", 0, "invalid issue id"},
		{"42.0", 0, "invalid issue id"},
	}
	for _, c := range cases {
		got, err := parseIssueID(c.in)
		if c.errMsg == "" {
			if err != nil {
				t.Errorf("parseIssueID(%q) unexpected error: %v", c.in, err)
				continue
			}
			if got != c.want {
				t.Errorf("parseIssueID(%q) = %d, want %d", c.in, got, c.want)
			}
		} else {
			if err == nil {
				t.Errorf("parseIssueID(%q) expected error containing %q, got nil (val=%d)",
					c.in, c.errMsg, got)
				continue
			}
			if !strings.Contains(err.Error(), c.errMsg) {
				t.Errorf("parseIssueID(%q) error = %q, want containing %q", c.in, err.Error(), c.errMsg)
			}
		}
	}
}

// ---- ParseRepoOwner ------------------------------------------------

func TestParseRepoOwner(t *testing.T) {
	cases := []struct {
		in        string
		owner     string
		repo      string
		errSubstr string
	}{
		{"git@github.com:cnlangzi/nightme.git", "cnlangzi", "nightme", ""},
		{"https://github.com/cnlangzi/nightme.git", "cnlangzi", "nightme", ""},
		{"https://github.com/cnlangzi/nightme", "cnlangzi", "nightme", ""},
		{"ssh://git@github.com/cnlangzi/nightme.git", "cnlangzi", "nightme", ""},
		// GitLab self-hosted under groups.
		{"https://gitlab.example.com/group/sub/repo.git", "group/sub", "repo", ""},
		{"", "", "", "empty remote URL"},
		{"not a url", "", "", "cannot parse"},
	}
	for _, c := range cases {
		owner, repo, err := ParseRepoOwner(c.in)
		if c.errSubstr == "" {
			if err != nil {
				t.Errorf("ParseRepoOwner(%q) unexpected error: %v", c.in, err)
				continue
			}
			if owner != c.owner || repo != c.repo {
				t.Errorf("ParseRepoOwner(%q) = (%q, %q), want (%q, %q)",
					c.in, owner, repo, c.owner, c.repo)
			}
		} else {
			if err == nil {
				t.Errorf("ParseRepoOwner(%q) expected error containing %q, got nil",
					c.in, c.errSubstr)
				continue
			}
			if !strings.Contains(err.Error(), c.errSubstr) {
				t.Errorf("ParseRepoOwner(%q) error = %q, want containing %q",
					c.in, err.Error(), c.errSubstr)
			}
		}
	}
}

// ---- DetectPlatform ------------------------------------------------

func TestDetectPlatform(t *testing.T) {
	cases := []struct {
		url      string
		want     PlatformKind
		errMatch string
	}{
		{"git@github.com:foo/bar.git", PlatformGitHub, ""},
		{"https://github.com/foo/bar", PlatformGitHub, ""},
		{"git@gitlab.com:foo/bar.git", PlatformGitLab, ""},
		{"https://gitlab.example.com/foo/bar", PlatformGitLab, ""},
		{"https://gitea.example.com/foo/bar", "", "unsupported git platform"},
		{"https://bitbucket.org/foo/bar", "", "unsupported git platform"},
	}
	for _, c := range cases {
		got, err := DetectPlatform(c.url)
		if c.errMatch == "" {
			if err != nil {
				t.Errorf("DetectPlatform(%q) unexpected error: %v", c.url, err)
				continue
			}
			if got != c.want {
				t.Errorf("DetectPlatform(%q) = %q, want %q", c.url, got, c.want)
			}
		} else {
			if err == nil {
				t.Errorf("DetectPlatform(%q) expected error containing %q, got nil (kind=%q)",
					c.url, c.errMatch, got)
				continue
			}
			if !strings.Contains(err.Error(), c.errMatch) {
				t.Errorf("DetectPlatform(%q) error = %q, want containing %q",
					c.url, err.Error(), c.errMatch)
			}
		}
	}
}

// ---- fake runners --------------------------------------------------

// fakeGit is a GitRunner that returns canned stdout/stderr per argv.
type fakeGit struct {
	responses map[string]fakeGitResp
}

type fakeGitResp struct {
	stdout string
	stderr string
	err    error
}

func (f *fakeGit) Run(_ context.Context, _ string, args ...string) (string, string, error) {
	key := strings.Join(args, " ")
	if r, ok := f.responses[key]; ok {
		return r.stdout, r.stderr, r.err
	}
	return "", "", nil
}

func TestRepoRoot_Success(t *testing.T) {
	g := &fakeGit{responses: map[string]fakeGitResp{
		"rev-parse --show-toplevel": {stdout: "/code/nightme\n"},
	}}
	got, err := RepoRoot(context.Background(), "/code/nightme/subdir", g)
	if err != nil {
		t.Fatalf("RepoRoot: %v", err)
	}
	if got != "/code/nightme" {
		t.Fatalf("RepoRoot = %q, want /code/nightme", got)
	}
}

func TestRepoRoot_NotInRepo(t *testing.T) {
	g := &fakeGit{responses: map[string]fakeGitResp{
		"rev-parse --show-toplevel": {
			stderr: "fatal: not a git repository",
			err:    &fakeExitErr{code: 128},
		},
	}}
	_, err := RepoRoot(context.Background(), "/nowhere", g)
	if err == nil {
		t.Fatal("expected error from RepoRoot in non-repo dir")
	}
}

type fakeExitErr struct{ code int }

func (e *fakeExitErr) Error() string { return "exit" }

// ExitCode satisfies the os/exec exitError interface shape (we
// only need the int method, not the full *exec.ExitError type).
func (e *fakeExitErr) ExitCode() int { return e.code }

// ---- tailLines ----------------------------------------------------

func TestTailLines(t *testing.T) {
	in := "a\nb\nc\nd\ne"
	got := tailLines(in, 3)
	want := "c\nd\ne"
	if got != want {
		t.Fatalf("tailLines = %q, want %q", got, want)
	}

	got = tailLines(in, 100)
	if got != in {
		t.Fatalf("tailLines(100) = %q, want %q", got, in)
	}

	got = tailLines("", 5)
	if got != "" {
		t.Fatalf("tailLines(\"\") = %q, want empty", got)
	}

	// Trailing newline stripped by the function.
	got = tailLines("a\nb\n", 5)
	if got != "a\nb" {
		t.Fatalf("tailLines trailing-nl = %q, want %q", got, "a\nb")
	}
}

// ---- rebuild test (smoke) -----------------------------------------

func TestRebuildContext_NilWhenNotInWorktree(t *testing.T) {
	// cwd is not under a worktree (no slash → repoRootFromCS
	// returns cwd; BranchExists uses repoRoot from cwd which is
	// the same as cwd → symbolic-ref returns "" because we
	// aren't on a `fix/` branch). Result must be the zero Context.
	g := &fakeGit{responses: map[string]fakeGitResp{
		"symbolic-ref --short HEAD": {stdout: "main\n"},
	}}
	cs := &fakeSender{cwd: "/code/nightme"}
	got := RebuildContext(context.Background(), cs, g, fakeNewPlatform)
	if got != (Context{}) {
		t.Fatalf("RebuildContext = %+v, want zero", got)
	}
}

// fakeSender satisfies gtw.Sender for tests.
type fakeSender struct{ cwd string }

func (f *fakeSender) ActiveCwd() string             { return f.cwd }
func (f *fakeSender) SetActiveCwd(cwd string) error { f.cwd = cwd; return nil }

// fakeNewPlatform returns a no-op client; tests that need
// richer behaviour build their own.
func fakeNewPlatform(_ PlatformKind) (PlatformClient, error) {
	return &fakePlatform{}, nil
}

type fakePlatform struct {
	issue *Issue
	err   error
}

func (f *fakePlatform) Kind() PlatformKind { return PlatformGitHub }
func (f *fakePlatform) GetIssue(_ context.Context, _, _ string, id int) (*Issue, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.issue == nil {
		return &Issue{ID: id, Title: "fake", State: "open", Labels: []string{LabelWIP}}, nil
	}
	// Return a copy with the requested ID stamped in.
	c := *f.issue
	c.ID = id
	return &c, nil
}
func (f *fakePlatform) AddLabel(context.Context, string, string, int, string) error {
	return nil
}
func (f *fakePlatform) RemoveLabel(context.Context, string, string, int, string) error {
	return nil
}

// ---- full happy path ---------------------------------------------

// TestRunFix_HappyPath exercises the entire §5.2 main flow:
// preflight → fetch issue → add label → create worktree →
// switch cwd → write gtwContext → render success card.
//
// It uses a real GitRunner wrapping `git` against a temporary
// repo so the worktree add is exercised end-to-end. Skipped if
// git is not on PATH.
func TestRunFix_HappyPath(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; skipping end-to-end happy path")
	}

	// Set up a tiny git repo with a github remote. Resolve the
	// temp dir through EvalSymlinks so we agree with git on the
	// canonical path (macOS temp dirs live under /private/var/...,
	// which differs from what t.TempDir() returns).
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"remote", "add", "origin", "https://github.com/cnlangzi/nightme"},
		{"commit", "--allow-empty", "-q", "-m", "initial"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	sent := []OutMsg{}
	plat := &fakePlatform{issue: &Issue{ID: 42, Title: "Login state expiration", State: "open"}}
	sender := &fakeSender{cwd: dir}
	slot := &fakeContextSlot{}
	drafts := &fakeDraftsMap{}
	deps := HandlerDeps{
		Send: func(_ context.Context, m OutMsg) error { sent = append(sent, m); return nil },
		Git:  ExecGitRunner{},
		NewPlatform: func(_ PlatformKind) (PlatformClient, error) { return plat, nil },
		Now: func() time.Time {
			return time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
		},
	}

	res, err := RunFix(context.Background(), sender, slot, drafts, deps,
		"chat-1", "msg-1", []string{"42"})
	if err != nil {
		t.Fatalf("RunFix: %v", err)
	}
	if !res.Consumed {
		t.Fatalf("RunFix result = %+v, want Consumed=true", res)
	}

	// gtwContext populated correctly.
	got := slot.Load()
	if got.Issue != 42 {
		t.Errorf("ctx.Issue = %d, want 42", got.Issue)
	}
	if got.Branch != "fix/42-login-state-expiration" {
		t.Errorf("ctx.Branch = %q", got.Branch)
	}
	if got.State != StateFixing {
		t.Errorf("ctx.State = %q, want %q", got.State, StateFixing)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("ctx.UpdatedAt is zero")
	}

	// CWD switched to the new worktree.
	wantWorktree := filepath.Join(filepath.Dir(dir), filepath.Base(dir)+".nightme", "42-login-state-expiration")
	if sender.cwd != wantWorktree {
		t.Errorf("sender.cwd = %q, want %q", sender.cwd, wantWorktree)
	}

	// One success card sent (plus the label-ok path doesn't send
	// anything when AddLabel succeeds; the AddLabel-fail branch
	// would send a warning card).
	if len(sent) != 1 {
		t.Fatalf("Send count = %d, want 1; sent = %+v", len(sent), sent)
	}
	if !strings.Contains(sent[0].Text, "Fix #42") {
		t.Errorf("card text = %q, want containing 'Fix #42'", sent[0].Text)
	}
	if sent[0].ReplyTo != "msg-1" {
		t.Errorf("ReplyTo = %q, want msg-1 (thread under user command)", sent[0].ReplyTo)
	}
}

// TestRunFix_DaemonRecovery exercises the §5.7 path: cwd is inside
// a worktree holding `fix/42-...` but the in-memory gtwContext is
// empty. RunFix should detect this via RebuildContext, restore
// the context, and emit a "Recovered" card instead of creating a
// duplicate worktree.
func TestRunFix_DaemonRecovery(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; skipping daemon-recovery test")
	}

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	wtPath := filepath.Join(dir, "wt")
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"remote", "add", "origin", "https://github.com/cnlangzi/nightme"},
		{"commit", "--allow-empty", "-q", "-m", "initial"},
		// Worktree add creates the branch as a side effect — we
		// don't pre-create it (would cause "branch already exists").
		{"worktree", "add", "-b", "fix/42-recovery-test", wtPath, "HEAD"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	sent := []OutMsg{}
	sender := &fakeSender{cwd: wtPath}
	slot := &fakeContextSlot{}
	deps := HandlerDeps{
		Send: func(_ context.Context, m OutMsg) error { sent = append(sent, m); return nil },
		Git:  ExecGitRunner{},
		NewPlatform: func(_ PlatformKind) (PlatformClient, error) {
			return &fakePlatform{issue: &Issue{ID: 42, Title: "recovery test", State: "open"}}, nil
		},
		Now: func() time.Time { return time.Now() },
	}

	// Slot is empty (post-restart) — RunFix should rebuild.
	res, err := RunFix(context.Background(), sender, slot, &fakeDraftsMap{}, deps,
		"chat-1", "msg-1", []string{"99"}) // 99 != 42; rebuild should win
	if err != nil {
		t.Fatalf("RunFix: %v", err)
	}
	if !res.Consumed {
		t.Fatal("RunFix not Consumed")
	}
	// Slot was populated with the rebuilt context for issue 42,
	// not the requested 99.
	got := slot.Load()
	if got.Issue != 42 {
		t.Errorf("ctx.Issue = %d, want 42 (rebuilt from branch)", got.Issue)
	}
	if got.Branch != "fix/42-recovery-test" {
		t.Errorf("ctx.Branch = %q, want fix/42-recovery-test", got.Branch)
	}
	// One recovery card was sent.
	if len(sent) != 1 {
		t.Fatalf("Send count = %d, want 1", len(sent))
	}
	if !strings.Contains(sent[0].Text, "Recovered") {
		t.Errorf("card text = %q, want containing 'Recovered'", sent[0].Text)
	}
	if !strings.Contains(sent[0].Text, "#42") {
		t.Errorf("card text = %q, want containing '#42'", sent[0].Text)
	}
}

// ---- reaction routing -------------------------------------------

// TestHandleAction_BranchExists_ConfirmCancellation exercises
// the end-to-end reaction flow that the F-45 §5.3.1 card relies
// on: a draft is stored, a ❌ reaction arrives, the draft is
// taken, the label is rolled back, and a cancellation card is
// sent. We use the fakeGit from earlier + a recording Send to
// observe the side effects without real git/gh.
func TestHandleAction_BranchExists_ConfirmCancellation(t *testing.T) {
	sent := []OutMsg{}
	drafts := &fakeDraftsMap{}
	slot := &fakeContextSlot{}
	cs := &fakeSender{cwd: "/code/nightme"}
	deps := HandlerDeps{
		Send: func(_ context.Context, m OutMsg) error { sent = append(sent, m); return nil },
		Git:  &fakeGit{},
		NewPlatform: func(_ PlatformKind) (PlatformClient, error) {
			return &fakePlatform{}, nil
		},
	}

	// Pre-populate a branch-exists draft (the kind RunFix
	// would have stored when the worktree add hit a name
	// collision). LabelAdded is false in this path — the
	// label hasn't been added yet at the time the card is
	// emitted, so ❌ should NOT call RemoveLabel.
	drafts.Store("om_card_msg", &Draft{
		Kind: DraftFixBranchExists,
		Payload: FixDraftPayload{
			IssueID:  42,
			Branch:   "fix/42-login-state-expiration",
			Slug:     "42-login-state-expiration",
			Repo:     "cnlangzi/nightme",
			Platform: "github",
			ChatID:   "chat-1",
		},
	})

	consumed, err := HandleAction(context.Background(), deps, cs, slot, drafts, ReactionEvent{
		TargetMsgID: "om_card_msg",
		Emoji:       string(ReactionCancel),
		UserID:      "ou_user_1",
		ChatID:      "chat-1",
	})
	if err != nil {
		t.Fatalf("HandleAction: %v", err)
	}
	if !consumed {
		t.Error("consumed = false, want true (draft matched)")
	}
	// Draft must be taken (one-shot per reaction).
	if got := drafts.Lookup("om_card_msg"); got != nil {
		t.Errorf("draft not taken: %+v", got)
	}
	// One cancellation card sent.
	if len(sent) != 1 {
		t.Fatalf("Send count = %d, want 1; sent = %+v", len(sent), sent)
	}
	if !strings.Contains(sent[0].Text, "Cancelled fix #42") {
		t.Errorf("card text = %q, want containing 'Cancelled fix #42'", sent[0].Text)
	}
}

// TestHandleAction_NoDraftFallsThrough verifies the
// non-consumption path: a reaction on a non-gtw message
// returns (false, nil) so the caller can fall through to
// future handlers (none today; placeholder for F-31+).
func TestHandleAction_NoDraftFallsThrough(t *testing.T) {
	cs := &fakeSender{cwd: "/code/nightme"}
	deps := HandlerDeps{
		Send: func(_ context.Context, _ OutMsg) error { return nil },
		Git:  &fakeGit{},
		NewPlatform: func(_ PlatformKind) (PlatformClient, error) { return nil, nil },
	}
	consumed, _ := HandleAction(context.Background(), deps, cs,
		&fakeContextSlot{}, &fakeDraftsMap{},
		ReactionEvent{TargetMsgID: "om_random", Emoji: "👍"},
	)
	if consumed {
		t.Error("consumed = true, want false (no draft matched)")
	}
}

// TestHandleAction_EmptyTargetMsgIDIgnored verifies the
// defensive early-return for malformed events (e.g. SDK
// delivering a half-parsed reaction).
func TestHandleAction_EmptyTargetMsgIDIgnored(t *testing.T) {
	cs := &fakeSender{cwd: "/code/nightme"}
	deps := HandlerDeps{
		Send: func(_ context.Context, _ OutMsg) error { return nil },
		Git:  &fakeGit{},
	}
	consumed, _ := HandleAction(context.Background(), deps, cs,
		&fakeContextSlot{}, &fakeDraftsMap{},
		ReactionEvent{TargetMsgID: "", Emoji: "✅"},
	)
	if consumed {
		t.Error("consumed = true, want false (empty target)")
	}
}

// ---- branch-exists path uses fake ----------------------------------

func TestBranchExists_True(t *testing.T) {
	g := &fakeGit{responses: map[string]fakeGitResp{
		"show-ref --verify --quiet refs/heads/fix/42-foo": {},
	}}
	exists, err := BranchExists(context.Background(), "/code/nightme", "fix/42-foo", g)
	if err != nil {
		t.Fatalf("BranchExists: %v", err)
	}
	if !exists {
		t.Fatal("BranchExists = false, want true")
	}
}

func TestBranchExists_FalseOnMiss(t *testing.T) {
	// "Miss" is signalled by a non-zero exit with EMPTY stderr
	// (git --quiet suppresses all output on a clean miss). A
	// non-empty stderr indicates a real error.
	g := &fakeGit{responses: map[string]fakeGitResp{
		"show-ref --verify --quiet refs/heads/fix/99-missing": {
			err: &fakeExitErr{code: 1},
		},
	}}
	exists, err := BranchExists(context.Background(), "/code/nightme", "fix/99-missing", g)
	if err != nil {
		t.Fatalf("BranchExists: %v", err)
	}
	if exists {
		t.Fatal("BranchExists = true, want false")
	}
}

func TestBranchExists_RealErrorBubblesUp(t *testing.T) {
	g := &fakeGit{responses: map[string]fakeGitResp{
		"show-ref --verify --quiet refs/heads/fix/99-broken": {
			stderr: "fatal: bad ref",
			err:    &fakeExitErr{code: 128},
		},
	}}
	_, err := BranchExists(context.Background(), "/code/nightme", "fix/99-broken", g)
	if err == nil {
		t.Fatal("BranchExists = nil err, want real error")
	}
}

// ---- rebuild further: branch on fix/<id>-* finds matching issue ---

func TestRebuildContext_FoundIssue(t *testing.T) {
	g := &fakeGit{responses: map[string]fakeGitResp{
		"symbolic-ref --short HEAD":            {stdout: "fix/42-login-state-expiration\n"},
		"remote get-url origin":                {stdout: "git@github.com:cnlangzi/nightme.git\n"},
	}}
	cs := &fakeSender{cwd: "/code/nightme.nightme/42-login-state-expiration"}
	ctx := RebuildContext(context.Background(), cs, g, fakeNewPlatform)
	if ctx == (Context{}) {
		t.Fatal("RebuildContext = zero, want populated")
	}
	if ctx.Issue != 42 {
		t.Errorf("ctx.Issue = %d, want 42", ctx.Issue)
	}
	if ctx.Branch != "fix/42-login-state-expiration" {
		t.Errorf("ctx.Branch = %q, want fix/42-login-state-expiration", ctx.Branch)
	}
	if ctx.State != StateFixing {
		t.Errorf("ctx.State = %q, want %q", ctx.State, StateFixing)
	}
}

// ---- handler-deps test (smoke) ------------------------------------

func TestRunFix_UsageWhenNoArgs(t *testing.T) {
	sent := []OutMsg{}
	deps := HandlerDeps{
		Send: func(_ context.Context, m OutMsg) error { sent = append(sent, m); return nil },
		Git:  &fakeGit{},
	}
	res, err := RunFix(context.Background(), &fakeSender{cwd: "/code/nightme"},
		&fakeContextSlot{}, &fakeDraftsMap{}, deps,
		"chat-1", "msg-1", []string{})
	if err != nil {
		t.Fatalf("RunFix: %v", err)
	}
	if res == nil || !res.Consumed {
		t.Fatalf("RunFix result = %+v, want Consumed=true", res)
	}
	if len(sent) != 1 {
		t.Fatalf("Send count = %d, want 1", len(sent))
	}
	if !strings.Contains(sent[0].Text, "Usage") {
		t.Fatalf("reply text = %q, want containing 'Usage'", sent[0].Text)
	}
}

func TestRunFix_PreflightRequiresCwd(t *testing.T) {
	sent := []OutMsg{}
	deps := HandlerDeps{
		Send: func(_ context.Context, m OutMsg) error { sent = append(sent, m); return nil },
		Git:  &fakeGit{},
	}
	res, _ := RunFix(context.Background(), &fakeSender{cwd: ""},
		&fakeContextSlot{}, &fakeDraftsMap{}, deps,
		"chat-1", "msg-1", []string{"42"})
	if res == nil || !res.Consumed {
		t.Fatalf("RunFix result = %+v, want Consumed=true", res)
	}
	if len(sent) != 1 || !strings.Contains(sent[0].Text, "No active workspace") {
		t.Fatalf("expected 'No active workspace' reply, got %+v", sent)
	}
}

// fakeContextSlot / fakeDraftsMap satisfy the package's interfaces.
type fakeContextSlot struct {
	c Context
}

func (f *fakeContextSlot) Load() Context   { return f.c }
func (f *fakeContextSlot) Store(c Context) { f.c = c }

type fakeDraftsMap struct {
	m map[string]*Draft
}

func (f *fakeDraftsMap) Store(id string, d *Draft) {
	if f.m == nil {
		f.m = map[string]*Draft{}
	}
	f.m[id] = d
}
func (f *fakeDraftsMap) Take(id string) *Draft {
	d, ok := f.m[id]
	if !ok {
		return nil
	}
	delete(f.m, id)
	return d
}
func (f *fakeDraftsMap) Lookup(id string) *Draft { return f.m[id] }

// suppress unused-time warning when time isn't referenced.
var _ = time.Now
