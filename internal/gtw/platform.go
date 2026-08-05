package gtw

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// PlatformKind names a supported issue-tracker backend. v1 supports
// GitHub + GitLab via the user's local `gh` / `glab` CLI; other hosts
// (Gitea, Bitbucket, self-hosted) return ErrUnsupportedPlatform.
type PlatformKind string

const (
	PlatformGitHub PlatformKind = "github"
	PlatformGitLab PlatformKind = "gitlab"
)

// Issue is the minimal issue shape we need to render a /gtw fix
// confirmation card. Title is the only required field; the
// remaining fields are best-effort (zero when the platform omits
// them).
type Issue struct {
	ID     int
	Title  string
	Body   string
	State  string // "open" / "closed"
	Labels []string
	URL    string
}

// PlatformClient is the abstract /gtw interface to an issue tracker.
// Production uses GitHubClient / GitLabClient; tests inject fakes.
type PlatformClient interface {
	// Kind returns PlatformGitHub or PlatformGitLab.
	Kind() PlatformKind
	// GetIssue fetches the issue with the given id. Returns
	// ErrIssueNotFound when the platform responds 404.
	GetIssue(ctx context.Context, owner, repo string, id int) (*Issue, error)
	// AddLabel adds `label` to the issue. Idempotent.
	AddLabel(ctx context.Context, owner, repo string, id int, label string) error
	// RemoveLabel removes `label` from the issue. Idempotent.
	RemoveLabel(ctx context.Context, owner, repo string, id int, label string) error
}

// ErrIssueNotFound is returned by GetIssue when the platform
// responds with 404 (or the glab/gh equivalent). Surfaces to the
// user as "issue #N not found in <repo>".
var ErrIssueNotFound = errors.New("gtw: issue not found")

// ErrUnsupportedPlatform is returned by DetectPlatform when the
// remote URL does not match github.com or gitlab.com. The user sees
// "暂不支持的 Git 平台" (F-45 §7.2).
var ErrUnsupportedPlatform = errors.New("gtw: unsupported git platform")

// ParseRepoOwner splits a "<owner>/<repo>" string into its two
// components. The git CLI prints "origin\thttps://github.com/foo/bar"
// or "origin\tgit@github.com:foo/bar.git"; we standardise to
// "<owner>/<repo>" here.
func ParseRepoOwner(remoteURL string) (owner, repo string, err error) {
	remoteURL = strings.TrimSpace(remoteURL)
	if remoteURL == "" {
		return "", "", errors.New("gtw: empty remote URL")
	}
	// Strip protocol.
	remoteURL = strings.TrimPrefix(remoteURL, "https://")
	remoteURL = strings.TrimPrefix(remoteURL, "http://")
	remoteURL = strings.TrimPrefix(remoteURL, "ssh://")
	// git@github.com:foo/bar.git → github.com:foo/bar.git
	remoteURL = strings.TrimPrefix(remoteURL, "git@")
	// Drop ".git" suffix.
	remoteURL = strings.TrimSuffix(remoteURL, ".git")
	// Now we have "github.com/owner/repo" or "github.com:owner/repo"
	// or "gitlab.com/sub/owner/repo" (self-hosted under groups).
	remoteURL = strings.Replace(remoteURL, ":", "/", 1)
	// Drop the host segment.
	parts := strings.Split(remoteURL, "/")
	if len(parts) < 3 {
		return "", "", fmt.Errorf("gtw: cannot parse repo path from %q", remoteURL)
	}
	// parts[0] = host; parts[1..] = owner/repo (possibly deeper
	// for self-hosted group/subgroup paths).
	owner = strings.Join(parts[1:len(parts)-1], "/")
	repo = parts[len(parts)-1]
	if owner == "" || repo == "" {
		return "", "", fmt.Errorf("gtw: empty owner/repo from %q", remoteURL)
	}
	return owner, repo, nil
}

// DetectPlatform picks GitHub or GitLab from a remote URL. Returns
// ErrUnsupportedPlatform for any other host.
func DetectPlatform(remoteURL string) (PlatformKind, error) {
	lower := strings.ToLower(remoteURL)
	switch {
	case strings.Contains(lower, "github.com"):
		return PlatformGitHub, nil
	case strings.Contains(lower, "gitlab"):
		return PlatformGitLab, nil
	default:
		return "", ErrUnsupportedPlatform
	}
}

// NewPlatformClient is the convenience constructor that picks
// GitHubClient or GitLabClient based on the remote URL. Pass the
// output of RemoteOriginURL.
func NewPlatformClient(kind PlatformKind) (PlatformClient, error) {
	switch kind {
	case PlatformGitHub:
		return &GitHubClient{}, nil
	case PlatformGitLab:
		return &GitLabClient{}, nil
	default:
		return nil, ErrUnsupportedPlatform
	}
}

// CLIRunner abstracts gh / glab. Same pattern as GitRunner: tests
// inject a fake; production uses exec.CommandContext.
type CLIRunner interface {
	Run(ctx context.Context, name string, args ...string) (stdout, stderr string, err error)
}

// ExecCLIRunner is the production CLIRunner.
type ExecCLIRunner struct{}

// Run implements CLIRunner.
func (ExecCLIRunner) Run(ctx context.Context, name string, args ...string) (string, string, error) {
	if name == "" {
		return "", "", errors.New("gtw: cli: empty command name")
	}
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return strings.TrimRight(stdout.String(), "\n"),
		strings.TrimRight(stderr.String(), "\n"),
		err
}

// GitHubClient wraps the `gh` CLI. Auth is delegated to `gh auth
// status` — the user is expected to have run `gh auth login` once.
// nightme never persists its own token (F-45 §4.1).
type GitHubClient struct {
	Runner CLIRunner
}

