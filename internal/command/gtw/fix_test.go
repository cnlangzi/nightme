package gtw

import "testing"

// TestIssueHasLabel pins the label lookup used by the
// already-claimed gate in runFixRemote: a claim is the presence
// of `nightme/wip` on the issue, matched case-insensitively
// because the platforms treat label names that way.
func TestIssueHasLabel(t *testing.T) {
	cases := []struct {
		name  string
		issue *Issue
		label string
		want  bool
	}{
		{
			name:  "nil labels",
			issue: &Issue{ID: 42},
			label: LabelWIP,
			want:  false,
		},
		{
			name:  "empty labels",
			issue: &Issue{ID: 42, Labels: []string{}},
			label: LabelWIP,
			want:  false,
		},
		{
			name:  "exact match",
			issue: &Issue{ID: 42, Labels: []string{LabelWIP}},
			label: LabelWIP,
			want:  true,
		},
		{
			name:  "match among other labels",
			issue: &Issue{ID: 42, Labels: []string{"priority/high", LabelWIP, "bug"}},
			label: LabelWIP,
			want:  true,
		},
		{
			name:  "different case",
			issue: &Issue{ID: 42, Labels: []string{"Nightme/WIP"}},
			label: LabelWIP,
			want:  true,
		},
		{
			name:  "upper case",
			issue: &Issue{ID: 42, Labels: []string{"NIGHTME/WIP"}},
			label: LabelWIP,
			want:  true,
		},
		{
			name:  "other label only",
			issue: &Issue{ID: 42, Labels: []string{"priority/high"}},
			label: LabelWIP,
			want:  false,
		},
		{
			name:  "looking for another label",
			issue: &Issue{ID: 42, Labels: []string{LabelWIP}},
			label: LabelDone,
			want:  false,
		},
		{
			name:  "prefix is not a match",
			issue: &Issue{ID: 42, Labels: []string{"nightme/wip-extra"}},
			label: LabelWIP,
			want:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := issueHasLabel(tc.issue, tc.label)
			if got != tc.want {
				t.Errorf("issueHasLabel(%+v, %q) = %v, want %v", tc.issue, tc.label, got, tc.want)
			}
		})
	}
}
