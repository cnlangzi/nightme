package agent

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/proc"
)

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

// TestTruncateDiffToBytes verifies the byte cutter keeps whole lines
// only and reports exactly how many bytes it dropped. A line cut in
// half would give the agent a diff it cannot parse.
func TestTruncateDiffToBytes(t *testing.T) {
	// Under the budget: verbatim, nothing dropped.
	small := "a\nb\nc\n"
	got, dropped := truncateDiffToBytes(small, len(small))
	if got != small || dropped != 0 {
		t.Errorf("truncateDiffToBytes(under-budget) = (%q, %d), want (%q, 0)", got, dropped, small)
	}

	// Over the budget: whole lines only, exact dropped count.
	big := strings.Repeat("line\n", 100)
	got, dropped = truncateDiffToBytes(big, 10)
	if got != "line\nline\n" {
		t.Errorf("truncateDiffToBytes(big, 10) = %q, want two whole lines", got)
	}
	if dropped != len(big)-len(got) {
		t.Errorf("truncateDiffToBytes dropped = %d, want %d", dropped, len(big)-len(got))
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("truncateDiffToBytes left a partial trailing line: %q", got)
	}
	if !strings.HasPrefix(big, got) {
		t.Error("truncateDiffToBytes kept text that is not a prefix of the original diff")
	}

	// No budget: nothing kept, everything dropped.
	got, dropped = truncateDiffToBytes(big, 0)
	if got != "" || dropped != len(big) {
		t.Errorf("truncateDiffToBytes(big, 0) = (%q, %d), want (\"\", %d)", got, dropped, len(big))
	}

	// Empty: empty (no marker added to nothing).
	got, dropped = truncateDiffToBytes("", 100)
	if got != "" || dropped != 0 {
		t.Errorf("truncateDiffToBytes(\"\", 100) = (%q, %d), want (\"\", 0)", got, dropped)
	}
}

// TestFitDiffBody verifies a diff body under the allowance is verbatim
// and carries no marker, while a body over it is cut at line
// boundaries and marked with the number of dropped bytes.
func TestFitDiffBody(t *testing.T) {
	// Under the allowance: verbatim, nothing dropped.
	body, dropped := fitDiffBody("small diff\n", 1024)
	if body != "small diff\n" || dropped != 0 {
		t.Errorf("fitDiffBody(under) = (%q, %d), want (%q, 0)", body, dropped, "small diff\n")
	}
	if strings.Contains(body, "diff truncated") {
		t.Error("fitDiffBody(under) must not carry a truncation marker")
	}

	// Over the allowance: cut at a line boundary + marker.
	content := strings.Repeat("+changed line\n", 100)
	body, dropped = fitDiffBody(content, 200)
	if dropped == 0 {
		t.Fatal("fitDiffBody(over) reported no dropped bytes")
	}
	marker := diffTruncationMarker(dropped)
	if !strings.HasSuffix(body, marker) {
		t.Errorf("fitDiffBody(over) body does not end with the truncation marker:\n%s", body)
	}
	if !strings.Contains(marker, "read the affected files directly") {
		t.Errorf("truncation marker does not tell the agent to read the files: %q", marker)
	}
	kept := strings.TrimSuffix(body, marker)
	if !strings.HasPrefix(content, kept) {
		t.Error("fitDiffBody(over) kept text that is not a whole-line prefix of the original")
	}
	if dropped != len(content)-len(kept) {
		t.Errorf("fitDiffBody(over) dropped = %d, want %d", dropped, len(content)-len(kept))
	}
	if len(body) > 200 {
		t.Errorf("fitDiffBody(over) body = %d bytes, want ≤ 200", len(body))
	}
}

// TestAllocateDiffQuotas verifies the group budget is split fairly:
// a section that fits keeps its full content, an oversized section
// absorbs the cut, and the quotas never exceed the budget.
func TestAllocateDiffQuotas(t *testing.T) {
	// Everything fits: every quota is the section's own size.
	quotas := allocateDiffQuotas([]int{10, 20, 30}, 100)
	for i, want := range []int{10, 20, 30} {
		if quotas[i] != want {
			t.Errorf("quotas[%d] = %d, want %d (section fits in the budget)", i, quotas[i], want)
		}
	}

	// One oversized section: the small section keeps its full content,
	// the oversized one is cut, and the total stays within the budget.
	const budget = 1 << 20
	quotas = allocateDiffQuotas([]int{10, 2 * budget}, budget)
	if quotas[0] != 10 {
		t.Errorf("small section quota = %d, want 10 (full content)", quotas[0])
	}
	if quotas[1] >= 2*budget {
		t.Errorf("oversized section quota = %d, want < %d (section must be cut)", quotas[1], 2*budget)
	}
	if sum := quotas[0] + quotas[1]; sum > budget {
		t.Errorf("quotas sum = %d, want ≤ %d", sum, budget)
	}

	// Equal sections share the budget evenly.
	quotas = allocateDiffQuotas([]int{1000, 1000, 1000}, 1500)
	for i, want := range []int{500, 500, 500} {
		if quotas[i] != want {
			t.Errorf("quotas[%d] = %d, want %d (even split)", i, quotas[i], want)
		}
	}
}

