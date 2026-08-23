package agent

import (
	"strings"
	"testing"
)

// TestStripRefsRemotes pins the #1 fix: detectDefaultBranch must return
// a RESOLVABLE ref ("origin/main"), not a bare name ("main"). The bare
// name made `git diff main...HEAD` fail on a feature branch with no
// local `main`, silently emptying committedDiff and dropping the
// review's main scenario. stripRefsRemotes turns the symbolic-ref
// output ("refs/remotes/origin/main") into the resolvable form.
func TestStripRefsRemotes(t *testing.T) {
	cases := []struct{ in, want string }{
		{"refs/remotes/origin/main", "origin/main"},
		{"refs/remotes/origin/master", "origin/master"},
		{"refs/remotes/origin/trunk", "origin/trunk"},
		{"origin/main", "origin/main"},         // already short: unchanged
		{"refs/heads/main", "refs/heads/main"}, // wrong prefix: unchanged
		{"", ""},
	}
	for _, c := range cases {
		if got := stripRefsRemotes(c.in); got != c.want {
			t.Errorf("stripRefsRemotes(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestIsReviewablePath verifies the Tier 3 noise filter drops
// well-known non-source directories but leaves real source paths.
// Tier 2 uses ocr's (richer) FileFilter instead; this is the fallback.
func TestIsReviewablePath(t *testing.T) {
	reject := []string{
		"pkg/generated/foo.go",
		"internal/testdata/fixtures.json",
		"vendor/github.com/x/y.go",
		"third_party/lib.c",
		"node_modules/left-pad/index.js",
		"app/build/bundle.js",
		"dist/main.js",
	}
	for _, p := range reject {
		if isReviewablePath(p) {
			t.Errorf("isReviewablePath(%q) = true, want false (noise dir)", p)
		}
	}
	keep := []string{
		"internal/agent/delegate_review.go",
		"cmd/nightme/main.go",
		"src/app/handler.ts",
		"docs/README.md",
		"pkg/mygenerated.go", // "mygenerated" has no /generated/ segment
	}
	for _, p := range keep {
		if !isReviewablePath(p) {
			t.Errorf("isReviewablePath(%q) = false, want true (real source)", p)
		}
	}
}

// TestTruncateDiff verifies the #2 fix: a diff over the line cap is
// truncated with a pointer marker, while a small diff is returned
// verbatim. Without this a large PR's full diff blows the host agent's
// context window.
func TestTruncateDiff(t *testing.T) {
	// Under the cap: verbatim.
	small := "a\nb\nc\n"
	if got := truncateDiff(small); got != small {
		t.Errorf("truncateDiff(under-cap) mutated the input:\ngot:  %q\nwant: %q", got, small)
	}

	// Over the cap: truncated + marker.
	var bigBuilder strings.Builder
	for i := 0; i < maxDiffLines+500; i++ {
		bigBuilder.WriteString("line\n")
	}
	big := bigBuilder.String()
	got := truncateDiff(big)
	if !strings.Contains(got, "diff truncated") {
		t.Errorf("truncateDiff(over-cap) missing truncation marker")
	}
	if len(got) >= len(big) {
		t.Errorf("truncateDiff(over-cap) did not shrink: got %d bytes, input %d", len(got), len(big))
	}
	// The kept head must still start with the first line (agent keeps
	// the beginning, not the end — the truncation marker is appended).
	if !strings.HasPrefix(got, "line\n") {
		t.Errorf("truncateDiff(over-cap) lost the head: %q", got[:min(len(got), 40)])
	}

	// Empty: empty (no marker added to nothing).
	if got := truncateDiff(""); got != "" {
		t.Errorf("truncateDiff(\"\") = %q, want \"\"", got)
	}
}

// TestAssembleReviewPrompt_Tier3Rubric verifies the #4 fix: Tier 3
// (ocr absent, ocrRules empty) uses the built-in "What to look for"
// list and does NOT inject an ocr-rules section.
func TestAssembleReviewPrompt_Tier3Rubric(t *testing.T) {
	rc := reviewContext{
		workspace:     "/repo",
		defaultBranch: "origin/main",
		mergeBase:     "abc123",
		committedDiff: "diff --git a/x b/x\n+hello\n",
		reviewable:    []string{"x.go"},
		// ocrRules empty → Tier 3.
	}
	prompt := assembleReviewPrompt(rc)
	if !strings.Contains(prompt, "What to look for") {
		t.Error("Tier 3 prompt missing built-in 'What to look for' rubric")
	}
	if strings.Contains(prompt, "Review rules (matched per file") {
		t.Error("Tier 3 prompt must NOT contain an ocr-rules section (rules never mixed, REVIEW.md §2.4)")
	}
	if !strings.Contains(prompt, "How to review") {
		t.Error("Tier 3 prompt missing 'How to review' methodology (shared by both tiers)")
	}
	if !strings.Contains(prompt, "coverage_rate") {
		t.Error("Tier 3 prompt missing coverage_rate field (SKILL Step 6)")
	}
}

// TestAssembleReviewPrompt_Tier2Rubric verifies the #4 fix: Tier 2
// (ocr present, ocrRules set) injects ocr's per-file rules and uses
// ONLY the language-agnostic methodology — the built-in "What to look
// for" list is NOT added, because ocr's rules ARE the language-specific
// guidance (mixing both is redundant and bloats the prompt).
func TestAssembleReviewPrompt_Tier2Rubric(t *testing.T) {
	rc := reviewContext{
		workspace:  "/repo",
		reviewable: []string{"x.go"},
		ocrRules:   "### Rule (pattern **/*.go) — applies to: x.go\nErrors returned from calls that are ignored...\n\n",
	}
	prompt := assembleReviewPrompt(rc)
	if !strings.Contains(prompt, "Review rules (matched per file") {
		t.Error("Tier 2 prompt missing ocr-rules section")
	}
	if !strings.Contains(prompt, "How to review") {
		t.Error("Tier 2 prompt missing 'How to review' methodology (shared by both tiers)")
	}
	if strings.Contains(prompt, "What to look for") {
		t.Error("Tier 2 prompt must NOT contain built-in 'What to look for' (ocr rules substitute it; REVIEW.md §2.4 — never mixed)")
	}
}

// TestAssembleReviewPrompt_EmptyWorkspace verifies the fallback
// contract: assembleReviewPrompt returns "" ONLY when workspace is
// empty, which is DelegateReview's signal to fall back to
// StandardPrompt verbatim.
func TestAssembleReviewPrompt_EmptyWorkspace(t *testing.T) {
	if got := assembleReviewPrompt(reviewContext{}); got != "" {
		t.Errorf("assembleReviewPrompt(empty workspace) = %q, want \"\" (fallback signal)", got)
	}
}

// TestAssembleReviewPrompt_DiffLabel verifies the committed-diff label
// reflects the resolvable merge-base (two-dot) when available, rather
// than the symbolic base (three-dot) — this is the #1 fix surface in
// the prompt itself.
func TestAssembleReviewPrompt_DiffLabel(t *testing.T) {
	rc := reviewContext{
		workspace:     "/repo",
		defaultBranch: "origin/main",
		mergeBase:     "abc123def",
		committedDiff: "diff\n",
	}
	prompt := assembleReviewPrompt(rc)
	if !strings.Contains(prompt, "abc123def..HEAD") {
		t.Errorf("prompt should label committed diff with the merge-base two-dot form; got:\n%s", prompt)
	}
	if strings.Contains(prompt, "origin/main...HEAD") {
		t.Errorf("prompt should NOT use the symbolic three-dot label when merge-base is known")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
