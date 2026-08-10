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
)

// pushGit is a GitRunner whose responses are configured per
// argv-prefix. Tests register (prefix → response) tuples; unmatched
// git calls return an error so the test fails fast on a surprise.
//
// One-off: each Run returns the captured stdout/stderr/error triple.
type pushGit struct {
	mu               sync.Mutex
	responses        map[string]pushGitResp
	responsesByArgs  map[string]pushGitResp
	calls            []pushGitCall
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
		responses:       make(map[string]pushGitResp),
		responsesByArgs: make(map[string]pushGitResp),
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

func (f *pushGit) Run(_ context.Context, dir string, args ...string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, pushGitCall{dir: dir, args: append([]string(nil), args...)})
	if len(args) == 0 {
		return "", "", errors.New("pushGit: empty argv")
	}
	// Specific (full-argv) match wins over first-token match.
	if resp, ok := f.responsesByArgs[joinArgs(args)]; ok {
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

	mustContain(t, p, "Working directory: /w")
	mustContain(t, p, "Branch: fix-42-foo")
	mustContain(t, p, "Issue: #42")
	mustContain(t, p, "feat, fix, chore, refactor")
	mustContain(t, p, "Reference issue with #42 in body")
	mustContain(t, p, "git push -u origin fix-42-foo")
}

func TestBuildAgentPrompt_Local(t *testing.T) {
	c := Context{
		Worktree: "/w",
		Branch:   "wt-local",
		Issue:    -1, // ModeLocal
	}
	p := buildAgentPrompt(c)

	mustContain(t, p, "Working directory: /w")
	mustContain(t, p, "Branch: wt-local")
	if strings.Contains(p, "Issue: #") {
		t.Fatalf("Local prompt should not contain 'Issue: #':\n%s", p)
	}
	if strings.Contains(p, "Reference issue with #") {
		t.Fatalf("Local prompt should not contain 'Reference issue':\n%s", p)
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
	git.on("status", "", "", nil)
	// rev-list errors with "no upstream configured" → countUnpushed
	// returns 0 (treated as fresh branch).
	git.on("rev-list", "", "fatal: no upstream configured for branch 'wt-clean'", errors.New("exit 128"))

	withAgent(t)
	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-clean",
		RepoRoot: mustPwd(t),
	})

	res, err := dispatchPush(context.Background(), &chatsession.ChatSession{},
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{})
	if err != nil || res == nil {
		t.Fatalf("dispatchPush err=%v res=%v", err, res)
	}
	// We expect: status once, then rev-list (no upstream → 0).
	// NO push call (0 unpushed → terminal "nothing to push" reply).
	for _, c := range git.calls {
		if c.args[0] == "push" {
			t.Fatalf("clean + 0 unpushed should NOT call git push: %v", c.args)
		}
	}
}

func TestRunPush_CleanWithUnpushed(t *testing.T) {
	git := newPushGit()
	git.on("status", "", "", nil)
	git.on("rev-parse", "", "", nil) // upstream exists
	git.on("rev-list", "3", "", nil) // 3 unpushed
	git.on("push", "To origin", "", nil)

	withAgent(t)
	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-clean-unpushed",
		RepoRoot: mustPwd(t),
	})

	_, err := dispatchPush(context.Background(), &chatsession.ChatSession{},
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
}

func TestRunPush_DirtyDelegatesToAgent(t *testing.T) {
	git := newPushGit()
	git.on("status", "M foo.go\n", "", nil)

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
	cs := &chatsession.ChatSession{}
	_ = cs.SetSelectedAgent("claude")

	_, err := dispatchPush(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{})
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
	if !strings.Contains(prompt, "Branch: wt-dirty") {
		t.Fatalf("prompt missing branch:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Issue: #7") {
		t.Fatalf("prompt missing issue:\n%s", prompt)
	}
}

func TestRunPush_DirtyWithAgentFlag(t *testing.T) {
	git := newPushGit()
	git.on("status", "M foo.go\n", "", nil)

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
	cs := &chatsession.ChatSession{}
	_ = cs.SetSelectedAgent("claude") // chat default = claude

	_, err := dispatchPush(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{Agent: "opencode"})
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

	_, err := dispatchPush(context.Background(), &chatsession.ChatSession{},
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{})
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
	cs := &chatsession.ChatSession{}
	_ = cs.SetSelectedAgent("claude")

	_, err := dispatchPush(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{Agent: "nope"})
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
	cs := &chatsession.ChatSession{}
	_ = cs.SetSelectedAgent("claude")

	_, err := dispatchPush(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{})
	if err != nil {
		t.Fatalf("RunPush: %v", err)
	}
	if len(claude.calls) != 0 {
		t.Fatalf("Detect failed → RunOnce must NOT be called")
	}
}

func TestRunPush_AgentRunOnceError(t *testing.T) {
	git := newPushGit()
	git.on("status", "M foo.go\n", "", nil)

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
	cs := &chatsession.ChatSession{}
	_ = cs.SetSelectedAgent("claude")

	_, err := dispatchPush(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{})
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
	cs := &chatsession.ChatSession{}
	_ = cs.SetSelectedAgent("claude")

	_, err := dispatchPush(context.Background(), cs,
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{})
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

	_, err := dispatchPush(context.Background(), &chatsession.ChatSession{},
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{})
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

	_, err := dispatchPush(context.Background(), &chatsession.ChatSession{},
		HandlerDeps{Git: git}, "chat", "msg", pushArgs{})
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
