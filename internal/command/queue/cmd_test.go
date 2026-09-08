// Tests for the queue package's slash command factory.
//
// Mirrors the test layout of internal/command/steer (factory
// spec, handler dispatch under various preflight conditions).
// End-to-end barrier / batch semantics are covered by the
// underlying MessageQueue.Peek tests in
// internal/chatsession/message_queue_test.go; this file
// focuses on the /queue factory's surface.
//
// The Kind=MessageKindQueue invariant — the entire point of
// the command — is asserted via the exported BuildMessage
// test seam (queuepkg.BuildMessage). This file does NOT reach
// into cs.queue's private linked list (the chatsession queue
// field is unexported, and a test in this package can't access
// it without an import cycle through internal/command).
package queue_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	queuepkg "github.com/cnlangzi/nightme/internal/command/queue"
	"github.com/cnlangzi/nightme/internal/messages"
)

// queueReply returns the Outbound[0] payload of out, asserting that
// exactly one OutReply is emitted and that the basic identity
// (Kind / ChatID / ReplyTo) is wired correctly. Centralising the
// boilerplate keeps each test focused on its specific assertion.
func queueReply(t *testing.T, out *command.SlashOutput, input command.SlashInput) string {
	t.Helper()
	if out == nil {
		t.Fatal("SlashOutput is nil")
	}
	if !out.Consumed {
		t.Error("Consumed = false, want true")
	}
	if len(out.Outbound) != 1 {
		t.Fatalf("Outbound len = %d, want 1 (main-work path)", len(out.Outbound))
	}
	ob := out.Outbound[0]
	if ob.Kind != messages.OutReply {
		t.Errorf("Outbound[0].Kind = %s, want %s", ob.Kind, messages.OutReply)
	}
	if ob.ChatID != input.ChatID {
		t.Errorf("Outbound[0].ChatID = %q, want %q", ob.ChatID, input.ChatID)
	}
	if ob.ReplyTo != input.MessageID {
		t.Errorf("Outbound[0].ReplyTo = %q, want %q", ob.ReplyTo, input.MessageID)
	}
	return ob.Text
}

func TestFactory_Spec(t *testing.T) {
	f := queuepkg.NewFactory()
	s := f.Spec()
	if s.Name != "queue" {
		t.Fatalf("Spec.Name = %q, want queue", s.Name)
	}
	if !strings.Contains(s.Usage, "<message>") {
		t.Fatalf("Spec.Usage = %q, want it to mention <message>", s.Usage)
	}
}

func TestFactory_Handle_NoSession_RepliesNoActive(t *testing.T) {
	f := queuepkg.NewFactory()

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, nil,
		command.SlashInput{ChatID: "no-such-chat"})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !out.Consumed {
		t.Fatalf("Consumed = false, want true")
	}
	// Preflight (no cs): stays on OutCommandReply — no placeholder
	// exists, no per-turn fold path to target.
	if !strings.Contains(out.Reply, "No active chat session") {
		t.Fatalf("Reply missing no-active message: %q", out.Reply)
	}
	if len(out.Outbound) != 0 {
		t.Errorf("preflight reply should not produce Outbound entries, got %d", len(out.Outbound))
	}
}

// /queue on a freshly-created chat with no activeCwd set — the
// RequireActiveCwd preflight fires before any QueueUserMessage.
func TestFactory_Handle_NoActiveCwd_RepliesHint(t *testing.T) {
	mgr := chatsession.NewManager()
	f := queuepkg.NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "claude")

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs,
		command.SlashInput{ChatID: "c1", Args: []string{"queue"}})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !out.Consumed {
		t.Fatalf("Consumed = false, want true")
	}
	// Preflight (no cwd): stays on OutCommandReply — failOut is
	// returned verbatim from RequireActiveCwd.
	if !strings.Contains(out.Reply, "Send /cwd") {
		t.Fatalf("Reply missing cwd hint: %q", out.Reply)
	}
	if len(out.Outbound) != 0 {
		t.Errorf("preflight reply should not produce Outbound entries, got %d", len(out.Outbound))
	}
}

// /queue with no trailing body is a usage error — silently
// falling back to follow-up would surprise users who typed
// /queue by mistake.
func TestFactory_Handle_EmptyBody_RepliesUsage(t *testing.T) {
	mgr := chatsession.NewManager()
	f := queuepkg.NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "claude")
	_ = cs.SetSelectedCwd("/tmp")

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs,
		command.SlashInput{
			ChatID:    "c1",
			MessageID: "m_empty_body", // bypass empty-MessageID guard
			Args:      []string{"queue"},
			Text:      "/queue   ", // trailing whitespace only
		})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.Reply, "Usage: /queue") {
		t.Fatalf("Reply missing usage: %q", out.Reply)
	}
	if len(out.Outbound) != 0 {
		t.Errorf("empty-body usage should stay on Reply, got %d Outbound", len(out.Outbound))
	}
}

