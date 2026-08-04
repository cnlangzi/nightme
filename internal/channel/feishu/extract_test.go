package feishu

import (
	"errors"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/channel"
)

// Tests for extractAttachments — one subtest per Feishu msg_type.
// Each test asserts the textual component (Text), the attachment
// list (Attachments), and (for post rich-text) the ordered block
// slice (Blocks) are correctly populated.
//
// v1.4b: post rich-text path now produces an ordered []ContentBlock
// whose order matches the source Feishu paragraph. The text
// collapsing into a flat string is replaced by per-node text blocks
// in source order; image blocks carry FileKey as a Path placeholder
// until resolveBlocks back-fills LocalPath after download.

func TestExtract_Text(t *testing.T) {
	got, atts, blocks := extractAttachments("text", `{"text":"hello"}`)
	if got != "hello" {
		t.Errorf("Text = %q, want %q", got, "hello")
	}
	if len(atts) != 0 {
		t.Errorf("Attachments = %d, want 0", len(atts))
	}
	if len(blocks) != 0 {
		t.Errorf("Blocks = %d, want 0 (text-only msg_type has no rich-text path)", len(blocks))
	}
}

func TestExtract_TextEmptyContent(t *testing.T) {
	// Legacy messageText() falls back to the raw content when the
	// "text" field is empty — this preserves the v0.1 behaviour
	// for already-deployed callers. The raw envelope is useless
	// to the agent but harmless; the dispatcher will filter it
	// downstream.
	got, atts, _ := extractAttachments("text", `{"text":""}`)
	if got != `{"text":""}` {
		t.Errorf("Text = %q, want raw passthrough (legacy)", got)
	}
	if len(atts) != 0 {
		t.Errorf("Attachments = %d, want 0", len(atts))
	}
}

func TestExtract_TextMissingField(t *testing.T) {
	// Feishu occasionally omits the "text" field; we preserve
	// the raw payload for forward compatibility (matches the
	// legacy messageText fallback).
	got, atts, _ := extractAttachments("text", `{"other":"foo"}`)
	if got != `{"other":"foo"}` {
		t.Errorf("Text = %q, want raw JSON passthrough", got)
	}
	if len(atts) != 0 {
		t.Errorf("Attachments = %d, want 0", len(atts))
	}
}

func TestExtract_Image(t *testing.T) {
	got, atts, blocks := extractAttachments("image", `{"image_key":"img_v2_abc"}`)
	if got != "" {
		t.Errorf("Text = %q, want empty", got)
	}
	if len(atts) != 1 {
		t.Fatalf("Attachments = %d, want 1", len(atts))
	}
	a := atts[0]
	if a.Type != "image" {
		t.Errorf("Type = %q, want %q", a.Type, "image")
	}
	if a.FileKey != "img_v2_abc" {
		t.Errorf("FileKey = %q, want %q", a.FileKey, "img_v2_abc")
	}
	if a.FileName != "" {
		t.Errorf("FileName = %q, want empty (synthesized at download)", a.FileName)
	}
	if len(blocks) != 0 {
		t.Errorf("Blocks = %d, want 0 (single-resource image uses legacy path)", len(blocks))
	}
}

func TestExtract_ImageMissingKey(t *testing.T) {
	got, atts, _ := extractAttachments("image", `{}`)
	if got != "" {
		t.Errorf("Text = %q, want empty", got)
	}
	if len(atts) != 0 {
		t.Errorf("Attachments = %d, want 0 (image_key is required)", len(atts))
	}
}

func TestExtract_File(t *testing.T) {
	got, atts, blocks := extractAttachments("file", `{"file_key":"file_xyz","file_name":"report.pdf"}`)
	if got != "" {
		t.Errorf("Text = %q, want empty", got)
	}
	if len(atts) != 1 {
		t.Fatalf("Attachments = %d, want 1", len(atts))
	}
	a := atts[0]
	if a.Type != "file" {
		t.Errorf("Type = %q, want %q", a.Type, "file")
	}
	if a.FileKey != "file_xyz" {
		t.Errorf("FileKey = %q, want %q", a.FileKey, "file_xyz")
	}
	if a.FileName != "report.pdf" {
		t.Errorf("FileName = %q, want %q", a.FileName, "report.pdf")
	}
	if len(blocks) != 0 {
		t.Errorf("Blocks = %d, want 0 (single-resource file uses legacy path)", len(blocks))
	}
}

func TestExtract_FileNoName(t *testing.T) {
	// file_name is optional for the file msg_type; we synthesize
	// a stable fallback at download time, not at extract time.
	_, atts, _ := extractAttachments("file", `{"file_key":"file_xyz"}`)
	if len(atts) != 1 {
		t.Fatalf("Attachments = %d, want 1", len(atts))
	}
	if atts[0].FileName != "" {
		t.Errorf("FileName = %q, want empty (synthesized at download)", atts[0].FileName)
	}
}

