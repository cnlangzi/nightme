package gtw

// Tests for the provider calls behind the PR half of /gtw close:
// the lookup of a branch's pull request in ANY state, the read of
// the labels it carries, and the removal of one of them.

import (
	"context"
	"strings"
	"testing"
)

// TestGitHubFindPRForBranch_AnyState pins the argv and the state
// the lookup reports.
//
// The state filter is "all": the close finalizes the pull request
// after the merge, and a merged pull request is not open anymore —
// the open-only lookups would answer "no PR" exactly when the close
// needs one.
func TestGitHubFindPRForBranch_AnyState(t *testing.T) {
	cli := &stubCLIRunner{stdout: `[
		{"number": 7, "url": "https://github.com/cnlangzi/nightme/pull/7", "state": "MERGED"}
	]`}
	p := &GitHubProvider{Runner: cli, host: "github.com"}

	pr, err := p.FindPRForBranch(context.Background(), "cnlangzi", "nightme", "fix-42")
	if err != nil {
		t.Fatalf("FindPRForBranch: %v", err)
	}
	if pr == nil {
		t.Fatal("the lookup found no pull request")
	}
	if pr.Number != 7 {
		t.Errorf("the number is %d, want 7", pr.Number)
	}
	if pr.State != "merged" {
		t.Errorf("the state is %q, want merged", pr.State)
	}

	want := []string{
		"gh", "pr", "list",
		"--head", "fix-42",
		"--state", "all",
		"--json", "number,url,state",
		"--repo", "cnlangzi/nightme",
	}
	if strings.Join(cli.argv, " ") != strings.Join(want, " ") {
		t.Errorf("the argv is %v, want %v", cli.argv, want)
	}
}

// TestGitHubFindPRForBranch_NoPR pins the fail-soft answer: a
// branch that never had a pull request is (nil, nil), not an error.
func TestGitHubFindPRForBranch_NoPR(t *testing.T) {
	cli := &stubCLIRunner{stdout: "[]"}
	p := &GitHubProvider{Runner: cli, host: "github.com"}

	pr, err := p.FindPRForBranch(context.Background(), "cnlangzi", "nightme", "fix-42")
	if err != nil {
		t.Fatalf("FindPRForBranch: %v", err)
	}
	if pr != nil {
		t.Errorf("the lookup returned %+v, want nil", pr)
	}
}

// TestGitLabFindPRForBranch_AnyState pins the argv and the state
// the lookup reports.
//
// The REST endpoint is used instead of `glab mr list`: the CLI lost
// its `--state` flag in 1.36 and the replacement flags do not spell
// "every state".
func TestGitLabFindPRForBranch_AnyState(t *testing.T) {
	cli := &stubCLIRunner{stdout: `[
		{"iid": 7, "web_url": "https://gitlab.com/cnlangzi/nightme/-/merge_requests/7", "state": "merged"}
	]`}
	p := &GitLabProvider{Runner: cli, host: "gitlab.com"}

	pr, err := p.FindPRForBranch(context.Background(), "cnlangzi", "nightme", "fix/42-test")
	if err != nil {
		t.Fatalf("FindPRForBranch: %v", err)
	}
	if pr == nil {
		t.Fatal("the lookup found no merge request")
	}
	if pr.Number != 7 {
		t.Errorf("the number is %d, want 7", pr.Number)
	}
	if pr.State != "merged" {
		t.Errorf("the state is %q, want merged", pr.State)
	}

	want := []string{
		"glab", "api",
		"projects/cnlangzi%2Fnightme/merge_requests?source_branch=fix%2F42-test&state=all",
	}
	if strings.Join(cli.argv, " ") != strings.Join(want, " ") {
		t.Errorf("the argv is %v, want %v", cli.argv, want)
	}
}

