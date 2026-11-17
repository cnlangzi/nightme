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
// so a test can drive a sequence of calls.
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

// TestGitHubAddBlockedBy_CLIArgs pins the two command lines of the
// dependency write: the blocker's global id is read first, then the
// relation is posted with it.
func TestGitHubAddBlockedBy_CLIArgs(t *testing.T) {
	cli := &seqCLIRunner{responses: []seqResponse{
		{stdout: `{"number": 42, "id": 4200, "title": "feat: prerequisite"}`},
		{stdout: `{}`},
	}}
	p := &GitHubProvider{Runner: cli, host: "github.com"}

	if err := p.AddBlockedBy(context.Background(), "cnlangzi", "nightme", 43, 42); err != nil {
		t.Fatalf("AddBlockedBy: %v", err)
	}
	if len(cli.calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(cli.calls))
	}
	wantLookup := "gh api repos/cnlangzi/nightme/issues/42"
	if got := strings.Join(cli.calls[0], " "); got != wantLookup {
		t.Errorf("lookup argv =\n%q\nwant\n%q", got, wantLookup)
	}
	wantPost := "gh api --method POST repos/cnlangzi/nightme/issues/43/dependencies/blocked_by -F issue_id=4200"
	if got := strings.Join(cli.calls[1], " "); got != wantPost {
		t.Errorf("post argv =\n%q\nwant\n%q", got, wantPost)
	}
}

// TestGitHubAddBlockedBy_Unsupported pins that a repository
// without the dependencies feature is reported, not silently
// dropped.
func TestGitHubAddBlockedBy_Unsupported(t *testing.T) {
	cli := &seqCLIRunner{responses: []seqResponse{
		{stdout: `{"number": 42, "id": 4200}`},
		{stderr: "gh: Not Found (HTTP 404)", err: errors.New("exit status 1")},
	}}
	p := &GitHubProvider{Runner: cli, host: "github.com"}

	err := p.AddBlockedBy(context.Background(), "cnlangzi", "nightme", 43, 42)
	if !errors.Is(err, ErrRelationsUnsupported) {
		t.Fatalf("err = %v, want ErrRelationsUnsupported", err)
	}
}

// TestGitHubAddBlockedBy_OtherError pins that a failure for
// another reason keeps the platform's own message and is not
// downgraded to "relations are not supported".
func TestGitHubAddBlockedBy_OtherError(t *testing.T) {
	cli := &seqCLIRunner{responses: []seqResponse{
		{stdout: `{"number": 42, "id": 4200}`},
		{stderr: "gh: Bad credentials (HTTP 401)", err: errors.New("exit status 1")},
	}}
	p := &GitHubProvider{Runner: cli, host: "github.com"}

	err := p.AddBlockedBy(context.Background(), "cnlangzi", "nightme", 43, 42)
	if err == nil {
		t.Fatal("err = nil, want the failure to surface")
	}
	if errors.Is(err, ErrRelationsUnsupported) {
		t.Errorf("err = %v, want it NOT to be ErrRelationsUnsupported", err)
	}
	if !strings.Contains(err.Error(), "Bad credentials") {
		t.Errorf("err = %v, want the platform's message", err)
	}
}

// TestGitHubAddBlockedBy_BlockerLookupFails pins that a blocker
// that cannot be read fails the relation without posting one —
// and that it is not read as "this platform has no relations".
func TestGitHubAddBlockedBy_BlockerLookupFails(t *testing.T) {
	cli := &seqCLIRunner{responses: []seqResponse{
		{stderr: "gh: Not Found (HTTP 404)", err: errors.New("exit status 1")},
	}}
	p := &GitHubProvider{Runner: cli, host: "github.com"}

	err := p.AddBlockedBy(context.Background(), "cnlangzi", "nightme", 43, 42)
	if err == nil {
		t.Fatal("err = nil, want the lookup failure to surface")
	}
	if errors.Is(err, ErrRelationsUnsupported) {
		t.Errorf("err = %v, want it NOT to be ErrRelationsUnsupported", err)
	}
	if len(cli.calls) != 1 {
		t.Errorf("calls = %d, want 1 — no relation is posted", len(cli.calls))
	}
}

// TestGitLabAddBlockedBy_CLIArgs pins the command line of the
// dependency write: the link is recorded from the blocker's side,
// with the dependent as the target.
func TestGitLabAddBlockedBy_CLIArgs(t *testing.T) {
	cli := &seqCLIRunner{responses: []seqResponse{{stdout: `{}`}}}
	p := &GitLabProvider{Runner: cli, host: "gitlab.com"}

	if err := p.AddBlockedBy(context.Background(), "cnlangzi", "nightme", 43, 42); err != nil {
		t.Fatalf("AddBlockedBy: %v", err)
	}
	want := "glab api --method POST projects/cnlangzi%2Fnightme/issues/42/links" +
		" -f target_project_id=cnlangzi/nightme -f target_issue_iid=43 -f link_type=blocks"
	if got := cli.lastArgv(); got != want {
		t.Errorf("argv =\n%q\nwant\n%q", got, want)
	}
}

