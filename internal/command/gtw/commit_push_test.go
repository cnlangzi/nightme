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
	mu                    sync.Mutex
	responses             map[string]pushGitResp
	responsesByArgs       map[string]pushGitResp
	// seqResponses[prefix] is the ordered list of responses
	// onSeq registered; the Nth call matching prefix gets the
	// Nth entry, last entry reused once exhausted. Call count
	// is per-prefix so a sequence on "rev-list" doesn't get
	// advanced by intervening "status" or "push" calls.
	seqResponses          map[string][]pushGitResp
	seqCallCount          map[string]int
	// seqResponsesByArgs is the per-full-argv equivalent of
	// seqResponses — for onArgsSeq, when the same exact argv
	// is called multiple times with different state-dependent
	// answers (e.g. `git rev-parse HEAD` before vs after agent).
	seqResponsesByArgs    map[string][]pushGitResp
	seqArgsCallCount      map[string]int
	calls                 []pushGitCall
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
// exhausted. Companion to onSeq (which keys on first-token
// only) — onArgsSeq handles per-argv sequences like "two
// `git rev-parse HEAD` calls with different SHAs (e.g. one
// before the agent, one after)". Each new onArgsSeq call
// overwrites any prior sequence for the same args key.
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
// number of registered responses, the LAST response is reused —
// so tests can register [before, after] to model "before push
// returns N unpushed, after push returns 0". Used to simulate
// stateful git operations where the answer changes after a
// side-effect (push, fetch, reset, …).
//
// Matched ONLY on first-token (no full-argv sequence support).
// For per-argv sequences, just use onArgs repeatedly — last
// registration wins.
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
	// Sequence-based per-argv match: Nth call to this exact argv
	// gets Nth response (last reused once exhausted). Wins over
	// plain onArgs because it's more specific.
	if seq, ok := f.seqResponsesByArgs[key]; ok {
		idx := f.seqArgsCallCount[key]
		f.seqArgsCallCount[key] = idx + 1
		if idx >= len(seq) {
			idx = len(seq) - 1
		}
		resp := seq[idx]
		return resp.stdout, resp.stderr, resp.err
	}
	// Specific (full-argv) match wins over first-token match.
	if resp, ok := f.responsesByArgs[key]; ok {
		return resp.stdout, resp.stderr, resp.err
	}
	// Sequence-based match: Nth call to prefix gets Nth response
	// (last reused once exhausted). Wins over plain `on` because
	// it's more specific. Call count is per-prefix so a
	// sequence on "rev-list" isn't perturbed by interleaved
	// "status" or "push" calls.
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
func (r *recordingAgent) RunOnce(_ context.Context, cfg agent.StartConfig, blocks []agent.ContentBlock) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, runOnceCall{workspace: cfg.Workspace, blocks: append([]agent.ContentBlock(nil), blocks...)})
	return r.runOnceText, r.runOnceErr
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

// writeYml is a tiny helper that drops a minimal .nightme/gtw.yml
// in dir so dispatchPush can read it via pushCwd(). It patches
// pushCwd's behaviour by also chdir-ing the test process — which
// is acceptable because pushCwd shells out to `pwd` and the
// dispatcher reads yml from that path.
func writeYml(t *testing.T, dir string, c Context) {
	t.Helper()
	nightmeDir := filepath.Join(dir, ".nightme")
	if err := os.MkdirAll(nightmeDir, 0o755); err != nil {
		t.Fatalf("mkdir .nightme: %v", err)
	}
	// Use the existing yml writer (commit.go / persist.go) — but
	// the simplest path is to hand-write a minimal yml that
	// ReadGTWYml can parse. ReadGTWYml is unexported; we use the
	// same package here so we can call it directly.
	data := buildTestYml(c)
	if err := os.WriteFile(filepath.Join(nightmeDir, "gtw.yml"), []byte(data), 0o644); err != nil {
		t.Fatalf("write gtw.yml: %v", err)
	}
}

// buildTestYml is intentionally minimal — only fields dispatchPush
// reads (Worktree/Branch/RepoRoot/Issue). ReadGTWYml's parser
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

// -----------------------------------------------------------------------------
// buildAgentPrompt tests (plan §2.7.3)
// -----------------------------------------------------------------------------

func TestBuildAgentPrompt_Remote(t *testing.T) {
	c := Context{
		Worktree: "/w",
		Branch:   "fix-42-foo",
		Issue:    42,
	}
	p := buildAgentPrompt(c)

	// F-56 §3 minimal prompt: role + task + 3 hard rules.
	mustContain(t, p, "You are a release engineer.")
	mustContain(t, p, "branch fix-42-foo in /w")
	mustContain(t, p, "for issue #42")
	mustContain(t, p, "Conventional Commits")
	mustContain(t, p, "feat, fix, chore, refactor")
	mustContain(t, p, "Do not push.")
	mustContain(t, p, "never run `git push`")
	mustContain(t, p, "Do not revert, restore, or stash")
	mustContain(t, p, "not `git add -A`")
	// Old prompt's "5-step checklist" + the actual push step
	// (not the "never run git push" warning) should be gone.
	if strings.Contains(p, "Step list") || strings.Contains(p, "Task:") ||
		strings.Contains(p, "git push -u origin") || strings.Contains(p, "Reply with: <commit_hash>") ||
		strings.Contains(p, "Working directory: /w\nBranch:") {
		t.Fatalf("old-style prompt leakage detected:\n%s", p)
	}
}