// TestGitLabFindPRForBranch_NoPR pins the fail-soft answer: a
// branch that never had a merge request is (nil, nil).
func TestGitLabFindPRForBranch_NoPR(t *testing.T) {
	cli := &stubCLIRunner{stdout: "[]"}
	p := &GitLabProvider{Runner: cli, host: "gitlab.com"}

	pr, err := p.FindPRForBranch(context.Background(), "cnlangzi", "nightme", "fix-42")
	if err != nil {
		t.Fatalf("FindPRForBranch: %v", err)
	}
	if pr != nil {
		t.Errorf("the lookup returned %+v, want nil", pr)
	}
}

// TestGitHubListPRLabels pins the argv and the decoding of the
// label names.
func TestGitHubListPRLabels(t *testing.T) {
	cli := &stubCLIRunner{stdout: `{
		"labels": [
			{"name": "nightme/ready"},
			{"name": "nightme/assigned"},
			{"name": "bug"}
		]
	}`}
	p := &GitHubProvider{Runner: cli, host: "github.com"}

	labels, err := p.ListPRLabels(context.Background(), "cnlangzi", "nightme", 7)
	if err != nil {
		t.Fatalf("ListPRLabels: %v", err)
	}
	want := []string{"nightme/ready", "nightme/assigned", "bug"}
	if strings.Join(labels, ",") != strings.Join(want, ",") {
		t.Errorf("the labels are %v, want %v", labels, want)
	}

	argv := []string{
		"gh", "pr", "view", "7",
		"--json", "labels",
		"--repo", "cnlangzi/nightme",
	}
	if strings.Join(cli.argv, " ") != strings.Join(argv, " ") {
		t.Errorf("the argv is %v, want %v", cli.argv, argv)
	}
}

// TestGitLabListPRLabels pins the argv and the decoding of the
// label names.
func TestGitLabListPRLabels(t *testing.T) {
	cli := &stubCLIRunner{stdout: `{
		"iid": 7,
		"labels": ["nightme/ready", "nightme/assigned", "bug"]
	}`}
	p := &GitLabProvider{Runner: cli, host: "gitlab.com"}

	labels, err := p.ListPRLabels(context.Background(), "cnlangzi", "nightme", 7)
	if err != nil {
		t.Fatalf("ListPRLabels: %v", err)
	}
	want := []string{"nightme/ready", "nightme/assigned", "bug"}
	if strings.Join(labels, ",") != strings.Join(want, ",") {
		t.Errorf("the labels are %v, want %v", labels, want)
	}

	argv := []string{
		"glab", "api",
		"projects/cnlangzi%2Fnightme/merge_requests/7",
	}
	if strings.Join(cli.argv, " ") != strings.Join(argv, " ") {
		t.Errorf("the argv is %v, want %v", cli.argv, argv)
	}
}

// TestGitHubRemovePRLabel pins the argv of the removal.
func TestGitHubRemovePRLabel(t *testing.T) {
	cli := &stubCLIRunner{}
	p := &GitHubProvider{Runner: cli, host: "github.com"}

	if err := p.RemovePRLabel(context.Background(), "cnlangzi", "nightme", 7, "nightme/ready"); err != nil {
		t.Fatalf("RemovePRLabel: %v", err)
	}
	want := []string{
		"gh", "pr", "edit", "7",
		"--repo", "cnlangzi/nightme",
		"--remove-label", "nightme/ready",
	}
	if strings.Join(cli.argv, " ") != strings.Join(want, " ") {
		t.Errorf("the argv is %v, want %v", cli.argv, want)
	}
}

// TestGitLabRemovePRLabel pins the argv of the removal.
func TestGitLabRemovePRLabel(t *testing.T) {
	cli := &stubCLIRunner{}
	p := &GitLabProvider{Runner: cli, host: "gitlab.com"}

	if err := p.RemovePRLabel(context.Background(), "cnlangzi", "nightme", 7, "nightme/ready"); err != nil {
		t.Fatalf("RemovePRLabel: %v", err)
	}
	want := []string{
		"glab", "mr", "update", "7",
		"--repo", "cnlangzi/nightme",
		"--unlabel", "nightme/ready",
	}
	if strings.Join(cli.argv, " ") != strings.Join(want, " ") {
		t.Errorf("the argv is %v, want %v", cli.argv, want)
	}
}
