package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
)

// fakeChannel records every Send call; safe for concurrent use.
type fakeChannel struct {
	mu    sync.Mutex
	sends []OutboundMessage
}

func (c *fakeChannel) Name() string { return "fake" }
func (c *fakeChannel) Incoming() <-chan InboundMessage {
	return make(<-chan InboundMessage) // never produces; tests feed via direct Handle calls
}

func (c *fakeChannel) Send(_ context.Context, m OutboundMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sends = append(c.sends, m)
	return nil
}

func (c *fakeChannel) LastText() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.sends) == 0 {
		return ""
	}
	return c.sends[len(c.sends)-1].Text
}

func (c *fakeChannel) AllTexts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.sends))
	for i, m := range c.sends {
		out[i] = m.Text
	}
	return out
}

// fakeAgentSession is a minimal agent.AgentSession for handler tests.
// (Same shape as chatsession/spawn_test.go's fake; duplicated here
// to keep the gateway tests independent of the chatsession test
// internals.)
type fakeAgentSession struct {
	mu     sync.Mutex
	pid    int
	events chan agent.AgentEvent
}

func newFakeAgentSession(pid int) *fakeAgentSession {
	return &fakeAgentSession{pid: pid, events: make(chan agent.AgentEvent, 16)}
}

func (f *fakeAgentSession) Events() <-chan agent.AgentEvent { return f.events }
func (f *fakeAgentSession) PID() int                        { return f.pid }
func (f *fakeAgentSession) SendText(_ string) error         { return nil }
func (f *fakeAgentSession) SendBlocks(_ context.Context, _ []agent.ContentBlock) error {
	return nil
}
func (f *fakeAgentSession) SendPermission(_ string) error { return nil }
func (f *fakeAgentSession) New(_ context.Context) error     { return nil }
func (f *fakeAgentSession) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	select {
	case <-f.events:
	default:
	}
	close(f.events)
	return nil
}

// fakeSpawner wraps chatsession.Spawner; implementation lives in
// chatsession package. We reuse it via a thin wrapper.
type fakeSpawnerAdapter struct {
	f *fakeSpawner
}

func (s *fakeSpawnerAdapter) Spawn(_ context.Context, name, cwd string, args []string, _ string) (agent.AgentSession, error) {
	return s.f.SpawnChatsession(name, cwd, args)
}

// fakeSpawner mirrors chatsession.fakeSpawner but is defined here
// to avoid an import cycle (chatsession tests would need to import
// gateway). Same logic, separate type.
type fakeSpawner struct {
	mu sync.Mutex
	n  int
}

func (s *fakeSpawner) SpawnChatsession(name, cwd string, args []string) (agent.AgentSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	return newFakeAgentSession(20000 + s.n), nil
}

func newTestManager(t *testing.T, withSpawner bool) (*chatsession.Manager, *fakeChannel) {
	t.Helper()
	mgr := chatsession.NewManager()
	if withSpawner {
		// Register fakeSpawner with chatsession.Manager.
		// We need a chatsession.Spawner, not gateway's adapter.
		// chatsession.fakeSpawner is unexported; use a thin inline.
		mgr.WithSpawner(testSpawner{})
	}
	ch := &fakeChannel{}
	return mgr, ch
}

// testSpawner wraps fakeAgentSession spawning into the chatsession.Spawner interface.
type testSpawner struct{}

func (testSpawner) Spawn(_ context.Context, name, cwd string, args []string, _ string) (agent.AgentSession, error) {
	static := newFakeAgentSession(30000 + len(name))
	return static, nil
}

// --- Tests ---

func TestHandleCwd_SetsActiveCwd(t *testing.T) {
	// Use a real (temp) directory so the existence check passes.
	dir := t.TempDir()

	mgr, ch := newTestManager(t, false)
	msg := &InboundMessage{ChatID: "oc_xxx", Text: "/cwd " + dir}

	res, err := handleCwd(context.Background(), mgr, ch, msg, []string{dir}, "claude")
	if err != nil {
		t.Fatalf("handleCwd: %v", err)
	}
	if !res.Consumed {
		t.Fatalf("expected Consumed=true")
	}
	if !strings.Contains(ch.LastText(), "Workspace set to "+dir) {
		t.Fatalf("reply: %q", ch.LastText())
	}

	cs := mgr.Get("oc_xxx")
	if cs == nil {
		t.Fatalf("ChatSession should exist")
	}
	if cs.ActiveCwd() != dir {
		t.Fatalf("ActiveCwd: %q, want %q", cs.ActiveCwd(), dir)
	}
}

