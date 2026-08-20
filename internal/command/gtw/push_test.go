package gtw

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/messages"
)

// pushGit is a GitRunner whose responses are configured per
// argv-prefix. Tests register (prefix → response) tuples; unmatched
// git calls return an error so the test fails fast on a surprise.
//
// One-off: each Run returns the captured stdout/stderr/error triple.
type pushGit struct {
	mu              sync.Mutex
	responses       map[string]pushGitResp
	responsesByArgs map[string]pushGitResp
	// seqResponses[prefix] is the ordered list of responses
	// onSeq registered; the Nth call matching prefix gets the
	// Nth entry, last entry reused once exhausted. Call count
	// is per-prefix so a sequence on "rev-list" doesn't get
	// advanced by intervening "status" or "push" calls.
	seqResponses       map[string][]pushGitResp
	seqCallCount       map[string]int
	seqResponsesByArgs map[string][]pushGitResp
	seqArgsCallCount   map[string]int
	calls              []pushGitCall
}

type pushGitResp struct {
	stdout string
	stderr string
	err    error
}

type pushGitCall struct {
	dir  string
	args []string
}

func newPushGit() *pushGit {
	return &pushGit{
		responses:          make(map[string]pushGitResp),
		responsesByArgs:    make(map[string]pushGitResp),
		seqResponses:       make(map[string][]pushGitResp),
		seqCallCount:       make(map[string]int),
		seqResponsesByArgs: make(map[string][]pushGitResp),
		seqArgsCallCount:   make(map[string]int),
	}
}

// on registers a response keyed by the FIRST argv token (e.g.
// "status", "rev-parse", "rev-list", "push"). The full argv is
// still recorded in calls for later assertion.
func (f *pushGit) on(prefix string, stdout, stderr string, err error) {
	f.responses[prefix] = pushGitResp{stdout, stderr, err}
}

// onArgs registers a response keyed by the FULL argv slice
// joined with NUL (a separator no shell argv can have). Used
// when two calls share the same first token (e.g. both
// countUnpushed and countBaseAhead run `git rev-list ...`,
// but with different range syntaxes — first-token matching
// would alias both to one response). Run first tries onArgs
// matches; only falls back to on() when none match.
func (f *pushGit) onArgs(args []string, stdout, stderr string, err error) {
	f.responsesByArgs[joinArgs(args)] = pushGitResp{stdout, stderr, err}
}

// onArgsSeq registers a SEQUENCE of responses for the same
// full-argv slice. The Nth Run call with matching full argv
// gets the Nth response; the LAST response is reused once
// exhausted.
func (f *pushGit) onArgsSeq(args []string, responses ...pushGitResp) {
	if len(responses) == 0 {
		return
	}
	key := joinArgs(args)
	f.responsesByArgs[key] = responses[0]
	f.seqResponsesByArgs[key] = responses
}

// onSeq registers responses that fire in call order for a given
// argv prefix. The Nth Run call matching prefix gets the Nth
// response (callIdx is 0-based). If the call count exceeds the
// number of registered responses, the LAST response is reused.
func (f *pushGit) onSeq(prefix string, responses ...pushGitResp) {
	if len(responses) == 0 {
		return
	}
	f.responses[prefix] = responses[0]
	f.seqResponses[prefix] = responses
}

func (f *pushGit) Run(_ context.Context, dir string, args ...string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, pushGitCall{dir: dir, args: append([]string(nil), args...)})
	if len(args) == 0 {
		return "", "", errors.New("pushGit: empty argv")
	}
	key := joinArgs(args)
	if seq, ok := f.seqResponsesByArgs[key]; ok {
		idx := f.seqArgsCallCount[key]
		f.seqArgsCallCount[key] = idx + 1
		if idx >= len(seq) {
			idx = len(seq) - 1
		}
		resp := seq[idx]
		return resp.stdout, resp.stderr, resp.err
	}
	if resp, ok := f.responsesByArgs[key]; ok {
		return resp.stdout, resp.stderr, resp.err
	}
	if seq, ok := f.seqResponses[args[0]]; ok {
		idx := f.seqCallCount[args[0]]
		f.seqCallCount[args[0]] = idx + 1
		if idx >= len(seq) {
			idx = len(seq) - 1
		}
		resp := seq[idx]
		return resp.stdout, resp.stderr, resp.err
	}
	resp, ok := f.responses[args[0]]
	if !ok {
		return "", "", errors.New("pushGit: no response for " + args[0])
	}
	return resp.stdout, resp.stderr, resp.err
}

// joinArgs is a tiny helper used by onArgs/Run to serialise an
// argv slice into a map key. NUL is a safe separator because
// git argv tokens can't contain it.
func joinArgs(args []string) string {
	return strings.Join(args, "\x00")
}

// recordingAgent is a minimal agent.Starter for the dispatcher
// tests. It captures RunOnce calls and returns a configurable
// (text, err) pair. Lives in this test file (not registry_test.go)
// because the gtw tests need a fake that satisfies agent.Starter;
// the agent package's fakes are too tightly coupled to registry
// semantics.
//
// Start is a stub: gtw tests drive RunOnce directly, so the
// Spawn path is never exercised here. Returning nil from Start
// is safe because dispatchPush never calls it (the dispatcher
// only uses RunOnce).
type recordingAgent struct {
	name        string
	detectErr   error
	runOnceText string
	runOnceErr  error
	mu          sync.Mutex
	calls       []runOnceCall
}

type runOnceCall struct {
	workspace string
	blocks    []agent.ContentBlock
}

func (r *recordingAgent) Info() agent.Info {
	return agent.NewInfo(r.name, agent.ModePTY, "fake-"+r.name, nil, nil)
}
func (r *recordingAgent) Detect() error { return r.detectErr }
func (r *recordingAgent) Start(context.Context, agent.StartConfig) (*agent.Agent, error) {
	return nil, errors.New("recordingAgent: Start not implemented")
}
func (r *recordingAgent) RunOnce(_ context.Context, cfg agent.StartConfig, blocks []agent.ContentBlock) (agent.RunResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, runOnceCall{workspace: cfg.Workspace, blocks: append([]agent.ContentBlock(nil), blocks...)})
	return agent.RunResult{Text: r.runOnceText}, r.runOnceErr
}

