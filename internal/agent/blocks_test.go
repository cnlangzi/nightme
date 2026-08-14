package agent

import "testing"

// TestBlocksToPrompt locks in the format the print-mode bridges
// pass to one-shot CLIs. If the placeholder shape changes
// (e.g. brackets → parens, or `image: path` → just `path`) the
// change is intentional and this test must move with it.
func TestBlocksToPrompt(t *testing.T) {
	cases := []struct {
		name string
		in   []ContentBlock
		want string
	}{
		{"no blocks", nil, ""},
		{"text only", []ContentBlock{{Type: ContentText, Text: "hello"}}, "hello"},
		{"multiple text join with newline", []ContentBlock{
			{Type: ContentText, Text: "line1"},
			{Type: ContentText, Text: "line2"},
		}, "line1\nline2"},
		{"empty text skipped", []ContentBlock{
			{Type: ContentText, Text: ""},
			{Type: ContentText, Text: "kept"},
		}, "kept"},
		{"image with media type", []ContentBlock{
			{Type: ContentText, Text: "look"},
			{Type: ContentImage, Path: "/tmp/img.png", MediaType: "image/png"},
			{Type: ContentFile, Path: "/tmp/data.json"},
		}, "look\n[image: /tmp/img.png (image/png)]\n[file: /tmp/data.json]"},
		{"image without media type still rendered", []ContentBlock{
			{Type: ContentImage, Path: "/tmp/img.png"},
		}, "[image: /tmp/img.png ()]"},
		{"image with empty path skipped", []ContentBlock{
			{Type: ContentImage, Path: ""},
			{Type: ContentText, Text: "kept"},
		}, "kept"},
		{"file with empty path skipped", []ContentBlock{
			{Type: ContentFile, Path: ""},
			{Type: ContentText, Text: "kept"},
		}, "kept"},
		{"unknown block type silently dropped", []ContentBlock{
			{Type: ContentBlockType("future"), Text: "x"},
			{Type: ContentText, Text: "kept"},
		}, "kept"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := BlocksToPrompt(c.in); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}