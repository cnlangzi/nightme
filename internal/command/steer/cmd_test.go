// Tests for the steer package's slash command factory.
//
// Mirrors the test layout of internal/command/close (factory
// spec, handler dispatch under various preflight conditions).
// End-to-end Stop + PushFront behavior is covered by the
// underlying ChatSession.SteerUserMessage tests.
package steer_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	steerpkg "github.com/cnlangzi/nightme/internal/command/steer"
)

func TestFactory_Spec(t *testing.T) {
	f := steerpkg.NewFactory()
	s := f.Spec()
	if s.Name != "steer" {
		t.Fatalf("Spec.Name = %q, want steer", s.Name)
	}
	if !strings.Contains(s.Usage, "<message>") {
		t.Fatalf("Spec.Usage = %q, want it to mention <message>", s.Usage)
	}
}

func TestFactory_Handle_NoSession_RepliesNoActive(t *testing.T) {
	f := steerpkg.NewFactory()

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, nil,
		command.SlashInput{ChatID: "no-such-chat"})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !out.Consumed {
		t.Fatalf("Consumed = false, want true")
	}
	if !strings.Contains(out.Reply, "No active chat session") {
		t.Fatalf("Reply missing no-active message: %q", out.Reply)
	}
}

// /steer on a freshly-created chat with no activeCwd set — the
// RequireActiveCwd preflight fires before any SteerUserMessage.
func TestFactory_Handle_NoActiveCwd_RepliesHint(t *testing.T) {
	mgr := chatsession.NewManager()
	f := steerpkg.NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "claude")

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs,
		command.SlashInput{ChatID: "c1", Args: []string{"steer"}})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !out.Consumed {
		t.Fatalf("Consumed = false, want true")
	}
	if !strings.Contains(out.Reply, "Send /cwd") {
		t.Fatalf("Reply missing cwd hint: %q", out.Reply)
	}
}

// /steer with no trailing body is a usage error — silently
// falling back to follow-up would surprise users who typed
// /steer by mistake.
func TestFactory_Handle_EmptyBody_RepliesUsage(t *testing.T) {
	mgr := chatsession.NewManager()
	f := steerpkg.NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "claude")
	_ = cs.SetSelectedCwd("/tmp")

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs,
		command.SlashInput{
			ChatID: "c1",
			Args:   []string{"steer"},
			Text:   "/steer   ", // trailing whitespace only
		})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.Reply, "Usage: /steer") {
		t.Fatalf("Reply missing usage: %q", out.Reply)
	}
}

// /steer <body> with an active chat: queue grows by 1 and the
// message body lands at the head (no other items in the queue).
func TestFactory_Handle_PrependsMessage(t *testing.T) {
	mgr := chatsession.NewManager()
	f := steerpkg.NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "claude")
	_ = cs.SetSelectedCwd("/tmp")

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs,
		command.SlashInput{
			ChatID:    "c1",
			MessageID: "m_steer",
			Text:      "/steer go this way instead",
			Args:      []string{"steer", "go", "this", "way", "instead"},
		})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !out.Consumed {
		t.Fatalf("Consumed = false, want true")
	}
	if !strings.Contains(out.Reply, "Steering") {
		t.Fatalf("Reply should mention steering: %q", out.Reply)
	}
	if got := cs.QueueLen(); got != 1 {
		t.Errorf("QueueLen after /steer: got %d, want 1", got)
	}
}

// /steer with multi-word body — Args may have been pre-parsed,
// but Text always has the full string. The handler uses Args[1:]
// joined (matching the commander flow) so multi-word bodies
// survive the slash dispatch.
func TestFactory_Handle_MultiWordBody(t *testing.T) {
	mgr := chatsession.NewManager()
	f := steerpkg.NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "claude")
	_ = cs.SetSelectedCwd("/tmp")

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs,
		command.SlashInput{
			ChatID:    "c1",
			MessageID: "m_steer_2",
			Text:      "/steer first second third",
			Args:      []string{"steer", "first", "second", "third"},
		})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := cs.QueueLen(); got != 1 {
		t.Errorf("QueueLen after multi-word /steer: got %d, want 1", got)
	}
	// Reply should preview the full body.
	if !strings.Contains(out.Reply, "first second third") {
		t.Errorf("Reply missing body preview: %q", out.Reply)
	}
}

