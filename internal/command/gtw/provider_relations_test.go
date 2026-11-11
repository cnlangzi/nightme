package gtw

// Tests for the two provider capabilities the automatic pick
// needs: listing the repository's open issues and listing the
// issues that block a given one.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// seqCLIRunner answers each call with the next canned response,
// so a test can drive a fallback chain (blocked_by → links).
type seqCLIRunner struct {
	responses []seqResponse
	calls     [][]string
}

type seqResponse struct {
	stdout string
	stderr string
	err    error
}

func (s *seqCLIRunner) Run(_ context.Context, name string, args ...string) (string, string, error) {
	s.calls = append(s.calls, append([]string{name}, args...))
	if len(s.calls) > len(s.responses) {
		return "", "", errors.New("stub: no response left")
	}
	r := s.responses[len(s.calls)-1]
	return r.stdout, r.stderr, r.err
}

func (s *seqCLIRunner) lastArgv() string {
	if len(s.calls) == 0 {
		return ""
	}
	return strings.Join(s.calls[len(s.calls)-1], " ")
}

// TestGitHubListIssues_CLIArgsAndDecoding pins the command line
// and the decoding of the issue list: the pull requests GitHub
// mixes into the same endpoint are dropped, and the creation time
// is carried over.
func TestGitHubListIssues_CLIArgsAndDecoding(t *testing.T) {
	cli := &seqCLIRunner{responses: []seqResponse{{stdout: `[
	  {"number": 7, "title": "feat: older", "body": "b", "state": "open",
	   "labels": [{"name": "nightme/assigned"}], "url": "https://x/7",
	   "created_at": "2026-01-02T03:04:05Z"},
	  {"number": 9, "title": "pr: a pull request", "state": "open",
	   "labels": [], "created_at": "2026-01-03T00:00:00Z",
	   "pull_request": {"url": "https://x/pull/9"}}
	]`}}}
	p := &GitHubProvider{Runner: cli, host: "github.com"}

	issues, err := p.ListIssues(context.Background(), "cnlangzi", "nightme")
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	want := "gh api repos/cnlangzi/nightme/issues?state=open&per_page=100&sort=created&direction=asc"
	if got := cli.lastArgv(); got != want {
		t.Errorf("argv =\n%q\nwant\n%q", got, want)
	}
	if len(issues) != 1 {
		t.Fatalf("issues = %d, want 1 (the pull request entry is dropped)", len(issues))
	}
	if issues[0].ID != 7 {
		t.Errorf("issue id = %d, want 7", issues[0].ID)
	}
	if issues[0].Labels[0] != LabelAssigned {
		t.Errorf("labels = %v, want [%s]", issues[0].Labels, LabelAssigned)
	}
	wantAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if !issues[0].CreatedAt.Equal(wantAt) {
		t.Errorf("created_at = %s, want %s", issues[0].CreatedAt, wantAt)
	}
}

// TestGitHubListBlockedBy_CLIArgs pins the command line of the
// dependency lookup.
func TestGitHubListBlockedBy_CLIArgs(t *testing.T) {
	cli := &seqCLIRunner{responses: []seqResponse{{stdout: `[
	  {"number": 30, "title": "feat: prerequisite", "state": "closed",
	   "labels": [], "created_at": "2026-01-01T00:00:00Z"}
	]`}}}
	p := &GitHubProvider{Runner: cli, host: "github.com"}

	blockers, err := p.ListBlockedBy(context.Background(), "cnlangzi", "nightme", 42)
	if err != nil {
		t.Fatalf("ListBlockedBy: %v", err)
	}
	want := "gh api repos/cnlangzi/nightme/issues/42/dependencies/blocked_by"
	if got := cli.lastArgv(); got != want {
		t.Errorf("argv =\n%q\nwant\n%q", got, want)
	}
	if len(blockers) != 1 || blockers[0].ID != 30 {
		t.Fatalf("blockers = %+v, want one blocker #30", blockers)
	}
}

// TestGitHubListBlockedBy_Unsupported pins that a repository
// without the dependencies feature is reported, not silently read
// as "no prerequisites".
func TestGitHubListBlockedBy_Unsupported(t *testing.T) {
	cli := &seqCLIRunner{responses: []seqResponse{{
		stderr: "gh: Not Found (HTTP 404)",
		err:    errors.New("exit status 1"),
	}}}
	p := &GitHubProvider{Runner: cli, host: "github.com"}

	_, err := p.ListBlockedBy(context.Background(), "cnlangzi", "nightme", 42)
	if !errors.Is(err, ErrRelationsUnsupported) {
		t.Fatalf("err = %v, want ErrRelationsUnsupported", err)
	}
}

// TestGitHubListBlockedBy_OtherError pins that a failure for
// another reason (network, auth) is not downgraded to "relations
// are not supported".
func TestGitHubListBlockedBy_OtherError(t *testing.T) {
	cli := &seqCLIRunner{responses: []seqResponse{{
		stderr: "gh: Bad credentials (HTTP 401)",
		err:    errors.New("exit status 1"),
	}}}
	p := &GitHubProvider{Runner: cli, host: "github.com"}

	_, err := p.ListBlockedBy(context.Background(), "cnlangzi", "nightme", 42)
	if err == nil {
		t.Fatal("err = nil, want the failure to surface")
	}
	if errors.Is(err, ErrRelationsUnsupported) {
		t.Errorf("err = %v, want it NOT to be ErrRelationsUnsupported", err)
	}
}

