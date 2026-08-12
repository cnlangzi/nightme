package chatsession

import (
	"context"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/messages"
)

// TestManager_HandleInbound_ShortTextBlockFallback is a regression
// for the F-54 bug: Feishu delivers short text messages with
// msg.Blocks == nil. The legacy cmd/nightme dispatcher fell back to
// feishu.BuildBlocks(msg.Text, msg.Attachments); Manager.HandleInbound
// did not. Every short text message was queued with 0 blocks, the
// bridge's SendBlocks no-op'd, and the user saw "Working…" forever.
//
// The fix walks msg.Attachments in the same way as feishu.BuildBlocks
// does locally, so the chat session can pack a single ContentText
// block (plus any downloaded image/file attachments) without an
// import cycle on the channel package.
func TestManager_HandleInbound_ShortTextBlockFallback(t *testing.T) {
	mgr := NewManager()
	cs, _ := mgr.GetOrCreate("oc_short", "claude")
	cs.SetSelectedCwd("/tmp")

	// Force the WatchMode gate open so the test message passes.
	cs.SetWatchMode(WatchModeAll)

	mgr.HandleInbound(context.Background(), &messages.InboundMessage{
		ChatID:    "oc_short",
		MessageID: "om_short_1",
		UserID:    "u_short",
		Text:      "hi",
		// Blocks intentionally nil — Feishu does not pre-populate
		// them for short text messages.
	})

	waitQueueLen(t, cs, 1, 2*time.Second)
	peek := cs.queue.Peek()
	if len(peek) != 1 {
		t.Fatalf("queue peek = %d, want 1", len(peek))
	}
	if got := len(peek[0].Blocks); got != 1 {
		t.Fatalf("block count = %d, want 1", got)
	}
	if peek[0].Blocks[0].Type != agent.ContentText || peek[0].Blocks[0].Text != "hi" {
		t.Errorf("block = %+v, want one ContentText block with text=hi", peek[0].Blocks[0])
	}
}

// TestManager_HandleInbound_AttachmentBlockFallback covers the
// attachment-only case: msg.Text == "" but msg.Attachments has a
// downloaded image. The legacy dispatcher would have built a
// ContentImage block via feishu.BuildBlocks; the Manager fallback
// must do the same.
func TestManager_HandleInbound_AttachmentBlockFallback(t *testing.T) {
	mgr := NewManager()
	cs, _ := mgr.GetOrCreate("oc_attach", "claude")
	cs.SetSelectedCwd("/tmp")

	cs.SetWatchMode(WatchModeAll)

	mgr.HandleInbound(context.Background(), &messages.InboundMessage{
		ChatID:    "oc_attach",
		MessageID: "om_attach_1",
		UserID:    "u_attach",
		// Text empty, Blocks nil, Attachments has a downloaded image.
		Attachments: []messages.Attachment{
			{Type: "image", LocalPath: "/tmp/photo.png", MimeType: "image/png"},
		},
	})

	waitQueueLen(t, cs, 1, 2*time.Second)
	peek := cs.queue.Peek()
	if len(peek) != 1 {
		t.Fatalf("queue peek = %d, want 1", len(peek))
	}
	blocks := peek[0].Blocks
	if len(blocks) != 1 {
		t.Fatalf("block count = %d, want 1", len(blocks))
	}
	if blocks[0].Type != agent.ContentImage {
		t.Errorf("block type = %s, want image", blocks[0].Type)
	}
	if blocks[0].Path != "/tmp/photo.png" {
		t.Errorf("block path = %s, want /tmp/photo.png", blocks[0].Path)
	}
	if blocks[0].MediaType != "image/png" {
		t.Errorf("block media = %s, want image/png", blocks[0].MediaType)
	}
}

// TestManager_HandleInbound_DropsEmptyLocalPath pins the
// "attachment with empty LocalPath" branch: the helper must skip
// it (so the channel-side download-failure note is the only user
// signal), not synthesise a block with a missing file.
func TestManager_HandleInbound_DropsEmptyLocalPath(t *testing.T) {
	mgr := NewManager()
	cs, _ := mgr.GetOrCreate("oc_skip", "claude")
	cs.SetSelectedCwd("/tmp")

	cs.SetWatchMode(WatchModeAll)

	mgr.HandleInbound(context.Background(), &messages.InboundMessage{
		ChatID:    "oc_skip",
		MessageID: "om_skip_1",
		UserID:    "u_skip",
		// text + attachment with empty LocalPath (download failed).
		Text: "what is this?",
		Attachments: []messages.Attachment{
			{Type: "image", LocalPath: ""}, // download failed
		},
	})

	waitQueueLen(t, cs, 1, 2*time.Second)
	peek := cs.queue.Peek()
	if len(peek) != 1 {
		t.Fatalf("queue peek = %d, want 1", len(peek))
	}
	// expect exactly one text block; the failed attachment
	// must be silently dropped (the channel emits a
	// user-visible download-failure note out of band).
	blocks := peek[0].Blocks
	if len(blocks) != 1 {
		t.Fatalf("block count = %d, want 1 (text only)", len(blocks))
	}
	if blocks[0].Type != agent.ContentText {
		t.Errorf("block type = %s, want text", blocks[0].Type)
	}
}

// TestManager_HandleInbound_RespectsPrePopulatedBlocks is the
// positive control: when Feishu DOES pre-populate Blocks (e.g.
// rich-text post messages), the helper must leave them untouched
// rather than rebuild from Text/Attachments.
func TestManager_HandleInbound_RespectsPrePopulatedBlocks(t *testing.T) {
	mgr := NewManager()
	cs, _ := mgr.GetOrCreate("oc_pre", "claude")
	cs.SetSelectedCwd("/tmp")

	cs.SetWatchMode(WatchModeAll)

	pre := []agent.ContentBlock{
		{Type: agent.ContentText, Text: "caption"},
		{Type: agent.ContentImage, Path: "/tmp/x.png", MediaType: "image/png"},
	}
	mgr.HandleInbound(context.Background(), &messages.InboundMessage{
		ChatID:    "oc_pre",
		MessageID: "om_pre_1",
		UserID:    "u_pre",
		Text:      "ignored because Blocks pre-populated",
		Blocks:    pre,
	})

	waitQueueLen(t, cs, 1, 2*time.Second)
	peek := cs.queue.Peek()
	if len(peek) != 1 {
		t.Fatalf("queue peek = %d, want 1", len(peek))
	}
	// Must equal the pre-populated slice, not a freshly-built one.
	if len(peek[0].Blocks) != len(pre) {
		t.Fatalf("block count = %d, want %d (pre-populated)",
			len(peek[0].Blocks), len(pre))
	}
	for i, b := range peek[0].Blocks {
		if b.Type != pre[i].Type || b.Text != pre[i].Text || b.Path != pre[i].Path {
			t.Errorf("block[%d] = %+v, want %+v", i, b, pre[i])
		}
	}
}

func waitQueueLen(t *testing.T, cs *ChatSession, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cs.queue.Len() >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("queue.Len() = %d, want >= %d (within %s)",
		cs.queue.Len(), want, timeout)
}