// TestGitLabAddBlockedBy_Unsupported pins that a platform without
// blocking links — the Premium gate — is reported as unsupported.
func TestGitLabAddBlockedBy_Unsupported(t *testing.T) {
	cli := &seqCLIRunner{responses: []seqResponse{
		{stderr: "403 Forbidden", err: errors.New("exit status 1")},
	}}
	p := &GitLabProvider{Runner: cli, host: "gitlab.com"}

	err := p.AddBlockedBy(context.Background(), "cnlangzi", "nightme", 43, 42)
	if !errors.Is(err, ErrRelationsUnsupported) {
		t.Fatalf("err = %v, want ErrRelationsUnsupported", err)
	}
}

// TestGitLabAddBlockedBy_UnsupportedBadRequest pins that a 400
// naming the relation type is the same answer as the license
// gate: this platform has no issue relations.
func TestGitLabAddBlockedBy_UnsupportedBadRequest(t *testing.T) {
	cli := &seqCLIRunner{responses: []seqResponse{
		{stderr: `400 Bad Request
{"message":{"link_type":["is not included in the list"]}}`, err: errors.New("exit status 1")},
	}}
	p := &GitLabProvider{Runner: cli, host: "gitlab.com"}

	err := p.AddBlockedBy(context.Background(), "cnlangzi", "nightme", 43, 42)
	if !errors.Is(err, ErrRelationsUnsupported) {
		t.Fatalf("err = %v, want ErrRelationsUnsupported", err)
	}
}

