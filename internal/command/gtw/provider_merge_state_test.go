package gtw

// Tests for the provider layer behind `/gtw pr view`: the two
// platforms' merge words, the platform-neutral verdict they map to,
// and the CLI calls that read them.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestGitHubMergeVerdict pins the GitHub mapping table. Every row of
// it has a case here.
//
// UNSTABLE is mergeable, not blocked: GitHub's UNSTABLE means "a
// check did not pass but does not gate the merge".
func TestGitHubMergeVerdict(t *testing.T) {
	cases := []struct {
		mergeable string
		state     string
		want      PRMergeVerdict
	}{
		{"MERGEABLE", "CLEAN", PRMergeMergeable},
		{"MERGEABLE", "UNSTABLE", PRMergeMergeable},
		{"MERGEABLE", "", PRMergeMergeable},
		{"CONFLICTING", "DIRTY", PRMergeConflicting},
		{"MERGEABLE", "DIRTY", PRMergeConflicting},
		{"UNKNOWN", "UNKNOWN", PRMergeUnknown},
		{"MERGEABLE", "UNKNOWN", PRMergeUnknown},
		{"MERGEABLE", "BEHIND", PRMergeBehind},
		{"MERGEABLE", "BLOCKED", PRMergeBlocked},
		{"MERGEABLE", "DRAFT", PRMergeBlocked},
		{"MERGEABLE", "HAS_HOOKS", PRMergeBlocked},
		{"MERGEABLE", "SOMETHING_NEW", PRMergeBlocked},
	}
	for _, tc := range cases {
		got := ghMergeVerdict(tc.mergeable, tc.state)
		if got != tc.want {
			t.Errorf("ghMergeVerdict(%q, %q) = %q, want %q",
				tc.mergeable, tc.state, got, tc.want)
		}
	}
}

// TestGitLabMergeVerdict pins the GitLab mapping table: the
// detailed status, the legacy fallback, and the words that block
// the merge.
func TestGitLabMergeVerdict(t *testing.T) {
	cases := []struct {
		detailed string
		legacy   string
		want     PRMergeVerdict
	}{
		// The detailed status wins over the legacy one.
		{"conflicts", "can_be_merged", PRMergeConflicting},
		{"checking", "can_be_merged", PRMergeUnknown},
		{"unchecked", "can_be_merged", PRMergeUnknown},
		{"need_rebase", "can_be_merged", PRMergeBehind},
		{"diverged_commits", "can_be_merged", PRMergeBehind},
		{"mergeable", "cannot_be_merged", PRMergeMergeable},
		{"not_approved", "", PRMergeBlocked},
		{"ci_must_pass", "", PRMergeBlocked},
		{"draft_status", "", PRMergeBlocked},
		{"something_new", "", PRMergeBlocked},
		// No detailed status: the legacy word decides.
		{"", "cannot_be_merged", PRMergeConflicting},
		{"", "checking", PRMergeUnknown},
		{"", "unchecked", PRMergeUnknown},
		{"", "cannot_be_merged_recheck", PRMergeUnknown},
		{"", "can_be_merged", PRMergeMergeable},
		{"", "", PRMergeBlocked},
	}
	for _, tc := range cases {
		got := gitlabMergeVerdict(tc.detailed, tc.legacy)
		if got != tc.want {
			t.Errorf("gitlabMergeVerdict(%q, %q) = %q, want %q",
				tc.detailed, tc.legacy, got, tc.want)
		}
	}
}