func TestExtract_Audio(t *testing.T) {
	got, atts, blocks := extractAttachments("audio", `{"file_key":"file_aud","file_name":"voice.m4a"}`)
	if got != "" {
		t.Errorf("Text = %q, want empty", got)
	}
	if len(atts) != 1 || atts[0].Type != "audio" {
		t.Fatalf("Attachments = %+v, want 1 audio", atts)
	}
	if atts[0].FileName != "voice.m4a" {
		t.Errorf("FileName = %q, want %q", atts[0].FileName, "voice.m4a")
	}
	if len(blocks) != 0 {
		t.Errorf("Blocks = %d, want 0", len(blocks))
	}
}

func TestExtract_AudioMissingFileName(t *testing.T) {
	// Synthesize a fallback filename so audio with no file_name
	// still has a stable on-disk name. The fallback uses the
	// file_key + ".m4a" extension at extract time so downstream
	// code does not need to special-case empty names.
	_, atts, _ := extractAttachments("audio", `{"file_key":"file_aud"}`)
	if len(atts) != 1 {
		t.Fatalf("Attachments = %d, want 1", len(atts))
	}
	if atts[0].FileName != "file_aud.m4a" {
		t.Errorf("FileName = %q, want %q", atts[0].FileName, "file_aud.m4a")
	}
}

func TestExtract_Media(t *testing.T) {
	// The cover (image_key) is intentionally NOT forwarded — the
	// user sent a video, not a cover image. Forwarding both would
	// misrepresent the message to the agent.
	got, atts, _ := extractAttachments("media", `{"file_key":"file_vid","image_key":"img_thumb","file_name":"clip.mp4"}`)
	if got != "" {
		t.Errorf("Text = %q, want empty", got)
	}
	if len(atts) != 1 {
		t.Fatalf("Attachments = %d, want 1 (video only — cover image is dropped)", len(atts))
	}
	if atts[0].Type != "media" || atts[0].FileKey != "file_vid" {
		t.Errorf("atts[0] = %+v, want media file_vid", atts[0])
	}
}

func TestExtract_MediaOnlyVideo(t *testing.T) {
	// Some media messages have file_key but no cover image_key.
	_, atts, _ := extractAttachments("media", `{"file_key":"file_vid","file_name":"clip.mp4"}`)
	if len(atts) != 1 || atts[0].Type != "media" {
		t.Fatalf("Attachments = %+v, want 1 media", atts)
	}
}

func TestExtract_Post_TextOnly(t *testing.T) {
	// Post with single text node. v1.4b: Text is "" (Blocks is
	// source of truth for post path), Blocks has 1 ContentText.
	got, atts, blocks := extractAttachments("post", `{"content":[[{"tag":"text","text":"hello"}]]}`)
	if got != "" {
		t.Errorf("Text = %q, want empty (post path uses Blocks)", got)
	}
	if len(atts) != 0 {
		t.Errorf("Attachments = %d, want 0", len(atts))
	}
	if len(blocks) != 1 || blocks[0].Type != agent.ContentText || blocks[0].Text != "hello" {
		t.Errorf("Blocks = %+v, want 1 ContentText(\"hello\")", blocks)
	}
}

func TestExtract_Post_TextAndImage_Ordered(t *testing.T) {
	// v1.4b: text-then-image in a single paragraph → ordered blocks
	// [text, image-placeholder]. Image block carries FileKey in
	// Path; resolveBlocks will back-fill LocalPath after download.
	content := `{"content":[[{"tag":"text","text":"看这张图"},{"tag":"img","image_key":"img_xxx"}]]}`
	got, atts, blocks := extractAttachments("post", content)
	if got != "" {
		t.Errorf("Text = %q, want empty (post path uses Blocks)", got)
	}
	if len(atts) != 1 {
		t.Fatalf("Attachments = %d, want 1", len(atts))
	}
	if atts[0].Type != "image" || atts[0].FileKey != "img_xxx" {
		t.Errorf("atts[0] = %+v, want image img_xxx", atts[0])
	}
	// v1.4b ordered assertion:
	if len(blocks) != 2 {
		t.Fatalf("Blocks = %d, want 2 (text then image)", len(blocks))
	}
	if blocks[0].Type != agent.ContentText || blocks[0].Text != "看这张图" {
		t.Errorf("blocks[0] = %+v, want ContentText(\"看这张图\")", blocks[0])
	}
	if blocks[1].Type != agent.ContentImage || blocks[1].Path != "img_xxx" {
		t.Errorf("blocks[1] = %+v, want ContentImage with Path=img_xxx (placeholder)", blocks[1])
	}
}