// Review is unimplemented for the gtw-push test fake — push tests
// never invoke /review, so returning ErrReviewNotSupported keeps
// the agent.Starter interface satisfied without affecting push
// semantics.
func (r *recordingAgent) Review(_ context.Context, _ agent.StartConfig) (agent.RunResult, error) {
	return agent.RunResult{}, agent.ErrReviewNotSupported
}

// withAgent swaps agent.Builtins for a local registry containing
// just the supplied fakes. Restores on test cleanup.
func withAgent(t *testing.T, agents ...*recordingAgent) {
	t.Helper()
	orig := agent.Builtins
	clean := agent.New()
	for _, a := range agents {
		clean.Register(a)
	}
	agent.Builtins = clean
	t.Cleanup(func() { agent.Builtins = orig })
}

// writeYml drops a minimal .nightme/gtw.yml in dir so the
// dispatchers can read it via pushCwd(). It patches pushCwd's
// behaviour by also chdir-ing the test process — which is
// acceptable because pushCwd shells out to `pwd` and the
// dispatcher reads yml from that path.
func writeYml(t *testing.T, dir string, c Context) {
	t.Helper()
	nightmeDir := filepath.Join(dir, ".nightme")
	if err := os.MkdirAll(nightmeDir, 0o755); err != nil {
		t.Fatalf("mkdir .nightme: %v", err)
	}
	data := buildTestYml(c)
	if err := os.WriteFile(filepath.Join(nightmeDir, "gtw.yml"), []byte(data), 0o644); err != nil {
		t.Fatalf("write gtw.yml: %v", err)
	}
}

// buildTestYml is intentionally minimal — only fields the
// dispatchers read (Worktree/Branch/RepoRoot/Issue). The parser
// ignores unknown keys.
//
// Repo + Provider are emitted when populated so /gtw pr tests
// can exercise the yml-pinned resolveProvider path without
// standing up a Detect fallback.
func buildTestYml(c Context) string {
	var sb strings.Builder
	sb.WriteString("worktree: " + c.Worktree + "\n")
	sb.WriteString("branch: " + c.Branch + "\n")
	sb.WriteString("repoRoot: " + c.RepoRoot + "\n")
	if c.Issue > 0 {
		sb.WriteString("issue: " + itoa10(c.Issue) + "\n")
	}
	if c.Repo != "" {
		sb.WriteString("repo: " + c.Repo + "\n")
	}
	if c.Provider != "" {
		sb.WriteString("provider: " + c.Provider + "\n")
	}
	return sb.String()
}

func itoa10(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// withCwd chdirs to dir for the duration of the test. pushCwd()
// shells out to `pwd` to read the daemon's cwd, so this is the
// only way to make dispatchPush see our yml.
func withCwd(t *testing.T, dir string) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
}

func mustPwd(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return dir
}

// newPushChatSession returns a *chatsession.ChatSession with
// SelectedCwd set to the test's current working directory (the
// same dir the yml helper writes to). loadDispatchContext reads
// cs.SelectedCwd() (not system pwd), so each dispatcher test
// needs a chat that mirrors the system-pwd that withCwd() set.
func newPushChatSession(t *testing.T) *chatsession.ChatSession {
	t.Helper()
	cs := &chatsession.ChatSession{}
	_ = cs.SetSelectedCwd(mustPwd(t))
	return cs
}

// -----------------------------------------------------------------------------
// parsePushArgs tests
// -----------------------------------------------------------------------------

