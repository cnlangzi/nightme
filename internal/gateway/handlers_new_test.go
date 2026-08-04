// F-34 tests for the `/new [<agent>]` slash command handler.
package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
)

// TestHandleNew_NoCwd verifies that /new before /cwd replies with
// the workspace guidance hint, never touching the pool.
func TestHandleNew_NoCwd(t *testing.T) {
	mgr := chatsession.NewManager()
	channel := &fakeChannel{}
	msg := &InboundMessage{ChatID: "chat-1", Text: "/new"}

	_, err := handleNew(context.Background(), mgr, channel, msg, nil, "cc")
	if err != nil {
		t.Fatalf("handleNew: %v", err)
	}
	got := channel.LastText()
	if !strings.Contains(got, "No active workspace") {
		t.Fatalf("want 'No active workspace' reply, got %q", got)
	}
}

// TestHandleNew_NoAgentSessions verifies that /new with an empty pool
// replies with the no-session hint.
func TestHandleNew_NoAgentSessions(t *testing.T) {
	mgr := chatsession.NewManager()
	channel := &fakeChannel{}

	cs := mgr.GetOrCreate("chat-2", "cc")
	if err := cs.SetActiveCwd(t.TempDir()); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}

	msg := &InboundMessage{ChatID: "chat-2", Text: "/new"}
	_, err := handleNew(context.Background(), mgr, channel, msg, nil, "cc")
	if err != nil {
		t.Fatalf("handleNew: %v", err)
	}
	got := channel.LastText()
	if !strings.Contains(got, "No agent session in current workspace") {
		t.Fatalf("want 'No agent session' reply, got %q", got)
	}
}

// TestHandleNew_NoAgentForName verifies that /new <agent> with no
// matching AS replies with the not-found hint (not "no agent session").
func TestHandleNew_NoAgentForName(t *testing.T) {
	mgr := chatsession.NewManager()
	channel := &fakeChannel{}

	cs := mgr.GetOrCreate("chat-3", "cc")
	if err := cs.SetActiveCwd(t.TempDir()); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}

	msg := &InboundMessage{ChatID: "chat-3", Text: "/new codex"}
	_, err := handleNew(context.Background(), mgr, channel, msg, []string{"codex"}, "cc")
	if err != nil {
		t.Fatalf("handleNew: %v", err)
	}
	got := channel.LastText()
	if !strings.Contains(got, `No agent session for "codex"`) {
		t.Fatalf("want 'No agent session for \"codex\"' reply, got %q", got)
	}
}

// TestHandleNew_EmptyArg verifies that /new with a whitespace-only
// arg returns the usage hint.
func TestHandleNew_EmptyArg(t *testing.T) {
	mgr := chatsession.NewManager()
	channel := &fakeChannel{}

	cs := mgr.GetOrCreate("chat-4", "cc")
	if err := cs.SetActiveCwd(t.TempDir()); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}

	msg := &InboundMessage{ChatID: "chat-4", Text: "/new  "}
	_, err := handleNew(context.Background(), mgr, channel, msg, []string{"   "}, "cc")
	if err != nil {
		t.Fatalf("handleNew: %v", err)
	}
	got := channel.LastText()
	if !strings.Contains(got, "Usage: /new") {
		t.Fatalf("want 'Usage: /new' reply, got %q", got)
	}
}