// /queue <body> with an active chat: queue grows by 1. The
// Message.Kind is the entire point of the command — the factory
// must construct Kind=MessageKindQueue, which is the barrier
// that Peek uses to terminate a preceding Normal run.
//
// We don't peek into the queue here (the underlying Queue
// keeps Message by value with no external accessor); instead
// we use cs.QueueLen to confirm the message landed and rely
// on the underlying MessageQueue tests for the barrier
// semantics. Direct Kind assertion is exercised by the
// integration tests in internal/chatsession.
func TestFactory_Handle_AppendsMessage(t *testing.T) {
	mgr := chatsession.NewManager()
	f := queuepkg.NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "claude")
	_ = cs.SetSelectedCwd("/tmp")

	in := command.SlashInput{
		ChatID:    "c1",
		MessageID: "m_queue",
		Text:      "/queue standalone please",
		Args:      []string{"queue", "standalone", "please"},
	}
	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs, in)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := cs.QueueLen(); got != 1 {
		t.Errorf("QueueLen after /queue: got %d, want 1", got)
	}
	text := queueReply(t, out, in)
	if !strings.Contains(text, "Queued") {
		t.Errorf("Outbound[0].Text should mention queued: %q", text)
	}
}

// /queue with multi-word body — Args may have been pre-parsed,
// but Text always has the full string. The handler uses Args[1:]
// joined (matching the commander flow) so multi-word bodies
// survive the slash dispatch.
func TestFactory_Handle_MultiWordBody(t *testing.T) {
	mgr := chatsession.NewManager()
	f := queuepkg.NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "claude")
	_ = cs.SetSelectedCwd("/tmp")

	in := command.SlashInput{
		ChatID:    "c1",
		MessageID: "m_queue_2",
		Text:      "/queue first second third",
		Args:      []string{"queue", "first", "second", "third"},
	}
	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs, in)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := cs.QueueLen(); got != 1 {
		t.Errorf("QueueLen after multi-word /queue: got %d, want 1", got)
	}
	// Reply should preview the full body.
	text := queueReply(t, out, in)
	if !strings.Contains(text, "first second third") {
		t.Errorf("Outbound[0].Text missing body preview: %q", text)
	}
}

// /queue triggered via the full-width slash "／" — Args is
// already populated by commander.extractCommand which normalizes
// both prefixes. The handler should still see the trailing args.
func TestFactory_Handle_FullWidthSlash_ArgsAlreadySet(t *testing.T) {
	mgr := chatsession.NewManager()
	f := queuepkg.NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "claude")
	_ = cs.SetSelectedCwd("/tmp")

	in := command.SlashInput{
		ChatID:    "c1",
		MessageID: "m_queue_fw",
		Text:      "／queue full width body",
		Args:      []string{"queue", "full", "width", "body"},
	}
	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs, in)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := cs.QueueLen(); got != 1 {
		t.Errorf("QueueLen: got %d, want 1", got)
	}
	text := queueReply(t, out, in)
	if !strings.Contains(text, "full width body") {
		t.Errorf("Outbound[0].Text missing body preview: %q", text)
	}
}

// /queue with a body longer than the preview cap — reply should
// preview only the first ~80 runes + ellipsis. Multi-byte CJK
// runes must NOT be split (byte-level truncation would corrupt
// the IM card payload).
func TestFactory_Handle_LongBody_RuneTruncation(t *testing.T) {
	mgr := chatsession.NewManager()
	f := queuepkg.NewFactory()
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
	args := []string{"queue"}
	for _, r := range bodyRunes {
		args = append(args, string(r))
	}

	in := command.SlashInput{
		ChatID:    "c1",
		MessageID: "m_long",
		Text:      "/queue " + body,
		Args:      args,
	}
	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs, in)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	text := queueReply(t, out, in)

	// The preview must end with "..." and must not contain any
	// U+FFFD (which would indicate a mid-rune cut from byte-level
	// truncation).
	if !strings.HasSuffix(text, "...") {
		t.Errorf("long body should be truncated with ellipsis: %q", text)
	}
	if strings.ContainsRune(text, '�') {
		t.Errorf("long body preview contains U+FFFD (mid-rune cut): %q", text)
	}
	// Counting runes in the preview (excluding the
	// "📥 Queued: " prefix and the trailing "...")
	// should be at most 80 runes (matches the production
	// previewRuneCap).
	preview := strings.TrimPrefix(text, "📥 Queued: ")
	preview = strings.TrimSuffix(preview, "...")
	runeCount := 0
	for range preview {
		runeCount++
	}
	if runeCount > 80 {
		t.Errorf("preview rune count: got %d, want <= 80", runeCount)
	}
}

