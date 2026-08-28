package gtw

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/cnlangzi/nightme/internal/messages"
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
	// issue. Each provider populates this from its private
	// attachmentsFromBody method inside GetIssue — per-provider
	// strategy lives on the provider type, not as a free
	// function next to it:
	//   - GitHub: image (`![](url)`) and plain link (`[](url)`)
	//     URLs parsed from the body. GitHub has no first-class
	//     "attachments" API; user uploads inline in the body.
	//   - GitLab: same body parser v1. Future migration to
	//     native `glab api … attachment_links` is a TODO in
	//     (*GitLabProvider).attachmentsFromBody.
	//
	// /gtw fix downloads each attachment into the worktree
	// before dispatching the issue to the agent — the agent
	// then sees them as ContentFile blocks alongside the text
	// dispatch. Empty slice / nil means "no attachment URLs
	// found in the body".
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

// PR (alias for messages.PR) is the abstract cross-platform
// handle for a single Pull Request / Merge Request. See
// internal/messages/footer.go for full field semantics.
//
// The canonical definition lives in internal/messages so the
// wire types package does not need to import the gtw package
// (avoids a gtw → messages → chatsession → gtw cycle). Existing
// gtw callers keep working via this alias.
type PR = messages.PR

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

	// AddIssueLabel attaches `label` to the issue. Requires the label
	// to already exist in the repository's label catalog — callers
	// uncertain about catalog state must call CreateLabel first.
	// Idempotent: re-adding an already-attached label is a no-op.
	AddIssueLabel(ctx context.Context, owner, repo string, id int, label string) error

	// CreateLabel creates `label` in the repository's label catalog
	// with the given color (6-char hex, no leading '#') and
	// description. Idempotent: when the label already exists, the
	// call is a no-op — color and description are NOT propagated,
	// so humans who hand-tuned a label don't get silently
	// overwritten on every /gtw fix.
	//
	// On failure (network / token scope / API rate-limit), the
	// raw provider stderr is preserved so callers can surface the
	// cause verbatim. The label set is small (see AllLabels in
	// types.go); call sites loop over AllLabels and treat the
	// first error as fatal.
	//
	// See docs/feat/F-59-gtw-label-bootstrap.md for the bootstrap
	// rationale: without CreateLabel, `gh issue edit --add-label`
	// on a freshly-cloned repo errors out with "'nightme/wip'
	// not found" and the /gtw fix flow has no way to recover.
	CreateLabel(ctx context.Context, owner, repo, name, color, description string) error

	// RemoveIssueLabel removes `label` from the issue. Idempotent:
	// removing an already-absent label is a no-op.
	RemoveIssueLabel(ctx context.Context, owner, repo string, id int, label string) error

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

	// FindOpenPRForBranch returns the single open pull request
	// (GitHub) or merge request (GitLab) whose head branch
	// matches `head`, or (nil, nil) when no open PR exists.
	//
	// Distinct from GetPR: FindOpenPRForBranch is used by
	// /gtw pr's precheck (the "already open?" gate), where
	// every error must be surfaced to the user verbatim. GetPR
	// is used by the footer render path, where "no PR" and
	// "API failed" collapse into the same fail-soft response.
	// Errors returned here follow the known-error / unknown-
	// pass-through contract — see wrapListPRError.
	FindOpenPRForBranch(ctx context.Context, owner, repo, head string) (*PR, error)

	// GetPR returns the open pull request (GitHub) or merge
	// request (GitLab) whose head branch matches `head`, or
	// (nil, nil) when no open PR currently exists for that
	// branch.
	//
	// "Open" is a v1 simplification: merged / closed / draft
	// are not surfaced here. The PR type keeps a State field
	// so a future variant can flip the platform filter without
	// touching the interface shape.
	//
	// Nil-vs-error semantics mirror GetIssue on the surface but
	// differ in spirit: GetIssue returning ErrIssueNotFound is
	// unusual (the user asked for a specific id, and 404 means
	// the id is wrong), whereas GetPR returning nil is the
	// common case — most chat sessions don't have a PR open
	// yet, and forcing the footer caller to discriminate
	// "network failed" from "no PR exists" every stamp would be
	// the wrong trade-off. The footer just omits the PR tail
	// segment in both cases; the only thing that surfaces PR
	// lookup failures is the warn-level log the runtime emits
	// when applicable.
	GetPR(ctx context.Context, owner, repo, head string) (*PR, error)
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

