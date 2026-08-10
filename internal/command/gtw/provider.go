package gtw

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ProviderKind names a supported git hosting platform. v1 supports
// GitHub + GitLab via the user's local `gh` / `glab` CLI; other hosts
// (Gitea, Bitbucket, self-hosted non-GitHub-Enterprise) return
// ErrUnsupportedProvider. See docs/feat/F-50-git-provider.md §1.1.
type ProviderKind string

const (
	ProviderGitHub ProviderKind = "github"
	ProviderGitLab ProviderKind = "gitlab"
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

	// Attachments are downloadable artifacts attached to the
	// issue. The provider fills this in as best it can:
	//   - GitLab: native attachment_links from the issue API
	//   - GitHub: image URLs parsed from the body markdown
	//     (GitHub issues don't have a first-class "attachments"
	//     API; user-uploaded images inline as `![](url)`).
	// /gtw fix downloads each attachment into the worktree
	// before dispatching the issue to the agent — the agent
	// then sees them as ContentFile blocks alongside the text
	// dispatch.
	Attachments []IssueAttachment
}

// IssueAttachment is one downloadable artifact attached to an
// issue. URL is required (it's where we GET from). Filename
// is best-effort: when the provider can extract a sensible
// filename from the URL or content-disposition header, it
// sets it; when not, downloadAttachments picks a numbered
// fallback. MIMEType is advisory — empty when the provider
// can't determine it.
type IssueAttachment struct {
	URL      string
	Filename string
	MIMEType string
}

// GitProvider is the abstract /gtw interface to a git hosting
// platform's issue tracker. Production has two implementations
// (GitHubProvider / GitLabProvider wrapping the user's local `gh`
// / `glab` CLI); tests inject fakes. Future hosts (Gitea,
// Bitbucket) plug in by satisfying this interface — no caller
// change needed.
//
// See docs/feat/F-50-git-provider.md §1.1 for the full design.
type GitProvider interface {
	// Kind returns ProviderGitHub or ProviderGitLab.
	Kind() ProviderKind

	// Host returns the server host this provider is bound to
	// (e.g. "github.com" / "gitlab.acme.internal"). Set by
	// Detect from the parsed remote URL.
	Host() string

	// Version returns the server-reported version string
	// (e.g. "16.5.0" for GitLab), or "" if the probe failed or
	// the provider has no version endpoint (e.g. github.com
	// and most GitHub Enterprise installations). Cached on
	// the provider instance after Detect probes once.
	Version() string

	// GetIssue fetches the issue with the given id. Returns
	// ErrIssueNotFound when the platform responds 404.
	GetIssue(ctx context.Context, owner, repo string, id int) (*Issue, error)

	// AddLabel adds `label` to the issue. Idempotent.
	AddLabel(ctx context.Context, owner, repo string, id int, label string) error

	// RemoveLabel removes `label` from the issue. Idempotent.
	RemoveLabel(ctx context.Context, owner, repo string, id int, label string) error

	// CreatePR opens a pull request (GitHub) or merge request
	// (GitLab). We keep the method name uniform across
	// providers because the user-facing /gtw pr UX is identical
	// on both platforms — same command, same card, different
	// backing CLI. This mirrors the "issue / MR is the same
	// concept" convention /gtw fix already uses.
	//
	// Returns the PR/MR URL on success. Errors should preserve
	// the underlying CLI's stderr so /gtw pr can echo it to the
	// IM (the underlying gh/glab messages are often the most
	// actionable error for the user, e.g. "head ref doesn't
	// exist" or "401 Unauthorized").
	CreatePR(ctx context.Context, owner, repo, base, head, title, body string) (string, error)
}

// ErrIssueNotFound is returned by GetIssue when the platform
// responds with 404 (or the glab/gh equivalent). Surfaces to the
// user as "issue #N not found in <repo>".
var ErrIssueNotFound = errors.New("gtw: issue not found")

// ErrUnsupportedProvider is returned by Detect when neither the
// URL hint (github.com / gitlab.com substring) nor the API
// endpoint probe (/api/v4/version for GitLab, /api/v3/meta for
// GitHub Enterprise) recognises the host. See F-50 §1.2.2 for the
// full failure semantics.
var ErrUnsupportedProvider = errors.New("gtw: unsupported git provider")