func TestBuildAgentPrompt_Local(t *testing.T) {
	c := Context{
		Worktree: "/w",
		Branch:   "wt-local",
		Issue:    -1, // ModeLocal
	}
	p := buildAgentPrompt(c)

	mustContain(t, p, "You are a release engineer.")
	mustContain(t, p, "branch wt-local in /w")
	if strings.Contains(p, "for issue #") {
		t.Fatalf("Local prompt should not contain 'for issue #':\n%s", p)
	}
	if strings.Contains(p, "Working directory: /w\nBranch:") {
		t.Fatalf("Local prompt should not contain the old 'Working directory / Branch' lines:\n%s", p)
	}
}

func mustContain(t *testing.T, s, sub string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Fatalf("prompt missing %q\n---\n%s\n---", sub, s)
	}
}

// -----------------------------------------------------------------------------
// parsePushArgs tests (plan §2.7.4)
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

// -----------------------------------------------------------------------------
// Three-state dispatcher tests (plan §2.7.2)
//
// These tests drive dispatchPush directly via the unexported entry
// point. They construct a real ChatSession with SelectedAgent set,
// mount a pushGit and a recordingAgent on agent.Builtins,
// drop a minimal yml in a temp dir, and chdir into it.
//
// We assert by inspecting the git-call log (pushGit.calls)
// and the agent-call log (recordingAgent.calls) — no need to
// intercept reply() because newTestChannel swallows it.
// -----------------------------------------------------------------------------

func TestRunPush_CleanNoUnpushed(t *testing.T) {
	git := newPushGit()
	// F-57: dispatchPush's first readiness probe is
	// `status --porcelain --branch --untracked-files=normal`.
	// Snapshot = clean tree + has upstream + ahead=0 →
	// HasNothingToPush() = true → Branch 1 no-op.
	git.onArgs(statusCmd, "## wt-clean...origin/wt-clean\n", "", nil)
	// headSHA helper (headBefore capture for verifyAgentCommitted
	// + success card revRange).
	git.onArgs([]string{"rev-parse", "HEAD"},
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n", "", nil)
	// F-56's "no upstream" path is no longer reachable from
	// dispatchPush at entry — HasNothingToPush on a
	// HasUpstream=true snap is what we exercise. countUnpushed
	// stays in programmaticPushWithRetry's verify loop (still
	// useful there) but dispatchPush itself doesn't ask for it.

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
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{}, "")

	if err != nil || res == nil {
		t.Fatalf("dispatchPush err=%v res=%v", err, res)
	}
	// F-56 Branch 1: clean + 0 unpushed → exit with "ℹ️ nothing
	// to push" message. NO push call.
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

func TestRunPush_CleanWithUnpushed(t *testing.T) {
	git := newPushGit()
	// F-57: dispatchPush's first readiness probe is
	// `status --porcelain --branch --untracked-files=normal`.
	// Snapshot = clean tree + has upstream + ahead=3 → NOT
	// HasNothingToPush → falls through to Branch 3 (push only).
	git.onArgs(statusCmd, "## wt-clean-unpushed...origin/wt-clean-unpushed [ahead 3]\n", "", nil)
	// headSHA helper.
	git.onArgs([]string{"rev-parse", "HEAD"},
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n", "", nil)
	// State change across push: before push, 3 unpushed; after
	// push (verified by programmaticPushWithRetry), 0 unpushed.
	// dispatchPush itself no longer queries rev-list @{u}..branch
	// at entry — AheadOfRemote came from the status porcelain —
	// but programmaticPushWithRetry still does, so this is the
	// mock for that loop.
	git.onSeq("rev-list",
		pushGitResp{"3", "", nil}, // before push (verify attempt 1)
		pushGitResp{"0", "", nil}, // after push (verify attempt 2)
	)
	git.on("push", "To origin", "", nil)
	// F-56: success card reads `git log headBefore..origin/<branch>`.
	git.on("log", "abc1234 commit 1\ndef5678 commit 2\n", "", nil)

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
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{}, "")

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
	// F-56 Branch 3: worktree clean, push-only. Card is
	// "✅ pushed N commit(s) to <branch>:" + log output.
	if !strings.Contains(r, "✅ pushed") || !strings.Contains(r, "wt-clean-unpushed") {
		t.Errorf("expected Branch 3 success card, got:\n%s", r)
	}
	if strings.Contains(r, "🤖") {
		t.Errorf("Branch 3 should not credit an agent, got:\n%s", r)
	}
}

// TestRunPush_VerifyDetectsSilentPushFailure is the regression
// test for the user's reported UX gap: /gtw push reported
// success, but a network race or silent remote rejection left
// commits behind. With verifyPushedAndRetry, the user now sees
// a diagnostic naming the unpushed commits instead of a green
// checkmark that lies.
//
// Mock sequence: push returns no error (claiming success), but
// rev-list keeps returning "1" — simulating the network race
// where the push silently no-ops. verifyPushedAndRetry retries
// once and still gets "1", then surfaces the unpushed commits.
func TestRunPush_VerifyDetectsSilentPushFailure(t *testing.T) {
	git := newPushGit()
	// F-57: status porcelain = clean tree + has upstream + ahead=3.
	// (The push never lands, so the verify loop keeps seeing
	// ahead>0; we model that by simulating the porcelain ahead
	// value. Since dispatchPush only takes ONE status snapshot
	// before pushing, the [ahead 3] in this mock represents the
	// initial state — the post-push verify loop uses rev-list
	// directly, not status.)
	git.onArgs(statusCmd, "## wt-silent-fail...origin/wt-silent-fail [ahead 3]\n", "", nil)
	// F-56 entry: headBefore is captured up-front.
	git.onArgs([]string{"rev-parse", "HEAD"},
		"cccccccccccccccccccccccccccccccccccccccc\n", "", nil)
	// All rev-list calls return "1" — the push never lands
	// upstream, even after retry. State change is impossible.
	git.on("rev-list", "1", "", nil)
	git.on("push", "To origin", "", nil) // claims success

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
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{}, "")
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

// TestRunPush_DirtyAgentLeavesFiles: when the agent claims
// commit+push succeeded but actually left files uncommitted
// (intentionally or by mistake), verifyAgentPushedAndRecover
// surfaces the file list. User can commit manually or retry.
func TestRunPush_DirtyAgentLeavesFiles(t *testing.T) {
	git := newPushGit()
	// F-57: dispatchPush's entry readiness probe is
	// status --porcelain --branch --untracked-files=normal.
	// Returns dirty porcelain (1 untracked file).
	git.onArgs(statusCmd,
		"## wt-dirty-leftover...origin/wt-dirty-leftover\n?? new-secret.env\n", "", nil)
	// F-56: dispatchPush captures headBefore + counts unpushed
	// at entry, before deciding which branch to take.
	git.onArgs([]string{"rev-parse", "HEAD"},
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef\n", "", nil)
	git.on("rev-list", "0\n", "", nil) // entry countUnpushed: 0
	// verifyAgentCommitted calls `status --porcelain` (no --branch)
	// — separate argv key from statusCmd, so a per-args mock is
	// enough. Pre-agent's first statusCmd already covered the
	// entry check; here we model "agent left the tree dirty".
	git.onArgs([]string{"status", "--porcelain"},
		"?? new-secret.env\n", "", nil)
	// F-56: dispatchPush captures headBefore (rev-parse HEAD).
	// verifyAgentCommitted checks branch first (rev-parse
	// --abbrev-ref HEAD), then HEAD-advance (rev-parse HEAD
	// again) — but only if worktree clean check passed. Here
	// uncommitted > 0 short-circuits, so HEAD-advance is skipped.
	git.on("rev-parse", "wt-dirty-leftover\n", "", nil) // --abbrev-ref HEAD

	claude := &recordingAgent{
		name:        "claude",
		runOnceText: "I committed the safe files. The .env is for the user.",
	}
	withAgent(t, claude)

	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-dirty-leftover",
		RepoRoot: mustPwd(t),
		Issue:    7,
	})

	cs := newPushChatSession(t)
	_ = cs.SetSelectedAgent("claude")
	s := captureCh(t, cs)
	_, err := dispatchPush(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{}, "")
	if err != nil {
		t.Fatalf("RunPush: %v", err)
	}
	r := s.lastText()
	if strings.Contains(r, "🤖 claude pushed") {
		t.Fatalf("expected warning, got the unverified success card:\n%s", r)
	}
	if !strings.Contains(r, "still uncommitted") {
		t.Fatalf("expected diagnostic to mention uncommitted files, got:\n%s", r)
	}
	if !strings.Contains(r, "new-secret.env") {
		t.Fatalf("expected diagnostic to name the uncommitted file, got:\n%s", r)
	}
	// Branch 2 verify failure MUST NOT call git push.
	for _, c := range git.calls {
		if c.args[0] == "push" {
			t.Fatalf("verify-failed Branch 2 must not call git push: %v", c.args)
		}
	}
}