// ErrCLINotInstalled is returned by GitHub/GitLab providers when
// the underlying `gh` / `glab` binary is missing on PATH. Wrapping
// `ErrCLINotInstalled` lets dispatchPR surface a single friendly
// "install via brew install gh" hint that covers both platforms
// instead of leaking the raw exec.LookPath error.
//
// Detection lives in isExecutableNotFound (below) — the wrapper
// preserves the provider name so the hint can be platform-specific.
var ErrCLINotInstalled = errors.New("gtw: provider CLI not installed")

// ErrStaleUpstream is returned by GitHub/GitLab providers when
// the platform rejects CreatePR because the head branch is
// missing on the origin (or the cached SHA is stale). It is the
// race-window safety net for /gtw pr's precheck: the dispatch
// already verified `git ls-remote --heads origin <branch>`
// succeeded, but the branch may have been deleted (or the cached
// SHA replaced) between probe and CreatePR.
//
// Detection lives in each provider's classifyCreatePRError
// method (see GitHubProvider / GitLabProvider below). Unknown
// stderr is propagated verbatim, NOT translated into
// ErrStaleUpstream.
var ErrStaleUpstream = errors.New("gtw: head branch missing on origin")

// ErrNoCommitsBetween is returned by GitHub when CreatePR
// fails after the head ref was found to exist on origin, because
// the platform refuses when base and head resolve to the same
// commit, so there is no diff to PR. This is DIFFERENT from
// ErrStaleUpstream: the branch is fine, the user just has
// nothing to PR.
//
// GitHub's GraphQL validator surfaces it as "No commits
// between <base> and <head>". Previously this was lumped into
// ghStaleUpstreamSubstrings and surfaced to the user as
// "origin/<head> no longer exists — /gtw push first to
// republish", which is misleading (push did nothing wrong; the
// branch is just empty relative to base). The fix routes it
// through this dedicated sentinel so the user gets the right
// next-step hint.
//
// GitLab coverage: glab generally accepts empty diffs and
// surfaces the problem pre-merge, so the GitLab provider does
// not classify this case today. If GitLab ever surfaces an
// analogous error, add the substring to the GitLab provider's
// classifyCreatePRError — not here.
var ErrNoCommitsBetween = errors.New("gtw: no commits between base and head")

// isExecutableNotFound reports whether err originates from
// os/exec failing to find the binary on PATH (the common case
// when `gh` / `glab` isn't installed). Covers both the modern
// *exec.Error wrapping exec.ErrNotFound (returned by
// os/exec.Command.Start / LookPath) and the *fs.PathError with
// syscall.ENOENT that some Go runtimes surface.
//
// We deliberately do NOT match generic "command not found"
// substrings inside stderr — the stderr-based detection is
// unreliable across shells / i18n. The exec.Error /
// fs.PathError unwrap is the only stable signal.
func isExecutableNotFound(err error) bool {
	if err == nil {
		return false
	}
	var execErr *exec.Error
	if errors.As(err, &execErr) && errors.Is(execErr.Err, exec.ErrNotFound) {
		return true
	}
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) && errors.Is(pathErr.Err, syscall.ENOENT) {
		return true
	}
	return false
}

// wrapCreatePRError is the SHARED, provider-agnostic fallback
// for CreatePR error mapping. Provider-specific detection
// (gh's "No commits between" / GraphQL validator messages,
// glab's "Source branch does not exist" / 404, etc.) lives in
// the provider's own classifyCreatePRError method and runs
// BEFORE this helper — by the time an error reaches here it
// is either "no provider-specific pattern matched" or it has
// already been wrapped with a typed sentinel.
//
// What stays at this layer:
//
//   - ErrCLINotInstalled: about the binary being missing from
//     PATH (provider-agnostic — *exec.Error / fs.PathError).
//     The providerName argument drives the install hint.
//   - ErrPRExists via generic "already exists" substring: a
//     reasonable cross-platform fallback. If a platform's
//     wording changes, the provider's classifyCreatePRError
//     should match the new wording and return ErrPRExists
//     directly with the original stderr preserved.
//   - Generic wrap with stderr preserved verbatim.
//
// What moved out (lives in the provider now): the gh/glab
// substring tables that used to live here. The shared helper
// no longer knows what gh or glab's stderr looks like.
func wrapCreatePRError(err error, stderr, providerName string) error {
	if err == nil {
		return nil
	}
	if isExecutableNotFound(err) {
		return fmt.Errorf("%w: %s — install via `brew install %s` or visit https://%s (%w)",
			ErrCLINotInstalled, providerName, providerName,
			providerCLIInstallURL(providerName), err)
	}
	trimmed := strings.TrimSpace(stderr)
	// Generic cross-platform "already exists" fallback. Both gh
	// ("a pull request for branch ... into ... already exists")
	// and glab ("Another open merge request already exists")
	// surface this phrase; if a future platform does not, the
	// provider's classifyCreatePRError should handle it.
	if strings.Contains(trimmed, "already exists") {
		return fmt.Errorf("%w: %s", ErrPRExists, trimmed)
	}
	if trimmed != "" {
		return fmt.Errorf("%s pr/mr create: %v: %s", providerName, err, trimmed)
	}
	return fmt.Errorf("%s pr/mr create: %w", providerName, err)
}