// ErrInvalidRemoteURL is returned by parseRemoteURL when the input
// is empty, has an unrecognised scheme, or otherwise cannot be
// lexed into a host segment. Kept separate from ErrUnsupportedProvider
// so the user-facing message can distinguish "URL is malformed"
// (user error) from "host not on github/gitlab" (no provider
// implementation yet — D3 split).
var ErrInvalidRemoteURL = errors.New("gtw: invalid remote URL")

// ErrPRExists is returned by CreatePR when the platform says a
// pull/merge request already exists for the same head→base.
// GitHub prints "a pull request already exists for this branch"
// in its stderr; GitLab's glab prints "already exists" or "MR
// already exists". The caller (dispatchPR) maps this to a
// friendly "PR already exists" message with the existing URL
// when available.
var ErrPRExists = errors.New("gtw: PR already exists")

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

// parseRemoteHost extracts just the host segment from a remote URL.
// Handles every git URL shape in the wild:
//
//	https://host/path.git                     → host
//	http://host/path.git                      → host
//	ssh://git@host/path.git                   → host  (ssh://)
//	ssh://user@host:port/path.git             → host  (ssh:// with userinfo+port)
//	git@host:path.git                         → host  (scp-style — legacy SSH form)
//	git://host/path.git                       → host  (rare but valid Git protocol)
//	https://user:token@host/path.git          → host  (userinfo stripped; common with gh / glab auth helper)
//	https://host/path.git?ref=main#frag       → host  (query + fragment stripped)
//
// Host extraction is purely lexical — the result is what we feed
// into the Stage B probe (`GET <host>/api/v4/version`). We do NOT
// validate that the host is resolvable; Detect treats probe failures
// as "host doesn't respond" and falls through.
//
// Returns ErrInvalidRemoteURL when the input is empty or unparsable;
// callers (Detect) translate that into a separate user-facing hint
// from "host not supported" (see D3 split).
func parseRemoteHost(remoteURL string) (host string, err error) {
	remoteURL = strings.TrimSpace(remoteURL)
	if remoteURL == "" {
		return "", ErrInvalidRemoteURL
	}
	u := remoteURL

	// 1. Strip protocol prefix (one of: https / http / ssh / git).
	switch {
	case strings.HasPrefix(u, "https://"):
		u = strings.TrimPrefix(u, "https://")
	case strings.HasPrefix(u, "http://"):
		u = strings.TrimPrefix(u, "http://")
	case strings.HasPrefix(u, "ssh://"):
		u = strings.TrimPrefix(u, "ssh://")
	case strings.HasPrefix(u, "git://"):
		u = strings.TrimPrefix(u, "git://")
	case strings.HasPrefix(u, "git@"):
		// scp-style legacy SSH form: "git@host:path". The
		// separator between host and path is ":" (not "/"),
		// so the colon→slash step below would clobber a real
		// port number. Strip the prefix, mark the form, and
		// handle the colon→slash specially below.
		u = strings.TrimPrefix(u, "git@")
		// Fall through to colon-handling.
	default:
		// Unknown protocol / plain hostname. Reject — we
		// don't want to feed "example.com" into Stage B and
		// silently probe it.
		return "", fmt.Errorf("%w: unknown scheme in %q", ErrInvalidRemoteURL, remoteURL)
	}

	// 2. Strip userinfo: "user@" or "user:token@" up to the next "/".
	//    We must do this BEFORE the colon→slash transform, because
	//    user:token contains a colon that would otherwise be clobbered.
	if i := strings.Index(u, "@"); i >= 0 {
		slash := strings.Index(u, "/")
		if slash < 0 || i < slash {
			u = u[i+1:]
		}
	}

	// 3. Strip query / fragment.
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}

	// 4. Strip trailing ".git" suffix.
	u = strings.TrimSuffix(u, ".git")

	// 5. Convert the FIRST ":" to "/" ONLY for scp-style URLs
	//    (`git@host:path` — colon is the host/path separator). For
	//    URL-style (`https://host:port/path` — colon is the port
	//    separator), keep the colon so the port stays attached to
	//    the host.
	//
	//    Heuristic: a scp-style separator is followed by a path
	//    component (no leading "/"); a port separator is followed
	//    by digits + ("/" | end-of-string).
	if i := strings.Index(u, ":"); i >= 0 {
		rest := u[i+1:]
		if isPort(rest) {
			// port colon — keep as-is
		} else {
			u = u[:i] + "/" + rest
		}
	}

	// 6. Split off the host (first segment).
	parts := strings.SplitN(u, "/", 2)
	if len(parts) < 1 || parts[0] == "" {
		return "", fmt.Errorf("%w: cannot parse host from %q", ErrInvalidRemoteURL, remoteURL)
	}
	return parts[0], nil
}