// TestRunPush_DirtyAgentClaimsDoneButNoCommit is the regression
// guard for the false-success class: the agent returns "Done."
// (or similar terse claim) WITHOUT actually committing. Pre-fix
// the verification only checked uncommitted + unpushed — both
// could read clean (HEAD was already at upstream, agent did
// nothing) and the user got a green checkmark for a no-op.
//
// The new HEAD-advance check (snapshot HEAD before the agent
// runs, compare after) catches this: HEAD unchanged + worktree
// pre-dirty → impossible without a commit → surface a warning
// naming the unchanged SHA.
func TestRunPush_DirtyAgentClaimsDoneButNoCommit(t *testing.T) {
	git := newPushGit()
	// F-57: status --porcelain --branch --untracked-files=normal
	// entry probe — dirty tree (1 modified). HasUpstream=true,
	// AheadOfRemote=0 (HEAD already at origin).
	git.onArgs(statusCmd,
		"## wt-noop...origin/wt-noop\n M foo.go\n", "", nil)
	// F-56: dispatchPush captures headBefore at entry.
	git.onArgs([]string{"rev-parse", "HEAD"},
		"cafebabecafebabecafebabecafebabecafebabe\n", "", nil)
	// countUnpushed at entry: 0 (HEAD already at origin).
	git.on("rev-list", "0\n", "", nil)
	// verifyAgentCommitted's worktree-clean check (per-args,
	// status --porcelain — no --branch). Agent did `git stash`
	// or similar — files moved out of the worktree without a
	// commit. Pre-fix this looked identical to a successful
	// commit+push.
	git.onArgs([]string{"status", "--porcelain"},
		"", "", nil) // post-agent: clean (no commit!)
	// HEAD-branch: agent stayed on c.Branch (didn't switch
	// branches), so the branch check passes.
	git.on("rev-parse", "wt-noop\n", "", nil)

	claude := &recordingAgent{
		name:        "claude",
		runOnceText: "Done.",
	}
	withAgent(t, claude)

	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-noop",
		RepoRoot: mustPwd(t),
	})

	cs := newPushChatSession(t)
	_ = cs.SetSelectedAgent("claude")
	s := captureCh(t, cs)
	_, err := dispatchPush(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{}, "")
	if err != nil {
		t.Fatalf("RunPush: %v", err)
	}
	r := s.lastText()
	if strings.Contains(r, "🤖 claude pushed") {
		t.Fatalf("expected warning, got the unverified success card:\n%s", r)
	}
	if !strings.Contains(r, "no new commit was created") {
		t.Fatalf("expected HEAD-advance diagnostic, got:\n%s", r)
	}
	if !strings.Contains(r, "cafebabe") {
		t.Fatalf("expected diagnostic to name the unchanged SHA, got:\n%s", r)
	}
}