// TestHandleCwd_PathResolution covers commit fix-4:
//   - "~" / "~/" expand to $HOME
//   - relative paths are $HOME-relative (not daemon-cwd-relative)
//   - absolute paths pass through
//   - non-existent paths are rejected at /cwd time (not at spawn time)
//
// Uses paths that exist on the test runner (`/tmp` is universally
// available; t.TempDir() for an absolute path case). The earlier
// version used `~/code` which broke on CI runners where $HOME/code
// doesn't exist.
func TestHandleCwd_PathResolution(t *testing.T) {
	dir := t.TempDir()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}

	// `tmpdir` is an actual subdirectory inside `dir` so the
	// tilde-with-subpath and relative-path tests resolve to a path
	// that exists on every CI runner. We pre-create the
	// subdirectory so the existence check passes.
	tmpdir := "tmpdir"
	realSubdir := filepath.Join(dir, tmpdir)
	if err := os.Mkdir(realSubdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	homeStyleSubdir := filepath.Join(home, tmpdir)
	if err := os.Mkdir(homeStyleSubdir, 0o755); err != nil {
		// HOME may not be writable (e.g., some CI containers); skip
		// the HOME-relative subpath tests in that case rather than
		// fail.
		t.Skipf("HOME not writable: %v", err)
	}

	mgr, ch := newTestManager(t, false)

	cases := []struct {
		name         string
		input        string
		wantResolved string
	}{
		{"absolute path", dir, dir},
		{"tilde alone", "~", home},
		{"tilde slash", "~/", home},
		{"tilde with subpath", "~/" + tmpdir, homeStyleSubdir},
		{"relative is HOME-relative", tmpdir, homeStyleSubdir},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Use a fresh chat per case to avoid state bleed.
			chatID := "oc_" + tc.name
			msg := &InboundMessage{ChatID: chatID}

			res, err := handleCwd(context.Background(), mgr, ch, msg, []string{tc.input}, "claude")
			if err != nil {
				t.Fatalf("handleCwd: %v", err)
			}
			if !res.Consumed {
				t.Fatalf("expected Consumed=true")
			}
			if !strings.Contains(ch.LastText(), "Workspace set to "+tc.wantResolved) {
				t.Fatalf("input %q: reply %q, want resolved to %q", tc.input, ch.LastText(), tc.wantResolved)
			}

			cs := mgr.Get(chatID)
			if cs == nil || cs.ActiveCwd() != tc.wantResolved {
				t.Fatalf("input %q: ActiveCwd=%q, want %q", tc.input, cs.ActiveCwd(), tc.wantResolved)
			}
		})
	}
}

// TestHandleCwd_RejectsNonExistentPath verifies the existence check
// from commit fix-4 (issue observed 2026-08-02: /cwd silently set a
// non-existent path, then spawn failed with a confusing error).
func TestHandleCwd_RejectsNonExistentPath(t *testing.T) {
	mgr, ch := newTestManager(t, false)
	missing := "/this/path/definitely/does/not/exist/xyz123"
	msg := &InboundMessage{ChatID: "oc_xxx"}

	res, err := handleCwd(context.Background(), mgr, ch, msg, []string{missing}, "claude")
	if err != nil {
		t.Fatalf("handleCwd: %v", err)
	}
	if !res.Consumed {
		t.Fatalf("expected Consumed=true")
	}
	if !strings.Contains(ch.LastText(), "Path does not exist") {
		t.Fatalf("reply should mention 'Path does not exist': %q", ch.LastText())
	}
	// ChatSession should NOT have been created (no state mutation
	// on rejection).
	if mgr.Get("oc_xxx") != nil {
		t.Fatalf("ChatSession should not exist after rejected /cwd")
	}
}