func TestParsePushArgs_ShortFlag(t *testing.T) {
	got, err := parsePushArgs([]string{"-a", "claude"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.Agent != "claude" {
		t.Fatalf("Agent = %q, want claude", got.Agent)
	}
}

func TestParsePushArgs_LongFlag(t *testing.T) {
	got, err := parsePushArgs([]string{"--agent", "opencode"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.Agent != "opencode" {
		t.Fatalf("Agent = %q, want opencode", got.Agent)
	}
}

func TestParsePushArgs_Empty(t *testing.T) {
	got, err := parsePushArgs(nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.Agent != "" {
		t.Fatalf("Agent = %q, want empty", got.Agent)
	}
}

func TestParsePushArgs_MissingValue(t *testing.T) {
	_, err := parsePushArgs([]string{"-a"})
	if err == nil {
		t.Fatalf("missing value should error")
	}
}

func TestParsePushArgs_Multiple(t *testing.T) {
	got, err := parsePushArgs([]string{"-a", "opencode", "-a", "claude"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.Agent != "claude" {
		t.Fatalf("Agent = %q, want claude (last one wins)", got.Agent)
	}
}

// -----------------------------------------------------------------------------
// dispatchPush tests (F-56 + F-57 + F-XX split)
//
// After the F-XX commit/push split, dispatchPush is push-only:
// it has Branch 1 (no-op) and Branch 3 (push). Branch 2
// (agent+push) is now dispatchCommit. The dirty-worktree case
// that used to fall through to Branch 2 is now a hard refusal
// (TestRunPush_DirtyRefused below) — push tells the user to
// commit first.
// -----------------------------------------------------------------------------

// TestRunPush_CleanNoUnpushed — Branch 1 no-op. Clean tree +
// upstream exists + ahead=0 → "ℹ️ nothing to push".
func TestRunPush_CleanNoUnpushed(t *testing.T) {
	git := newPushGit()
	// F-57: dispatchPush's first readiness probe is
	// `status --porcelain --branch --untracked-files=normal`.
	git.onArgs(statusCmd, "## wt-clean...origin/wt-clean\n", "", nil)

	withAgent(t)
	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-clean",
		RepoRoot: mustPwd(t),
	})

	cs := newPushChatSession(t)
	s := captureCh(t, cs)
	res, err := dispatchPush(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{})

	if err != nil || res == nil {
		t.Fatalf("dispatchPush err=%v res=%v", err, res)
	}
	for _, c := range git.calls {
		if c.args[0] == "push" {
			t.Fatalf("clean + 0 unpushed should NOT call git push: %v", c.args)
		}
	}
	r := s.lastText()
	if !strings.Contains(r, "ℹ️ nothing to push") {
		t.Errorf("expected Branch 1 no-op card, got:\n%s", r)
	}
}

// TestRunPush_CleanWithUnpushed — Branch 3. Clean + has upstream
// + ahead=3 → programmatic push with retry verify.
//
// Mock reflects REAL git semantics (this used to be wrong — see
// the "pushed 0 commits" bug fixed alongside this rename). After
// a successful push, `origin/<branch>` advances to the same SHA
// as HEAD, so the success card MUST read commits off the
// *previous* origin tip (captured before the push into
// `originBefore`) not off `headBefore`.
func TestRunPush_CleanWithUnpushed(t *testing.T) {
	git := newPushGit()
	git.onArgs(statusCmd, "## wt-clean-unpushed...origin/wt-clean-unpushed [ahead 3]\n", "", nil)
	// origin/<branch> BEFORE push = aaaa… (40 chars). dispatchPush
	// captures this into `originBefore`, then re-uses it as the
	// left side of the post-push rev range
	// `aaaa..origin/wt-clean-unpushed`. The exact same 40-char
	// string MUST appear in the log mock below — otherwise the
	// post-push `git log` call lands on a key with no registered
	// response and replyPushSuccessCard errors out (this is what
	// masked the original bug for so long: the old test "worked"
	// only because the broken rev range collapsed to empty).
	git.onArgs([]string{"rev-parse", "--verify", "origin/wt-clean-unpushed"},
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n", "", nil)
	// rev-list calls: countUnpushed pre-push (3) and post-push
	// verify (0). The 0 lets programmaticPushWithRetry exit cleanly.
	git.onSeq("rev-list",
		pushGitResp{"3", "", nil},
		pushGitResp{"0", "", nil},
	)
	git.on("push", "To origin", "", nil)
	// Post-push log of `aaaa…..origin/wt-clean-unpushed` lists the
	// 3 commits that just landed. Used by replyPushSuccessCard.
	git.onArgs([]string{"log", "--oneline", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa..origin/wt-clean-unpushed"},
		"abc1234 commit 1\ndef5678 commit 2\n123abcd commit 3\n", "", nil)

	withAgent(t)
	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-clean-unpushed",
		RepoRoot: mustPwd(t),
	})

	cs := newPushChatSession(t)
	s := captureCh(t, cs)
	_, err := dispatchPush(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{})

	if err != nil {
		t.Fatalf("RunPush: %v", err)
	}
	pushed := false
	for _, c := range git.calls {
		if c.args[0] == "push" {
			pushed = true
		}
	}
	if !pushed {
		t.Fatalf("expected git push call, got %v", git.calls)
	}
	r := s.lastText()
	if !strings.Contains(r, "✅ pushed 3 commit(s)") {
		t.Errorf("expected success card to count the 3 pushed commits, got:\n%s", r)
	}
	if !strings.Contains(r, "wt-clean-unpushed") {
		t.Errorf("expected card to name the branch, got:\n%s", r)
	}
	if strings.Contains(r, "🤖") {
		t.Errorf("Branch 3 should not credit an agent, got:\n%s", r)
	}
}

// TestRunPush_FirstPush_NoUpstream — Branch 3 first-push case.
// Branch has never been on origin, so `rev-parse origin/<branch>`
// fails; dispatchPush sets originBefore="" and the success card
// falls back to `git log origin/<default>..origin/<branch>`
// (anchored to the default branch) to show ONLY the commits this
// push uploaded — not the entire local branch history.
//
// Regression for the over-counting bug found after the F-56/first
// push fix: previously this fallback used `git log <branch>` which
// would have included main's whole history when the branch was
// forked from main, making the card say "pushed N+main commits"
// instead of "pushed N".
func TestRunPush_FirstPush_NoUpstream(t *testing.T) {
	git := newPushGit()
	git.onArgs(statusCmd, "## wt-fresh-branch\n", "", nil)
	// origin/<branch> does NOT exist — git exits non-zero with
	// "unknown revision" on stderr. dispatchPush catches this and
	// sets originBefore="".
	git.onArgs([]string{"rev-parse", "--verify", "origin/wt-fresh-branch"},
		"", "fatal: unknown revision or path not in the working tree: 'origin/wt-fresh-branch'", errors.New("exit status 128"))
	// countUnpushed pre-push: branch@{u} is unset so countUnpushed
	// returns 0 via the "no upstream configured" branch. Post-push
	// verify also returns 0 (upstream now equals tip).
	git.onSeq("rev-list",
		pushGitResp{"0", "", nil},
		pushGitResp{"0", "", nil},
	)
	git.on("push", "To origin\n * [new branch]      wt-fresh-branch -> wt-fresh-branch\n", "", nil)
	// DefaultBranch lookup: the repo's default is "main".
	git.onArgs([]string{"symbolic-ref", "--short", "refs/remotes/origin/HEAD"},
		"origin/main\n", "", nil)
	// First-push fallback: log of origin/main..origin/wt-fresh-branch
	// lists the 2 commits that landed — NOT the 5 commits of main's
	// history that the branch was forked from.
	git.onArgs([]string{"log", "--oneline", "origin/main..origin/wt-fresh-branch"},
		"beef000 initial commit\ndead111 second commit\n", "", nil)

	withAgent(t)
	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-fresh-branch",
		RepoRoot: mustPwd(t),
	})

	cs := newPushChatSession(t)
	s := captureCh(t, cs)
	_, err := dispatchPush(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{})
	if err != nil {
		t.Fatalf("RunPush: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "✅ pushed 2 commit(s)") {
		t.Errorf("first-push card must count the 2 commits that landed, got:\n%s", r)
	}
	if !strings.Contains(r, "wt-fresh-branch") {
		t.Errorf("expected branch name in card, got:\n%s", r)
	}
	// Anti-regression: main's commits must NOT leak into the count.
	if strings.Contains(r, "✅ pushed 7 commit(s)") ||
		strings.Contains(r, "✅ pushed 5 commit(s)") {
		t.Errorf("first-push card over-counted (likely listed main's history), got:\n%s", r)
	}
}

// TestRunPush_FirstPush_PushingDefaultBranch — Branch 3 first-push
// case where the branch being pushed IS the default branch itself
// (e.g. fresh repo, no prior `git push`). originBefore="" AND the
// default-branch look-up returns c.Branch itself, so the helper
// falls back to listing the entire branch — which IS what was
// pushed (everything reachable from local main).
func TestRunPush_FirstPush_PushingDefaultBranch(t *testing.T) {
	git := newPushGit()
	git.onArgs(statusCmd, "## main\n", "", nil)
	git.onArgs([]string{"rev-parse", "--verify", "origin/main"},
		"", "fatal: unknown revision or path not in the working tree: 'origin/main'", errors.New("exit status 128"))
	git.onSeq("rev-list",
		pushGitResp{"0", "", nil},
		pushGitResp{"0", "", nil},
	)
	git.on("push", "To origin\n * [new branch]      main -> main\n", "", nil)
	// DefaultBranch: same as the branch we're pushing → fallback path.
	git.onArgs([]string{"symbolic-ref", "--short", "refs/remotes/origin/HEAD"},
		"origin/main\n", "", nil)
	// Entire branch history — that's what was pushed.
	git.onArgs([]string{"log", "--oneline", "main"},
		"aaa000 initial\nbbb001 second\nccc002 third\n", "", nil)

	withAgent(t)
	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "main",
		RepoRoot: mustPwd(t),
	})

	cs := newPushChatSession(t)
	s := captureCh(t, cs)
	_, err := dispatchPush(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{})
	if err != nil {
		t.Fatalf("RunPush: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "✅ pushed 3 commit(s)") {
		t.Errorf("pushing default branch first time should list all 3 commits, got:\n%s", r)
	}
}

// TestRunPush_FirstPush_NoOriginRemote — degenerate case: brand-new
// repo with no `origin/HEAD` set. firstPushRevRange's DefaultBranch
// lookup fails; the helper falls back to `c.Branch`, listing
// everything reachable (which IS what was uploaded).
func TestRunPush_FirstPush_NoOriginRemote(t *testing.T) {
	git := newPushGit()
	git.onArgs(statusCmd, "## solo-branch\n", "", nil)
	git.onArgs([]string{"rev-parse", "--verify", "origin/solo-branch"},
		"", "fatal: unknown revision or path not in the working tree: 'origin/solo-branch'", errors.New("exit status 128"))
	git.onSeq("rev-list",
		pushGitResp{"0", "", nil},
		pushGitResp{"0", "", nil},
	)
	git.on("push", "To origin\n * [new branch]      solo-branch -> solo-branch\n", "", nil)
	// DefaultBranch errors — no origin/HEAD.
	git.onArgs([]string{"symbolic-ref", "--short", "refs/remotes/origin/HEAD"},
		"", "fatal: no such ref", errors.New("exit status 128"))
	// Falls back to `git log solo-branch` — lists all 4 reachable
	// commits (which is correct: this is the only thing on origin).
	git.onArgs([]string{"log", "--oneline", "solo-branch"},
		"111 first\n222 second\n333 third\n444 fourth\n", "", nil)

	withAgent(t)
	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "solo-branch",
		RepoRoot: mustPwd(t),
	})

	cs := newPushChatSession(t)
	s := captureCh(t, cs)
	_, err := dispatchPush(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{})
	if err != nil {
		t.Fatalf("RunPush: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "✅ pushed 4 commit(s)") {
		t.Errorf("no-origin-remote case should list all 4 reachable commits, got:\n%s", r)
	}
}

// TestRunPush_VerifyDetectsSilentPushFailure — regression for
// the case where `git push` exits 0 but commits didn't land. The
// retry+verify loop catches it and surfaces an unpushed-commits
// diagnostic.
func TestRunPush_VerifyDetectsSilentPushFailure(t *testing.T) {
	git := newPushGit()
	git.onArgs(statusCmd, "## wt-silent-fail...origin/wt-silent-fail [ahead 3]\n", "", nil)
	git.onArgs([]string{"rev-parse", "--verify", "origin/wt-silent-fail"},
		"cccccccccccccccccccccccccccccccccccccccc\n", "", nil)
	// All rev-list calls return "1" — the push never lands
	// upstream, even after retry.
	git.on("rev-list", "1", "", nil)
	git.on("push", "To origin", "", nil)

	withAgent(t)
	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-silent-fail",
		RepoRoot: mustPwd(t),
	})

	cs := newPushChatSession(t)
	s := captureCh(t, cs)
	_, err := dispatchPush(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{})
	if err != nil {
		t.Fatalf("RunPush: %v", err)
	}
	r := s.lastText()
	if strings.Contains(r, "✅ pushed") {
		t.Fatalf("expected surface-on-failure diagnostic, got success card:\n%s", r)
	}
	if !strings.Contains(r, "still don't appear on origin/wt-silent-fail") {
		t.Fatalf("expected diagnostic to name unpushed commits, got:\n%s", r)
	}
	if !strings.Contains(r, "after retry") {
		t.Fatalf("expected retry hint in diagnostic, got:\n%s", r)
	}
}

