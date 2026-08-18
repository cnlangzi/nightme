package bot

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/wfe"
)

func TestBot_Name(t *testing.T) {
	b := New(Config{})
	if got := b.Name(); got != "bot" {
		t.Errorf("Name = %q, want bot", got)
	}
}

// TestBot_Incoming_Buffered verifies Incoming returns a buffered
// channel. The test accesses b.in directly (same package) since
// Incoming() returns the receive-only view per the Channel
// contract.
func TestBot_Incoming_Buffered(t *testing.T) {
	b := New(Config{})
	if b.Incoming() == nil {
		t.Fatal("Incoming() returned nil")
	}
	select {
	case b.in <- messages.InboundMessage{ChatID: "test", Text: "hi"}:
	default:
		t.Error("incoming channel should be buffered")
	}
	<-b.in
}

func TestBot_Send_NoRun(t *testing.T) {
	b := New(Config{})
	err := b.Send(context.Background(), messages.OutboundMessage{
		ChatID: "unknown",
		Kind:   messages.OutReply,
		Text:   "hello",
	})
	if err != nil {
		t.Errorf("Send on unknown chatID should be no-op, got %v", err)
	}
}

func TestBot_Send_DeliversToRun(t *testing.T) {
	b := New(Config{})
	chatID := "bot:wf:test:1"
	r := &botRun{chatID: chatID, reply: make(chan string, 1)}
	b.muRuns.Lock()
	b.runsByChatID[chatID] = r
	b.muRuns.Unlock()

	if err := b.Send(context.Background(), messages.OutboundMessage{
		ChatID: chatID,
		Kind:   messages.OutReply,
		Text:   "agent reply",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case got := <-r.reply:
		if got != "agent reply" {
			t.Errorf("reply = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for reply")
	}
}

func TestBot_HealthSnapshot(t *testing.T) {
	b := New(Config{})
	name, payload, err := b.HealthSnapshot()
	if err != nil {
		t.Fatalf("HealthSnapshot: %v", err)
	}
	if name != "bot" {
		t.Errorf("name = %q", name)
	}
	if !json.Valid(payload) {
		t.Errorf("payload not valid JSON: %s", payload)
	}
}

func TestBot_BuildBlocks(t *testing.T) {
	b := New(Config{})
	blocks := b.BuildBlocks("hello", nil)
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(blocks))
	}
	if blocks[0].Text != "hello" {
		t.Errorf("block text = %q", blocks[0].Text)
	}
	if blocks[0].Type != agent.ContentText {
		t.Errorf("block type = %q, want ContentText", blocks[0].Type)
	}
	if b.BuildBlocks("", nil) != nil {
		t.Error("empty text should return nil")
	}
}

func TestOutboundText(t *testing.T) {
	tests := []struct {
		name string
		msg  messages.OutboundMessage
		want string
	}{
		{"plain text", messages.OutboundMessage{Kind: messages.OutReply, Text: "hello"}, "hello"},
		{"empty text returns empty", messages.OutboundMessage{Kind: messages.OutReply}, ""},
		{"tool kind returns empty (v0 ignores)", messages.OutboundMessage{Kind: messages.OutToolStart, Text: "noise"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := outboundText(tt.msg); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCanonicalRepoURL(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"https://github.com/foo/bar.git", "foo/bar"},
		{"https://github.com/foo/bar", "foo/bar"},
		{"git@github.com:foo/bar.git", "foo/bar"},
		{"git@github.com:foo/bar", "foo/bar"},
		{"", ""},
		{"  https://github.com/foo/bar.git  ", "foo/bar"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := canonicalRepoURL(tt.in); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseKVOutput(t *testing.T) {
	tests := []struct {
		in   string
		want map[string]string
	}{
		{"FOO=bar\nBAZ=qux", map[string]string{"FOO": "bar", "BAZ": "qux"}},
		{"  KEY = val  \n", map[string]string{"KEY": "val"}},
		{"no-equals\n=starts-equals\nKEY=", nil},
		{"", nil},
		{"just text\nA=1", map[string]string{"A": "1"}},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := parseKVOutput(tt.in)
			if !mapsEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

func TestSanitize(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"~/work/nightme", "home_work_nightme"},
		{"/tmp/foo", "tmp_foo"},
		{"./relative", "relative"},
		{"a:b/c d", "a_b_c_d"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := sanitize(tt.in); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStateStore_SaveLoad(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStateStore(dir)
	if err != nil {
		t.Fatalf("NewStateStore: %v", err)
	}
	state := newRunState("test-run-1", nil, "/tmp",
		wfe.Event{}, "chat-1",
		map[string]string{"X": "1"}, time.Now())
	if err := store.Save(state); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := store.Load("test-run-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.ChatID != "chat-1" {
		t.Errorf("ChatID = %q", loaded.ChatID)
	}
	if loaded.Env["X"] != "1" {
		t.Errorf("Env[X] = %q", loaded.Env["X"])
	}
	// Verify atomic write: no .tmp files left behind
	if _, err := os.Stat(filepath.Join(dir, "test-run-1.json.tmp")); !os.IsNotExist(err) {
		t.Errorf("tmp file should not exist after Save")
	}
}

func TestActionRegistry_RegisterAndRun(t *testing.T) {
	r := NewActionRegistry()
	a := &testActionImpl{name: "hello", result: "world"}
	r.Register(a)
	r.Register(a) // re-register same name should be idempotent
	if len(r.List()) != 1 {
		t.Errorf("List = %v, want 1 entry", r.List())
	}
	res, err := r.Run(context.Background(), wfe.ActionSpec{Name: "hello"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Outputs["r"] != "world" {
		t.Errorf("Output[r] = %v", res.Outputs["r"])
	}
}

func TestActionRegistry_UnknownAction(t *testing.T) {
	r := NewActionRegistry()
	_, err := r.Run(context.Background(), wfe.ActionSpec{Name: "nope"})
	if err == nil {
		t.Error("expected error for unknown action")
	}
}

type testActionImpl struct {
	name   string
	result string
}

func (a *testActionImpl) Name() string { return a.name }
func (a *testActionImpl) Execute(_ context.Context, _ map[string]any, _ map[string]string) (*wfe.ActionResult, error) {
	return &wfe.ActionResult{Outputs: map[string]any{"r": a.result}}, nil
}

var _ Action = (*testActionImpl)(nil)