// TestRunPush_DirtyAgentSwitchesBranch is the regression guard
// for the "agent committed on a side branch" failure mode:
// agent does `git checkout -b wt-side`, commits there, never
// merges back to c.Branch. From c.Branch's perspective the
// worktree is clean (HEAD still on c.Branch, files unchanged
// relative to c.Branch) and unpushed=0 (HEAD=c.Branch@upstream),
// so pre-fix verification passed with a green checkmark. The
// agent's commit lives on a side branch the user never asked
// for, and will never land on c.Branch.
func TestRunPush_DirtyAgentSwitchesBranch(t *testing.T) {
	git := newPushGit()
	// F-57: status --porcelain --branch --untracked-files=normal
	// entry probe — dirty (1 modified).
	git.onArgs(statusCmd,
		"## wt-cbranch...origin/wt-cbranch\n M foo.go\n", "", nil)
	// F-56: dispatchPush captures headBefore + counts unpushed at entry.
	git.onArgs([]string{"rev-parse", "HEAD"},
		"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee\n", "", nil)
	git.on("rev-list", "0\n", "", nil)
	// HEAD-branch snapshot after the agent: on wt-side,
	// NOT on wt-c.Branch. This is the trigger for the
	// branch-mismatch check.
	git.on("rev-parse", "wt-side\n", "", nil)

	claude := &recordingAgent{
		name:        "claude",
		runOnceText: "I made a side branch for this work.",
	}
	withAgent(t, claude)

	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-cbranch",
		RepoRoot: mustPwd(t),
	})

	cs := newPushChatSession(t)
	_ = cs.SetSelectedAgent("claude")
	s := captureCh(t, cs)
	_, err := dispatchPush(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{}, "")
	if err != nil {
		t.Fatalf("RunPush: %v", err)
	}
	r := s.lastText()
	if strings.Contains(r, "🤖 claude pushed") {
		t.Fatalf("expected warning, got the unverified success card:\n%s", r)
	}
	if !strings.Contains(r, "wt-side") || !strings.Contains(r, "wt-cbranch") {
		t.Fatalf("expected branch-mismatch diagnostic naming both branches, got:\n%s", r)
	}
}

func TestRunPush_DirtyDelegatesToAgent(t *testing.T) {
	git := newPushGit()
	// F-57: status --porcelain --branch --untracked-files=normal
	// entry probe — dirty (1 modified).
	git.onArgs(statusCmd,
		"## wt-dirty...origin/wt-dirty\n M foo.go\n", "", nil)
	// F-56: dispatchPush entry snapshots.
	git.onArgs([]string{"rev-parse", "HEAD"},
		"1111111111111111111111111111111111111111\n", "", nil)
	git.on("rev-list", "0\n", "", nil)

	claude := &recordingAgent{
		name:        "claude",
		runOnceText: "abc1234 pushed via claude",
	}
	withAgent(t, claude)

	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-dirty",
		RepoRoot: mustPwd(t),
		Issue:    7,
	})
	cs := newPushChatSession(t)
	_ = cs.SetSelectedAgent("claude")

	_, err := dispatchPush(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{}, "")
	if err != nil {
		t.Fatalf("RunPush: %v", err)
	}

	claude.mu.Lock()
	defer claude.mu.Unlock()
	if len(claude.calls) != 1 {
		t.Fatalf("agent.RunOnce called %d times, want 1", len(claude.calls))
	}
	call := claude.calls[0]
	if call.workspace == "" {
		t.Fatalf("RunOnce workspace empty")
	}
	if len(call.blocks) != 1 || call.blocks[0].Type != agent.ContentText {
		t.Fatalf("RunOnce blocks malformed: %+v", call.blocks)
	}
	prompt := call.blocks[0].Text
	if !strings.Contains(prompt, "branch wt-dirty") {
		t.Fatalf("prompt missing branch:\n%s", prompt)
	}
	if !strings.Contains(prompt, "issue #7") {
		t.Fatalf("prompt missing issue:\n%s", prompt)
	}
}

