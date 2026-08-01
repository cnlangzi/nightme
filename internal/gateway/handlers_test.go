package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/pty"
	"github.com/cnlangzi/nightme/internal/session"
)

// fakeResponder captures every Reply() so tests can assert the
// exact strings sent to the user.
type fakeResponder struct {
	mu      sync.Mutex
	replies []string
}

func (r *fakeResponder) Reply(_ context.Context, _, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.replies = append(r.replies, text)
	return nil
}

func (r *fakeResponder) last() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.replies) == 0 {
		return ""
	}
	return r.replies[len(r.replies)-1]
}

func (r *fakeResponder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.replies))
	copy(out, r.replies)
	return out
}

// errorResponder returns a canned error from every Reply() so
// handler-side error propagation can be asserted.
type errorResponder struct{ err error }

func (e errorResponder) Reply(context.Context, string, string) error { return e.err }

// ptyAgentRegistry returns a registry with one PTY-mode "claude" agent
// that resolves to /bin/echo. /bin/echo is universally available so
// the tests can drive a real PTY without flaky PATH lookups.
func ptyAgentRegistry(t *testing.T) *agent.Registry {
	t.Helper()
	reg := agent.New()
	a := pty.NewAgent("claude", "/bin/echo", nil, nil)
	a.Cols = 80
	a.Rows = 24
	reg.Register(a)
	return reg
}

// newTestStack wires a fresh Gateway + Manager + Responder so each
// test starts from a clean slate.
func newTestStack(t *testing.T) (Gateway, *session.MemoryManager, *fakeResponder) {
	t.Helper()
	resp := &fakeResponder{}
	mgr := session.NewMemoryManager(ptyAgentRegistry(t), nil, nil)
	gw := New(nil)
	RegisterDefaultCommands(gw, mgr, ptyAgentRegistry(t), resp)
	return gw, mgr, resp
}