// TestRunPush_PushErrorSurfaced — regression for the silent
// failure mode where `git push` itself errors (auth, network,
// remote protection rule). Pre-fix, programmaticPushWithRetry
// discarded the push error and relied solely on the post-push
// `unpushedIsZero` verify. On a fresh branch (no @{u} yet),
// countUnpushed returns 0 silently via its "no upstream
// configured" branch, the verify "succeeds", and dispatchPush
// renders a misleading "✅ pushed 0 commit(s)" card while
// /gtw pr correctly reports "branch has no upstream". This test
// pins the new contract: when `git push` itself errors, the
// error must surface — not be masked by a phantom success
// card.
//
// Two attempts of the push are scripted to also error —
// programmaticPushWithRetry must NOT retry on a hard push
// failure (auth/protection errors don't heal on retry); it
// surfaces the first attempt's error immediately.
func TestRunPush_PushErrorSurfaced(t *testing.T) {
	git := newPushGit()
	git.onArgs(statusCmd, "## wt-auth-fail\n", "", nil)
	// origin/<branch> does not exist — first-push path.
	git.onArgs([]string{"rev-parse", "--verify", "origin/wt-auth-fail"},
		"", "fatal: unknown revision or path not in the working tree: 'origin/wt-auth-fail'", errors.New("exit status 128"))
	// Pre-push verify: @{u} is unset → countUnpushed returns 0
	// silently. The post-push verify would do the same if the
	// push errored again.
	git.onSeq("rev-list",
		pushGitResp{"0", "", nil},
	)
	// `git push` itself fails — both attempts, with the same
	// auth error.
	authErr := errors.New("exit status 128")
	git.onSeq("push",
		// Attempt 1: errored with stderr hint of the real cause.
		pushGitResp{"", "remote: Permission to org/repo.git denied to user.\nfatal: unable to access 'https://github.com/org/repo.git/': The requested URL returned error: 403", authErr},
		// Attempt 2: still fails (we expect the new code to bail
		// out at attempt 1, but the fixture is here defensively
		// in case the implementation runs both).
		pushGitResp{"", "remote: Permission to org/repo.git denied to user.\nfatal: unable to access 'https://github.com/org/repo.git/': The requested URL returned error: 403", authErr},
	)

	withAgent(t)
	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-auth-fail",
		RepoRoot: mustPwd(t),
	})

	cs := newPushChatSession(t)
	s := captureCh(t, cs)
	_, err := dispatchPush(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{})
	if err != nil {
		t.Fatalf("RunPush: %v", err)
	}
	r := s.lastText()

	// Anti-regression: the misleading "✅ pushed" success card
	// must NOT appear when the push itself failed.
	if strings.Contains(r, "✅ pushed") {
		t.Fatalf("push error must NOT be masked by a success card; got:\n%s", r)
	}
	// New contract: the actual git push error message surfaces.
	if !strings.Contains(r, "git push failed") {
		t.Errorf("expected 'git push failed' diagnostic, got:\n%s", r)
	}
	// The real upstream-protection message should bubble up so
	// the user can diagnose.
	if !strings.Contains(r, "Permission to org/repo.git denied") {
		t.Errorf("expected stderr hint to surface, got:\n%s", r)
	}
}