// TestHandleCwd_RejectsFileNotDirectory verifies the directory check.
func TestHandleCwd_RejectsFileNotDirectory(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "regular_file.txt")
	if err := os.WriteFile(file, []byte("not a dir"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	mgr, ch := newTestManager(t, false)
	msg := &InboundMessage{ChatID: "oc_xxx"}

	res, err := handleCwd(context.Background(), mgr, ch, msg, []string{file}, "claude")
	if err != nil {
		t.Fatalf("handleCwd: %v", err)
	}
	if res == nil || !res.Consumed {
		t.Fatalf("expected Consumed=true, got %v", res)
	}
	if !strings.Contains(ch.LastText(), "Not a directory") {
		t.Fatalf("reply should mention 'Not a directory': %q", ch.LastText())
	}
}

func TestHandleCwd_NoArg_RepliesUsage(t *testing.T) {
	mgr, ch := newTestManager(t, false)
	res, err := handleCwd(context.Background(), mgr, ch, &InboundMessage{ChatID: "oc_xxx"}, nil, "claude")
	if err != nil {
		t.Fatalf("handleCwd: %v", err)
	}
	if !res.Consumed {
		t.Fatalf("expected Consumed=true")
	}
	if !strings.Contains(ch.LastText(), "Usage") {
		t.Fatalf("reply: %q", ch.LastText())
	}
}

func TestHandleUse_NoActiveCwd_RepliesError(t *testing.T) {
	mgr, ch := newTestManager(t, true)
	res, err := handleUse(context.Background(), mgr, ch,
		&InboundMessage{ChatID: "oc_xxx"},
		[]string{"claude"}, "claude")
	if err != nil {
		t.Fatalf("handleUse: %v", err)
	}
	if !res.Consumed {
		t.Fatalf("expected Consumed=true")
	}
	if !strings.Contains(ch.LastText(), "/cwd") {
		t.Fatalf("reply should mention /cwd: %q", ch.LastText())
	}
}

func TestHandleUse_LazySpawn(t *testing.T) {
	mgr, ch := newTestManager(t, true)
	cs := mgr.GetOrCreate("oc_xxx", "claude")
	cs.SetActiveCwd("/code/bailing")

	res, err := handleUse(context.Background(), mgr, ch,
		&InboundMessage{ChatID: "oc_xxx"},
		[]string{"claude"}, "claude")
	if err != nil {
		t.Fatalf("handleUse: %v", err)
	}
	if !res.Consumed {
		t.Fatalf("expected Consumed=true")
	}
	if !strings.Contains(ch.LastText(), "Now using claude") {
		t.Fatalf("reply: %q", ch.LastText())
	}
	if as := cs.ActiveAgentSession(); as == nil {
		t.Fatalf("expected active AgentSession")
	} else if as.Status() != chatsession.StatusRunning {
		t.Fatalf("expected Running after /use, got %q", as.Status())
	}
}

func TestHandleUse_NoArg_RepliesUsage(t *testing.T) {
	mgr, ch := newTestManager(t, false)
	res, err := handleUse(context.Background(), mgr, ch,
		&InboundMessage{ChatID: "oc_xxx"},
		nil, "claude")
	if err != nil {
		t.Fatalf("handleUse: %v", err)
	}
	if !res.Consumed {
		t.Fatalf("expected Consumed=true")
	}
	if !strings.Contains(ch.LastText(), "Usage") {
		t.Fatalf("reply: %q", ch.LastText())
	}
}

func TestHandleKill_NoChatSession_RepliesCalmly(t *testing.T) {
	mgr, ch := newTestManager(t, false)
	res, err := handleKill(context.Background(), mgr, ch,
		&InboundMessage{ChatID: "oc_unknown"})
	if err != nil {
		t.Fatalf("handleKill: %v", err)
	}
	if !res.Consumed {
		t.Fatalf("expected Consumed=true")
	}
	if !strings.Contains(ch.LastText(), "No active") {
		t.Fatalf("reply: %q", ch.LastText())
	}
}

func TestHandleKill_ClearsPool(t *testing.T) {
	mgr, ch := newTestManager(t, true)
	cs := mgr.GetOrCreate("oc_xxx", "claude")
	cs.SetActiveCwd("/code/bailing")
	cs.SetActiveAgent("claude")
	cs.LookupActiveAgentSession() // spawn
	cs.LookupActiveAgentSession() // second lookup same agent

	if len(cs.Pool()) == 0 {
		t.Fatalf("precondition: pool should have entries")
	}

	res, err := handleKill(context.Background(), mgr, ch,
		&InboundMessage{ChatID: "oc_xxx"})
	if err != nil {
		t.Fatalf("handleKill: %v", err)
	}
	if !res.Consumed {
		t.Fatalf("expected Consumed=true")
	}
	if !strings.Contains(ch.LastText(), "Killed") {
		t.Fatalf("reply: %q", ch.LastText())
	}
	if len(cs.Pool()) != 0 {
		t.Fatalf("pool should be empty after kill, got %d", len(cs.Pool()))
	}
	// activeCwd and activeAgent survive /kill.
	if cs.ActiveCwd() != "/code/bailing" {
		t.Fatalf("ActiveCwd should survive /kill, got %q", cs.ActiveCwd())
	}
}

func TestRegisterChatSessionCommands_RegistersAllThree(t *testing.T) {
	mgr, ch := newTestManager(t, false)
	gw := New(nil)
	RegisterChatSessionCommands(gw, mgr, ch, "claude")

	// Command.Name is stored without the leading slash (Gateway strips
	// it in ParseCommand). The user-facing slash stays in the chat
	// message text — see TestParseCommand_StripsLeadingSlash.
	for _, name := range []string{"cwd", "use", "kill"} {
		found := false
		for _, c := range gw.ListCommands() {
			if c.Name == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("command %s not registered", name)
		}
	}
}

// Compile-time guard: handleUse / handleCwd / handleKill are
// consumed by RegisterChatSessionCommands. Pinning the references
// here so future refactors don't accidentally drop them.
var (
	_ = handleCwd
	_ = handleUse
	_ = handleKill
)
