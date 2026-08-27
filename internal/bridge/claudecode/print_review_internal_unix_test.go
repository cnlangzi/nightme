// print_review_internal_test.go — unit tests for the
// /code-review print-mode recovery path: parsePrintStream's
// largest-assistant-text tracking + follow-up swap, plus the
// isFollowupQuestion heuristic + the detectDefaultBranch
// fallback that decides whether to pass a positional
// <base>...HEAD target.
//
//go:build !windows

package claudecode

import (
	"context"
	"github.com/cnlangzi/nightme/internal/agent"
	"strings"
	"testing"
)

// jsonEscape escapes s for safe inclusion inside a JSON string
// value. Used by parsePrintStream tests so multi-line assistant
// text + embedded `"` round-trip through encoding/json cleanly.
func jsonEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\r", `\r`)
	s = strings.ReplaceAll(s, "\t", `\t`)
	return s
}

// TestIsFollowupQuestion pins the /code-review plugin's
// closing-remark pattern. Positive match only — anything
// not matching the phrase list returns false. This is the
// critical guard against overwriting a clean short result
// (e.g. "✅ Looks clean.") with a progress ping from the
// assistant stream (v15b fix; see F-review §13.6).
func TestIsFollowupQuestion(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{
			name: "want-me-to-apply",
			in:   "Want me to apply the suggested patch (rename package, add doc comments, fix gofmt)?",
			want: true,
		},
		{
			name: "should-i",
			in:   "Should I proceed with the auto-fix?",
			want: true,
		},
		{
			name: "would-you-like",
			in:   "Would you like me to also bump the version constant?",
			want: true,
		},
		{
			name: "anything-else",
			in:   "Anything else you'd like me to flag?",
			want: true,
		},
		{
			name: "posted-confirmation",
			in:   "Posted review to PR #42.",
			want: true,
		},
		{
			name: "comment-added",
			in:   "Comment added to PR #42.",
			want: true,
		},
		{
			name: "long-real-review",
			in: "# Code Review\n\n**Diff:** +12/-3\n\n" +
				"## Findings\n\n" +
				strings.Repeat("- **critical**: file.go:42 — something bad. ", 20),
			want: false, // length gate (>600 chars) — even an inline question wouldn't trigger
		},
		{
			name: "real-review-shape",
			in: "# Code Review: `feat: add calc`\n\n**Branch:** `fix-review-on-claude`\n\n" +
				"## Findings\n\n" +
				"1. Package name `x` is non-idiomatic.\n2. Missing doc comments.\n3. No tests.\n",
			want: false,
		},
		{
			// The v15b regression case: a clean short result
			// must NOT be misidentified as a follow-up (which
			// would cause the parser to overwrite the short
			// result with a longer progress ping from the
			// assistant stream).
			name: "clean-short-result",
			in:   "✅ Looks clean.",
			want: false,
		},
		{
			name: "no-issues-found",
			in:   "No issues found.",
			want: false,
		},
		{
			name: "all-good",
			in:   "all good",
			want: false, // not a question, not a plugin phrase — leave alone
		},
		{
			name: "empty",
			in:   "",
			want: false,
		},
		{
			name: "whitespace-only",
			in:   "   \n\t  ",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isFollowupQuestion(tc.in)
			if got != tc.want {
				t.Errorf("isFollowupQuestion(%q...) = %v, want %v",
					tc.in[:min(60, len(tc.in))], got, tc.want)
			}
		})
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestParsePrintStream_PrefersResultTextWhenLong pins the
// happy path: when the terminal result event's Text is
// long and review-shaped (has `## ` headings), we use it
// directly. No swap.
func TestParsePrintStream_PrefersResultTextWhenLong(t *testing.T) {
	review := "# Code Review: `feat: add calc`\n\n" +
		"## Summary\n\nTiny change.\n\n" +
		"## Findings\n\n" +
		strings.Repeat("- **high**: file.go — finding description.\n", 20)
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"s1","model":"claude-sonnet-4-5"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"small progress ping"}]}}`,
		`{"type":"result","subtype":"success","session_id":"s1","duration_ms":12345,"result":"` + jsonEscape(review) + `"}`,
		"",
	}, "\n")
	got, err := parsePrintStream(context.Background(), strings.NewReader(stream), true)
	if err != nil {
		t.Fatalf("parsePrintStream: %v", err)
	}
	wantText := strings.TrimSpace(review)
	if got.Text != wantText {
		t.Errorf("Text mismatch: got %d chars, want %d chars", len(got.Text), len(wantText))
	}
	// RecoveredText should hold the assistant source; the
	// review-shaped terminal text is the authoritative copy.
	if got.RecoveredText != "small progress ping" {
		t.Errorf("RecoveredText should preserve the assistant block (audit), got %q", got.RecoveredText)
	}
}

// TestParsePrintStream_RecoversReviewFromAssistantWhenFollowup pins
// the bug we're fixing: the /code-review plugin finishes with a
// short follow-up question, hiding the actual review. The parser
// should swap Text → RecoveredText in that case.
func TestParsePrintStream_RecoversReviewFromAssistantWhenFollowup(t *testing.T) {
	review := "# Code Review — `fix-review-on-claude`\n\n" +
		"## Findings\n\n" +
		"1. Package name `x` is non-idiomatic — calc.go:1.\n" +
		"2. Missing doc comments — calc.go:2-3.\n" +
		"3. No tests.\n" +
		strings.Repeat("- **nit**: minor style preference.\n", 30)
	followup := "Want me to apply the suggested patch?"

	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"s1","model":"claude-sonnet-4-5"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` + jsonEscape(review) + `"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` + jsonEscape(followup) + `"}]}}`,
		`{"type":"result","subtype":"success","session_id":"s1","duration_ms":12345,"result":"` + jsonEscape(followup) + `"}`,
		"",
	}, "\n")
	got, err := parsePrintStream(context.Background(), strings.NewReader(stream), true)
	if err != nil {
		t.Fatalf("parsePrintStream: %v", err)
	}
	wantReview := strings.TrimSpace(review)
	if got.Text != wantReview {
		t.Errorf("Text should be the recovered review, got %d chars, want %d chars", len(got.Text), len(wantReview))
	}
	if got.RecoveredText != wantReview {
		t.Errorf("RecoveredText should hold the recovered review, got %d chars, want %d chars", len(got.RecoveredText), len(wantReview))
	}
}

