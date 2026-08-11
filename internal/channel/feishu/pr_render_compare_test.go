// PR-rendering A/B comparison harness — historical artifact.
//
// Documents the two layout variants explored during the PR-tile
// design pass (2026-08). The conclusion (see git history for the
// commit that landed this) was:
//
//   - The cardFooterElements `strings.Contains(line, "](")` bypass
//     heuristic was based on an outdated empirical claim that
//     lark_md strips `[..](..)` inside <font>. Verified against
//     the live Feishu client: the claim was wrong — lark_md
//     renders the link correctly while the surrounding text
//     stays inside the <font> wrap. Heuristic removed.
//
//   - The PR tile format settled on `[#N](url)` — markdown link,
//     no emoji prefix. Earlier revisions explored a `🔗: [#N](url)`
//     form (emoji+colon prefix), and a plain `#N` form (no link);
//     the final pick was the middle ground. The URL is still
//     available on the PR struct for the /gtw pr success card.
//
// Variants dumped below are kept for reference so anyone who
// wants to revisit the choice can re-eyeball the byte-for-byte
// diff without a live Feishu round-trip:
//
//	A) `[#N](url)` — current production (markdown link, no prefix).
//	B) `🔗: [#N](url)` — historical alternate shape with the
//	   emoji+colon prefix on top of the link.
//
// Run with:
//
//	go test ./internal/channel/feishu -run TestPRRenderCompare -v
package feishu

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/command/gtw"
	"github.com/cnlangzi/nightme/internal/messages"
)

// formatPRSegmentWithEmojiPrefix renders the historical
// alternate PR-tile format `🔗: [#N](url)` — kept so the A/B
// harness can re-render the byte-for-byte payload anyone might
// want to revisit. Production formatPRSegment emits the bare
// plain `#N`; this helper is NOT used by production code.
func formatPRSegmentWithEmojiPrefix(ctx *messages.SessionContext) string {
	if ctx == nil {
		return ""
	}
	pr := ctx.PullRequest
	if pr == nil || pr.Number <= 0 || pr.URL == "" {
		return ""
	}
	return fmt.Sprintf("🔗: [#%d](%s)", pr.Number, pr.URL)
}

func TestPRRenderCompare(t *testing.T) {
	ctx := &messages.SessionContext{
		Agent:     "claude",
		Model:     "MiniMax-M3[1m]",
		SessionID: "639cc546-3647-44a5-ac0c-4f532cad04f4",
		Workspace: "/home/devin/code/nightme.nightme/fix-gitstatus",
		GitStatus: &gtw.GitStatusSnapshot{
			Branch: "fix-gitstatus",
		},
		PullRequest: &gtw.PR{
			Number: 116,
			URL:    "https://github.com/cnlangzi/nightme/pull/116",
			State:  "OPEN",
		},
	}

	// --- Variant A: current production format (plain #N) ---
	linesA := formatSessionFooterLines(ctx)
	cardA, err := buildResultCardJSON("Hello from the agent.\n\nThis is the reply body.", linesA)
	if err != nil {
		t.Fatalf("variant A build: %v", err)
	}

	// --- Variant B: historical 🔗: [#N](url) form ---
	// Production can't be swapped for this test, so we hand-
	// rebuild the line list with the alternate tail. The git
	// line otherwise matches production exactly.
	linesB := make([]string, 0, len(linesA))
	for _, l := range linesA {
		// Replace the trailing `[#N](url)` segment with the
		// historical emoji+link form. The `[#116](` token is
		// a stable substring (no other footer line contains it).
		if strings.Contains(l, "[#116](") {
			parts := strings.Split(l, " · ")
			parts[len(parts)-1] = formatPRSegmentWithEmojiPrefix(ctx)
			linesB = append(linesB, strings.Join(parts, " · "))
		} else {
			linesB = append(linesB, l)
		}
	}
	cardB, err := buildResultCardJSON("Hello from the agent.\n\nThis is the reply body.", linesB)
	if err != nil {
		t.Fatalf("variant B build: %v", err)
	}

	// Pretty-print for human reading.
	var prettyA, prettyB any
	_ = json.Unmarshal([]byte(cardA), &prettyA)
	_ = json.Unmarshal([]byte(cardB), &prettyB)
	pA, _ := json.MarshalIndent(prettyA, "", "  ")
	pB, _ := json.MarshalIndent(prettyB, "", "  ")

	t.Logf("\n========== Variant A: [#N](url) (production) ==========\n%s\n", pA)
	t.Logf("\n========== Variant B: 🔗: [#N](url) (historical) ==========\n%s\n", pB)

	// Also print the footer markdown-element content side-by-side
	// so the visual diff is obvious without JSON noise.
	t.Logf("\n--- footer lines A ---\n%s", strings.Join(linesA, "\n"))
	t.Logf("\n--- footer lines B ---\n%s", strings.Join(linesB, "\n"))

	// Sanity: both payloads must validate as JSON and reference #116.
	for _, c := range []string{cardA, cardB} {
		var probe any
		if err := json.Unmarshal([]byte(c), &probe); err != nil {
			t.Fatalf("payload not valid JSON: %v\n%s", err, c)
		}
		if !strings.Contains(c, "116") {
			t.Errorf("payload missing PR number 116:\n%s", c)
		}
	}
}