// TestRunPush_DirtyRefused — F-XX (commit/push split) new
// behavior. A dirty worktree used to fall through to Branch 2
// (agent+push); push no longer auto-commits. The user is told
// to run `/gtw commit` first.
func TestRunPush_DirtyRefused(t *testing.T) {
	git := newPushGit()
	git.onArgs(statusCmd,
		"## wt-dirty...origin/wt-dirty\n M foo.go\n", "", nil)

	withAgent(t)
	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-dirty",
		RepoRoot: mustPwd(t),
	})

	cs := newPushChatSession(t)
	s := captureCh(t, cs)
	_, err := dispatchPush(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{})
	if err != nil {
		t.Fatalf("RunPush: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "/gtw push no longer auto-commits") {
		t.Errorf("expected dirty-refusal hint, got:\n%s", r)
	}
	if !strings.Contains(r, "/gtw commit") {
		t.Errorf("expected guidance to run /gtw commit, got:\n%s", r)
	}
	for _, c := range git.calls {
		if c.args[0] == "push" {
			t.Fatalf("dirty worktree must not call push: %v", c.args)
		}
	}
}

// TestRunPush_ConflictState — F-57 invariant: conflicts are
// hard-refused at every gtw entry. /gtw push surfaces the
// conflict gate (PushBlockReason) and exits without pushing.
func TestRunPush_ConflictState(t *testing.T) {
	git := newPushGit()
	git.on("status", "UU conflicted.go\n", "", nil)

	withAgent(t, &recordingAgent{name: "claude"})
	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-conflict",
		RepoRoot: mustPwd(t),
	})
	cs := newPushChatSession(t)
	_ = cs.SetSelectedAgent("claude")

	_, err := dispatchPush(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{})
	if err != nil {
		t.Fatalf("RunPush: %v", err)
	}
	for _, c := range git.calls {
		if c.args[0] == "push" {
			t.Fatalf("conflict must not call push: %v", c.args)
		}
	}
}

// TestDispatchPush_NoYml — /cwd set, no .nightme/gtw.yml →
// loadDispatchContext fails first → no git status call.
func TestDispatchPush_NoYml(t *testing.T) {
	git := newPushGit()
	withAgent(t)
	withCwd(t, t.TempDir())

	_, err := dispatchPush(context.Background(), newPushChatSession(t),
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{})

	if err != nil {
		t.Fatalf("dispatchPush: %v", err)
	}
	for _, c := range git.calls {
		if c.args[0] == "status" {
			t.Fatalf("no yml should short-circuit before status call: %v", c.args)
		}
	}
}

