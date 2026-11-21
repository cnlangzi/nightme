package agent

import (
	"testing"
)

// TestIsCodePath pins the code-file classification: the filename table
// wins over the extension, the extension match is case-insensitive, and
// everything neither table names is not a code file.
func TestIsCodePath(t *testing.T) {
	code := []string{
		"a.go",
		"internal/agent/review.go",
		"main.GO", // the extension match is case-insensitive
		"web/app.tsx",
		"scripts/run.sh",
		"infra/main.tf",
		"Makefile", // named by the filename table
		"GNUmakefile",
		"CMakeLists.txt", // the filename table wins over ".txt"
		"Dockerfile",
		"BUILD.bazel",
		"Jenkinsfile",
	}
	for _, p := range code {
		if !isCodePath(p) {
			t.Errorf("isCodePath(%q) = false, want true (code file)", p)
		}
	}

	notCode := []string{
		"README.md",
		"docs/SPEC.md",
		"notes.txt",
		"data.json",
		"config.yaml",
		".gitignore", // a dotfile is classified by name only
		"x.unknownext",
		"logo.png",
		"archive.tar.gz",
	}
	for _, p := range notCode {
		if isCodePath(p) {
			t.Errorf("isCodePath(%q) = true, want false (not a code file)", p)
		}
	}
}

// TestCodeFiles_PreservesOrder verifies the filter keeps the order of
// the paths it was given — the simplify group's file list follows the
// changeset, not the table.
func TestCodeFiles_PreservesOrder(t *testing.T) {
	got := codeFiles([]string{"a.go", "README.md", "b.ts", "docs/README.md", "Makefile"})
	want := []string{"a.go", "b.ts", "Makefile"}
	if len(got) != len(want) {
		t.Fatalf("codeFiles = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("codeFiles[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestCodeFiles_NoCodeFile verifies the filter returns nothing when
// the changeset holds no code file — the caller uses that to skip the
// simplify group.
func TestCodeFiles_NoCodeFile(t *testing.T) {
	if got := codeFiles([]string{"README.md", "docs/README.md"}); len(got) != 0 {
		t.Errorf("codeFiles = %v, want none", got)
	}
}

// TestSimplifyGroup_CodeFilesOnly verifies the simplify group covers
// the code files only: the lens has nothing to act on in prose.
func TestSimplifyGroup_CodeFilesOnly(t *testing.T) {
	g := simplifyGroup([]string{"a.go", "README.md", "b.ts"})
	if g.Pattern != PatternSimplify {
		t.Errorf("simplifyGroup pattern = %q, want %q", g.Pattern, PatternSimplify)
	}
	if g.Rule != simplifyPrompt {
		t.Errorf("simplifyGroup rule is not the simplify prompt")
	}
	want := []string{"a.go", "b.ts"}
	if len(g.Files) != len(want) {
		t.Fatalf("simplifyGroup files = %v, want %v", g.Files, want)
	}
	for i := range want {
		if g.Files[i] != want[i] {
			t.Errorf("simplifyGroup files[%d] = %q, want %q", i, g.Files[i], want[i])
		}
	}
}

// TestReviewGroups_SkipsSimplifyWithoutCodeFiles verifies the fan-out
// carries no simplify group when the changeset has no code file: the
// group would answer "0 covered, 0 findings" and cost a session turn.
//
// The builtin group keeps its own scope — every tracked changed file,
// code or not.
func TestReviewGroups_SkipsSimplifyWithoutCodeFiles(t *testing.T) {
	pre := reviewContext{
		reviewable: []string{"README.md"},
		ocrGroups: []reviewGroup{{
			Pattern: PatternBuiltin,
			Files:   []string{"README.md"},
			Rule:    BuiltinPrompt,
		}},
	}
	groups := reviewGroups(pre)
	if len(groups) != 1 {
		t.Fatalf("reviewGroups = %d groups, want 1 (the builtin group)", len(groups))
	}
	if groups[0].Pattern == PatternSimplify {
		t.Error("the fan-out carries a simplify group although the changeset has no code file")
	}
	if len(groups[0].Files) != 1 || groups[0].Files[0] != "README.md" {
		t.Errorf("builtin group files = %v, want [README.md] (its scope is unchanged)", groups[0].Files)
	}
}

// TestReviewGroups_KeepsSimplifyWithCodeFiles verifies the simplify
// group is appended — and narrowed to the code files — when the
// changeset has code.
func TestReviewGroups_KeepsSimplifyWithCodeFiles(t *testing.T) {
	pre := reviewContext{
		reviewable: []string{"a.go", "README.md", "b.ts"},
		ocrGroups: []reviewGroup{{
			Pattern: PatternBuiltin,
			Files:   []string{"a.go", "README.md", "b.ts"},
			Rule:    BuiltinPrompt,
		}},
	}
	groups := reviewGroups(pre)
	if len(groups) != 2 {
		t.Fatalf("reviewGroups = %d groups, want 2 (builtin + simplify)", len(groups))
	}
	if groups[1].Pattern != PatternSimplify {
		t.Fatalf("the last group pattern = %q, want %q", groups[1].Pattern, PatternSimplify)
	}
	want := []string{"a.go", "b.ts"}
	if len(groups[1].Files) != len(want) {
		t.Fatalf("simplify group files = %v, want %v", groups[1].Files, want)
	}
	for i := range want {
		if groups[1].Files[i] != want[i] {
			t.Errorf("simplify group files[%d] = %q, want %q", i, groups[1].Files[i], want[i])
		}
	}
	// The builtin group keeps the full tracked list.
	if len(groups[0].Files) != 3 {
		t.Errorf("builtin group files = %v, want all 3 tracked files", groups[0].Files)
	}
}