// isPort reports whether s looks like a TCP port spec ("8080",
// "8080/path", "8080?x=y") — the part that follows the host:colon
// in URL-style URLs (https://host:port/...). Used to keep ports
// attached to the host during parseRemoteHost's scp-vs-URL
// disambiguation.
func isPort(s string) bool {
	if s == "" {
		return false
	}
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return false
	}
	if i == len(s) {
		return true
	}
	switch s[i] {
	case '/', '?', '#':
		return true
	}
	return false
}

// redactForDisplay returns a credential-free version of remoteURL
// safe to echo in user-facing IM replies. Strips:
//   - protocol prefix (https / http / ssh / git / git@)
//   - userinfo ("user@" / "user:token@") → replaced with "<redacted>@"
//   - query string / fragment
//   - caps output to 256 runes to bound message size
//
// Returns "" when the input is empty / pure whitespace / fully
// unparseable. Callers should fall back to a generic hint in that
// case (no URL echo) rather than risk leaking partial data.
//
// Test coverage: `TestRedactForDisplay` in gtw_test.go.
func redactForDisplay(remoteURL string) string {
	remoteURL = strings.TrimSpace(remoteURL)
	if remoteURL == "" {
		return ""
	}
	u := remoteURL
	// Strip protocol prefix.
	for _, p := range []string{"https://", "http://", "ssh://", "git://"} {
		if strings.HasPrefix(u, p) {
			u = u[len(p):]
			break
		}
	}
	// Strip scp-style "git@" prefix.
	u = strings.TrimPrefix(u, "git@")
	// Strip userinfo: "@" before any "/". Replace with "<redacted>@".
	if at := strings.Index(u, "@"); at >= 0 {
		slash := strings.Index(u, "/")
		if slash < 0 || at < slash {
			u = "<redacted>@" + u[at+1:]
		}
	}
	// Strip query / fragment.
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	// Cap length to bound reply size (URLs can be arbitrarily long
	// when self-hosted paths nest deeply).
	const maxRunes = 256
	runes := []rune(u)
	if len(runes) > maxRunes {
		u = string(runes[:maxRunes]) + "…"
	}
	return u
}