// TestRunPush_DirtyAgentCommitsAndPushes is the F-56 §8.1
// happy-path coverage for Branch 2. Pre-F-56 there was no test
// that exercised the full chain: agent runs → verify passes →
// nightme pushes → success card rendered from git log. The
// old TestRunPush_DirtyDelegatesToAgent only checked the agent
// was called, leaving the rest of the flow unverified.
//
// This test mocks the full sequence:
//
//   - dispatchPush entry: rev-parse HEAD (headBefore),
//     countUnpushed (0), status (dirty → Branch 2).
//   - agent.RunOnce fires.
//   - verifyAgentCommitted: status (now clean), rev-parse
//     --abbrev-ref HEAD (still on wt-dirty), rev-parse HEAD
//     (headAfter — different SHA, advance OK).
//   - re-count unpushed: 1 commit now.
//   - programmaticPushWithRetry: push (success), countUnpushed
//     re-verify (0).
//   - replySuccessCard: git log headBefore..origin/wt-dirty
//     returns 1 commit.
//
// Asserts: agent called 1×, push called 1×, IM card contains
// the Branch 2 header "🤖 pi committed 1 change(s) and pushed
// to wt-dirty" and the commit's oneline.
func TestRunPush_DirtyAgentCommitsAndPushes(t *testing.T) {
	git := newPushGit()
	// Entry: headBefore + countUnpushed (0 — fresh agent run).
	git.onArgsSeq([]string{"rev-parse", "HEAD"},
		pushGitResp{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n", "", nil}, // entry: headBefore
		pushGitResp{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n", "", nil}, // verify: headAfter
	)
	// rev-list (countUnpushed) sequence: 0 at entry → 1 after
	// agent → 0 after push verify. All calls share the first
	// token, so onSeq is the only way to differentiate them.
	git.onSeq("rev-list",
		pushGitResp{"0", "", nil}, // entry: no unpushed yet
		pushGitResp{"1", "", nil}, // re-count after agent: 1 commit
		pushGitResp{"0", "", nil}, // programmaticPushWithRetry verify: 0
	)
	// dispatchPush: statusCmd (--porcelain --branch
	// --untracked-files=normal) pre-agent is dirty → Branch 2;
	// then re-snapshot after the agent verifies clean AND shows
	// ahead=1 (the agent's commit). Per-args sequence: entry
	// dirty → post-verify clean-with-ahead.
	git.onArgsSeq(statusCmd,
		pushGitResp{"## wt-dirty...origin/wt-dirty\n M foo.go\n", "", nil},
		pushGitResp{"## wt-dirty...origin/wt-dirty [ahead 1]\n", "", nil},
	)
	// verifyAgentCommitted's listUncommittedFiles probe (a
	// different argv: status --porcelain, no --branch).
	git.onArgs([]string{"status", "--porcelain"}, "", "", nil)
	// verifyAgentCommitted (1): branch still on c.Branch.
	git.on("rev-parse", "wt-dirty\n", "", nil) // --abbrev-ref HEAD
	// programmaticPushWithRetry: push call.
	git.on("push", "To origin\n", "", nil)
	// replySuccessCard: git log for the range.
	git.on("log", "bbbbbbbb feat: agent did the thing\n", "", nil)

	pi := &recordingAgent{
		name:        "pi",
		runOnceText: "", // intentionally empty — agent prose is ignored
	}
	withAgent(t, pi)

	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-dirty",
		RepoRoot: mustPwd(t),
		Issue:    7,
	})
	cs := newPushChatSession(t)
	_ = cs.SetSelectedAgent("pi")
	s := captureCh(t, cs)

	_, err := dispatchPush(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{}, "")
	if err != nil {
		t.Fatalf("RunPush: %v", err)
	}

	// Agent was called exactly once.
	pi.mu.Lock()
	piCalls := len(pi.calls)
	pi.mu.Unlock()
	if piCalls != 1 {
		t.Fatalf("agent.RunOnce called %d times, want 1", piCalls)
	}

	// Push was called exactly once.
	pushCount := 0
	for _, c := range git.calls {
		if c.args[0] == "push" {
			pushCount++
		}
	}
	if pushCount != 1 {
		t.Fatalf("git push called %d times, want 1; calls=%v", pushCount, git.calls)
	}

	// IM card is Branch 2 format, sourced from git log (not agent prose).
	r := s.lastText()
	if !strings.Contains(r, "🤖 pi committed 1 change(s) and pushed to wt-dirty") {
		t.Errorf("Branch 2 IM card missing, got:\n%s", r)
	}
	if !strings.Contains(r, "feat: agent did the thing") {
		t.Errorf("commit oneline from git log missing, got:\n%s", r)
	}
	if !strings.Contains(r, "> wt-dirty") {
		t.Errorf("> branch line missing, got:\n%s", r)
	}
	// Agent's prose (empty string here) must NOT appear in the card.
	if strings.Contains(r, "runOnceText") {
		t.Errorf("agent prose leaked into card:\n%s", r)
	}
}