// TestGitLabAddBlockedBy_OtherError pins that a failure for
// another reason — including a 400 that says nothing about the
// relation type — keeps the platform's own message.
func TestGitLabAddBlockedBy_OtherError(t *testing.T) {
	cases := map[string]string{
		"server error": "500 Internal Server Error",
		"bad request": `400 Bad Request
{"message":{"title":["is too short"]}}`,
		"unauthorized": "401 Unauthorized",
	}
	for name, stderr := range cases {
		t.Run(name, func(t *testing.T) {
			cli := &seqCLIRunner{responses: []seqResponse{
				{stderr: stderr, err: errors.New("exit status 1")},
			}}
			p := &GitLabProvider{Runner: cli, host: "gitlab.com"}

			err := p.AddBlockedBy(context.Background(), "cnlangzi", "nightme", 43, 42)
			if err == nil {
				t.Fatal("err = nil, want the failure to surface")
			}
			if errors.Is(err, ErrRelationsUnsupported) {
				t.Errorf("err = %v, want it NOT to be ErrRelationsUnsupported", err)
			}
			if !strings.Contains(err.Error(), strings.TrimSpace(stderr)) {
				t.Errorf("err = %v, want the platform's message", err)
			}
		})
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
	want := "glab api projects/cnlangzi%2Fnightme/issues?state=opened&per_page=100&order_by=created_at&sort=asc"
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

// TestGitLabListBlockedBy_LinksEndpoint pins the command line
// and the decoding of the issue-links response — GitLab's only
// REST route to an issue's relations.
//
// The response is a flat array of the linked issues, each carrying
// the link type that says how it relates to the issue the links
// were listed for: "is_blocked_by" means the linked issue blocks
// it, "blocks" means it blocks the linked one. The blockers are
// the "is_blocked_by" entries.
func TestGitLabListBlockedBy_LinksEndpoint(t *testing.T) {
	cli := &seqCLIRunner{responses: []seqResponse{{stdout: `[
	  {"iid": 30, "id": 300, "title": "feat: dependent", "state": "opened",
	   "labels": [], "web_url": "https://x/30", "created_at": "2026-01-01T00:00:00Z",
	   "issue_link_id": 7, "link_type": "blocks"},
	  {"iid": 31, "id": 310, "title": "feat: prerequisite", "state": "opened",
	   "labels": [], "web_url": "https://x/31", "created_at": "2026-01-02T00:00:00Z",
	   "issue_link_id": 8, "link_type": "is_blocked_by"},
	  {"iid": 32, "id": 320, "title": "feat: related", "state": "opened",
	   "labels": [], "web_url": "https://x/32", "created_at": "2026-01-03T00:00:00Z",
	   "issue_link_id": 9, "link_type": "relates_to"}
	]`}}}
	p := &GitLabProvider{Runner: cli, host: "gitlab.com"}

	blockers, err := p.ListBlockedBy(context.Background(), "cnlangzi", "nightme", 42)
	if err != nil {
		t.Fatalf("ListBlockedBy: %v", err)
	}
	want := "glab api projects/cnlangzi%2Fnightme/issues/42/links"
	if got := cli.lastArgv(); got != want {
		t.Errorf("argv =\n%q\nwant\n%q", got, want)
	}
	if len(blockers) != 1 || blockers[0].ID != 31 {
		t.Fatalf("blockers = %+v, want one blocker #31", blockers)
	}
}

// TestGitLabListBlockedBy_Unsupported pins that a platform
// without the links endpoint is reported as unsupported.
func TestGitLabListBlockedBy_Unsupported(t *testing.T) {
	cli := &seqCLIRunner{responses: []seqResponse{
		{stderr: "403 Forbidden", err: errors.New("exit status 1")},
	}}
	p := &GitLabProvider{Runner: cli, host: "gitlab.com"}

	_, err := p.ListBlockedBy(context.Background(), "cnlangzi", "nightme", 42)
	if !errors.Is(err, ErrRelationsUnsupported) {
		t.Fatalf("err = %v, want ErrRelationsUnsupported", err)
	}
}

// TestGitLabListBlockedBy_OtherError pins that a failure for
// another reason (network, auth) is not downgraded to "relations
// are not supported".
func TestGitLabListBlockedBy_OtherError(t *testing.T) {
	cli := &seqCLIRunner{responses: []seqResponse{
		{stderr: "500 Internal Server Error", err: errors.New("exit status 1")},
	}}
	p := &GitLabProvider{Runner: cli, host: "gitlab.com"}

	_, err := p.ListBlockedBy(context.Background(), "cnlangzi", "nightme", 42)
	if err == nil {
		t.Fatal("err = nil, want the failure to surface")
	}
	if errors.Is(err, ErrRelationsUnsupported) {
		t.Errorf("err = %v, want it NOT to be ErrRelationsUnsupported", err)
	}
}

// TestGitHubListBlockedBy_KeepsPullRequests pins that a pull
// request in the dependency graph stays a blocker: dropping it
// would read as "no blocker" for a prerequisite that has not
// landed.
func TestGitHubListBlockedBy_KeepsPullRequests(t *testing.T) {
	cli := &seqCLIRunner{responses: []seqResponse{{stdout: `[
	  {"number": 30, "title": "pr: a prerequisite pull request", "state": "open",
	   "labels": [], "created_at": "2026-01-01T00:00:00Z",
	   "pull_request": {"url": "https://x/pull/30"}}
	]`}}}
	p := &GitHubProvider{Runner: cli, host: "github.com"}

	blockers, err := p.ListBlockedBy(context.Background(), "cnlangzi", "nightme", 42)
	if err != nil {
		t.Fatalf("ListBlockedBy: %v", err)
	}
	if len(blockers) != 1 || blockers[0].ID != 30 {
		t.Fatalf("blockers = %+v, want the pull request kept as a blocker", blockers)
	}
}

// TestHasHTTPStatus_StatusLine pins that the status line glab
// prints — "400 Bad Request" — reads as a status, while a number
// that merely appears in the message does not.
func TestHasHTTPStatus_StatusLine(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		want   bool
	}{
		{"status line", "400 Bad Request", true},
		{"status line with body", "400 Bad Request\n{\"message\":{\"title\":[\"is too short\"]}}", true},
		{"number before a lowercase word", "42 issues left", false},
		{"number inside a word", "issues/4002", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasHTTPStatus(c.stderr, 400); got != c.want {
				t.Errorf("hasHTTPStatus(%q, 400) = %v, want %v", c.stderr, got, c.want)
			}
		})
	}
}

// TestHasHTTPStatus_IgnoresUnrelatedDigits pins that the status is
// read as a status, not as any number in the text: a request URL
// carrying the issue id must not turn a 500 into "not found".
func TestHasHTTPStatus_IgnoresUnrelatedDigits(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		want   bool
	}{
		{"gh not found", "gh: Not Found (HTTP 404)", true},
		{"gitlab status text", "404 Not Found", true},
		{"gitlab error body", `{"message":"404 Project Not Found"}`, true},
		{"gitlab request log", "GET https://gitlab.example.com/api/v4/projects/x/issues/42/links: 404 Not Found", true},
		{"issue id in the url", "GET https://gitlab.example.com/api/v4/projects/x/issues/4042/links: 500 Internal Server Error", false},
		{"issue id in the url, 403", "GET https://gitlab.example.com/api/v4/projects/x/issues/4031/links: 401 Unauthorized", false},
		{"unrelated text", "something went wrong", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasHTTPStatus(c.stderr, 404); got != c.want {
				t.Errorf("hasHTTPStatus(%q, 404) = %v, want %v", c.stderr, got, c.want)
			}
		})
	}
}