// TestNormalizePRState pins the cross-platform state vocabulary:
// GitLab says "opened" where GitHub says "open".
func TestNormalizePRState(t *testing.T) {
	cases := map[string]string{
		"OPEN":    "open",
		"open":    "open",
		"opened":  "open",
		"OPENED":  "open",
		"CLOSED":  "closed",
		"MERGED":  "merged",
		"unknown": "unknown",
	}
	for in, want := range cases {
		if got := normalizePRState(in); got != want {
			t.Errorf("normalizePRState(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestGitHubGetPRMergeState_ReadsTheJSON pins the argv and the
// decoding of `gh pr view --json`.
func TestGitHubGetPRMergeState_ReadsTheJSON(t *testing.T) {
	cli := &stubCLIRunner{stdout: `{
		"number": 123,
		"url": "https://github.com/cnlangzi/nightme/pull/123",
		"state": "OPEN",
		"mergeable": "CONFLICTING",
		"mergeStateStatus": "DIRTY",
		"baseRefName": "main",
		"headRefName": "fix-42"
	}`}
	p := &GitHubProvider{Runner: cli, host: "github.com"}

	state, err := p.GetPRMergeState(context.Background(), "cnlangzi", "nightme", 123)
	if err != nil {
		t.Fatalf("GetPRMergeState: %v", err)
	}
	if state.Number != 123 {
		t.Errorf("the number is %d, want 123", state.Number)
	}
	if state.URL != "https://github.com/cnlangzi/nightme/pull/123" {
		t.Errorf("the url is %q", state.URL)
	}
	if state.Head != "fix-42" || state.Base != "main" {
		t.Errorf("the branches are %q → %q, want fix-42 → main", state.Head, state.Base)
	}
	if state.State != "open" {
		t.Errorf("the state is %q, want open", state.State)
	}
	if state.Verdict != PRMergeConflicting {
		t.Errorf("the verdict is %q, want %q", state.Verdict, PRMergeConflicting)
	}
	if state.Raw != "DIRTY" {
		t.Errorf("the raw word is %q, want DIRTY", state.Raw)
	}

	// The argv: gh pr view <number> --json ... --repo owner/repo.
	want := []string{
		"gh", "pr", "view", "123",
		"--json", "number,url,state,mergeable,mergeStateStatus,baseRefName,headRefName",
		"--repo", "cnlangzi/nightme",
	}
	if strings.Join(cli.argv, " ") != strings.Join(want, " ") {
		t.Errorf("the argv is %v, want %v", cli.argv, want)
	}
}

// TestGitHubGetPRMergeState_NotFound: gh says the pull request does
// not exist → ErrPRNotFound.
func TestGitHubGetPRMergeState_NotFound(t *testing.T) {
	cli := &stubCLIRunner{
		stderr: "GraphQL: Could not resolve to a PullRequest with the number of 999.",
		err:    errors.New("exit status 1"),
	}
	p := &GitHubProvider{Runner: cli, host: "github.com"}

	_, err := p.GetPRMergeState(context.Background(), "cnlangzi", "nightme", 999)
	if !errors.Is(err, ErrPRNotFound) {
		t.Fatalf("the error is %v, want ErrPRNotFound", err)
	}
}

// TestGitHubGetPRMergeState_CLIFailure: a failure that is not "no
// such pull request" stays a plain error.
func TestGitHubGetPRMergeState_CLIFailure(t *testing.T) {
	cli := &stubCLIRunner{
		stderr: "gh: To use GitHub CLI in a GitHub Actions workflow, set the GH_TOKEN environment variable.",
		err:    errors.New("exit status 1"),
	}
	p := &GitHubProvider{Runner: cli, host: "github.com"}

	_, err := p.GetPRMergeState(context.Background(), "cnlangzi", "nightme", 1)
	if err == nil {
		t.Fatal("the call succeeded, want an error")
	}
	if errors.Is(err, ErrPRNotFound) {
		t.Fatalf("the error is %v, want a plain provider error", err)
	}
	if !strings.Contains(err.Error(), "GH_TOKEN") {
		t.Errorf("the error is %v, want the CLI's own stderr", err)
	}
}

// TestGitLabGetPRMergeState_ReadsTheJSON pins the argv and the
// decoding of the merge request endpoint.
//
// The state is "opened" here — GitLab's word for what GitHub calls
// "open" — and it must come back as "open".
func TestGitLabGetPRMergeState_ReadsTheJSON(t *testing.T) {
	cli := &stubCLIRunner{stdout: `{
		"iid": 7,
		"web_url": "https://gitlab.com/cnlangzi/nightme/-/merge_requests/7",
		"state": "opened",
		"source_branch": "fix-42",
		"target_branch": "main",
		"merge_status": "can_be_merged",
		"detailed_merge_status": "conflicts"
	}`}
	p := &GitLabProvider{Runner: cli, host: "gitlab.com"}

	state, err := p.GetPRMergeState(context.Background(), "cnlangzi", "nightme", 7)
	if err != nil {
		t.Fatalf("GetPRMergeState: %v", err)
	}
	if state.Number != 7 {
		t.Errorf("the number is %d, want 7", state.Number)
	}
	if state.Head != "fix-42" || state.Base != "main" {
		t.Errorf("the branches are %q → %q, want fix-42 → main", state.Head, state.Base)
	}
	if state.State != "open" {
		t.Errorf("the state is %q, want open", state.State)
	}
	if state.Verdict != PRMergeConflicting {
		t.Errorf("the verdict is %q, want %q", state.Verdict, PRMergeConflicting)
	}
	if state.Raw != "conflicts" {
		t.Errorf("the raw word is %q, want conflicts", state.Raw)
	}

	// The argv: glab api projects/<owner%2Frepo>/merge_requests/<iid>.
	want := []string{
		"glab", "api", "projects/cnlangzi%2Fnightme/merge_requests/7",
	}
	if strings.Join(cli.argv, " ") != strings.Join(want, " ") {
		t.Errorf("the argv is %v, want %v", cli.argv, want)
	}
}

// TestGitLabGetPRMergeState_LegacyFallback: an instance that
// predates the detailed status reports only the legacy word.
func TestGitLabGetPRMergeState_LegacyFallback(t *testing.T) {
	cli := &stubCLIRunner{stdout: `{
		"iid": 7,
		"web_url": "https://gitlab.com/cnlangzi/nightme/-/merge_requests/7",
		"state": "opened",
		"target_branch": "main",
		"merge_status": "cannot_be_merged"
	}`}
	p := &GitLabProvider{Runner: cli, host: "gitlab.com"}

	state, err := p.GetPRMergeState(context.Background(), "cnlangzi", "nightme", 7)
	if err != nil {
		t.Fatalf("GetPRMergeState: %v", err)
	}
	if state.Verdict != PRMergeConflicting {
		t.Errorf("the verdict is %q, want %q", state.Verdict, PRMergeConflicting)
	}
	if state.Raw != "cannot_be_merged" {
		t.Errorf("the raw word is %q, want cannot_be_merged", state.Raw)
	}
}

// TestGitLabGetPRMergeState_NotFound: GitLab answers 404 →
// ErrPRNotFound.
func TestGitLabGetPRMergeState_NotFound(t *testing.T) {
	cli := &stubCLIRunner{
		stderr: `{"message":"404 Merge Request Not Found"}`,
		err:    errors.New("exit status 1"),
	}
	p := &GitLabProvider{Runner: cli, host: "gitlab.com"}

	_, err := p.GetPRMergeState(context.Background(), "cnlangzi", "nightme", 999)
	if !errors.Is(err, ErrPRNotFound) {
		t.Fatalf("the error is %v, want ErrPRNotFound", err)
	}
}