// BuildMessage is the factory's only Message-construction site;
// asserting on its output directly is the strongest possible check
// that /queue does in fact set Kind=MessageKindQueue (the whole
// point of the command). Tests in this file otherwise only verify
// QueueLen, which is a weaker invariant — a Normal-kind message
// pushed via /queue would also pass those.
//
// We don't reach into cs.queue (the linked list is private to the
// chatsession package); BuildMessage is the test seam.
func TestBuildMessage_SetsBarrierKind(t *testing.T) {
	msg := queuepkg.BuildMessage(
		command.SlashInput{
			ChatID:    "c1",
			MessageID: "m_q_build",
		},
		"hello barrier world",
	)

	if msg.ID != "m_q_build" {
		t.Errorf("msg.ID = %q, want %q", msg.ID, "m_q_build")
	}
	if msg.ChatID != "c1" {
		t.Errorf("msg.ChatID = %q, want %q", msg.ChatID, "c1")
	}
	if msg.Kind != chatsession.MessageKindQueue {
		t.Errorf("msg.Kind = %s, want %s", msg.Kind, chatsession.MessageKindQueue)
	}
	if msg.ReceivedAt.IsZero() {
		t.Errorf("msg.ReceivedAt is zero, want non-zero (set at construction)")
	}
	if len(msg.Blocks) != 1 {
		t.Fatalf("msg.Blocks = %d blocks, want 1", len(msg.Blocks))
	}
	if msg.Blocks[0].Text != "hello barrier world" {
		t.Errorf("msg.Blocks[0].Text = %q, want %q",
			msg.Blocks[0].Text, "hello barrier world")
	}
}

// BuildMessage should set Kind=Queue regardless of trailing
// whitespace / casing / etc — it just constructs a Message
// with the body's exact text and the barrier kind.
func TestBuildMessage_KindAlwaysQueue(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"plain", "hello"},
		{"multiline", "line1\nline2"},
		{"cjk", "中文消息 body"},
		{"emoji", "🚀 launch"},
		{"with code", "fmt.Println(\"hi\")"},
		{"very long", strings.Repeat("a", 1000)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := queuepkg.BuildMessage(
				command.SlashInput{ChatID: "c", MessageID: "m"}, tc.body)
			if msg.Kind != chatsession.MessageKindQueue {
				t.Errorf("body=%q: msg.Kind = %s, want %s",
					tc.body, msg.Kind, chatsession.MessageKindQueue)
			}
			if msg.Blocks[0].Text != tc.body {
				t.Errorf("body=%q: msg.Blocks[0].Text = %q, want %q",
					tc.body, msg.Blocks[0].Text, tc.body)
			}
		})
	}
}

// /queue when the queue already has pending items — the queue
// length grows by 1 (the existing items are not consumed; /queue
// is non-interrupting and appends to tail).
func TestFactory_Handle_QueueGrows(t *testing.T) {
	mgr := chatsession.NewManager()
	f := queuepkg.NewFactory()
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

	in := command.SlashInput{
		ChatID:    "c1",
		MessageID: "m_queue_ahead",
		Text:      "/queue standalone at tail",
		Args:      []string{"queue", "standalone", "at", "tail"},
	}
	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs, in)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !out.Consumed {
		t.Fatalf("Consumed = false, want true")
	}
	if got := cs.QueueLen(); got != 2 {
		t.Errorf("QueueLen after /queue: got %d, want 2", got)
	}
	// Main-work success path emits exactly one OutReply; no Reply.
	if len(out.Outbound) != 1 {
		t.Fatalf("Outbound len = %d, want 1", len(out.Outbound))
	}
	if out.Reply != "" {
		t.Errorf("Reply should be empty on OutReply path, got %q", out.Reply)
	}
}

// /queue with empty MessageID — ChatSession.QueueUserMessage
// silently no-ops on msg.ID == ""; the factory must surface
// this as a non-success reply (otherwise the user gets a
// "Queued: …" reply but no actual enqueue happened).
func TestFactory_Handle_EmptyMessageID_RepliesDiagnostic(t *testing.T) {
	mgr := chatsession.NewManager()
	f := queuepkg.NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "claude")
	_ = cs.SetSelectedCwd("/tmp")

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs,
		command.SlashInput{
			ChatID:    "c1",
			MessageID: "", // synthetic inbound with no channel message id
			Text:      "/queue hello",
			Args:      []string{"queue", "hello"},
		})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !out.Consumed {
		t.Fatalf("Consumed = false, want true")
	}
	// Preflight guard: stays on OutCommandReply, no Outbound.
	if !strings.Contains(out.Reply, "missing message id") {
		t.Errorf("Reply should mention missing message id: %q", out.Reply)
	}
	if len(out.Outbound) != 0 {
		t.Errorf("preflight guard should stay on Reply, got %d Outbound", len(out.Outbound))
	}
	if got := cs.QueueLen(); got != 0 {
		t.Errorf("QueueLen after empty-MessageID /queue: got %d, want 0", got)
	}
}
