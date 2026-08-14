package agent

import (
	"fmt"
	"strings"
)

// BlocksToPrompt joins multiple ContentBlock entries into the
// single string passed to a one-shot print-mode CLI flag (e.g.
// `claude -p`, `pi -p`, `opencode run`). ContentText blocks
// contribute their text directly; ContentImage / ContentFile
// blocks degrade to bracketed placeholders carrying the path
// (and MediaType when present) so the model can see WHERE each
// attachment sits in the user's message. Empty blocks and
// blocks with empty paths are skipped.
//
// All print-mode bridges that accept a single positional prompt
// use this same shape. Bridges that have a structured input
// mechanism for images (e.g. codex's `-i <path>` argv flag)
// compose this prompt on top of their argv and may add
// placeholder text that doesn't include the path; those bridges
// can opt out of using this helper.
//
// Format examples:
//
//	[{Text: "hello"}, {Type: Image, Path: "/tmp/x.png", MediaType: "image/png"}]
//	→ "hello\n[image: /tmp/x.png (image/png)]"
//
//	[{Type: File, Path: "/tmp/data.json"}]
//	→ "[file: /tmp/data.json]"
//
// Mirrors the original claudecode / pi / dsh blocksToPrompt
// helpers that lived in each bridge package before
// F-RUNONCEDRAIN-INTERNAL consolidation.
func BlocksToPrompt(blocks []ContentBlock) string {
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case ContentText:
			if b.Text == "" {
				continue
			}
			parts = append(parts, b.Text)
		case ContentImage:
			if b.Path == "" {
				continue
			}
			parts = append(parts, fmt.Sprintf("[image: %s (%s)]", b.Path, b.MediaType))
		case ContentFile:
			if b.Path == "" {
				continue
			}
			parts = append(parts, fmt.Sprintf("[file: %s]", b.Path))
		}
	}
	return strings.Join(parts, "\n")
}