func TestDispatchPush_MalformedYml(t *testing.T) {
	git := newPushGit()
	withAgent(t)
	withCwd(t, t.TempDir())
	dir := mustPwd(t)
	nightmeDir := filepath.Join(dir, ".nightme")
	if err := os.MkdirAll(nightmeDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nightmeDir, "gtw.yml"), []byte("worktree:\nbranch:\nrepoRoot:\n"), 0o644); err != nil {
		t.Fatalf("write yml: %v", err)
	}

	_, err := dispatchPush(context.Background(), newPushChatSession(t),
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{})

	if err != nil {
		t.Fatalf("dispatchPush: %v", err)
	}
	for _, c := range git.calls {
		if c.args[0] == "status" {
			t.Fatalf("malformed yml should short-circuit before status: %v", c.args)
		}
	}
}

// TestRunPush_NonWorktree_CleanWithUnpushed — /gtw push on a
// manually-checked-out branch (no /gtw fix pre-amble, no yml).
// loadDispatchContext derives Branch / Worktree / RepoRoot from
// git rev-parse, then dispatchPush proceeds as Branch 3.
func TestRunPush_NonWorktree_CleanWithUnpushed(t *testing.T) {
	tmp := t.TempDir()
	withCwd(t, tmp)

	git := newPushGit()
	git.onArgs([]string{"rev-parse", "--show-toplevel"}, tmp, "", nil)
	git.onArgs([]string{"rev-parse", "--abbrev-ref", "HEAD"}, "feat/manual", "", nil)
	git.onArgs(statusCmd,
		"## feat/manual...origin/feat/manual [ahead 3]\n", "", nil)
	// dispatchPush now reads origin/<branch> via `rev-parse --verify` to
	// seed the success-card rev range. The previous `headBefore..origin/<branch>`
	// shape collapsed to empty after a successful push; using
	// `originBefore..origin/<branch>` is what fixes the "pushed 0 commits" bug.
	git.onArgs([]string{"rev-parse", "--verify", "origin/feat/manual"},
		"3333333333333333333333333333333333333333\n", "", nil)
	git.onSeq("rev-list",
		pushGitResp{"3", "", nil},
		pushGitResp{"0", "", nil},
	)
	git.on("push", "To origin\n", "", nil)
	// The post-push log query reads `333..origin/feat/manual` (the
	// pre-push origin tip .. new origin tip), so the onArgs key must
	// match that exact range or the mock returns "no response".
	git.onArgs([]string{"log", "--oneline", "3333333333333333333333333333333333333333..origin/feat/manual"},
		"abc1234 feat: thing\ndef5678 fix: stuff\n", "", nil)

	withAgent(t)
	cs := &chatsession.ChatSession{}
	_ = cs.SetSelectedCwd(tmp)
	s := captureCh(t, cs)

	_, err := dispatchPush(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{})
	if err != nil {
		t.Fatalf("RunPush: %v", err)
	}
	pushed := false
	for _, c := range git.calls {
		if c.args[0] == "push" {
			pushed = true
		}
	}
	if !pushed {
		t.Fatalf("expected git push call, got %v", git.calls)
	}
	r := s.lastText()
	if !strings.Contains(r, "✅ pushed") || !strings.Contains(r, "feat/manual") {
		t.Errorf("expected Branch 3 success card, got:\n%s", r)
	}
	if !strings.Contains(r, "> feat/manual") {
		t.Errorf("expected `> <branch>` intent line, got:\n%s", r)
	}
	if strings.Contains(r, "🤖") {
		t.Errorf("Branch 3 should not credit an agent, got:\n%s", r)
	}
}

func TestCountUnpushed_NoUpstream(t *testing.T) {
	git := newPushGit()
	git.on("rev-list", "", "fatal: no upstream configured for branch 'wt'", errors.New("exit 128"))

	withCwd(t, t.TempDir())
	n, err := countUnpushed(context.Background(), mustPwd(t), "wt", HandlerDeps{Git: git})
	if err != nil {
		t.Fatalf("countUnpushed: %v", err)
	}
	if n != 0 {
		t.Fatalf("no-upstream should return 0, got %d", n)
	}
}

func TestCountUnpushed_WithUpstream(t *testing.T) {
	git := newPushGit()
	git.on("rev-list", "5\n", "", nil)

	withCwd(t, t.TempDir())
	n, err := countUnpushed(context.Background(), mustPwd(t), "wt", HandlerDeps{Git: git})
	if err != nil {
		t.Fatalf("countUnpushed: %v", err)
	}
	if n != 5 {
		t.Fatalf("got %d, want 5", n)
	}
}

func TestCountUnpushed_RealErrorPropagates(t *testing.T) {
	git := newPushGit()
	git.on("rev-list", "", "fatal: unable to read current working directory: No such file or directory", errors.New("exit 128"))

	withCwd(t, t.TempDir())
	_, err := countUnpushed(context.Background(), mustPwd(t), "wt", HandlerDeps{Git: git})
	if err == nil {
		t.Fatalf("non-upstream error must propagate; got nil")
	}
}

// TestCountUnpushed_UpstreamConfigured_NotStoredAsRemote —
// DEFENSIVE hardening for the ghost-upstream stderr shape (issue
// #235). branch.<name>.merge is configured but
// refs/remotes/origin/<name> is missing, so older git's
// `git rev-list branch@{u}..branch` fails with "fatal: upstream
// branch 'refs/heads/<name>' not stored as a remote-tracking
// branch". isBenignNoUpstream treats that as a benign "0 unpushed"
// answer rather than a hard failure.
//
// NOTE: this exercises the countUnpushed belt-and-suspenders only.
// The PRIMARY fix for #235 is at the readiness gate (parseBranchHeader
// flags `[gone]` → HasNothingToPush returns false → the push
// actually runs and `git push -u` materialises the tracking ref,
// so this verify path is normally unreachable) — see
// TestHasNothingToPush_GoneUpstream and TestRunPush_GoneUpstream_Pushes.
func TestCountUnpushed_UpstreamConfigured_NotStoredAsRemote(t *testing.T) {
	git := newPushGit()
	git.on("rev-list", "",
		"fatal: upstream branch 'refs/heads/chore-docs-init' not stored as a remote-tracking branch",
		errors.New("exit status 128"))

	withCwd(t, t.TempDir())
	n, err := countUnpushed(context.Background(), mustPwd(t), "chore-docs-init", HandlerDeps{Git: git})
	if err != nil {
		t.Fatalf("ghost upstream should not error; got %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 unpushed, got %d", n)
	}
}