func TestRunPush_DirtyWithAgentFlag(t *testing.T) {
	git := newPushGit()
	// F-57: status --porcelain --branch --untracked-files=normal
	// entry probe — dirty (1 modified).
	git.onArgs(statusCmd,
		"## wt-flag...origin/wt-flag\n M foo.go\n", "", nil)
	// F-56: dispatchPush entry snapshots.
	git.onArgs([]string{"rev-parse", "HEAD"},
		"2222222222222222222222222222222222222222\n", "", nil)
	git.on("rev-list", "0\n", "", nil)

	opencode := &recordingAgent{
		name:        "opencode",
		runOnceText: "deadbee pushed via opencode",
	}
	claude := &recordingAgent{
		name:        "claude",
		runOnceText: "should-not-be-called",
	}
	withAgent(t, claude, opencode)

	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-flag",
		RepoRoot: mustPwd(t),
	})
	cs := newPushChatSession(t)
	_ = cs.SetSelectedAgent("claude") // chat default = claude

	_, err := dispatchPush(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{Agent: "opencode"}, "")
	if err != nil {
		t.Fatalf("RunPush: %v", err)
	}

	if len(claude.calls) != 0 {
		t.Fatalf("claude should NOT be called when -a opencode; got %d", len(claude.calls))
	}
	if len(opencode.calls) != 1 {
		t.Fatalf("opencode should be called exactly once; got %d", len(opencode.calls))
	}
}

func TestRunPush_NoAgentSelected(t *testing.T) {
	git := newPushGit()
	git.on("status", "M foo.go\n", "", nil)

	withAgent(t)
	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-noagent",
		RepoRoot: mustPwd(t),
	})

	_, err := dispatchPush(context.Background(), newPushChatSession(t),
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{}, "")

	if err != nil {
		t.Fatalf("RunPush: %v", err)
	}
	// No agent call expected.
	for _, a := range git.calls {
		_ = a // noop, just want to assert no panic
	}
}

func TestRunPush_UnknownAgent(t *testing.T) {
	git := newPushGit()
	git.on("status", "M foo.go\n", "", nil)

	withAgent(t) // no agents registered
	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-unknown",
		RepoRoot: mustPwd(t),
	})
	cs := newPushChatSession(t)
	_ = cs.SetSelectedAgent("claude")

	_, err := dispatchPush(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{Agent: "nope"}, "")
	if err != nil {
		t.Fatalf("RunPush: %v", err)
	}
	// No RunOnce call possible — registry was empty. Dispatcher
	// should short-circuit on "unknown agent".
}

func TestRunPush_AgentBinaryMissing(t *testing.T) {
	git := newPushGit()
	git.on("status", "M foo.go\n", "", nil)

	claude := &recordingAgent{
		name:        "claude",
		detectErr:   errors.New("claude: command not found"),
		runOnceText: "should-not-be-called",
	}
	withAgent(t, claude)

	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-missing",
		RepoRoot: mustPwd(t),
	})
	cs := newPushChatSession(t)
	_ = cs.SetSelectedAgent("claude")

	_, err := dispatchPush(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{}, "")
	if err != nil {
		t.Fatalf("RunPush: %v", err)
	}
	if len(claude.calls) != 0 {
		t.Fatalf("Detect failed → RunOnce must NOT be called")
	}
}

func TestRunPush_AgentRunOnceError(t *testing.T) {
	git := newPushGit()
	// F-57: status --porcelain --branch --untracked-files=normal
	// entry probe — dirty (1 modified).
	git.onArgs(statusCmd,
		"## wt-error...origin/wt-error\n M foo.go\n", "", nil)
	// F-56: dispatchPush entry snapshots.
	git.onArgs([]string{"rev-parse", "HEAD"},
		"3333333333333333333333333333333333333333\n", "", nil)
	git.on("rev-list", "0\n", "", nil)

	claude := &recordingAgent{
		name:       "claude",
		runOnceErr: errors.New("agent crashed"),
	}
	withAgent(t, claude)

	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-agenterr",
		RepoRoot: mustPwd(t),
	})
	cs := newPushChatSession(t)
	_ = cs.SetSelectedAgent("claude")

	_, err := dispatchPush(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{}, "")
	if err != nil {
		t.Fatalf("RunPush: %v", err)
	}
	if len(claude.calls) != 1 {
		t.Fatalf("expected 1 RunOnce call (which returned error)")
	}
}

func TestRunPush_ConflictState(t *testing.T) {
	git := newPushGit()
	// UU marker = both sides modified during merge
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
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{}, "")
	if err != nil {
		t.Fatalf("RunPush: %v", err)
	}
	// Conflict short-circuits — no agent call.
}

// -----------------------------------------------------------------------------
// Edge-case tests (review §6)
// -----------------------------------------------------------------------------

func TestDispatchPush_NoYml(t *testing.T) {
	// /cwd is set, but .nightme/gtw.yml doesn't exist → "no active fix"
	git := newPushGit()
	withAgent(t)
	withCwd(t, t.TempDir())
	// Don't call writeYml — leave the dir empty.

	_, err := dispatchPush(context.Background(), newPushChatSession(t),
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{}, "")

	if err != nil {
		t.Fatalf("dispatchPush: %v", err)
	}
	// No git calls expected — we bail before status.
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
	// Empty worktree/branch fields → "malformed" branch.
	if err := os.WriteFile(filepath.Join(nightmeDir, "gtw.yml"), []byte("worktree:\nbranch:\nrepoRoot:\n"), 0o644); err != nil {
		t.Fatalf("write yml: %v", err)
	}

	_, err := dispatchPush(context.Background(), newPushChatSession(t),
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{}, "")

	if err != nil {
		t.Fatalf("dispatchPush: %v", err)
	}
	// No git status call — malformed yml short-circuits.
	for _, c := range git.calls {
		if c.args[0] == "status" {
			t.Fatalf("malformed yml should short-circuit before status: %v", c.args)
		}
	}
}