func TestExtract_Post_MultipleParagraphs_Ordered(t *testing.T) {
	// v1.4b: multiple paragraphs separated by ContentText("\n")
	// in the block stream. Order of nodes is preserved verbatim.
	content := `{"content":[
		[{"tag":"text","text":"line one"}],
		[{"tag":"text","text":"line two"},{"tag":"img","image_key":"img_a"}],
		[{"tag":"img","image_key":"img_b"}]
	]}`
	got, atts, blocks := extractAttachments("post", content)
	if got != "" {
		t.Errorf("Text = %q, want empty (post path)", got)
	}
	if len(atts) != 2 {
		t.Fatalf("Attachments = %d, want 2", len(atts))
	}
	if atts[0].FileKey != "img_a" || atts[1].FileKey != "img_b" {
		t.Errorf("attachment order / keys wrong: %+v", atts)
	}
	// Expected blocks: [text("line one"), text("\n"), text("line two"),
	//                   image(img_a), text("\n"), image(img_b)]
	wantBlocks := []agent.ContentBlock{
		{Type: agent.ContentText, Text: "line one"},
		{Type: agent.ContentText, Text: "\n"},
		{Type: agent.ContentText, Text: "line two"},
		{Type: agent.ContentImage, Path: "img_a"},
		{Type: agent.ContentText, Text: "\n"},
		{Type: agent.ContentImage, Path: "img_b"},
	}
	if len(blocks) != len(wantBlocks) {
		t.Fatalf("Blocks = %d, want %d (%+v)", len(blocks), len(wantBlocks), blocks)
	}
	for i, want := range wantBlocks {
		got := blocks[i]
		if got.Type != want.Type {
			t.Errorf("blocks[%d].Type = %q, want %q", i, got.Type, want.Type)
		}
		if got.Text != want.Text {
			t.Errorf("blocks[%d].Text = %q, want %q", i, got.Text, want.Text)
		}
		if got.Path != want.Path {
			t.Errorf("blocks[%d].Path = %q, want %q", i, got.Path, want.Path)
		}
	}
}

func TestExtract_Post_IgnoresOtherTags(t *testing.T) {
	// tag:"media" (inline video, v1.4 out of scope), "a" (link — its
	// visible text becomes a ContentText), "at" (mention),
	// "emotion" (reaction) — these all skip or fold into text.
	content := `{"content":[[
		{"tag":"text","text":"see "},
		{"tag":"a","href":"https://x","text":"link"},
		{"tag":"at","user_id":"ou_1","user_name":"alice"},
		{"tag":"emotion","emoji_type":"SMILE"},
		{"tag":"img","image_key":"img_zzz"}
	]]}`
	_, atts, blocks := extractAttachments("post", content)
	if len(atts) != 1 || atts[0].FileKey != "img_zzz" {
		t.Errorf("expected only the img attachment, got %+v", atts)
	}
	// Expected blocks: [text("see "), text("link"), image(img_zzz)] —
	// the empty "see " text is preserved; tag:"a" visible text
	// becomes its own ContentText so the agent sees both.
	if len(blocks) != 3 {
		t.Fatalf("Blocks = %d, want 3 (%+v)", len(blocks), blocks)
	}
	if blocks[0].Type != agent.ContentText || blocks[0].Text != "see " {
		t.Errorf("blocks[0] = %+v, want ContentText(\"see \")", blocks[0])
	}
	if blocks[1].Type != agent.ContentText || blocks[1].Text != "link" {
		t.Errorf("blocks[1] = %+v, want ContentText(\"link\")", blocks[1])
	}
	if blocks[2].Type != agent.ContentImage || blocks[2].Path != "img_zzz" {
		t.Errorf("blocks[2] = %+v, want ContentImage img_zzz", blocks[2])
	}
}

func TestExtract_Post_MalformedJSON(t *testing.T) {
	// Post with malformed content falls back to raw payload as Text,
	// no attachments, no blocks — matches the legacy messageText
	// fallback for non-text msg_types that fail to parse.
	got, atts, blocks := extractAttachments("post", `not-json`)
	if got != "not-json" {
		t.Errorf("Text = %q, want raw passthrough", got)
	}
	if len(atts) != 0 {
		t.Errorf("Attachments = %d, want 0", len(atts))
	}
	if len(blocks) != 0 {
		t.Errorf("Blocks = %d, want 0", len(blocks))
	}
}