// TestHasNothingToPush_GoneUpstream — the PRIMARY fix for issue
// #235: a clean branch whose upstream is "gone" (branch.<name>.merge
// set but refs/remotes/origin/<name> missing → porcelain
// "## b...origin/b [gone]") must NOT be reported as "nothing to
// push". Pre-fix, parseBranchHeader read [gone] as HasUpstream=true,
// ahead=0, so HasNothingToPush returned true and dispatchPush
// short-circuited — stranding genuinely-unpushed commits. The
// UpstreamGone flag makes the predicate return false so the push
// proceeds and `git push -u` re-publishes the branch.
func TestHasNothingToPush_GoneUpstream(t *testing.T) {
	gone := messages.GitStatusSnapshot{
		Branch:       "chore-docs-init",
		HasUpstream:  true,
		UpstreamGone: true,
	}
	if !gone.WorkingTreeIsClean() {
		t.Fatalf("setup: snapshot must be clean")
	}
	if gone.HasNothingToPush() {
		t.Fatalf("gone-upstream clean branch must NOT be 'nothing to push' — it needs publishing")
	}

	// Sanity: the same branch with a live, in-sync upstream IS
	// nothing to push (the gone flag is what flips the answer).
	live := gone
	live.UpstreamGone = false
	if !live.HasNothingToPush() {
		t.Fatalf("in-sync clean branch with a live upstream should be 'nothing to push'")
	}
}

// TestParseBranchHeader_GoneUpstream pins the porcelain→snapshot
// translation for the `[gone]` token: hasUpstream=true (the
// "## b...origin/b" part is present), ahead/behind=0 (git can't
// compute them without a tracking ref), and gone=true (the token
// that HasNothingToPush gates on).
func TestParseBranchHeader_GoneUpstream(t *testing.T) {
	name, hasUpstream, ahead, behind, gone := parseBranchHeader(
		"chore-docs-init...origin/chore-docs-init [gone]")
	if name != "chore-docs-init" || !hasUpstream || ahead != 0 || behind != 0 || !gone {
		t.Fatalf("gone header mis-parsed: name=%q hasUpstream=%v ahead=%d behind=%d gone=%v",
			name, hasUpstream, ahead, behind, gone)
	}

	// In-sync (no [gone]) must NOT set gone — regression guard.
	_, _, _, _, gone2 := parseBranchHeader("chore-docs-init...origin/chore-docs-init")
	if gone2 {
		t.Fatalf("in-sync header must not set gone")
	}
}

// TestRunPush_GoneUpstream_Pushes — end-to-end gate fix: a clean
// branch in the ghost-upstream ([gone]) state must reach
// programmaticPush (not short-circuit to "nothing to push"). The
// push materialises the tracking ref and lands the commits, so
// verify succeeds and the success card renders.
func TestRunPush_GoneUpstream_Pushes(t *testing.T) {
	rig := newPushTestRig(t)
	rig.installDeps(t, "chore-docs-init")
	// [gone]: merge configured, tracking ref missing. Pre-fix this
	// snapshot → HasNothingToPush=true → "nothing to push" (bug).
	setupPushMocks(rig.git, "chore-docs-init", messages.GitStatusSnapshot{
		Branch:       "chore-docs-init",
		HasUpstream:  true,
		UpstreamGone: true,
	})
	// origin/<branch> does NOT exist (ghost) → originBefore="" →
	// first-push rev-range path.
	rig.git.onArgs([]string{"rev-parse", "--verify", "origin/chore-docs-init"},
		"", "fatal: unknown revision or path not in the working tree: 'origin/chore-docs-init'", errors.New("exit status 128"))
	rig.git.onArgs([]string{"push", "-u", "origin", "chore-docs-init"},
		"To origin\n * [new branch]      chore-docs-init -> chore-docs-init\n", "", nil)
	// Post-push verify: `git push -u` materialised the tracking ref,
	// so @{u} resolves and countUnpushed reports 0.
	rig.git.on("rev-list", "0", "", nil)
	// firstPushRevRange → DefaultBranch lookup.
	rig.git.onArgs([]string{"symbolic-ref", "--short", "refs/remotes/origin/HEAD"},
		"origin/main", "", nil)
	// first-push fallback log: the 2 commits this push landed.
	rig.git.onArgs([]string{"log", "--oneline", "origin/main..origin/chore-docs-init"},
		"abc1234 doc 1\ndef5678 doc 2\n", "", nil)

	s := captureCh(t, rig.cs)
	_, err := dispatchPush(context.Background(), rig.cs,
		HandlerDeps{Git: rig.git}, "chat", "msg", pushArgs{})
	if err != nil {
		t.Fatalf("dispatchPush err: %v", err)
	}
	pushed := false
	for _, c := range rig.git.calls {
		if len(c.args) >= 2 && c.args[0] == "push" && c.args[1] == "-u" {
			pushed = true
		}
	}
	if !pushed {
		t.Fatalf("ghost-upstream [gone] must still push, got calls=%v", rig.git.calls)
	}
	r := s.lastText()
	if strings.Contains(r, "nothing to push") {
		t.Fatalf("[gone] must not short-circuit to 'nothing to push', got:\n%s", r)
	}
	if !strings.Contains(r, "✅ pushed 2 commit(s)") {
		t.Fatalf("expected success card counting the 2 pushed commits, got:\n%s", r)
	}
}