// wrapListPRError is the FindOpenPRForBranch / GetPR shared
// error-mapping helper. Only ErrCLINotInstalled is translated;
// stale-upstream / already-exists don't apply to a list call.
// Unknown stderr is wrapped verbatim.
func wrapListPRError(err error, stderr, providerName string) error {
	if err == nil {
		return nil
	}
	if isExecutableNotFound(err) {
		return fmt.Errorf("%w: %s — install via `brew install %s` or visit https://%s (%w)",
			ErrCLINotInstalled, providerName, providerName,
			providerCLIInstallURL(providerName), err)
	}
	trimmed := strings.TrimSpace(stderr)
	if trimmed != "" {
		return fmt.Errorf("%s pr/mr list: %v: %s", providerName, err, trimmed)
	}
	return fmt.Errorf("%s pr/mr list: %w", providerName, err)
}

// providerCLIInstallURL returns the install page for the named
// provider CLI (gh / glab). Empty string is returned for
// unknown names — wrapCreatePRError / wrapListPRError fall back
// to the brew hint only.
func providerCLIInstallURL(name string) string {
	switch name {
	case "gh":
		return "cli.github.com"
	case "glab":
		return "gitlab.com/gitlab-org/cli"
	}
	return ""
}

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
//	ssh://user@host:port/path.git             → host:port (ssh:// with userinfo+port)
//	git@host:path.git                         → host  (scp-style — legacy SSH form)
//	git@host:port:path.git                    → host:port (scp-style with explicit port — git supports this; see git-clone docs)
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

	// 5. Three-way colon disambiguation:
	//
	//      URL-style with port:    https://host:NNNN/path
	//        → first colon is the port separator, keep it; the
	//          split below extracts host:NNNN as one piece.
	//
	//      SCP-style with port:    git@host:NNNN:path
	//        → first colon is port separator, second is path
	//          separator. Convert the second colon to "/" so the
	//          split below extracts host:NNNN cleanly. This is the
	//          case that breaks without isSCPStylePort — see
	//          TestParseRemoteHost_SCPWithPort.
	//
	//      SCP-style without port: git@host:path
	//        → first colon is the path separator, convert to "/".
	//
	//    Heuristic: URL-style port → digits + ("/" | "?" | "#" |
	//    end-of-string). SCP-style port → digits + ":" + path.
	//    Otherwise treat the colon as a path separator.
	if i := strings.Index(u, ":"); i >= 0 {
		rest := u[i+1:]
		switch {
		case isPort(rest):
			// URL-style port — keep as-is.
		case isSCPStylePort(rest):
			// SCP-style port: keep "host:NNNN", convert the
			// SECOND colon (after the digits) to "/".
			second := strings.Index(rest, ":")
			u = u[:i+1+second] + "/" + rest[second+1:]
		default:
			// SCP-style path colon — convert to "/".
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

// isSCPStylePort reports whether s looks like the SCP-style
// "port:path" fragment — digits followed by a colon and a path.
// This is the `git@host:NNNN:path.git` form where the colon
// between the port number and the path is the same character
// that separates host from port. Distinct from isPort: URL-style
// ports are digits-followed-by-slash, scp-style ports are
// digits-followed-by-colon.
//
// Without this helper, parseRemoteHost sees the second `:` in
// `host:NNNN:path` and treats the first `:` as the scp-style
// host/path separator, swallowing the port and breaking the
// self-hosted GitLab-on-non-default-port case.
func isSCPStylePort(s string) bool {
	if s == "" {
		return false
	}
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return false // no leading digits
	}
	if i >= len(s) {
		return false // bare digits with nothing after — degenerate
	}
	return s[i] == ':'
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
		// Strong fingerprint: GitLab's version object.
		var v struct {
			Version  string `json:"version"`
			Revision string `json:"revision"`
		}
		if json.Unmarshal(body, &v) == nil && (v.Version != "" || v.Revision != "") {
			return &GitLabProvider{host: host, version: v.Version, Worktree: worktree}, nil
		}
		// Soft fingerprint: GitLab's auth/permission error envelope.
		// /api/v4 is GitLab-only — GitHub Enterprise uses /api/v3, so
		// a response with the GitLab envelope ({"message":"401
		// Unauthorized"} etc.) at this path is GitLab-shaped. This
		// catches auth-protected self-hosted GitLab reachable only
		// via plain HTTP (corporate proxy blocks :443), where
		// /api/v4/version answers 401 instead of 200.
		var env struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(body, &env) == nil && env.Message != "" {
			return &GitLabProvider{host: host, Worktree: worktree}, nil
		}
	}
	if body, err := prober.Probe(ctx, host, "/api/v3/meta"); err == nil {
		var meta map[string]json.RawMessage
		if json.Unmarshal(body, &meta) == nil {
			// GitHub Enterprise's /api/v3/meta returns
			// {"verifiable_password_authentication":..., ...}.
			// We deliberately do NOT loosen this branch to
			// "any JSON body" the way the /api/v4 probe is:
			// GitLab answers 404 with a JSON envelope on
			// /api/v3/meta, which would misclassify as GitHub.
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
	// body on 2xx/3xx/4xx. Any non-5xx response counts as
	// "this endpoint actually responded" — including 401 auth-
	// required, which Detect fingerprints via the GitLab /
	// GitHub Enterprise error envelope. Transport failure and
	// 5xx return an error so callers (Detect) can move on.
	//
	// Implementations may apply scheme fallback (HTTPS then
	// HTTP) — see ExecHTTPProber for the production behaviour.
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
//
// Scheme fallback: tries HTTPS first, then plain HTTP on the same
// path on transport error (DNS / TCP / TLS / proxy CONNECT-tunnel
// refusal / timeout) or 5xx. The fallback is essential for
// internal self-hosted GitLab / GitHub Enterprise — many of those
// instances run on bare IPs behind a corporate HTTP proxy that
// refuses CONNECT to :443, but expose the API on plain HTTP. HTTPS
// alone would silently fail with "502 CONNECT tunnel failed" and
// return ErrUnsupportedProvider to /gtw pr.
//
// Returns the response body on 2xx/3xx/4xx (any status that proves
// the endpoint actually responded — including 401 auth-required,
// which Detect fingerprints as a GitLab / GitHub Enterprise error
// envelope). 5xx and transport failure of BOTH schemes return an
// error.
//
// Total wall-clock is bounded by a single Timeout budget shared
// across both schemes via context.WithTimeout — a stuck HTTPS
// connect caps at 3s, then HTTP gets the remaining time.
func (p *ExecHTTPProber) Probe(ctx context.Context, host, path string) ([]byte, error) {
	if p == nil {
		p = &ExecHTTPProber{}
	}
	timeout := p.Timeout
	if timeout == 0 {
		timeout = 3 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var transport http.RoundTripper = http.DefaultTransport
	if p.InsecureSkipVerify {
		transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}
	cli := &http.Client{Transport: transport}

	if body, ok := probeOnce(ctx, cli, "https://"+host+path); ok {
		return body, nil
	}
	if body, ok := probeOnce(ctx, cli, "http://"+host+path); ok {
		return body, nil
	}
	return nil, fmt.Errorf("probe %s%s: both https and http failed", host, path)
}

// probeOnce issues a single GET against url. Returns (body, true)
// on 2xx/3xx/4xx; returns (nil, false) on transport error or 5xx
// so the caller can try the next scheme.
func probeOnce(ctx context.Context, cli *http.Client, url string) ([]byte, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "nightme-git-provider-detect/1.0")
	resp, err := cli.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false
	}
	if resp.StatusCode >= 500 {
		return nil, false
	}
	return body, true
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
		Attachments: c.attachmentsFromBody(raw.Body),
	}, nil
}

// attachmentsFromBody extracts download candidates from the
// GitHub issue body. GitHub doesn't have a first-class issue
// attachments API — user uploads (screenshots, logs, dumps)
// inline as `![](url)` or `[](url)` in the body markdown.
// parseMarkdownAttachmentLinks pulls every link candidate;
// attachmentsFromHints resolves each to an IssueAttachment
// (filename + MIME hint). Both helpers are shared with the
// GitLab provider — the body convention is identical, so the
// per-provider method is a thin seam rather than a parallel
// parser.
//
// v1 accepts any http(s) URL regardless of host; non-upload
// hosts may include redirects that downloadAttachments has to
// handle. Returns nil when the body has no link candidates.
//
// Method is named on the provider (not a free function) so the
// strategy is discoverable from the GitHubProvider type —
// future Gitea / Bitbucket providers each get their own
// attachmentsFromBody with their own parser rules.
func (c *GitHubProvider) attachmentsFromBody(body string) []IssueAttachment {
	return attachmentsFromMarkdownBody(body)
}

// AddIssueLabel runs `gh issue edit <id> --add-label <label> --repo ...`.
// The label must already exist in the repository's catalog; use
// CreateLabel first if uncertain. gh does not auto-create labels
// here (would fail with '"<label>" not found' on a fresh repo).
func (c *GitHubProvider) AddIssueLabel(ctx context.Context, owner, repo string, id int, label string) error {
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

// RemoveIssueLabel runs `gh issue edit <id> --remove-label <label> --repo ...`.
func (c *GitHubProvider) RemoveIssueLabel(ctx context.Context, owner, repo string, id int, label string) error {
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

// CreateLabel runs `gh label create <name> --color <color>
// --description <description> --repo <owner>/<repo>`. Idempotent:
// gh creates the label if missing. When the label already exists,
// gh exits 1 with stderr
//
// 	label with name "<name>" already exists; use `--force` to update
// 	its color and description
//
// We deliberately DO NOT pass --force: --force would update the
// existing label's color / description, which contradicts the
// GitProvider.CreateLabel contract ("existing labels are no-ops;
// color / description are NOT propagated"). Humans who hand-tuned
// a label's color or description must not have their changes
// silently overwritten on every /gtw fix.
//
// The "already exists" stderr substring is sniffed as success —
// mirroring the GitLab implementation, which also lacks an
// explicit idempotency flag. gh's stderr wording has been stable
// since gh 2.0 (when `gh label create` shipped); the substring
// match is case-sensitive to avoid false positives on unrelated
// errors like "label name contains invalid characters" or 403
// permission denied.
func (c *GitHubProvider) CreateLabel(ctx context.Context, owner, repo, name, color, description string) error {
	args := []string{
		"label", "create", name,
		"--repo", owner + "/" + repo,
		"--color", color,
		"--description", description,
	}
	_, stderr, err := c.runner().Run(ctx, "gh", args...)
	if err == nil {
		return nil
	}
	// gh stderr for an existing label: 'label with name "<name>"
	// already exists; use `--force` to update its color and
	// description'. Match the substring exactly; the surrounding
	// text is not relied on.
	if strings.Contains(stderr, "already exists") {
		return nil
	}
	return fmt.Errorf("gh label create: %v: %s", err, strings.TrimSpace(stderr))
}

// CreatePR runs `gh pr create --base <base> --head <head> --title
// <title> --body <body> --repo <owner>/<repo>`. The head branch must
// already be pushed to origin (dispatchPR's first readiness gate
// enforces this via `git ls-remote --heads origin <branch>` —
// see pr.go's gate 1); if not, gh prints a "head ref" GraphQL
// validator error and wrapCreatePRError translates it to
// ErrStaleUpstream.
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
		if classified := c.classifyCreatePRError(stderr); classified != nil {
			return "", classified
		}
		return "", wrapCreatePRError(err, stderr, "gh")
	}
	return strings.TrimSpace(stdout), nil
}

// ghStaleUpstreamSubstrings are the GitHub GraphQL validator
// messages createPullRequest surfaces for "head ref is bad /
// branch missing on origin". These are the literals GitHub's
// GraphQL validator returns; see F-237 for the original bug.
//
// Lives next to GitHubProvider (not in the shared layer) so
// adding a third provider (Gitea, Bitbucket) does not require
// editing this file's substring table — each provider owns its
// own detection.
var ghStaleUpstreamSubstrings = []string{
	"Head ref must be a branch",
	"Head sha can't be blank",
	"Base sha can't be blank",
}

// classifyCreatePRError translates known gh CreatePR stderr
// fragments into typed sentinel errors. Returns nil for stderr
// that does not match any known pattern — the caller falls
// back to wrapCreatePRError (the provider-agnostic helper).
//
// Detection order matters: ErrNoCommitsBetween is checked
// before ErrStaleUpstream because the two classes are distinct
// (branch-exists-but-empty vs branch-missing-on-origin) but
// can superficially look similar in failure logs. The dispatch
// layer surfaces each with a different user-facing hint, so
// getting the classification wrong directly misleads the user.
func (c *GitHubProvider) classifyCreatePRError(stderr string) error {
	trimmed := strings.TrimSpace(stderr)
	if strings.Contains(trimmed, "No commits between") {
		return fmt.Errorf("%w: %s", ErrNoCommitsBetween, trimmed)
	}
	for _, s := range ghStaleUpstreamSubstrings {
		if strings.Contains(trimmed, s) {
			return fmt.Errorf("%w: %s", ErrStaleUpstream, trimmed)
		}
	}
	return nil
}

// FindOpenPRForBranch runs `gh pr list --head <head> --state
// open --json number,url,state --repo <owner>/<repo>` and returns
// the freshest matching open PR. Returns (nil, nil) on empty list
// — the common case when the branch has never had a PR opened.
//
// Error contract: known failure modes (CLI not installed) are
// translated to ErrCLINotInstalled; everything else is wrapped
// verbatim. Stale-upstream doesn't apply to a list call (gh
// returns empty list, not an error, when the head doesn't exist
// on origin), so it has no special handling here.
func (c *GitHubProvider) FindOpenPRForBranch(ctx context.Context, owner, repo, head string) (*PR, error) {
	args := []string{
		"pr", "list",
		"--head", head,
		"--state", "open",
		"--json", "number,url,state",
		"--repo", owner + "/" + repo,
	}
	stdout, stderr, err := c.runner().Run(ctx, "gh", args...)
	if err != nil {
		return nil, wrapListPRError(err, stderr, "gh")
	}
	out := strings.TrimSpace(stdout)
	if out == "" || out == "[]" {
		return nil, nil
	}
	var rows []struct {
		Number int    `json:"number"`
		URL    string `json:"url"`
		State  string `json:"state"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, fmt.Errorf("gh pr list: decode json: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	row := rows[0]
	state := strings.ToLower(row.State)
	if state == "" {
		state = "open"
	}
	return &PR{Number: row.Number, URL: row.URL, State: state}, nil
}

// GetPR runs `gh pr list --head <head> --state open --json
// number,url,state --repo <owner>/<repo>` and returns the first
// matching PR.
//
// gh emits an empty JSON array `[]` when no open PR exists for
// the head branch; we surface that as (nil, nil) so callers
// don't have to special-case "no PR yet". A non-zero exit with
// stderr (auth failure, network down, rate-limited) wraps the
// underlying error verbatim.
func (c *GitHubProvider) GetPR(ctx context.Context, owner, repo, head string) (*PR, error) {
	args := []string{
		"pr", "list",
		"--head", head,
		"--state", "open",
		"--json", "number,url,state",
		"--repo", owner + "/" + repo,
	}
	stdout, stderr, err := c.runner().Run(ctx, "gh", args...)
	if err != nil {
		return nil, fmt.Errorf("gh pr list --head %s: %v: %s", head, err, strings.TrimSpace(stderr))
	}
	out := strings.TrimSpace(stdout)
	if out == "" || out == "[]" {
		return nil, nil
	}
	var rows []struct {
		Number int    `json:"number"`
		URL    string `json:"url"`
		State  string `json:"state"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, fmt.Errorf("gh pr list: decode json: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	// gh returns all open PRs for the head, sorted by recency;
	// the footer only ever shows one. Pick the freshest.
	row := rows[0]
	state := strings.ToLower(row.State)
	if state == "" {
		state = "open"
	}
	return &PR{Number: row.Number, URL: row.URL, State: state}, nil
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

// FindOpenPRForBranch runs `glab mr list --source-branch <head>
// --output json --repo <owner>/<repo>` and returns the first
// matching MR. Returns (nil, nil) on empty list — the common
// case when the branch has never had an MR opened.
//
// glab mr list's default state is "open" (regression: prior
// versions of this code passed `--state opened`, which started
// erroring with "Unknown flag: --state" on glab 1.36+ — the
// `--state` flag was removed from glab mr list in favour of
// dedicated boolean flags --closed/--merged/--draft; the default
// behaviour is exactly what we want, so we omit the flag
// entirely. See https://docs.gitlab.com/cli/mr/list/).
//
// Error contract: known failure modes (CLI not installed) are
// translated to ErrCLINotInstalled; everything else is wrapped
// verbatim. Stale-upstream doesn't apply to a list call (glab
// returns empty list, not an error, when the source branch
// doesn't exist), so it has no special handling here.
func (c *GitLabProvider) FindOpenPRForBranch(ctx context.Context, owner, repo, head string) (*PR, error) {
	args := []string{
		"mr", "list",
		"--source-branch", head,
		"--output", "json",
		"--repo", owner + "/" + repo,
	}
	stdout, stderr, err := c.runner().Run(ctx, "glab", args...)
	if err != nil {
		return nil, wrapListPRError(err, stderr, "glab")
	}
	out := strings.TrimSpace(stdout)
	if out == "" || out == "[]" {
		return nil, nil
	}
	var rows []struct {
		IID    int    `json:"iid"`
		WebURL string `json:"web_url"`
		State  string `json:"state"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, fmt.Errorf("glab mr list: decode json: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	row := rows[0]
	state := strings.ToLower(row.State)
	if state == "" {
		state = "open"
	}
	// glab reports "opened" not "open" — normalise to the
	// convention the PR.State comment documents ("open") so
	// downstream consumers don't have to know the platform.
	if state == "opened" {
		state = "open"
	}
	return &PR{Number: row.IID, URL: row.WebURL, State: state}, nil
}

// GetPR runs `glab mr list --source-branch <head> --output json
// --repo <owner>/<repo>` and returns the first matching MR.
//
// glab's --output json shape: array of objects with `iid`,
// `web_url`, `state` (lowercase: "opened" / "closed" / "merged").
// "opened" is GitLab's term for the equivalent of GitHub's "open".
//
// Regression: prior versions passed `--state opened` here too.
// glab 1.36+ removed `--state` from `mr list` (default is open;
// dedicated flags --closed/--merged/--draft cover the others) and
// returned `Unknown flag: --state`. The fix is the same as
// FindOpenPRForBranch: drop the flag entirely.
//
// Returns (nil, nil) when no MR matches the head branch — same
// fail-soft contract as GitHubProvider.GetPR.
func (c *GitLabProvider) GetPR(ctx context.Context, owner, repo, head string) (*PR, error) {
	args := []string{
		"mr", "list",
		"--source-branch", head,
		"--output", "json",
		"--repo", owner + "/" + repo,
	}
	stdout, stderr, err := c.runner().Run(ctx, "glab", args...)
	if err != nil {
		return nil, fmt.Errorf("glab mr list --source-branch %s: %v: %s", head, err, strings.TrimSpace(stderr))
	}
	out := strings.TrimSpace(stdout)
	if out == "" || out == "[]" {
		return nil, nil
	}
	var rows []struct {
		IID    int    `json:"iid"`
		WebURL string `json:"web_url"`
		State  string `json:"state"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, fmt.Errorf("glab mr list: decode json: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	row := rows[0]
	state := strings.ToLower(row.State)
	if state == "" {
		state = "opened"
	}
	// glab reports "opened" not "open" — normalise to the
	// convention the PR.State comment documents ("open") so
	// downstream consumers don't have to know the platform.
	if state == "opened" {
		state = "open"
	}
	return &PR{Number: row.IID, URL: row.WebURL, State: state}, nil
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
		ID:          raw.IID,
		Title:       raw.Title,
		Body:        raw.Body,
		State:       strings.ToLower(raw.State),
		Labels:      raw.Labels,
		URL:         raw.WebURL,
		Attachments: c.attachmentsFromBody(raw.Body),
	}, nil
}

// attachmentsFromBody extracts download candidates from the
// GitLab issue body.
//
// TODO: switch to native `glab api /projects/:id/issues/:iid`
// attachment_links once the API response shape is confirmed. The
// native path would return richer metadata (uploaded-at, uploader,
// byte size, content-type from GitLab's own MIME sniffing) that
// body parsing can't reconstruct; the body-parsing v1 is here so
// GitLab users get attachment support immediately without waiting
// for the API probe + response-shape work.
//
// v1 reuses the same attachmentsFromMarkdownBody helper as the
// GitHub provider — body markdown is the same convention. Per-
// provider divergence (URL allowlist for `/uploads/...` host,
// filename sanitization rules) lives here when it lands; until
// then this is a thin seam.
//
// Returns nil when the body has no link candidates.
func (c *GitLabProvider) attachmentsFromBody(body string) []IssueAttachment {
	return attachmentsFromMarkdownBody(body)
}

// AddIssueLabel runs `glab issue update <id> --label <label> --repo ...`.
// The label must already exist in the repository's catalog; use
// CreateLabel first if uncertain.
func (c *GitLabProvider) AddIssueLabel(ctx context.Context, owner, repo string, id int, label string) error {
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

// RemoveIssueLabel runs `glab issue update <id> --unlabel <label> --repo ...`.
func (c *GitLabProvider) RemoveIssueLabel(ctx context.Context, owner, repo string, id int, label string) error {
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

// CreateLabel runs `glab label create --name <name> --color <color>
// --description <description> --repo <owner>/<repo>`. glab does
// NOT have a `--force` flag (as of 1.82.x), so we treat the
// "already exists" stderr as success — equivalent to gh's
// --force but via stderr sniffing rather than an explicit flag.
//
// "already exists" substring covers both 1.x and the older
// "Label already exists" wording; the match is case-sensitive
// to avoid false positives on unrelated errors. A truly broken
// state (e.g. label-create permission denied on a 403) will
// surface a different stderr and reach the caller unchanged.
func (c *GitLabProvider) CreateLabel(ctx context.Context, owner, repo, name, color, description string) error {
	args := []string{
		"label", "create",
		"--repo", owner + "/" + repo,
		"--name", name,
		"--color", color,
		"--description", description,
	}
	_, stderr, err := c.runner().Run(ctx, "glab", args...)
	if err == nil {
		return nil
	}
	// glab 1.x prints the message in English; older versions
	// occasionally capitalised "Label" — match both.
	if strings.Contains(stderr, "already exists") || strings.Contains(stderr, "Already exists") {
		return nil
	}
	return fmt.Errorf("glab label create: %v: %s", err, strings.TrimSpace(stderr))
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
		if classified := c.classifyCreatePRError(stderr); classified != nil {
			return "", classified
		}
		return "", wrapCreatePRError(err, stderr, "glab")
	}
	return strings.TrimSpace(stdout), nil
}

// glStaleUpstreamSubstrings are the glab-side fragments that
// mean "source branch missing on origin". glab's HTTP layer
// surfaces these directly in stderr / error message; substring
// match on stderr is the same stable channel used by the
// GitHub side.
//
// "Branch not found" matches glab's API 404 message; "Source
// branch does not exist" matches the validation error;
// "404 Not Found" matches the raw HTTP layer. The list is
// deliberately narrow — unknown stderr is propagated verbatim,
// NOT translated into ErrStaleUpstream.
//
// Lives next to GitLabProvider (not in the shared layer) so
// adding a third provider does not require editing this table.
var glStaleUpstreamSubstrings = []string{
	"Source branch does not exist",
	"Branch not found",
	"404 Not Found",
}

// classifyCreatePRError translates known glab CreatePR stderr
// fragments into typed sentinel errors. Returns nil for stderr
// that does not match any known pattern — the caller falls
// back to wrapCreatePRError.
//
// GitLab does not currently surface a "base == head" error
// (glab generally accepts empty diffs and lets the user fix
// it pre-merge). The ErrNoCommitsBetween path is GitHub-only;
// add a glNoCommitsBetweenSubstrings slice here if GitLab ever
// surfaces an analogous message.
func (c *GitLabProvider) classifyCreatePRError(stderr string) error {
	trimmed := strings.TrimSpace(stderr)
	for _, s := range glStaleUpstreamSubstrings {
		if strings.Contains(trimmed, s) {
			return fmt.Errorf("%w: %s", ErrStaleUpstream, trimmed)
		}
	}
	return nil
}