// Detect identifies the git provider for the given remote URL via
// two-stage detection. See docs/feat/F-50-git-provider.md §1.2 for
// the full design.
//
//	Stage A · URL hint (zero network)
//	  - substring "github.com"  → GitHubProvider{host: <host>}
//	  - substring "gitlab"      → GitLabProvider{host: <host>}
//	  - otherwise                → fall through to Stage B
//
//	Stage B · Live API probe (only when Stage A is ambiguous)
//	  - GET <host>/api/v4/version → GitLabProvider{host, version}
//	                                 (response: {"version": "16.x.x"})
//	  - GET <host>/api/v3/meta    → GitHubProvider{host}
//	                                 (response: instance metadata
//	                                  containing verifiable_password_authentication)
//
// Stage A wins whenever it matches — even if Stage B would also
// recognise the host, because URL hint has zero latency and no
// failure modes (no DNS, no TLS, no 5xx).
//
// `prober` may be nil; production uses ExecHTTPProber{} with a 3s
// default timeout. Tests inject a fake that returns canned JSON.
func Detect(ctx context.Context, remoteURL string, prober HTTPProber, worktree string) (GitProvider, error) {
	if prober == nil {
		prober = &ExecHTTPProber{}
	}
	host, err := parseRemoteHost(remoteURL)
	if err != nil {
		// URL itself is malformed (empty / unknown scheme / no
		// host segment). Don't wrap with ErrUnsupportedProvider
		// — D3 split: this is "URL error", not "provider
		// unsupported". Callers can errors.Is(err, ErrInvalidRemoteURL).
		return nil, err
	}

	// Stage A · URL hint (zero network).
	lower := strings.ToLower(remoteURL)
	switch {
	case strings.Contains(lower, "github.com"):
		return &GitHubProvider{host: host, Worktree: worktree}, nil
	case strings.Contains(lower, "gitlab"):
		return &GitLabProvider{host: host, Worktree: worktree}, nil
	}

	// Stage B · Live API probe (only when Stage A was ambiguous).
	// Order: GitLab first (cheaper, more deterministic), then
	// GitHub. Single-probe failure is not fatal; we move on.
	if body, err := prober.Probe(ctx, host, "/api/v4/version"); err == nil {
		var v struct {
			Version  string `json:"version"`
			Revision string `json:"revision"`
		}
		if json.Unmarshal(body, &v) == nil && (v.Version != "" || v.Revision != "") {
			return &GitLabProvider{host: host, version: v.Version, Worktree: worktree}, nil
		}
	}
	if body, err := prober.Probe(ctx, host, "/api/v3/meta"); err == nil {
		var meta map[string]json.RawMessage
		if json.Unmarshal(body, &meta) == nil {
			// GitHub's /api/v3/meta returns
			// {"verifiable_password_authentication":..., ...}.
			// No top-level "version" but identifiable by content.
			if _, hasGH := meta["verifiable_password_authentication"]; hasGH {
				return &GitHubProvider{host: host, Worktree: worktree}, nil
			}
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrUnsupportedProvider, host)
}

// NewProvider is the kind+host convenience constructor. Production
// callers should prefer Detect, which performs two-stage detection
// and binds host + version in one call. Use NewProvider only when
// the caller has already identified the kind (e.g. tests, or a
// future "force provider" override flag).
func NewProvider(kind ProviderKind, host, worktree string) (GitProvider, error) {
	switch kind {
	case ProviderGitHub:
		return &GitHubProvider{host: host, Worktree: worktree}, nil
	case ProviderGitLab:
		return &GitLabProvider{host: host, Worktree: worktree}, nil
	default:
		return nil, ErrUnsupportedProvider
	}
}

// HTTPProber abstracts the HTTP client used to probe provider
// version / meta endpoints. Same pattern as CLIRunner / GitRunner:
// production uses ExecHTTPProber; tests inject a fake that returns
// canned JSON for fixture-driven unit tests.
type HTTPProber interface {
	// Probe issues a GET <host><path> and returns the response
	// body on 200. Any non-200 status, network error, TLS
	// error, or timeout returns an error — callers (Detect)
	// treat errors as "this endpoint is not it" and move on.
	Probe(ctx context.Context, host, path string) ([]byte, error)
}

// ExecHTTPProber is the production HTTPProber.
type ExecHTTPProber struct {
	// Timeout bounds the entire Probe call. Zero defaults to
	// 3s — long enough for a healthy GitLab / GitHub Enterprise
	// response, short enough that stalled servers don't block
	// the /gtw message path.
	Timeout time.Duration

	// InsecureSkipVerify disables TLS verification. Defaults to
	// false. Self-hosted GitHub Enterprise / GitLab with self-
	// signed certs require this; users set it via the prober at
	// construction (cfg wiring is F-50 §8 backlog).
	InsecureSkipVerify bool
}

// Probe implements HTTPProber.
func (p *ExecHTTPProber) Probe(ctx context.Context, host, path string) ([]byte, error) {
	if p == nil {
		p = &ExecHTTPProber{}
	}
	if p.Timeout == 0 {
		p.Timeout = 3 * time.Second
	}
	url := "https://" + host + path
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "nightme-git-provider-detect/1.0")

	var transport http.RoundTripper = http.DefaultTransport
	if p.InsecureSkipVerify {
		transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}
	cli := &http.Client{
		Timeout:   p.Timeout,
		Transport: transport,
	}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("probe %s: status %d", url, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// CLIRunner abstracts gh / glab. Same pattern as GitRunner: tests
// inject a fake; production delegates to runCmd (see exec.go).
//
// Note: this interface does NOT take a dir argument. Callers that
// have a known worktree path should bind it via a wrapper (see
// resolveProvider) so ExecCLIRunner.Dir is non-empty when invoked
// — otherwise gh will inherit the daemon CWD, which can be stale.
type CLIRunner interface {
	Run(ctx context.Context, name string, args ...string) (stdout, stderr string, err error)
}

// ExecCLIRunner is the production CLIRunner. Dir, when set, is
// passed to runCmd so the spawned gh/glab process runs in that
// directory instead of inheriting the daemon CWD (see exec.go).
type ExecCLIRunner struct {
	Dir string
}

// Run implements CLIRunner by delegating to runCmd. Dir flows
// through transparently — empty means "inherit parent CWD", which
// preserves the pre-refactor behavior for callers that have no
// known workspace yet.
func (e ExecCLIRunner) Run(ctx context.Context, name string, args ...string) (string, string, error) {
	return runCmd(ctx, e.Dir, name, args...)
}

// GitHubProvider wraps the `gh` CLI. Auth is delegated to
// `gh auth status` — the user is expected to have run `gh auth
// login` once. nightme never persists its own token (F-50 §4.3).
//
// host is set by Detect / NewProvider. Version() always returns ""
// because GitHub has no public version endpoint (neither github.com
// nor GitHub Enterprise exposes one via /api/v3/meta).
type GitHubProvider struct {
	Runner CLIRunner

	// Worktree is the working directory spawned gh/glab processes
	// should run in. Set by resolveProvider from c.Worktree so gh
	// doesn't fall back to the daemon CWD (which may have been
	// stale'd since startup — see exec.go for the full rationale).
	// Empty means "inherit parent CWD", matching the pre-fix
	// behavior for callers that haven't yet discovered their
	// workspace.
	Worktree string

	host string
}

// Kind implements GitProvider.
func (c *GitHubProvider) Kind() ProviderKind { return ProviderGitHub }

// Host implements GitProvider.
func (c *GitHubProvider) Host() string { return c.host }

// Version implements GitProvider. Always "" for GitHub.
func (c *GitHubProvider) Version() string { return "" }

func (c *GitHubProvider) runner() CLIRunner {
	if c.Runner != nil {
		return c.Runner
	}
	return ExecCLIRunner{Dir: c.Worktree}
}

// GetIssue runs `gh issue view <id> --repo <owner>/<repo> --json ...`
// and decodes the result. The --json flag was added in gh 2.8; if
// the user's gh is older the command fails with a clear message.
func (c *GitHubProvider) GetIssue(ctx context.Context, owner, repo string, id int) (*Issue, error) {
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
		ID:          raw.Number,
		Title:       raw.Title,
		Body:        raw.Body,
		State:       strings.ToLower(raw.State),
		Labels:      labels,
		URL:         raw.URL,
		Attachments: extractGitHubAttachments(raw.Body),
	}, nil
}

// extractGitHubAttachments pulls image URLs out of the issue
// body markdown. GitHub doesn't have a first-class "issue
// attachments" API — images are inline `![](url)` in the
// body. We only handle github-user-images URLs (the canonical
// upload host) for v1; other hosts work but may include
// broken redirects that the download helper has to handle.
//
// Returns nil when no image URLs are found. Order matches the
// order they appear in the body (top-down), which is what the
// user expects when reviewing the issue.
func extractGitHubAttachments(body string) []IssueAttachment {
	if body == "" {
		return nil
	}
	var out []IssueAttachment
	for i := 0; i < len(body)-4; i++ {
		if i+1 >= len(body) || body[i] != '!' || body[i+1] != '[' {
			continue
		}
		// Find matching ](
		closeBracket := -1
		for j := i + 2; j < len(body); j++ {
			if body[j] == ']' {
				closeBracket = j
				break
			}
		}
		if closeBracket < 0 || closeBracket+1 >= len(body) || body[closeBracket+1] != '(' {
			continue
		}
		// Find matching )
		closeParen := -1
		for j := closeBracket + 2; j < len(body); j++ {
			if body[j] == ')' {
				closeParen = j
				break
			}
			if body[j] == '\n' {
				// URL doesn't span newlines
				break
			}
		}
		if closeParen < 0 {
			continue
		}
		url := body[closeBracket+2 : closeParen]
		// Skip non-http / data: URIs.
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			continue
		}
		// Filename from URL's last path segment.
		fn := url[strings.LastIndex(url, "/")+1:]
		// Strip query string.
		if q := strings.Index(fn, "?"); q >= 0 {
			fn = fn[:q]
		}
		if fn == "" {
			fn = "image"
		}
		out = append(out, IssueAttachment{
			URL:      url,
			Filename: fn,
			MIMEType: "image/png", // best guess; downloadAttachments refines from HTTP response
		})
		// Skip past this image to avoid overlapping matches.
		i = closeParen
	}
	return out
}

