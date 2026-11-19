package gtw

// The rig for the /gtw commands whose argument is free-form text:
// the chat session talks to a capture agent instead of a real bridge,
// so a test can read back what the agent session received.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
)

// captureAgent records the blocks the chat session hands to the
// agent — one entry per turn.
type captureAgent struct {
	mu     sync.Mutex
	turns  [][]agent.ContentBlock
	events chan agent.AgentEvent
}

func (c *captureAgent) SendBlocks(_ context.Context, blocks []agent.ContentBlock) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.turns = append(c.turns, blocks)
	return nil
}

func (c *captureAgent) SendPermission(string) error { return nil }
func (c *captureAgent) Reset(context.Context) error { return nil }
func (c *captureAgent) Close() error                { return nil }
func (c *captureAgent) Stop(context.Context) error  { return agent.ErrNotSupported }
func (c *captureAgent) Keepalive(context.Context, func(context.Context) error) error {
	return nil
}

// sent returns the turns the agent received.
func (c *captureAgent) sent() [][]agent.ContentBlock {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]agent.ContentBlock(nil), c.turns...)
}

// captureSpawner hands the chat session a captureAgent instead of
// forking a real agent process.
type captureSpawner struct {
	cap *captureAgent
}

func (s *captureSpawner) Spawn(_ context.Context, _, _ string, _ []string, _ string) (*agent.Agent, error) {
	return agent.NewAgent(agent.NewInfo("capture", agent.ModePTY, "capture", nil, nil), 4242, s.cap.events, s.cap), nil
}

// materialRig is a chat session in a real temp repo whose agent
// session is the capture agent.
type materialRig struct {
	f    *Factory
	cs   *chatsession.ChatSession
	prov *fakeGitProvider
	cap  *captureAgent
	ch   *recordingCh
}

func newMaterialRig(t *testing.T, chatID string) *materialRig {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	repo := initTempRepo(t)
	mustGit(t, repo, "remote", "add", "origin", "https://github.com/o/r.git")

	ch := &recordingCh{}
	cs, err := chatsession.New(chatID, "test-agent")
	if err != nil {
		t.Fatalf("chatsession.New: %v", err)
	}
	cs.WithEmitter(ch)
	cap := &captureAgent{events: make(chan agent.AgentEvent, 8)}
	cs.WithSpawner(&captureSpawner{cap: cap})
	if err := cs.SetSelectedCwd(repo); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	f := NewFactoryWithDeps(NewManager(), HandlerDeps{
		Git:    ExecGitRunner{},
		Detect: fakeDetect(prov),
		Now:    time.Now,
	})
	return &materialRig{f: f, cs: cs, prov: prov, cap: cap, ch: ch}
}

// draftDir is the chat's issue draft directory.
func (r *materialRig) draftDir(t *testing.T) string {
	t.Helper()
	dir, err := issueDraftDir(r.cs.ChatID)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// send runs one slash message the way the commander does: the raw
// text plus the argv it derived from it.
func (r *materialRig) send(t *testing.T, text string) *command.SlashOutput {
	t.Helper()
	fields := strings.Fields(text)
	if len(fields) == 0 {
		t.Fatal("send: empty text")
	}
	out, err := r.f.Handle(context.Background(), command.RuntimeServices{}, nil, r.cs, command.SlashInput{
		Text:      text,
		Args:      append([]string{strings.TrimPrefix(fields[0], "/")}, fields[1:]...),
		ChatID:    r.cs.ChatID,
		MessageID: "msg-1",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	return out
}
