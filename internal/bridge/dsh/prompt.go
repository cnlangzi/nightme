// prompt.go — translate agent.ContentBlock slice to a single
// string suitable for `dsh --profile headless -- "<prompt>"`.
//
// headless profile takes one positional prompt. dsh headless does
// not expose image / file flags (unlike codex's `-i`); rich blocks
// degrade to bracketed annotations on the text message. This matches
// the fallback strategy in claudecode / pi print-mode and keeps the
// bridge consistent across all print-mode bridges.
package dsh

import (
	"fmt"
	"strings"

	"github.com/cnlangzi/nightme/internal/agent"
)

// blocksToPrompt joins ContentText blocks with "\n" and degrades
// ContentImage / ContentFile blocks to compact "[image: ...]" /
// "[file: ...]" suffixes on the message. Empty blocks are skipped.
//
// The output is the single string passed as the positional prompt
// to `dsh --profile headless`. When /gtw commit starts carrying
// image attachments this is where a base64-encoding or staged-file
// strategy would land — but dsh headless has no native image flag
// today, so the annotation strategy is the right stopgap.
func blocksToPrompt(blocks []agent.ContentBlock) string {
	var sb strings.Builder
	for _, b := range blocks {
		switch b.Type {
		case agent.ContentText:
			if b.Text == "" {
				continue
			}
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(b.Text)
		case agent.ContentImage:
			if b.Path == "" {
				continue
			}
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			fmt.Fprintf(&sb, "[image: %s (%s)]", b.Path, b.MediaType)
		case agent.ContentFile:
			if b.Path == "" {
				continue
			}
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			fmt.Fprintf(&sb, "[file: %s]", b.Path)
		}
	}
	return sb.String()
}