func TestExtract_Sticker_SilentSkip(t *testing.T) {
	got, atts, _ := extractAttachments("sticker", `{"file_key":"file_sticker","sticker_id":"sticker_1"}`)
	if got != "" {
		t.Errorf("Text = %q, want empty", got)
	}
	if len(atts) != 0 {
		t.Errorf("Sticker must not produce attachments (Feishu blocks download), got %+v", atts)
	}
}

func TestExtract_UnknownPassthrough(t *testing.T) {
	// Unknown msg_types (interactive, share_chat, share_user, …)
	// preserve the raw JSON in Text — no attachments.
	content := `{"header":{"title":"card"},"elements":[]}`
	got, atts, blocks := extractAttachments("interactive", content)
	if got != content {
		t.Errorf("Text = %q, want raw payload passthrough", got)
	}
	if len(atts) != 0 {
		t.Errorf("Attachments = %d, want 0", len(atts))
	}
	if len(blocks) != 0 {
		t.Errorf("Blocks = %d, want 0 (unknown msg_type has no rich-text path)", len(blocks))
	}
}

func TestExtract_EmptyMsgType(t *testing.T) {
	// Legacy callers that don't populate msg_type fall through
	// to messageText for backwards compatibility.
	got, atts, _ := extractAttachments("", `{"text":"hi"}`)
	if got != "hi" {
		t.Errorf("Text = %q, want %q", got, "hi")
	}
	if len(atts) != 0 {
		t.Errorf("Attachments = %d, want 0", len(atts))
	}
}

func TestExtract_MalformedJSON(t *testing.T) {
	// Malformed image content: json.Unmarshal fails, so we have
	// no usable data. Return empty text + no attachments. The
	// "fall through to raw" behaviour only applies to text and
	// unknown msg_types — for image/file/etc we either have a
	// valid key or nothing at all.
	got, atts, _ := extractAttachments("image", `not-json`)
	if got != "" {
		t.Errorf("Text = %q, want empty", got)
	}
	if len(atts) != 0 {
		t.Errorf("Attachments = %d, want 0", len(atts))
	}
}

// --- BuildForwardedText ---

func TestBuildForwardedText_TextOnly(t *testing.T) {
	got := BuildForwardedText("hello", nil)
	if got != "hello" {
		t.Errorf("BuildForwardedText = %q, want %q", got, "hello")
	}
}

func TestBuildForwardedText_EmptyTextWithAttachment(t *testing.T) {
	got := BuildForwardedText("", []channel.Attachment{
		{Type: "image", FileKey: "img_x", LocalPath: "/tmp/img_x.png"},
	})
	want := "attachment (image): /tmp/img_x.png"
	if got != want {
		t.Errorf("BuildForwardedText = %q, want %q", got, want)
	}
}

func TestBuildForwardedText_TextAndAttachments(t *testing.T) {
	got := BuildForwardedText("看这张图", []channel.Attachment{
		{Type: "image", LocalPath: "/tmp/a.png"},
		{Type: "file", LocalPath: "/tmp/b.pdf"},
	})
	want := "看这张图\nattachment (image): /tmp/a.png\nattachment (file): /tmp/b.pdf"
	if got != want {
		t.Errorf("BuildForwardedText = %q, want %q", got, want)
	}
}

func TestBuildForwardedText_SkipsFailedDownloads(t *testing.T) {
	// Attachments with LocalPath == "" failed to download — must
	// be silently omitted (the dispatcher sends a separate
	// notification for them via the Reply path).
	got := BuildForwardedText("caption", []channel.Attachment{
		{Type: "image", LocalPath: "/tmp/ok.png"},
		{Type: "image", FileKey: "img_fail", LocalPath: "", Error: errors.New("network")},
	})
	want := "caption\nattachment (image): /tmp/ok.png"
	if got != want {
		t.Errorf("BuildForwardedText = %q, want %q", got, want)
	}
}

func TestBuildForwardedText_AllFailedProducesCaptionOnly(t *testing.T) {
	// Defensive: when no attachments succeeded, we still return
	// the caption. The dispatcher's AllFailed branch normally
	// drops the message before reaching here, but if it doesn't,
	// the agent should at least see the caption rather than an
	// empty string.
	got := BuildForwardedText("just caption", []channel.Attachment{
		{Type: "image", LocalPath: "", Error: errors.New("fail")},
	})
	if got != "just caption" {
		t.Errorf("BuildForwardedText = %q, want %q", got, "just caption")
	}
}

func TestBuildForwardedText_EmptyTextAllFailed(t *testing.T) {
	got := BuildForwardedText("", []channel.Attachment{
		{Type: "image", LocalPath: "", Error: errors.New("fail")},
	})
	if got != "" {
		t.Errorf("BuildForwardedText = %q, want empty", got)
	}
}