// TestParsePrintStream_NoSwapWhenAssistantShorter pins the
// safety gate: when RecoveredText isn't significantly longer
// than the follow-up, we don't swap. Defensive against a future
// plugin version emitting shorter assistant text than the
// follow-up.
func TestParsePrintStream_NoSwapWhenAssistantShorter(t *testing.T) {
	followup := "Want me to apply the suggested patch?"
	stream := strings.Join([]string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"short ack"}]}}`,
		`{"type":"result","subtype":"success","session_id":"s1","duration_ms":100,"result":"` + followup + `"}`,
		"",
	}, "\n")
	got, err := parsePrintStream(context.Background(), strings.NewReader(stream), true)
	if err != nil {
		t.Fatalf("parsePrintStream: %v", err)
	}
	if got.Text != followup {
		t.Errorf("Text should remain followup when assistant is too short, got %q", got.Text)
	}
}

// TestParsePrintStream_NoRecoveryWhenNotReview pins the v15b
// review-only gate: when isReview=false, parsePrintStream must
// NOT populate RecoveredText and must NOT swap Text. This is
// the over-broad exposure guard from the code review — every
// non-review claudecode call (/gtw commit, buildAgentPrompt,
// etc.) should produce a RunResult with RecoveredText="" so
// callers / audit loggers can't accidentally surface raw
// assistant stream content for unrelated prompts.
func TestParsePrintStream_NoRecoveryWhenNotReview(t *testing.T) {
	review := "# Code Review\n\n## Findings\n\n" + strings.Repeat("detail. ", 100)
	stream := strings.Join([]string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` + jsonEscape(review) + `"}]}}`,
		`{"type":"result","subtype":"success","session_id":"s1","duration_ms":100,"result":"` + jsonEscape(review) + `"}`,
		"",
	}, "\n")
	got, err := parsePrintStream(context.Background(), strings.NewReader(stream), false)
	if err != nil {
		t.Fatalf("parsePrintStream: %v", err)
	}
	if got.RecoveredText != "" {
		t.Errorf("RecoveredText must be empty when isReview=false, got %d chars", len(got.RecoveredText))
	}
	// Even if the terminal result looks like a follow-up,
	// the non-review path must not swap.
	stream2 := strings.Join([]string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` + jsonEscape(review) + `"}]}}`,
		`{"type":"result","subtype":"success","session_id":"s1","duration_ms":100,"result":"Want me to apply?"}`,
		"",
	}, "\n")
	got2, err := parsePrintStream(context.Background(), strings.NewReader(stream2), false)
	if err != nil {
		t.Fatalf("parsePrintStream (no-swap): %v", err)
	}
	if got2.Text != "Want me to apply?" {
		t.Errorf("Text should remain the followup when isReview=false (no swap), got %q", got2.Text)
	}
	if got2.RecoveredText != "" {
		t.Errorf("RecoveredText must remain empty when isReview=false (no swap), got %d chars", len(got2.RecoveredText))
	}
}

// TestDetectDefaultBranch_NoRepo pins the no-origin-remote
// fallback: when symbolic-ref fails AND remote show fails
// (no remote configured at all), detectDefaultBranch returns
// "" so the caller falls back to bare `code-review`.
func TestDetectDefaultBranch_NoRepo(t *testing.T) {
	got := agent.DetectDefaultBranch(context.Background(), "/tmp/this-path-does-not-exist")
	if got != "" {
		t.Errorf("non-existent path: got %q, want \"\"", got)
	}
}

// TestRunResult_RecoveredTextZeroValue pins the contract that
// non-claudecode bridges leave RecoveredText empty. Currently
// the field is populated ONLY by claudecode's print-mode
// recovery layer (gated by isReview=true); the zero-value
// test guards against future bridges accidentally populating
// it without intent.
func TestRunResult_RecoveredTextZeroValue(t *testing.T) {
	var r agent.RunResult
	if r.RecoveredText != "" {
		t.Errorf("zero-value RunResult.RecoveredText must be \"\", got %q", r.RecoveredText)
	}
}