// TestListUnpushedCommits_UpstreamConfigured_NotStoredAsRemote —
// the ghost-upstream stderr shape must map to an empty commit
// list, mirroring countUnpushed's benign semantics. Without this,
// the verify-failed diagnostic's unpushedCommitsForDisplay would
// surface a spurious "list unpushed commits" error on top of the
// (already-fixed) count failure.
func TestListUnpushedCommits_UpstreamConfigured_NotStoredAsRemote(t *testing.T) {
	git := newPushGit()
	git.on("rev-list", "",
		"fatal: upstream branch 'refs/heads/chore-docs-init' not stored as a remote-tracking branch",
		errors.New("exit status 128"))

	withCwd(t, t.TempDir())
	commits, err := listUnpushedCommits(context.Background(), mustPwd(t), "chore-docs-init", HandlerDeps{Git: git})
	if err != nil {
		t.Fatalf("ghost upstream should not error; got %v", err)
	}
	if len(commits) != 0 {
		t.Fatalf("expected empty list, got %v", commits)
	}
}

// -----------------------------------------------------------------------------
// F-57 §8.2 push matrix tests.
//
// Each test stages a single readiness snapshot via setupPushMocks
// (readiness_test.go), writes a yml, and asserts:
//   1. the dispatched state matches the dimension-under-test;
//   2. push is / is not invoked as expected.
//
// pushTestRig wraps the bare pushGit + chat session into a struct
// so the matrix tests can install deps in one call (matching the
// prTestRig pattern in pr_test.go).
// -----------------------------------------------------------------------------

type pushTestRig struct {
	git *pushGit
	cs  *chatsession.ChatSession
}

func newPushTestRig(t *testing.T) *pushTestRig {
	t.Helper()
	tmp := t.TempDir()
	withCwd(t, tmp)
	cs := &chatsession.ChatSession{}
	_ = cs.SetSelectedCwd(tmp)
	return &pushTestRig{
		git: newPushGit(),
		cs:  cs,
	}
}

func (r *pushTestRig) installDeps(t *testing.T, branch string) {
	t.Helper()
	withAgent(t)
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   branch,
		RepoRoot: mustPwd(t),
	})
	_ = r.cs.SetSelectedAgent("claude")
}

// TestDispatchPush_HardRefuse_Conflicts — F-57 §8.2 row 1.
// HasConflicts → PushBlockReason → exit before push.
func TestDispatchPush_HardRefuse_Conflicts(t *testing.T) {
	rig := newPushTestRig(t)
	rig.installDeps(t, "wt-conflict")
	setupPushMocks(rig.git, "wt-conflict", messages.GitStatusSnapshot{
		Branch:       "wt-conflict",
		HasUpstream:  true,
		HasConflicts: true,
		Modified:     1,
	})

	s := captureCh(t, rig.cs)
	_, err := dispatchPush(context.Background(), rig.cs,
		HandlerDeps{Git: rig.git}, "chat", "msg", pushArgs{})
	if err != nil {
		t.Fatalf("dispatchPush err: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "unmerged paths") {
		t.Fatalf("expected conflict hard-refuse, got:\n%s", r)
	}
	for _, c := range rig.git.calls {
		if c.args[0] == "push" {
			t.Fatalf("conflict must not call push: %v", c.args)
		}
	}
}

// TestDispatchPush_NothingToPush — F-57 §8.2 row 2.
// Clean + has upstream + ahead=0 → HasNothingToPush → exit.
func TestDispatchPush_NothingToPush(t *testing.T) {
	rig := newPushTestRig(t)
	rig.installDeps(t, "wt-clean")
	setupPushMocks(rig.git, "wt-clean", messages.GitStatusSnapshot{
		Branch:      "wt-clean",
		HasUpstream: true,
	})

	s := captureCh(t, rig.cs)
	_, err := dispatchPush(context.Background(), rig.cs,
		HandlerDeps{Git: rig.git}, "chat", "msg", pushArgs{})
	if err != nil {
		t.Fatalf("dispatchPush err: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "ℹ️ nothing to push") {
		t.Fatalf("expected nothing-to-push reply, got:\n%s", r)
	}
	for _, c := range rig.git.calls {
		if c.args[0] == "push" {
			t.Fatalf("nothing-to-push must not call push: %v", c.args)
		}
	}
}

// TestDispatchPush_NoUpstreamFreshBranch — F-57 §8.2 row 5.
// Clean + !HasUpstream → push with -u (Branch 3 first-push).
func TestDispatchPush_NoUpstreamFreshBranch(t *testing.T) {
	rig := newPushTestRig(t)
	rig.installDeps(t, "wt-fresh")
	setupPushMocks(rig.git, "wt-fresh", messages.GitStatusSnapshot{
		Branch:      "wt-fresh",
		HasUpstream: false,
	})
	// First-push: origin/<branch> does NOT exist. dispatchPush
	// captures this with `rev-parse --verify origin/<branch>`,
	// treats the "unknown revision" stderr as originBefore="",
	// and falls back to `git log <branch>` for the success card.
	rig.git.onArgs([]string{"rev-parse", "--verify", "origin/wt-fresh"},
		"", "fatal: unknown revision or path not in the working tree: 'origin/wt-fresh'", errors.New("exit status 128"))
	rig.git.onArgs([]string{"push", "-u", "origin", "wt-fresh"},
		"To origin\n", "", nil)
	rig.git.on("rev-list", "0", "", nil)
	rig.git.onArgs([]string{"log", "--oneline", "wt-fresh"},
		"5555555555 first commit\n", "", nil)

	s := captureCh(t, rig.cs)
	_, err := dispatchPush(context.Background(), rig.cs,
		HandlerDeps{Git: rig.git}, "chat", "msg", pushArgs{})
	if err != nil {
		t.Fatalf("dispatchPush err: %v", err)
	}
	pushed := false
	for _, c := range rig.git.calls {
		if len(c.args) >= 2 && c.args[0] == "push" && c.args[1] == "-u" {
			pushed = true
			break
		}
	}
	if !pushed {
		t.Fatalf("expected `git push -u origin <branch>` call, got %v", rig.git.calls)
	}
	r := s.lastText()
	if !strings.Contains(r, "✅ pushed") {
		t.Fatalf("expected success card, got:\n%s", r)
	}
}
