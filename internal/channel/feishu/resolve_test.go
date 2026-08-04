package feishu

import (
	"errors"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/channel"
)

// Tests for resolveBlocks (F-14 v1.4b) — back-fills LocalPath into
// post rich-text image/file blocks whose Path is currently a
// Feishu file_key placeholder. Order is preserved end-to-end.

func TestResolve_AllSuccess_BackFillsPath(t *testing.T) {
	// Two images in source order. Both download successfully.
	preBlocks := []agent.ContentBlock{
		{Type: agent.ContentText, Text: "看"},
		{Type: agent.ContentImage, Path: "img_a"},
		{Type: agent.ContentText, Text: "和"},
		{Type: agent.ContentImage, Path: "img_b"},
	}
	ats := []channel.Attachment{
		{Type: "image", FileKey: "img_a", LocalPath: "/inbox/a.png"},
		{Type: "image", FileKey: "img_b", LocalPath: "/inbox/b.png"},
	}
	out := resolveBlocks(preBlocks, ats)
	if len(out) != 4 {
		t.Fatalf("len = %d, want 4", len(out))
	}
	if out[1].Type != agent.ContentImage || out[1].Path != "/inbox/a.png" {
		t.Errorf("out[1] = %+v, want ContentImage /inbox/a.png", out[1])
	}
	if out[3].Type != agent.ContentImage || out[3].Path != "/inbox/b.png" {
		t.Errorf("out[3] = %+v, want ContentImage /inbox/b.png", out[3])
	}
	// Text blocks should be passed through verbatim.
	if out[0].Text != "看" || out[2].Text != "和" {
		t.Errorf("text blocks mutated: %+v", out)
	}
}

func TestResolve_PartialFailure_DropsFailedImage(t *testing.T) {
	// First image downloads, second fails. The failed image
	// block is silently dropped; surrounding text is preserved.
	preBlocks := []agent.ContentBlock{
		{Type: agent.ContentText, Text: "first"},
		{Type: agent.ContentImage, Path: "img_ok"},
		{Type: agent.ContentText, Text: "between"},
		{Type: agent.ContentImage, Path: "img_fail"},
		{Type: agent.ContentText, Text: "last"},
	}
	ats := []channel.Attachment{
		{Type: "image", FileKey: "img_ok", LocalPath: "/inbox/ok.png"},
		{Type: "image", FileKey: "img_fail", LocalPath: "", Error: errors.New("network")},
	}
	out := resolveBlocks(preBlocks, ats)
	if len(out) != 4 {
		t.Fatalf("len = %d, want 4 (failed image dropped)", len(out))
	}
	want := []agent.ContentBlock{
		{Type: agent.ContentText, Text: "first"},
		{Type: agent.ContentImage, Path: "/inbox/ok.png"},
		{Type: agent.ContentText, Text: "between"},
		{Type: agent.ContentText, Text: "last"},
	}
	for i, w := range want {
		if out[i].Type != w.Type {
			t.Errorf("out[%d].Type = %q, want %q", i, out[i].Type, w.Type)
		}
		if out[i].Text != w.Text {
			t.Errorf("out[%d].Text = %q, want %q", i, out[i].Text, w.Text)
		}
		if out[i].Path != w.Path {
			t.Errorf("out[%d].Path = %q, want %q", i, out[i].Path, w.Path)
		}
	}
}

func TestResolve_AllFailed_EmptyBlocks(t *testing.T) {
	// All images failed. resolveBlocks returns only the text
	// context (no images). The handleMessage AllFailed branch
	// would have already aborted the message at the channel
	// layer; this defensive test confirms resolveBlocks does
	// not panic if called on a partial input.
	preBlocks := []agent.ContentBlock{
		{Type: agent.ContentImage, Path: "img_x"},
		{Type: agent.ContentText, Text: "still here"},
		{Type: agent.ContentImage, Path: "img_y"},
	}
	ats := []channel.Attachment{
		{Type: "image", FileKey: "img_x", LocalPath: "", Error: errors.New("net")},
		{Type: "image", FileKey: "img_y", LocalPath: "", Error: errors.New("net")},
	}
	out := resolveBlocks(preBlocks, ats)
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1 (only the text block)", len(out))
	}
	if out[0].Type != agent.ContentText || out[0].Text != "still here" {
		t.Errorf("out[0] = %+v, want the text block", out[0])
	}
}

func TestResolve_PreservesTextAndKnownFileBlocks(t *testing.T) {
	// A non-image file block (ContentFile) should also be
	// resolved by FileKey. Text blocks are passed through.
	preBlocks := []agent.ContentBlock{
		{Type: agent.ContentText, Text: "report:"},
		{Type: agent.ContentFile, Path: "file_rep"},
	}
	ats := []channel.Attachment{
		{Type: "file", FileKey: "file_rep", LocalPath: "/inbox/report.pdf"},
	}
	out := resolveBlocks(preBlocks, ats)
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	if out[1].Type != agent.ContentFile || out[1].Path != "/inbox/report.pdf" {
		t.Errorf("out[1] = %+v, want ContentFile /inbox/report.pdf", out[1])
	}
}

func TestResolve_EmptyInputs_NoOp(t *testing.T) {
	// Empty blocks → empty output; empty ats with non-empty
	// blocks → blocks with image nodes dropped.
	if out := resolveBlocks(nil, nil); len(out) != 0 {
		t.Errorf("nil/nil = %+v, want empty", out)
	}
	preBlocks := []agent.ContentBlock{
		{Type: agent.ContentText, Text: "hi"},
		{Type: agent.ContentImage, Path: "img_x"},
	}
	if out := resolveBlocks(preBlocks, nil); len(out) != 1 || out[0].Text != "hi" {
		t.Errorf("empty ats = %+v, want 1 text block", out)
	}
}