func TestDetectConflicts(t *testing.T) {
	cases := []struct {
		status string
		want   bool
	}{
		{"", false},
		{"M foo.go\n", false},
		{"?? new.go\n", false},                 // untracked, not a conflict
		{"UU conflicted.go\n", true},           // both modified
		{"AA both_added.go\n", true},           // both added
		{"DD both_deleted.go\n", true},         // both deleted
		{"AU added_vs_modified.go\n", true},    // mixed conflict markers
		{"M foo.go\nUU conflicted.go\n", true}, // mixed with conflict
	}
	for _, tc := range cases {
		got := detectConflicts(tc.status)
		if got != tc.want {
			t.Errorf("detectConflicts(%q) = %v, want %v", tc.status, got, tc.want)
		}
	}
}

func TestCountUnpushed_NoUpstream(t *testing.T) {
	git := newPushGit()
	// git emits "fatal: no upstream configured for branch 'X'"
	// on stderr when @{u} is unset. countUnpushed matches that
	// substring and returns 0.
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
	// Non-upstream errors (permission denied, corrupt repo, etc.)
	// must NOT be silently swallowed. Otherwise the dispatcher
	// would tell the user "nothing to push" while the worktree
	// actually has unpushed commits it can't see.
	git := newPushGit()
	git.on("rev-list", "", "fatal: unable to read current working directory: No such file or directory", errors.New("exit 128"))

	withCwd(t, t.TempDir())
	_, err := countUnpushed(context.Background(), mustPwd(t), "wt", HandlerDeps{Git: git})
	if err == nil {
		t.Fatalf("non-upstream error must propagate; got nil")
	}
}

