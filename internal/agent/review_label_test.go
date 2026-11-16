package agent

// How a review group is named for the reader: the name of what it
// reviews, and the summary of the files it covers.

import (
	"strings"
	"testing"
)

// TestReviewGroupLabel_NamesTheNightmeGroups: the nightme-owned groups
// are named by what they review — the sentinel pattern is an internal
// marker and must not reach the chat.
func TestReviewGroupLabel_NamesTheNightmeGroups(t *testing.T) {
	builtin := reviewGroupLabel(reviewGroup{
		Pattern: PatternBuiltin,
		Files:   []string{"internal/foo.go"},
	})
	if !strings.HasPrefix(builtin, "nightme code reviewer") {
		t.Errorf("the builtin group's label = %q, want it to start with %q", builtin, "nightme code reviewer")
	}

	simplify := reviewGroupLabel(reviewGroup{
		Pattern: PatternSimplify,
		Files:   []string{"internal/foo.go"},
	})
	if !strings.HasPrefix(simplify, "Simplify review lens") {
		t.Errorf("the simplify group's label = %q, want it to start with %q", simplify, "Simplify review lens")
	}
}

// TestReviewGroupLabel_NamesAnOcrGroupByItsIdentity: an ocr group is
// named by the rule's source and pattern. The rule's own text is not
// used — its first line describes only the rule's first category,
// while the group checks every category of that rule.
func TestReviewGroupLabel_NamesAnOcrGroupByItsIdentity(t *testing.T) {
	got := reviewGroupLabel(reviewGroup{
		Pattern: "**/*.rs",
		Source:  "system",
		Files:   []string{"src/main.rs"},
	})
	if !strings.HasPrefix(got, "system / **/*.rs") {
		t.Errorf("the ocr group's label = %q, want it to start with %q", got, "system / **/*.rs")
	}
}

// TestReviewGroupLabel_AppendsTheFileSummary: every group's name is
// followed by the summary of the files it covers.
func TestReviewGroupLabel_AppendsTheFileSummary(t *testing.T) {
	got := reviewGroupLabel(reviewGroup{
		Pattern: PatternBuiltin,
		Files:   []string{"a.go", "b.go"},
	})
	if got != "nightme code reviewer · Go" {
		t.Errorf("the builtin group's label = %q, want %q", got, "nightme code reviewer · Go")
	}
}

// TestReviewFileSummary: the summary names the extensions the group
// covers, most frequent first, at most three of them.
func TestReviewFileSummary(t *testing.T) {
	tests := []struct {
		name  string
		files []string
		want  string
	}{
		{
			name:  "one extension",
			files: []string{"a.go", "b.go", "c.go"},
			want:  "Go",
		},
		{
			name:  "most frequent first",
			files: []string{"a.go", "b.go", "c.rs", "d.toml"},
			want:  "Go, Rust, TOML",
		},
		{
			name:  "at most three",
			files: []string{"a.go", "b.rs", "c.toml", "d.md", "e.yaml"},
			want:  "Go, Rust, TOML 等",
		},
		{
			name:  "a file without an extension contributes its name",
			files: []string{"a.go", "Makefile"},
			want:  "Go, Makefile",
		},
		{
			name:  "no files",
			files: nil,
			want:  "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := reviewFileSummary(tc.files); got != tc.want {
				t.Errorf("reviewFileSummary(%v) = %q, want %q", tc.files, got, tc.want)
			}
		})
	}
}

// TestReviewPlan_NamesEveryGroup: the plan the fan-out reports names
// every group by what it reviews, and carries the source that names
// an ocr group.
func TestReviewPlan_NamesEveryGroup(t *testing.T) {
	plan := reviewPlan([]reviewGroup{
		{Pattern: PatternBuiltin, Files: []string{"a.go"}},
		{Pattern: PatternSimplify, Files: []string{"a.go"}},
		{Pattern: "**/*.rs", Source: "system", Files: []string{"b.rs"}},
	})
	if len(plan) != 3 {
		t.Fatalf("the plan carries %d groups, want 3: %+v", len(plan), plan)
	}
	for i, want := range []string{
		"nightme code reviewer · Go",
		"Simplify review lens · Go",
		"system / **/*.rs · Rust",
	} {
		if plan[i].Label != want {
			t.Errorf("plan[%d].Label = %q, want %q", i, plan[i].Label, want)
		}
	}
	if plan[2].Source != "system" {
		t.Errorf("plan[2].Source = %q, want %q", plan[2].Source, "system")
	}
}

// TestReviewGroupName_TwoRulesOverTheSameGlob: two rules over the
// same glob are two groups — the source tells them apart.
func TestReviewGroupName_TwoRulesOverTheSameGlob(t *testing.T) {
	security := ReviewGroupName("security", "**/*.go")
	performance := ReviewGroupName("performance", "**/*.go")
	if security == performance {
		t.Errorf("the two groups over the same glob share a name: %q", security)
	}
}