// Kind implements PlatformClient.
func (c *GitHubClient) Kind() PlatformKind { return PlatformGitHub }

func (c *GitHubClient) runner() CLIRunner {
	if c.Runner != nil {
		return c.Runner
	}
	return ExecCLIRunner{}
}

// GetIssue runs `gh issue view <id> --repo <owner>/<repo> --json ...`
// and decodes the result. The --json flag was added in gh 2.8; if
// the user's gh is older the command fails with a clear message.
func (c *GitHubClient) GetIssue(ctx context.Context, owner, repo string, id int) (*Issue, error) {
	args := []string{
		"issue", "view", fmt.Sprintf("%d", id),
		"--repo", owner + "/" + repo,
		"--json", "number,title,body,state,labels,url",
	}
	stdout, stderr, err := c.runner().Run(ctx, "gh", args...)
	if err != nil {
		if strings.Contains(stderr, "Could not resolve to an Issue") ||
			strings.Contains(stderr, "404 Not Found") {
			return nil, fmt.Errorf("%w: %s", ErrIssueNotFound, stderr)
		}
		return nil, fmt.Errorf("gh issue view: %v: %s", err, stderr)
	}
	var raw struct {
		Number int      `json:"number"`
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		State  string   `json:"state"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		return nil, fmt.Errorf("gh issue view: decode json: %w", err)
	}
	labels := make([]string, 0, len(raw.Labels))
	for _, l := range raw.Labels {
		labels = append(labels, l.Name)
	}
	return &Issue{
		ID:     raw.Number,
		Title:  raw.Title,
		Body:   raw.Body,
		State:  strings.ToLower(raw.State),
		Labels: labels,
		URL:    raw.URL,
	}, nil
}

// AddLabel runs `gh issue edit <id> --add-label <label> --repo ...`.
func (c *GitHubClient) AddLabel(ctx context.Context, owner, repo string, id int, label string) error {
	_, stderr, err := c.runner().Run(ctx, "gh",
		"issue", "edit", fmt.Sprintf("%d", id),
		"--repo", owner+"/"+repo,
		"--add-label", label,
	)
	if err != nil {
		return fmt.Errorf("gh issue edit --add-label: %v: %s", err, stderr)
	}
	return nil
}

// RemoveLabel runs `gh issue edit <id> --remove-label <label> --repo ...`.
func (c *GitHubClient) RemoveLabel(ctx context.Context, owner, repo string, id int, label string) error {
	_, stderr, err := c.runner().Run(ctx, "gh",
		"issue", "edit", fmt.Sprintf("%d", id),
		"--repo", owner+"/"+repo,
		"--remove-label", label,
	)
	if err != nil {
		return fmt.Errorf("gh issue edit --remove-label: %v: %s", err, stderr)
	}
	return nil
}

// GitLabClient wraps the `glab` CLI. Same auth delegation as gh.
type GitLabClient struct {
	Runner CLIRunner
}

// Kind implements PlatformClient.
func (c *GitLabClient) Kind() PlatformKind { return PlatformGitLab }

func (c *GitLabClient) runner() CLIRunner {
	if c.Runner != nil {
		return c.Runner
	}
	return ExecCLIRunner{}
}

// GetIssue runs `glab issue view <id> --output json`. Older glab
// versions emit a different shape; we accept either.
func (c *GitLabClient) GetIssue(ctx context.Context, owner, repo string, id int) (*Issue, error) {
	// glab uses --repo for the "owner/repo" form (since 1.11). We
	// pass it explicitly to avoid relying on the user's local
	// `glab repo clone` state.
	args := []string{
		"issue", "view", fmt.Sprintf("%d", id),
		"--repo", owner + "/" + repo,
		"--output", "json",
	}
	stdout, stderr, err := c.runner().Run(ctx, "glab", args...)
	if err != nil {
		if strings.Contains(stderr, "404 Not Found") ||
			strings.Contains(stderr, "not found") {
			return nil, fmt.Errorf("%w: %s", ErrIssueNotFound, stderr)
		}
		return nil, fmt.Errorf("glab issue view: %v: %s", err, stderr)
	}
	var raw struct {
		IID     int    `json:"iid"`
		Title   string `json:"title"`
		Body    string `json:"description"`
		State   string `json:"state"`
		Labels  []string `json:"labels"`
		WebURL  string `json:"web_url"`
	}
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		return nil, fmt.Errorf("glab issue view: decode json: %w", err)
	}
	return &Issue{
		ID:     raw.IID,
		Title:  raw.Title,
		Body:   raw.Body,
		State:  strings.ToLower(raw.State),
		Labels: raw.Labels,
		URL:    raw.WebURL,
	}, nil
}

// AddLabel runs `glab issue update <id> --label <label> --repo ...`.
func (c *GitLabClient) AddLabel(ctx context.Context, owner, repo string, id int, label string) error {
	_, stderr, err := c.runner().Run(ctx, "glab",
		"issue", "update", fmt.Sprintf("%d", id),
		"--repo", owner+"/"+repo,
		"--label", label,
	)
	if err != nil {
		return fmt.Errorf("glab issue update --label: %v: %s", err, stderr)
	}
	return nil
}

// RemoveLabel runs `glab issue update <id> --unlabel <label> --repo ...`.
func (c *GitLabClient) RemoveLabel(ctx context.Context, owner, repo string, id int, label string) error {
	_, stderr, err := c.runner().Run(ctx, "glab",
		"issue", "update", fmt.Sprintf("%d", id),
		"--repo", owner+"/"+repo,
		"--unlabel", label,
	)
	if err != nil {
		return fmt.Errorf("glab issue update --unlabel: %v: %s", err, stderr)
	}
	return nil
}