// TestGitLabListIssues_CLIArgsAndDecoding pins the command line
// and the decoding of the GitLab issue list.
func TestGitLabListIssues_CLIArgsAndDecoding(t *testing.T) {
	cli := &seqCLIRunner{responses: []seqResponse{{stdout: `[
	  {"iid": 7, "title": "feat: older", "description": "b", "state": "opened",
	   "labels": ["nightme/assigned"], "web_url": "https://x/7",
	   "created_at": "2026-01-02T03:04:05Z"}
	]`}}}
	p := &GitLabProvider{Runner: cli, host: "gitlab.com"}

	issues, err := p.ListIssues(context.Background(), "cnlangzi", "nightme")
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	want := "glab api projects/cnlangzi%2Fnightme/issues?state=opened&per_page=100&order_by=created&sort=asc"
	if got := cli.lastArgv(); got != want {
		t.Errorf("argv =\n%q\nwant\n%q", got, want)
	}
	if len(issues) != 1 || issues[0].ID != 7 {
		t.Fatalf("issues = %+v, want one issue #7", issues)
	}
	if issues[0].Labels[0] != LabelAssigned {
		t.Errorf("labels = %v, want [%s]", issues[0].Labels, LabelAssigned)
	}
	wantAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if !issues[0].CreatedAt.Equal(wantAt) {
		t.Errorf("created_at = %s, want %s", issues[0].CreatedAt, wantAt)
	}
}

// TestGitLabListBlockedBy_BlockedByEndpoint pins the command line
// and the decoding of the dedicated endpoint.
func TestGitLabListBlockedBy_BlockedByEndpoint(t *testing.T) {
	cli := &seqCLIRunner{responses: []seqResponse{{stdout: `[
	  {"iid": 30, "title": "feat: prerequisite", "state": "opened",
	   "labels": [], "web_url": "https://x/30", "created_at": "2026-01-01T00:00:00Z"}
	]`}}}
	p := &GitLabProvider{Runner: cli, host: "gitlab.com"}

	blockers, err := p.ListBlockedBy(context.Background(), "cnlangzi", "nightme", 42)
	if err != nil {
		t.Fatalf("ListBlockedBy: %v", err)
	}
	want := "glab api projects/cnlangzi%2Fnightme/issues/42/blocked_by"
	if got := cli.lastArgv(); got != want {
		t.Errorf("argv =\n%q\nwant\n%q", got, want)
	}
	if len(blockers) != 1 || blockers[0].ID != 30 {
		t.Fatalf("blockers = %+v, want one blocker #30", blockers)
	}
}

// TestGitLabListBlockedBy_LinksFallback pins the fallback to the
// issue-links endpoint: a link means "the source is blocked by
// the target", so the blockers of the issue are the targets of
// the links whose source is the issue.
func TestGitLabListBlockedBy_LinksFallback(t *testing.T) {
	cli := &seqCLIRunner{responses: []seqResponse{
		{stderr: "403 Forbidden", err: errors.New("exit status 1")},
		{stdout: `[
		  {"source_issue": {"iid": 42, "title": "feat: dependent"},
		   "target_issue": {"iid": 30, "title": "feat: prerequisite", "state": "opened"},
		   "link_type": "is_blocked_by"},
		  {"source_issue": {"iid": 41, "title": "feat: other"},
		   "target_issue": {"iid": 31, "title": "feat: unrelated"},
		   "link_type": "is_blocked_by"}
		]`},
	}}
	p := &GitLabProvider{Runner: cli, host: "gitlab.com"}

	blockers, err := p.ListBlockedBy(context.Background(), "cnlangzi", "nightme", 42)
	if err != nil {
		t.Fatalf("ListBlockedBy: %v", err)
	}
	if len(cli.calls) != 2 {
		t.Fatalf("calls = %d, want 2 (the dedicated endpoint, then the links fallback)", len(cli.calls))
	}
	want := "glab api projects/cnlangzi%2Fnightme/issues/42/links?link_type=is_blocked_by"
	if got := cli.lastArgv(); got != want {
		t.Errorf("argv =\n%q\nwant\n%q", got, want)
	}
	if len(blockers) != 1 || blockers[0].ID != 30 {
		t.Fatalf("blockers = %+v, want one blocker #30 (the link whose source is #42)", blockers)
	}
}

// TestGitLabListBlockedBy_Unsupported pins that a platform
// without blocking relations — and without the links fallback —
// is reported as unsupported.
func TestGitLabListBlockedBy_Unsupported(t *testing.T) {
	cli := &seqCLIRunner{responses: []seqResponse{
		{stderr: "403 Forbidden", err: errors.New("exit status 1")},
		{stderr: "404 Not Found", err: errors.New("exit status 1")},
	}}
	p := &GitLabProvider{Runner: cli, host: "gitlab.com"}

	_, err := p.ListBlockedBy(context.Background(), "cnlangzi", "nightme", 42)
	if !errors.Is(err, ErrRelationsUnsupported) {
		t.Fatalf("err = %v, want ErrRelationsUnsupported", err)
	}
}