// /steer triggered via the full-width slash "／" — Args is
// already populated by commander.extractCommand which normalizes
// both prefixes. The handler should still see the trailing args.
func TestFactory_Handle_FullWidthSlash_ArgsAlreadySet(t *testing.T) {
	mgr := chatsession.NewManager()
	f := steerpkg.NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "claude")
	_ = cs.SetSelectedCwd("/tmp")

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs,
		command.SlashInput{
			ChatID:    "c1",
			MessageID: "m_steer_fw",
			Text:      "／steer full width body",
			Args:      []string{"steer", "full", "width", "body"},
		})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := cs.QueueLen(); got != 1 {
		t.Errorf("QueueLen: got %d, want 1", got)
	}
	if !strings.Contains(out.Reply, "full width body") {
		t.Errorf("Reply missing body preview: %q", out.Reply)
	}
}

// /steer with a body longer than the preview cap — reply should
// preview only the first ~80 runes + ellipsis. Multi-byte CJK
// runes must NOT be split (byte-level truncation would corrupt
// the IM card payload).
func TestFactory_Handle_LongBody_RuneTruncation(t *testing.T) {
	mgr := chatsession.NewManager()
	f := steerpkg.NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "claude")
	_ = cs.SetSelectedCwd("/tmp")

	// 200 CJK runes (each 3 bytes in UTF-8 → 600 bytes, way over
	// the 80-rune cap). All single chars so no internal
	// whitespace.
	bodyRunes := make([]rune, 200)
	for i := range bodyRunes {
		bodyRunes[i] = '中' // 3 bytes each
	}
	body := string(bodyRunes)
	args := []string{"steer"}
	for _, r := range bodyRunes {
		args = append(args, string(r))
	}

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs,
		command.SlashInput{
			ChatID:    "c1",
			MessageID: "m_long",
			Text:      "/steer " + body,
			Args:      args,
		})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// The preview in the reply must end with "..." and must
	// not contain any U+FFFD (which would indicate a mid-rune
	// cut from byte-level truncation).
	if !strings.HasSuffix(out.Reply, "...") {
		t.Errorf("long body should be truncated with ellipsis: %q", out.Reply)
	}
	if strings.ContainsRune(out.Reply, '�') {
		t.Errorf("long body preview contains U+FFFD (mid-rune cut): %q", out.Reply)
	}
	// Counting runes in the preview (excluding the "🛑 Steering: "
	// prefix and the trailing "...") should be at most 80 runes
	// (matches the production previewRuneCap).
	preview := strings.TrimPrefix(out.Reply, "🛑 Steering: ")
	preview = strings.TrimSuffix(preview, "...")
	runeCount := 0
	for range preview {
		runeCount++
	}
	if runeCount > 80 {
		t.Errorf("preview rune count: got %d, want <= 80", runeCount)
	}
}

// /steer when the queue already has pending items — the queue
// length grows by 1. (The "prepend at head" semantics are
// covered by the MessageQueue.PushFront tests in
// internal/chatsession/message_queue_test.go; the steer factory
// just delegates to ChatSession.SteerUserMessage which calls
// PushFront.)
func TestFactory_Handle_QueueGrows(t *testing.T) {
	mgr := chatsession.NewManager()
	f := steerpkg.NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "claude")
	_ = cs.SetSelectedCwd("/tmp")

	// Queue an initial follow-up (no spawn needed — CS allows
	// QueueUserMessage without a live AS; FlushHook is a no-op
	// until the AS is ready).
	if err := cs.QueueUserMessage(chatsession.Message{
		ID:     "m_follow",
		ChatID: "c1",
		Blocks: nil,
		Kind:   chatsession.MessageKindNormal,
	}); err != nil {
		t.Fatalf("QueueUserMessage setup: %v", err)
	}
	if got := cs.QueueLen(); got != 1 {
		t.Fatalf("precondition QueueLen: got %d, want 1", got)
	}

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs,
		command.SlashInput{
			ChatID:    "c1",
			MessageID: "m_steer_ahead",
			Text:      "/steer jump to head",
			Args:      []string{"steer", "jump", "to", "head"},
		})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !out.Consumed {
		t.Fatalf("Consumed = false, want true")
	}
	if got := cs.QueueLen(); got != 2 {
		t.Errorf("QueueLen after /steer: got %d, want 2", got)
	}
}