// print_review_internal_test.go — unit tests for the
// /code-review print-mode recovery path: parsePrintStream's
// assistant-text tracking + follow-up swap, plus the
// isFollowupQuestion + longestText helpers + the
// detectDefaultBranch gate that decides whether to pass a
// positional <base>...HEAD target.
//
//go:build !windows

package claudecode

import (
	"context"
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
// post-multi-agent closing-remark pattern. Every real review
// has `## ` markdown headings; closing remarks don't. The
// short-text gate keeps the heuristic conservative.
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
			want: false,
		},
		{
			name: "real-review-shape",
			in: "# Code Review: `feat: add calc`\n\n**Branch:** `fix-review-on-claude`\n\n" +
				"## Findings\n\n" +
				"1. Package name `x` is non-idiomatic.\n2. Missing doc comments.\n3. No tests.\n",
			want: false, // has `## Findings` heading → not a follow-up
		},
		{
			name: "short-non-followup",
			in:   "all good",
			want: true, // short, no `## ` heading
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

// TestLongestText pins the largest-text picker used by the
// parsePrintStream assistant-tracking loop.
func TestLongestText(t *testing.T) {
	cases := []struct {
		name  string
		texts []string
		want  string
	}{
		{name: "empty", texts: nil, want: ""},
		{name: "single", texts: []string{"only"}, want: "only"},
		{
			name:  "first-wins-on-tie",
			texts: []string{"abc", "xyz"},
			want:  "abc",
		},
		{
			name:  "longest-middle",
			texts: []string{"a", "abcdef", "ab"},
			want:  "abcdef",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := longestText(tc.texts)
			if got != tc.want {
				t.Errorf("longestText(%v) = %q, want %q", tc.texts, got, tc.want)
			}
		})
	}
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
	got, err := parsePrintStream(context.Background(), strings.NewReader(stream))
	if err != nil {
		t.Fatalf("parsePrintStream: %v", err)
	}
	wantText := strings.TrimSpace(review)
	if got.Text != wantText {
		t.Errorf("Text mismatch: got %d chars, want %d chars", len(got.Text), len(wantText))
	}
	if got.AssistantText != "small progress ping" {
		t.Errorf("AssistantText should preserve the assistant block (audit), got %q", got.AssistantText)
	}
}

// TestParsePrintStream_RecoversReviewFromAssistantWhenFollowup pins
// the bug we're fixing: the /code-review plugin finishes with a
// short follow-up question, hiding the actual review. The parser
// should swap Text → AssistantText in that case.
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
	got, err := parsePrintStream(context.Background(), strings.NewReader(stream))
	if err != nil {
		t.Fatalf("parsePrintStream: %v", err)
	}
	wantReview := strings.TrimSpace(review)
	if got.Text != wantReview {
		t.Errorf("Text should be the recovered review, got %d chars, want %d chars", len(got.Text), len(wantReview))
	}
	if got.AssistantText != wantReview {
		t.Errorf("AssistantText should hold the recovered review, got %d chars, want %d chars", len(got.AssistantText), len(wantReview))
	}
}

// TestParsePrintStream_NoSwapWhenAssistantShorter pins the
// safety gate: when AssistantText isn't significantly longer
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
	got, err := parsePrintStream(context.Background(), strings.NewReader(stream))
	if err != nil {
		t.Fatalf("parsePrintStream: %v", err)
	}
	if got.Text != followup {
		t.Errorf("Text should remain followup when assistant is too short, got %q", got.Text)
	}
}

// TestDetectDefaultBranch_NoRepo pins the no-origin-remote
// fallback: when symbolic-ref fails AND remote show fails
// (no remote configured at all), detectDefaultBranch returns
// "" so the caller falls back to bare `code-review`.
func TestDetectDefaultBranch_NoRepo(t *testing.T) {
	got := detectDefaultBranch(context.Background(), "/tmp/this-path-does-not-exist")
	if got != "" {
		t.Errorf("non-existent path: got %q, want \"\"", got)
	}
}