// TestDiffSectionRender_Budget verifies a group whose sections exceed
// the budget keeps the small section whole, cuts the oversized one at
// a line boundary and stays inside the budget.
func TestDiffSectionRender_Budget(t *testing.T) {
	const budget = 4096
	small := diffSection{title: "Staged — group-filtered", content: "small diff\n"}
	big := diffSection{title: "Committed — group-filtered", content: strings.Repeat("+changed line\n", 2000)}

	quotas := allocateDiffQuotas(diffSectionSizes([]diffSection{small, big}), budget)
	smallBlock, smallDropped := small.render(quotas[0])
	bigBlock, bigDropped := big.render(quotas[1])

	if smallDropped != 0 {
		t.Errorf("small section dropped %d bytes, want 0 (it fits the budget)", smallDropped)
	}
	if !strings.Contains(smallBlock, small.content) {
		t.Errorf("small section lost its content:\n%s", smallBlock)
	}
	if bigDropped == 0 {
		t.Error("oversized section was not cut")
	}
	if !strings.Contains(bigBlock, "diff truncated") {
		t.Error("cut section carries no truncation marker")
	}
	if total := len(smallBlock) + len(bigBlock); total > budget {
		t.Errorf("rendered sections total %d bytes, want ≤ %d", total, budget)
	}
}

// TestAssembleGroupPrompt_DiffBudget drives a real git repo whose
// group diff exceeds the byte budget and verifies the assembled prompt
// stays inside the budget, that the cut is reported to the agent with
// the exact dropped byte count, and that the cut is logged.
func TestAssembleGroupPrompt_DiffBudget(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)

	base := gitCommitFile(t, dir, "a.go", "package a\n", "base")
	// A committed change far bigger than the group budget.
	gitCommitFile(t, dir, "a.go", "package a\n"+strings.Repeat("+changed line\n", 200000), "huge")
	// A staged change and an unstaged change, so all three sections exist.
	gitStageFile(t, dir, "b.go", "package b\n")
	gitWriteFile(t, dir, "a.go", "package a\n"+strings.Repeat("+changed line\n", 200000)+"+tail\n")

	rc := reviewContext{
		workspace:     dir,
		defaultBranch: "origin/main",
		mergeBase:     base,
		reviewable:    []string{"a.go", "b.go"},
	}
	g := &reviewGroup{Pattern: "**/*.go", Files: []string{"a.go", "b.go"}, Rule: "rule text"}

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(prev)

	prompt := assembleGroupPrompt(t.Context(), rc, g)

	blocks := diffBlocks(prompt)
	if len(blocks) != 3 {
		t.Fatalf("prompt has %d diff sections, want 3 (committed / staged / unstaged)", len(blocks))
	}
	total := 0
	for _, block := range blocks {
		total += len(block)
	}
	if total > groupDiffBudgetBytes {
		t.Errorf("diff payload = %d bytes, want ≤ %d", total, groupDiffBudgetBytes)
	}

	// The committed section is cut and says so.
	committed := diffBody(blocks[0])
	if !strings.Contains(committed, "diff truncated") {
		t.Fatalf("committed section carries no truncation marker:\n%s", committed[:200])
	}
	full := runGit(t.Context(), dir, "diff", base+"..HEAD", "--", "a.go", "b.go")
	markerStart := strings.LastIndex(committed, "\n…(diff truncated: ")
	if markerStart < 0 {
		t.Fatalf("committed section has no truncation marker:\n%s", committed[:200])
	}
	kept := committed[:markerStart]
	if !strings.HasPrefix(full, kept) {
		t.Error("committed section kept text that is not a whole-line prefix of the original diff")
	}
	dropped := len(full) - len(kept)
	if marker := committed[markerStart:]; marker != diffTruncationMarker(dropped) {
		t.Errorf("committed section marker does not report the dropped byte count (%d):\n%s",
			dropped, marker)
	}

	// The small sections keep their full content.
	staged := runGit(t.Context(), dir, "diff", "--staged", "--", "a.go", "b.go")
	if body := diffBody(blocks[1]); body != staged {
		t.Errorf("staged section is not verbatim:\ngot:  %q\nwant: %q", body, staged)
	}
	unstaged := runGit(t.Context(), dir, "diff", "--", "a.go", "b.go")
	if body := diffBody(blocks[2]); body != unstaged {
		t.Errorf("unstaged section is not verbatim:\ngot:  %q\nwant: %q", body, unstaged)
	}

	// The cut is logged with the group pattern and the dropped bytes.
	var rec struct {
		Msg     string `json:"msg"`
		Pattern string `json:"pattern"`
		Budget  int    `json:"budget_bytes"`
		Payload int    `json:"payload_bytes"`
		Dropped int    `json:"dropped_bytes"`
	}
	if err := json.Unmarshal(logs.Bytes(), &rec); err != nil {
		t.Fatalf("log record is not valid JSON: %v\n%s", err, logs.String())
	}
	if rec.Pattern != g.Pattern {
		t.Errorf("logged pattern = %q, want %q", rec.Pattern, g.Pattern)
	}
	if !strings.Contains(rec.Msg, "byte budget") {
		t.Errorf("logged message = %q, want it to name the byte budget", rec.Msg)
	}
	if rec.Budget != groupDiffBudgetBytes {
		t.Errorf("logged budget = %d, want %d", rec.Budget, groupDiffBudgetBytes)
	}
	if rec.Dropped != dropped {
		t.Errorf("logged dropped bytes = %d, want %d", rec.Dropped, dropped)
	}
	if rec.Payload > groupDiffBudgetBytes {
		t.Errorf("logged payload = %d bytes, want ≤ %d", rec.Payload, groupDiffBudgetBytes)
	}
}