// AddLabel runs `gh issue edit <id> --add-label <label> --repo ...`.
func (c *GitHubProvider) AddLabel(ctx context.Context, owner, repo string, id int, label string) error {
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
func (c *GitHubProvider) RemoveLabel(ctx context.Context, owner, repo string, id int, label string) error {
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

// CreatePR runs `gh pr create --base <base> --head <head> --title
// <title> --body <body> --repo <owner>/<repo>`. The head branch must
// already be pushed to origin (dispatchPR enforces this via the
// countUnpushed early-return); if not, gh prints "head ref
// doesn't exist" and we forward that stderr to the user.
//
// gh exits 0 with the PR URL on stdout when the PR is created;
// non-zero + stderr "already exists" → ErrPRExists; any other
// non-zero → wrapped error with stderr preserved.
func (c *GitHubProvider) CreatePR(ctx context.Context, owner, repo, base, head, title, body string) (string, error) {
	args := []string{
		"pr", "create",
		"--base", base,
		"--head", head,
		"--title", title,
		"--body", body,
		"--repo", owner + "/" + repo,
	}
	stdout, stderr, err := c.runner().Run(ctx, "gh", args...)
	if err != nil {
		if strings.Contains(stderr, "already exists") {
			return "", fmt.Errorf("%w: %s", ErrPRExists, strings.TrimSpace(stderr))
		}
		return "", fmt.Errorf("gh pr create: %v: %s", err, strings.TrimSpace(stderr))
	}
	return strings.TrimSpace(stdout), nil
}

// GitLabProvider wraps the `glab` CLI. Same auth delegation as gh.
// host is set by Detect / NewProvider; version is populated from
// /api/v4/version probe when Detect's Stage B succeeds.
type GitLabProvider struct {
	Runner CLIRunner

	// Worktree is the working directory spawned gh/glab processes
	// should run in. See GitHubProvider.Worktree for the rationale.
	Worktree string

	host    string
	version string
}

// Kind implements GitProvider.
func (c *GitLabProvider) Kind() ProviderKind { return ProviderGitLab }

// Host implements GitProvider.
func (c *GitLabProvider) Host() string { return c.host }

// Version implements GitProvider. Returns the server-reported
// version (e.g. "16.5.0") when Detect's Stage B probe succeeded;
// "" when Stage A short-circuited (github.com / gitlab.com) or
// when the probe failed.
func (c *GitLabProvider) Version() string { return c.version }

func (c *GitLabProvider) runner() CLIRunner {
	if c.Runner != nil {
		return c.Runner
	}
	return ExecCLIRunner{Dir: c.Worktree}
}

// GetIssue runs `glab issue view <id> --output json`. Older glab
// versions emit a different shape; we accept either.
func (c *GitLabProvider) GetIssue(ctx context.Context, owner, repo string, id int) (*Issue, error) {
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
		IID    int      `json:"iid"`
		Title  string   `json:"title"`
		Body   string   `json:"description"`
		State  string   `json:"state"`
		Labels []string `json:"labels"`
		WebURL string   `json:"web_url"`
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
func (c *GitLabProvider) AddLabel(ctx context.Context, owner, repo string, id int, label string) error {
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
func (c *GitLabProvider) RemoveLabel(ctx context.Context, owner, repo string, id int, label string) error {
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

// CreatePR runs `glab mr create --target-branch <base> --source-branch
// <head> --title <title> --description <body> --repo <owner>/<repo>`.
// (GitLab's term is "merge request", but the GitProvider method is
// named CreatePR for cross-platform UX consistency — see the
// interface doc.)
//
// glab ≥ 1.11 is required for the --repo flag with "owner/repo"
// form. Older versions use --target-project with the numeric
// project id; we don't try to bridge that gap in v1 (callers
// with older glab get a clear stderr from glab itself).
//
// glab prints the MR URL on stdout on success.
func (c *GitLabProvider) CreatePR(ctx context.Context, owner, repo, base, head, title, body string) (string, error) {
	args := []string{
		"mr", "create",
		"--target-branch", base,
		"--source-branch", head,
		"--title", title,
		"--description", body,
		"--repo", owner + "/" + repo,
	}
	stdout, stderr, err := c.runner().Run(ctx, "glab", args...)
	if err != nil {
		if strings.Contains(stderr, "already exists") {
			return "", fmt.Errorf("%w: %s", ErrPRExists, strings.TrimSpace(stderr))
		}
		return "", fmt.Errorf("glab mr create: %v: %s", err, strings.TrimSpace(stderr))
	}
	return strings.TrimSpace(stdout), nil
}