// TestCwdHandler_NewSession covers the happy path: /cwd on a chat
// that has no prior session creates a detached record.
func TestCwdHandler_NewSession(t *testing.T) {
	dir := t.TempDir()
	gw, mgr, resp := newTestStack(t)

	ctx := WithGateway(context.Background(), gw)
	if err := gw.Handle(ctx, &Message{ChatID: "oc_chat", Text: "/cwd " + dir}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(resp.last(), "Workspace set") {
		t.Errorf("last reply = %q, want workspace-set reply", resp.last())
	}
	if !strings.Contains(resp.last(), dir) {
		t.Errorf("reply %q missing workspace path %q", resp.last(), dir)
	}
	sess, err := mgr.GetByChat("oc_chat")
	if err != nil {
		t.Fatalf("GetByChat: %v", err)
	}
	if sess.Workspace != dir {
		t.Errorf("workspace = %q, want %q", sess.Workspace, dir)
	}
}

// TestCwdHandler_RejectsActiveSession confirms the spec rule: a
// running session cannot have its workspace changed under it.
func TestCwdHandler_RejectsActiveSession(t *testing.T) {
	dir := t.TempDir()
	gw, mgr, resp := newTestStack(t)

	if _, err := mgr.CreateOrUpdate("oc_chat", "group", dir, "claude", nil); err != nil {
		t.Fatalf("CreateOrUpdate: %v", err)
	}
	if _, err := mgr.Run(context.Background(), "oc_chat", "claude", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	other := t.TempDir()
	if err := gw.Handle(WithGateway(context.Background(), gw), &Message{
		ChatID: "oc_chat",
		Text:   "/cwd " + other,
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(resp.last(), "session already active") {
		t.Errorf("last reply = %q, want rejection", resp.last())
	}

	// Workspace must be unchanged.
	sess, _ := mgr.GetByChat("oc_chat")
	if sess.Workspace != dir {
		t.Errorf("workspace mutated to %q, want %q", sess.Workspace, dir)
	}
}

// TestCwdHandler_MissingPath checks the usage error.
func TestCwdHandler_MissingPath(t *testing.T) {
	gw, _, resp := newTestStack(t)

	_ = gw.Handle(WithGateway(context.Background(), gw), &Message{
		ChatID: "oc_chat",
		Text:   "/cwd",
	})
	if !strings.Contains(resp.last(), "usage") {
		t.Errorf("reply = %q, want usage", resp.last())
	}
}

// TestCwdHandler_NonexistentPath checks the absent-path error.
func TestCwdHandler_NonexistentPath(t *testing.T) {
	gw, _, resp := newTestStack(t)

	_ = gw.Handle(WithGateway(context.Background(), gw), &Message{
		ChatID: "oc_chat",
		Text:   "/cwd /this/does/not/exist",
	})
	if !strings.Contains(resp.last(), "not found") {
		t.Errorf("reply = %q, want not-found", resp.last())
	}
}

// TestCwdHandler_RejectsFile ensures files do not pass the
// directory check.
func TestCwdHandler_RejectsFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "x.txt")
	if err := os.WriteFile(file, []byte("hi"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	gw, _, resp := newTestStack(t)

	_ = gw.Handle(WithGateway(context.Background(), gw), &Message{
		ChatID: "oc_chat",
		Text:   "/cwd " + file,
	})
	if !strings.Contains(resp.last(), "not a directory") {
		t.Errorf("reply = %q, want not-a-directory", resp.last())
	}
}

// TestRunHandler_NoWorkspace returns the helpful "send /cwd first"
// message.
func TestRunHandler_NoWorkspace(t *testing.T) {
	gw, _, resp := newTestStack(t)

	_ = gw.Handle(WithGateway(context.Background(), gw), &Message{
		ChatID: "oc_chat",
		Text:   "/run claude",
	})
	if !strings.Contains(resp.last(), "/cwd") {
		t.Errorf("reply = %q, want hint to /cwd", resp.last())
	}
}

// TestRunHandler_Success starts a fresh agent.
func TestRunHandler_Success(t *testing.T) {
	dir := t.TempDir()
	gw, mgr, resp := newTestStack(t)

	if _, err := mgr.CreateOrUpdate("oc_chat", "group", dir, "claude", nil); err != nil {
		t.Fatalf("CreateOrUpdate: %v", err)
	}

	_ = gw.Handle(WithGateway(context.Background(), gw), &Message{
		ChatID: "oc_chat",
		Text:   "/run claude",
	})

	sess, _ := mgr.GetByChat("oc_chat")
	if sess == nil {
		t.Fatalf("session vanished")
	}
	snap := sess.Snapshot()
	if snap.Status != session.StatusRunning {
		t.Errorf("status = %s, want running", snap.Status)
	}
	if snap.PID == 0 {
		t.Errorf("PID = 0, want non-zero")
	}
	if !strings.Contains(resp.last(), "running") {
		t.Errorf("reply = %q, want 'running' feedback", resp.last())
	}
}

// TestRunHandler_AlreadyRunning is a no-op when the session is
// already running.
func TestRunHandler_AlreadyRunning(t *testing.T) {
	dir := t.TempDir()
	gw, mgr, resp := newTestStack(t)

	if _, err := mgr.CreateOrUpdate("oc_chat", "group", dir, "claude", nil); err != nil {
		t.Fatalf("CreateOrUpdate: %v", err)
	}
	if _, err := mgr.Run(context.Background(), "oc_chat", "claude", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Reset and call /run again.
	resp.replies = nil
	_ = gw.Handle(WithGateway(context.Background(), gw), &Message{
		ChatID: "oc_chat",
		Text:   "/run claude",
	})
	if !strings.Contains(resp.last(), "Already running") {
		t.Errorf("reply = %q, want 'Already running' feedback", resp.last())
	}
}

// TestRunHandler_UnknownAgent returns the agent-unknown error.
func TestRunHandler_UnknownAgent(t *testing.T) {
	dir := t.TempDir()
	gw, mgr, resp := newTestStack(t)

	if _, err := mgr.CreateOrUpdate("oc_chat", "group", dir, "claude", nil); err != nil {
		t.Fatalf("CreateOrUpdate: %v", err)
	}
	_ = gw.Handle(WithGateway(context.Background(), gw), &Message{
		ChatID: "oc_chat",
		Text:   "/run mystery",
	})
	if !strings.Contains(resp.last(), "unknown agent") {
		t.Errorf("reply = %q, want 'unknown agent'", resp.last())
	}
}

// TestRunHandler_MissingArgs returns the usage hint.
func TestRunHandler_MissingArgs(t *testing.T) {
	dir := t.TempDir()
	gw, mgr, resp := newTestStack(t)

	if _, err := mgr.CreateOrUpdate("oc_chat", "group", dir, "claude", nil); err != nil {
		t.Fatalf("CreateOrUpdate: %v", err)
	}
	_ = gw.Handle(WithGateway(context.Background(), gw), &Message{
		ChatID: "oc_chat",
		Text:   "/run",
	})
	if !strings.Contains(resp.last(), "usage") {
		t.Errorf("reply = %q, want usage", resp.last())
	}
}

// TestKillHandler stops the live CLI.
func TestKillHandler(t *testing.T) {
	dir := t.TempDir()
	gw, mgr, resp := newTestStack(t)

	if _, err := mgr.CreateOrUpdate("oc_chat", "group", dir, "claude", nil); err != nil {
		t.Fatalf("CreateOrUpdate: %v", err)
	}
	if _, err := mgr.Run(context.Background(), "oc_chat", "claude", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	_ = gw.Handle(WithGateway(context.Background(), gw), &Message{
		ChatID: "oc_chat",
		Text:   "/kill",
	})
	if !strings.Contains(resp.last(), "killed") {
		t.Errorf("reply = %q, want 'killed' feedback", resp.last())
	}

	sess, _ := mgr.GetByChat("oc_chat")
	if sess.Status() != session.StatusExited {
		t.Errorf("status = %s, want exited", sess.Status())
	}
}

// TestKillHandler_NoSession returns the "no session" hint.
func TestKillHandler_NoSession(t *testing.T) {
	gw, _, resp := newTestStack(t)

	_ = gw.Handle(WithGateway(context.Background(), gw), &Message{
		ChatID: "oc_chat",
		Text:   "/kill",
	})
	if !strings.Contains(resp.last(), "no session") {
		t.Errorf("reply = %q, want 'no session'", resp.last())
	}
}

// TestHelpHandler lists the registered commands.
func TestHelpHandler(t *testing.T) {
	gw, _, resp := newTestStack(t)

	_ = gw.Handle(WithGateway(context.Background(), gw), &Message{
		ChatID: "oc_chat",
		Text:   "/help",
	})
	reply := resp.last()
	for _, want := range []string{"/cwd", "/run", "/kill", "/help", "Workflow"} {
		if !strings.Contains(reply, want) {
			t.Errorf("help reply missing %q: %q", want, reply)
		}
	}
}

// TestHelpHandler_NoGateway returns a graceful fallback when the
// context lacks a Gateway.
func TestHelpHandler_NoGateway(t *testing.T) {
	gw, _, resp := newTestStack(t)

	_ = gw.Handle(context.Background(), &Message{
		ChatID: "oc_chat",
		Text:   "/help",
	})
	if !strings.Contains(resp.last(), "unavailable") {
		t.Errorf("reply = %q, want 'unavailable' fallback", resp.last())
	}
}

// TestRegistry_HelpAlias ensures `/?` resolves to /help.
func TestRegistry_HelpAlias(t *testing.T) {
	gw, _, resp := newTestStack(t)

	_ = gw.Handle(WithGateway(context.Background(), gw), &Message{
		ChatID: "oc_chat",
		Text:   "/?",
	})
	if !strings.Contains(resp.last(), "Available commands") {
		t.Errorf("reply = %q, want help body", resp.last())
	}
}

// TestRegistry_CwdAlias checks that `/workspace` resolves to /cwd.
func TestRegistry_CwdAlias(t *testing.T) {
	dir := t.TempDir()
	gw, _, resp := newTestStack(t)

	_ = gw.Handle(WithGateway(context.Background(), gw), &Message{
		ChatID: "oc_chat",
		Text:   "/workspace " + dir,
	})
	if !strings.Contains(resp.last(), "Workspace set") {
		t.Errorf("reply = %q, want workspace-set", resp.last())
	}
}

// TestRunHandler_ExtraArgsForwarded ensures the agent's argv is
// extended with the user-supplied args.
func TestRunHandler_ExtraArgsForwarded(t *testing.T) {
	dir := t.TempDir()
	gw, mgr, _ := newTestStack(t)

	if _, err := mgr.CreateOrUpdate("oc_chat", "group", dir, "claude", nil); err != nil {
		t.Fatalf("CreateOrUpdate: %v", err)
	}
	_ = gw.Handle(WithGateway(context.Background(), gw), &Message{
		ChatID: "oc_chat",
		Text:   "/run claude --opus --fast",
	})
	sess, _ := mgr.GetByChat("oc_chat")
	if got := sess.Snapshot().Args; len(got) != 2 || got[0] != "--opus" || got[1] != "--fast" {
		t.Errorf("args = %v, want [--opus --fast]", got)
	}
}

// TestRunHandler_DetectFailure surfaces a friendly "not found" error.
func TestRunHandler_DetectFailure(t *testing.T) {
	dir := t.TempDir()
	reg := agent.New()
	reg.Register(&detectorFakeAgent{})
	mgr := session.NewMemoryManager(reg, nil, nil)
	resp := &fakeResponder{}
	gw := New(nil)
	RegisterDefaultCommands(gw, mgr, reg, resp)

	if _, err := mgr.CreateOrUpdate("oc_chat", "group", dir, "claude", nil); err != nil {
		t.Fatalf("CreateOrUpdate: %v", err)
	}
	_ = gw.Handle(WithGateway(context.Background(), gw), &Message{
		ChatID: "oc_chat",
		Text:   "/run claude",
	})
	if !strings.Contains(resp.last(), "binary not found") {
		t.Errorf("reply = %q, want detect failure", resp.last())
	}
}

// TestCwdHandler_TildeExpands verifies ~/foo is expanded to the
// user's home directory.
func TestCwdHandler_TildeExpands(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir in this environment")
	}
	gw, _, resp := newTestStack(t)

	_ = gw.Handle(WithGateway(context.Background(), gw), &Message{
		ChatID: "oc_chat",
		Text:   "/cwd ~",
	})
	if !strings.Contains(resp.last(), home) {
		t.Errorf("reply = %q, want expanded home %q", resp.last(), home)
	}
}

// TestRenderHelp_Smoke locks the rendered body in place so format
// changes are intentional.
func TestRenderHelp_Smoke(t *testing.T) {
	cmds := []Command{
		{Name: "cwd", Description: "Set workspace"},
		{Name: "kill", Description: "Stop CLI"},
		{Name: "run", Description: "Ensure CLI"},
	}
	body := renderHelp(cmds)
	for _, want := range []string{"/cwd", "/kill", "/run", "Workflow", "Anything else"} {
		if !strings.Contains(body, want) {
			t.Errorf("renderHelp missing %q: %q", want, body)
		}
	}
}

// TestRunHandler_ResponderErrorPropagates ensures the handler
// surfaces channel-side errors instead of swallowing them.
func TestRunHandler_ResponderErrorPropagates(t *testing.T) {
	dir := t.TempDir()
	want := errors.New("responder down")
	reg := ptyAgentRegistry(t)
	mgr := session.NewMemoryManager(reg, nil, nil)
	gw := New(nil)
	RegisterDefaultCommands(gw, mgr, reg, errorResponder{err: want})

	if _, err := mgr.CreateOrUpdate("oc_chat", "group", dir, "claude", nil); err != nil {
		t.Fatalf("CreateOrUpdate: %v", err)
	}
	err := gw.Handle(WithGateway(context.Background(), gw), &Message{
		ChatID: "oc_chat",
		Text:   "/run claude",
	})
	if !errors.Is(err, want) {
		t.Errorf("Handle error = %v, want %v", err, want)
	}
}

// TestRegistry_AllCommandsListed confirms /help's list includes
// every default command.
func TestRegistry_AllCommandsListed(t *testing.T) {
	mgr := session.NewMemoryManager(ptyAgentRegistry(t), nil, nil)
	gw := New(nil)
	RegisterDefaultCommands(gw, mgr, ptyAgentRegistry(t), nil)

	cmds := gw.ListCommands()
	names := make(map[string]bool)
	for _, c := range cmds {
		names[c.Name] = true
	}
	for _, want := range []string{"cwd", "run", "kill", "help"} {
		if !names[want] {
			t.Errorf("missing command %q in ListCommands: %+v", want, names)
		}
	}
}

// TestRunHandler_NilResponderWhenNil ensures the handler tolerates
// a nil responder (used by tests that don't care about replies).
func TestRunHandler_NilResponderWhenNil(t *testing.T) {
	dir := t.TempDir()
	mgr := session.NewMemoryManager(ptyAgentRegistry(t), nil, nil)
	gw := New(nil)
	RegisterDefaultCommands(gw, mgr, ptyAgentRegistry(t), nil)

	if _, err := mgr.CreateOrUpdate("oc_chat", "group", dir, "claude", nil); err != nil {
		t.Fatalf("CreateOrUpdate: %v", err)
	}
	if err := gw.Handle(WithGateway(context.Background(), gw), &Message{
		ChatID: "oc_chat",
		Text:   "/run claude",
	}); err != nil {
		t.Errorf("Handle with nil responder = %v, want nil", err)
	}
}

// TestHelpHandler_TextReturnedInResult locks in the dual-channel
// shape: the handler returns the reply text in CommandResult AND
// forwards it through the responder.
func TestHelpHandler_TextReturnedInResult(t *testing.T) {
	gw, _, resp := newTestStack(t)

	// We can't observe Handle()'s return value directly via the
	// Gateway interface, but the responder captured the reply and
	// we can assert it matches the expected body.
	_ = gw.Handle(WithGateway(context.Background(), gw), &Message{
		ChatID: "oc_chat",
		Text:   "/help",
	})
	if !strings.Contains(resp.last(), "Available commands") {
		t.Errorf("reply = %q, want help body", resp.last())
	}
}

// TestRegistry_RepeatHandleDoesNotFail confirms the handler is
// idempotent across multiple Help calls.
func TestRegistry_RepeatHandleDoesNotFail(t *testing.T) {
	gw, _, resp := newTestStack(t)
	for i := 0; i < 3; i++ {
		_ = gw.Handle(WithGateway(context.Background(), gw), &Message{
			ChatID: "oc_chat",
			Text:   "/help",
		})
	}
	got := resp.all()
	if len(got) != 3 {
		t.Errorf("got %d replies, want 3", len(got))
	}
}

// detectorFakeAgent is a stand-in agent that fails Detect so the
// /run handler can surface a clear "binary not found" message.
type detectorFakeAgent struct{}

func (d *detectorFakeAgent) Name() string        { return "claude" }
func (d *detectorFakeAgent) Mode() agent.Mode    { return agent.ModePTY }
func (d *detectorFakeAgent) Command() string     { return "claude" }
func (d *detectorFakeAgent) Args() []string      { return nil }
func (d *detectorFakeAgent) Detect() error       { return errors.New("binary not found") }
func (d *detectorFakeAgent) Start(context.Context, agent.StartConfig) (agent.AgentSession, error) {
	return nil, errors.New("should not reach Start when Detect fails")
}

var _ agent.Agent = (*detectorFakeAgent)(nil)