func TestParsePushArgs_Multiple(t *testing.T) {
	got, err := parsePushArgs([]string{"-a", "opencode", "-a", "claude"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	// Last one wins (silent accept policy).
	if got.Agent != "claude" {
		t.Fatalf("Agent = %q, want claude (last one wins)", got.Agent)
	}
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

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
// cs.SelectedCwd() (not system pwd), so each dispatchPush test
// needs a chat that mirrors the system-pwd that withCwd() set.
//
// Tests that want a chat with no SelectedCwd (to exercise the
// "no active workspace" early return) should construct the
// chat session themselves and leave cwd empty.
func newPushChatSession(t *testing.T) *chatsession.ChatSession {
	t.Helper()
	cs := &chatsession.ChatSession{}
	_ = cs.SetSelectedCwd(mustPwd(t))
	return cs
}

// TestRunPush_NonWorktree_CleanWithUnpushed covers /gtw push on
// a manually-checked-out branch (no /gtw fix pre-amble, no
// .nightme/gtw.yml). loadDispatchContext derives Branch / Worktree
// / RepoRoot from git rev-parse, then dispatchPush proceeds
// normally.
func TestRunPush_NonWorktree_CleanWithUnpushed(t *testing.T) {
	tmp := t.TempDir()
	withCwd(t, tmp)

	git := newPushGit()
	// loadDispatchContext's git calls:
	git.onArgs([]string{"rev-parse", "--show-toplevel"}, tmp, "", nil)
	git.onArgs([]string{"rev-parse", "--abbrev-ref", "HEAD"}, "feat/manual", "", nil)
	// F-57: status --porcelain --branch --untracked-files=normal
	// entry probe — clean + ahead=3 → Branch 3 push-only.
	git.onArgs(statusCmd,
		"## feat/manual...origin/feat/manual [ahead 3]\n", "", nil)
	// F-56: dispatchPush captures headBefore at entry, then
	// countUnpushed (3) before deciding Branch 3.
	git.onArgs([]string{"rev-parse", "HEAD"},
		"4444444444444444444444444444444444444444\n", "", nil)
	git.onSeq("rev-list",
		pushGitResp{"3", "", nil}, // entry countUnpushed: 3
		pushGitResp{"0", "", nil}, // post-push verify: 0
	)
	git.on("push", "To origin\n", "", nil)
	// F-56: success card reads `git log headBefore..origin/<branch>`.
	git.on("log", "abc1234 feat: thing\ndef5678 fix: stuff\n", "", nil)

	withAgent(t)
	cs := &chatsession.ChatSession{}
	_ = cs.SetSelectedCwd(tmp)
	s := captureCh(t, cs)

	_, err := dispatchPush(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{}, "")
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
	// F-56 Branch 3 card: "✅ pushed N commit(s) to <branch>:" +
	// `> <branch>` line + commit log entries. No 🤖 (no agent).
	if !strings.Contains(r, "✅ pushed") || !strings.Contains(r, "feat/manual") {
		t.Errorf("expected Branch 3 success card, got:\n%s", r)
	}
	if !strings.Contains(r, "> feat/manual") {
		t.Errorf("expected `> <branch>` intent line, got:\n%s", r)
	}
	if strings.Contains(r, "🤖") {
		t.Errorf("Branch 3 should not credit an agent, got:\n%s", r)
	}
	if strings.Contains(r, "━━━━━━━━━━━━━━") {
		t.Errorf("legacy `━━━━━━━━━━━━━━` separator should be gone after F-56 rewrite, got:\n%s", r)
	}
}

// -----------------------------------------------------------------------------
// F-57 §8.2 push matrix tests.
//
// Each test stages a single readiness snapshot via setupPushMocks,
// writes a yml, and asserts:
//   1. the dispatched state matches the dimension-under-test;
//   2. the agent is / is not invoked as expected;
//   3. programmaticPush is / is not called as expected.
//
// Helper pushTestRig / setupPushReadiness live in readiness_test.go;
// this section only writes the assertions.
// -----------------------------------------------------------------------------

// pushTestRig wraps the bare pushGit + chat session into a struct
// so the matrix tests can install deps in one call (matching the
// prTestRig pattern in pr_test.go).
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
// HasConflicts → PushBlockReason → exit before agent, before push.
func TestDispatchPush_HardRefuse_Conflicts(t *testing.T) {
	rig := newPushTestRig(t)
	rig.installDeps(t, "wt-conflict")
	setupPushMocks(rig.git, "wt-conflict", messages.GitStatusSnapshot{
		Branch:       "wt-conflict",
		HasUpstream:  true,
		HasConflicts: true,
		Uncommitted:  1,
	})

	s := captureCh(t, rig.cs)
	_, err := dispatchPush(context.Background(), rig.cs,
		HandlerDeps{Git: rig.git}, "chat", "msg", pushArgs{}, "")
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
		HandlerDeps{Git: rig.git}, "chat", "msg", pushArgs{}, "")
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
// Clean + !HasUpstream → NOT HasNothingToPush → push with -u.
// (Worktree is clean, no agent, just programmatic push.)
func TestDispatchPush_NoUpstreamFreshBranch(t *testing.T) {
	rig := newPushTestRig(t)
	rig.installDeps(t, "wt-fresh")
	setupPushMocks(rig.git, "wt-fresh", messages.GitStatusSnapshot{
		Branch:      "wt-fresh",
		HasUpstream: false,
	})
	// headBefore capture.
	rig.git.onArgs([]string{"rev-parse", "HEAD"},
		"5555555555555555555555555555555555555555\n", "", nil)
	// programmaticPush runs `git push -u origin <branch>`.
	rig.git.onArgs([]string{"push", "-u", "origin", "wt-fresh"},
		"To origin\n", "", nil)
	// programmaticPushWithRetry's verify loop: rev-list @{u}..branch
	// returns 0 after the first push.
	rig.git.on("rev-list", "0", "", nil)
	// success card log query.
	rig.git.on("log", "5555555555 first commit\n", "", nil)

	s := captureCh(t, rig.cs)
	_, err := dispatchPush(context.Background(), rig.cs,
		HandlerDeps{Git: rig.git}, "chat", "msg", pushArgs{}, "")
	if err != nil {
		t.Fatalf("dispatchPush err: %v", err)
	}
	// Push must have been called with -u.
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

// TestDispatchPush_AgentIntroducedConflicts — F-57 §8.2 row 7.
// Agent runs, verifyAgentCommitted passes (worktree clean from
// its perspective — listUncommittedFiles saw empty porcelain),
// but the re-snapshot at the dispatchPush layer sees a UU entry.
// This is the "agent introduced a conflict via some side-effect
// (e.g. running a merge mid-flight) that listUncommittedFiles
// happened to miss" defensive case. The re-snapshot's
// PushBlockReason is the second line of defense.
func TestDispatchPush_AgentIntroducedConflicts(t *testing.T) {
	rig := newPushTestRig(t)
	rig.installDeps(t, "wt-bad-agent")
	// Entry: dirty (1 uncommitted, no conflict yet).
	rig.git.onArgsSeq(statusCmd,
		pushGitResp{"## wt-bad-agent...origin/wt-bad-agent\n M foo.go\n", "", nil},
		// Post-agent: agent somehow left a conflict.
		pushGitResp{"## wt-bad-agent...origin/wt-bad-agent\nUU conflict.go\n", "", nil},
	)
	// verifyAgentCommitted sees the worktree as clean (empty
	// status --porcelain). This is the realistic mismatch:
	// listUncommittedFiles only looks at the first 4 chars of
	// each line — if the line is malformed or empty, the
	// conflict can slip past it. (UU <path> has 4 chars before
	// the path, so it SHOULD be caught; this test pins the
	// behaviour at the dispatchPush re-snapshot layer in case
	// that ever regresses.)
	rig.git.onArgs([]string{"status", "--porcelain"}, "", "", nil)
	// verifyAgentCommitted branch check.
	rig.git.onArgs([]string{"rev-parse", "--abbrev-ref", "HEAD"},
		"wt-bad-agent\n", "", nil)
	// headBefore capture (entry) + headAfter (post-agent).
	// Different SHAs → HEAD advanced.
	rig.git.onArgsSeq([]string{"rev-parse", "HEAD"},
		pushGitResp{"6666666666666666666666666666666666666666\n", "", nil},
		pushGitResp{"7777777777777777777777777777777777777777\n", "", nil},
	)

	claude := &recordingAgent{name: "claude", runOnceText: "I introduced a conflict."}
	withAgent(t, claude)
	_ = rig.cs.SetSelectedAgent("claude")

	s := captureCh(t, rig.cs)
	_, err := dispatchPush(context.Background(), rig.cs,
		HandlerDeps{Git: rig.git}, "chat", "msg", pushArgs{}, "")
	if err != nil {
		t.Fatalf("dispatchPush err: %v", err)
	}
	r := s.lastText()
	if !strings.Contains(r, "unmerged paths") {
		t.Fatalf("expected conflict reply after agent, got:\n%s", r)
	}
	for _, c := range rig.git.calls {
		if c.args[0] == "push" {
			t.Fatalf("agent-introduced conflict must not call push: %v", c.args)
		}
	}
}