// TestAssembleGroupPrompt_DiffUnderBudget verifies a group whose
// sections fit the budget is embedded verbatim, with no truncation
// marker anywhere in the prompt.
func TestAssembleGroupPrompt_DiffUnderBudget(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)

	base := gitCommitFile(t, dir, "a.go", "package a\n", "base")
	gitCommitFile(t, dir, "a.go", "package a\n+changed line\n", "small")

	rc := reviewContext{
		workspace:     dir,
		defaultBranch: "origin/main",
		mergeBase:     base,
		reviewable:    []string{"a.go"},
	}
	g := &reviewGroup{Pattern: "**/*.go", Files: []string{"a.go"}, Rule: "rule text"}

	prompt := assembleGroupPrompt(t.Context(), rc, g)

	blocks := diffBlocks(prompt)
	if len(blocks) != 1 {
		t.Fatalf("prompt has %d diff sections, want 1", len(blocks))
	}
	full := runGit(t.Context(), dir, "diff", base+"..HEAD", "--", "a.go")
	if body := diffBody(blocks[0]); body != full {
		t.Errorf("diff section is not verbatim:\ngot:  %q\nwant: %q", body, full)
	}
	if strings.Contains(prompt, "diff truncated") {
		t.Error("prompt carries a truncation marker although the diff fits the budget")
	}
}

// diffBlocks returns the rendered diff sections of an assembled group
// prompt — each "## <title>" header with its fenced body. The blocks
// together are the group's diff payload, the quantity the byte budget
// bounds.
func diffBlocks(prompt string) []string {
	start := strings.Index(prompt, "# Diffs (precomputed")
	if start < 0 {
		return nil
	}
	region := prompt[start:]
	nl := strings.Index(region, "\n")
	if nl < 0 {
		return nil
	}
	region = region[nl+1:]
	var blocks []string
	for {
		region = strings.TrimPrefix(region, "\n")
		if !strings.HasPrefix(region, "## ") {
			return blocks
		}
		fence := strings.Index(region, "```\n")
		if fence < 0 {
			return blocks
		}
		body := region[fence+len("```\n"):]
		end := strings.Index(body, "\n```\n")
		if end < 0 {
			return blocks
		}
		blocks = append(blocks, region[:fence+len("```\n")+end+len("\n```\n")])
		region = body[end+len("\n```\n"):]
	}
}

// diffBody returns the fenced body of a rendered diff section.
func diffBody(block string) string {
	open := strings.Index(block, "```\n")
	if open < 0 {
		return ""
	}
	return strings.TrimSuffix(block[open+len("```\n"):], "\n```\n")
}

// gitWriteFile writes content to path (relative to dir).
func gitWriteFile(t *testing.T, dir, path, content string) {
	t.Helper()
	full := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// gitStageFile writes content to path and stages it without committing.
func gitStageFile(t *testing.T, dir, path, content string) {
	t.Helper()
	gitWriteFile(t, dir, path, content)
	c := proc.New(t.Context(), "git", "-C", dir, "add", path)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git add %s: %v\n%s", path, err, out)
	}
}

// gitCommitFile writes content to path, commits it and returns the
// new commit hash.
func gitCommitFile(t *testing.T, dir, path, content, msg string) string {
	t.Helper()
	gitWriteFile(t, dir, path, content)
	c := proc.New(t.Context(), "git", "-C", dir, "add", path)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git add %s: %v\n%s", path, err, out)
	}
	c = proc.New(t.Context(), "git", "-C", dir, "commit", "-q", "-m", msg)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	c = proc.New(t.Context(), "git", "-C", dir, "rev-parse", "HEAD")
	out, err := c.Output()
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}
