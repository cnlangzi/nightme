package gtw

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// utf8RuneCount wraps utf8.RuneCountInString for table-test brevity.
func utf8RuneCount(s string) int { return utf8.RuneCountInString(s) }

// ---- slug / branch derivation -------------------------------------

func TestDeriveBranch_Typical(t *testing.T) {
	got := DeriveBranch(42, "Login state expiration")
	want := "fix/42-login-state-expiration"
	if got != want {
		t.Fatalf("DeriveBranch(42, %q) = %q, want %q", "Login state expiration", got, want)
	}
}

func TestDeriveBranch_EmptyTitle(t *testing.T) {
	got := DeriveBranch(42, "")
	want := "fix/42"
	if got != want {
		t.Fatalf("DeriveBranch(42, \"\") = %q, want %q", got, want)
	}
}

func TestDeriveBranch_AllSymbols(t *testing.T) {
	// Title that's all non-ASCII / punctuation → no slug.
	got := DeriveBranch(7, "中文标题 🚀 !@#")
	// Allowed: digits + "-" only. With no usable letters, the
	// slug drops to the bare issue id.
	want := "fix/7"
	if got != want {
		t.Fatalf("DeriveBranch(7, weird) = %q, want %q", got, want)
	}
}

func TestDeriveBranch_LongTitleTruncated(t *testing.T) {
	// 50 chars in slug; budget after "<id>-" = SlugMaxLen - 3 = 27.
	title := "the quick brown fox jumps over the lazy dog again and again"
	got := DeriveBranch(3, title)
	if len(got) > len("fix/3-")+SlugMaxLen {
		t.Fatalf("DeriveBranch length %d > %d: %q", len(got),
			len("fix/3-")+SlugMaxLen, got)
	}
	// Should NOT have a trailing dash.
	if strings.HasSuffix(got, "-") {
		t.Fatalf("trailing dash: %q", got)
	}
	// Sanity: starts with the right prefix.
	if !strings.HasPrefix(got, "fix/3-") {
		t.Fatalf("missing prefix: %q", got)
	}
}

func TestDeriveBranch_MultipleSeparatorsCollapsed(t *testing.T) {
	// "foo  bar -- baz" should slugify to "foo-bar-baz", not
	// "foo---bar----baz".
	got := DeriveBranch(1, "foo  bar -- baz")
	want := "fix/1-foo-bar-baz"
	if got != want {
		t.Fatalf("DeriveBranch(1, foo  bar -- baz) = %q, want %q", got, want)
	}
}

func TestDeriveBranch_UnderscoresAndDotsTreatedAsSeparators(t *testing.T) {
	got := DeriveBranch(1, "fix_login.bug")
	want := "fix/1-fix-login-bug"
	if got != want {
		t.Fatalf("DeriveBranch(1, fix_login.bug) = %q, want %q", got, want)
	}
}

func TestDeriveSlug_BareID(t *testing.T) {
	got := DeriveSlug(99, "   ")
	want := "99"
	if got != want {
		t.Fatalf("DeriveSlug(99, whitespace) = %q, want %q", got, want)
	}
}

// ---- worktree path -------------------------------------------------

func TestWorktreePath_StandardLayout(t *testing.T) {
	got := WorktreePath("/home/dev/code/nightme", "42-login-state-expiration")
	want := "/home/dev/code/nightme.nightme/42-login-state-expiration"
	if got != want {
		t.Fatalf("WorktreePath() = %q, want %q", got, want)
	}
}

func TestWorktreePath_NoTrailingSlash(t *testing.T) {
	got := WorktreePath("/code/nightme/", "42-foo")
	want := "/code/nightme.nightme/42-foo"
	if got != want {
		t.Fatalf("WorktreePath() = %q, want %q", got, want)
	}
}

func TestWorktreePath_DeepRepoPath(t *testing.T) {
	got := WorktreePath("/work/group/sub/nightme", "1-foo")
	want := "/work/group/sub/nightme.nightme/1-foo"
	if got != want {
		t.Fatalf("WorktreePath() = %q, want %q", got, want)
	}
}

func TestBranchVariant(t *testing.T) {
	cases := []struct {
		branch string
		n      int
		want   string
	}{
		{"fix/42-foo", 2, "fix/42-foo-v2"},
		{"fix/42-foo", 3, "fix/42-foo-v3"},
		{"fix/42-foo", 10, "fix/42-foo-v10"},
		// n < 2 is treated as the original (sanity guard).
		{"fix/42-foo", 1, "fix/42-foo"},
		{"fix/42-foo", 0, "fix/42-foo"},
	}
	for _, c := range cases {
		got := BranchVariant(c.branch, c.n)
		if got != c.want {
			t.Errorf("BranchVariant(%q, %d) = %q, want %q", c.branch, c.n, got, c.want)
		}
	}
}

// ---- parseIssueID --------------------------------------------------

func TestParseIssueID(t *testing.T) {
	cases := []struct {
		in     string
		want   int
		errMsg string
	}{
		{"42", 42, ""},
		{"#42", 42, ""},
		{"  42  ", 42, ""},
		{"1", 1, ""},
		{"0", 0, "issue id cannot be 0"},
		{"", 0, "empty issue id"},
		{"abc", 0, "invalid issue id"},
		{"42abc", 0, "invalid issue id"},
		{"-1", 0, "invalid issue id"},
		{"42.0", 0, "invalid issue id"},
	}
	for _, c := range cases {
		got, err := parseIssueID(c.in)
		if c.errMsg == "" {
			if err != nil {
				t.Errorf("parseIssueID(%q) unexpected error: %v", c.in, err)
				continue
			}
			if got != c.want {
				t.Errorf("parseIssueID(%q) = %d, want %d", c.in, got, c.want)
			}
		} else {
			if err == nil {
				t.Errorf("parseIssueID(%q) expected error containing %q, got nil (val=%d)",
					c.in, c.errMsg, got)
				continue
			}
			if !strings.Contains(err.Error(), c.errMsg) {
				t.Errorf("parseIssueID(%q) error = %q, want containing %q", c.in, err.Error(), c.errMsg)
			}
		}
	}
}

// ---- ParseRepoOwner ------------------------------------------------

func TestParseRepoOwner(t *testing.T) {
	cases := []struct {
		in        string
		owner     string
		repo      string
		errSubstr string
	}{
		{"git@github.com:cnlangzi/nightme.git", "cnlangzi", "nightme", ""},
		{"https://github.com/cnlangzi/nightme.git", "cnlangzi", "nightme", ""},
		{"https://github.com/cnlangzi/nightme", "cnlangzi", "nightme", ""},
		{"ssh://git@github.com/cnlangzi/nightme.git", "cnlangzi", "nightme", ""},
		// GitLab self-hosted under groups.
		{"https://gitlab.example.com/group/sub/repo.git", "group/sub", "repo", ""},
		{"", "", "", "empty remote URL"},
		{"not a url", "", "", "cannot parse"},
	}
	for _, c := range cases {
		owner, repo, err := ParseRepoOwner(c.in)
		if c.errSubstr == "" {
			if err != nil {
				t.Errorf("ParseRepoOwner(%q) unexpected error: %v", c.in, err)
				continue
			}
			if owner != c.owner || repo != c.repo {
				t.Errorf("ParseRepoOwner(%q) = (%q, %q), want (%q, %q)",
					c.in, owner, repo, c.owner, c.repo)
			}
		} else {
			if err == nil {
				t.Errorf("ParseRepoOwner(%q) expected error containing %q, got nil",
					c.in, c.errSubstr)
				continue
			}
			if !strings.Contains(err.Error(), c.errSubstr) {
				t.Errorf("ParseRepoOwner(%q) error = %q, want containing %q",
					c.in, err.Error(), c.errSubstr)
			}
		}
	}
}

// ---- Detect (two-stage URL hint + API probe) -----------------------

// fakeHTTPProber returns canned JSON per (host,path) pair. Used
// to exercise Detect's Stage B without real network. Set
// `callOrder` to assert the probe sequence (GitLab first, GitHub
// second). Set `err` per path to simulate timeouts / 5xx.
type fakeHTTPProber struct {
	responses map[string]fakeHTTPResp
	callOrder []string
	err       error // blanket error for any (host, path) not in responses
}

type fakeHTTPResp struct {
	body []byte
	err  error
}

func (p *fakeHTTPProber) Probe(_ context.Context, host, path string) ([]byte, error) {
	p.callOrder = append(p.callOrder, host+path)
	if r, ok := p.responses[host+path]; ok {
		return r.body, r.err
	}
	if p.err != nil {
		return nil, p.err
	}
	return nil, fmt.Errorf("fakeHTTPProber: no canned response for %s%s", host, path)
}

func TestDetect_URLHint(t *testing.T) {
	cases := []struct {
		url      string
		wantKind ProviderKind
		wantHost string
	}{
		{"git@github.com:foo/bar.git", ProviderGitHub, "github.com"},
		{"https://github.com/foo/bar", ProviderGitHub, "github.com"},
		{"git@gitlab.com:foo/bar.git", ProviderGitLab, "gitlab.com"},
		// gitlab.com substring also hits Stage A — self-hosted GitLab
		// URLs containing "gitlab" also short-circuit (no probe).
		{"https://gitlab.example.com/foo/bar", ProviderGitLab, "gitlab.example.com"},
	}
	for _, c := range cases {
		prov, err := Detect(context.Background(), c.url, nil)
		if err != nil {
			t.Errorf("Detect(%q) unexpected error: %v", c.url, err)
			continue
		}
		if prov.Kind() != c.wantKind {
			t.Errorf("Detect(%q).Kind() = %q, want %q", c.url, prov.Kind(), c.wantKind)
		}
		if prov.Host() != c.wantHost {
			t.Errorf("Detect(%q).Host() = %q, want %q", c.url, prov.Host(), c.wantHost)
		}
	}
}

func TestDetect_StageB_GitLabVersion(t *testing.T) {
	prober := &fakeHTTPProber{
		responses: map[string]fakeHTTPResp{
			"gl.acme.internal/api/v4/version": {
				body: []byte(`{"version":"16.5.0","revision":"abc123"}`),
			},
		},
	}
	prov, err := Detect(context.Background(), "git@gl.acme.internal:group/foo.git", prober)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if prov.Kind() != ProviderGitLab {
		t.Errorf("Kind() = %q, want %q", prov.Kind(), ProviderGitLab)
	}
	if prov.Host() != "gl.acme.internal" {
		t.Errorf("Host() = %q, want %q", prov.Host(), "gl.acme.internal")
	}
	if prov.Version() != "16.5.0" {
		t.Errorf("Version() = %q, want %q", prov.Version(), "16.5.0")
	}
	// Probe order: GitLab first.
	if len(prober.callOrder) != 1 || prober.callOrder[0] != "gl.acme.internal/api/v4/version" {
		t.Errorf("expected single GitLab probe; got callOrder=%v", prober.callOrder)
	}
}

func TestDetect_StageB_GitHubEnterpriseMeta(t *testing.T) {
	prober := &fakeHTTPProber{
		responses: map[string]fakeHTTPResp{
			"gh.acme.internal/api/v4/version": {err: fmt.Errorf("404 not found")},
			"gh.acme.internal/api/v3/meta": {
				body: []byte(`{"verifiable_password_authentication":true,"github_token_smashed":false}`),
			},
		},
	}
	prov, err := Detect(context.Background(), "git@gh.acme.internal:foo/bar.git", prober)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if prov.Kind() != ProviderGitHub {
		t.Errorf("Kind() = %q, want %q", prov.Kind(), ProviderGitHub)
	}
	if prov.Host() != "gh.acme.internal" {
		t.Errorf("Host() = %q, want %q", prov.Host(), "gh.acme.internal")
	}
	// Both probes were attempted.
	if len(prober.callOrder) != 2 {
		t.Errorf("expected 2 probes; got %d (callOrder=%v)", len(prober.callOrder), prober.callOrder)
	}
}

func TestDetect_StageB_BothFail(t *testing.T) {
	prober := &fakeHTTPProber{err: fmt.Errorf("connection refused")}
	_, err := Detect(context.Background(), "https://gitea.example.com/foo/bar", prober)
	if err == nil {
		t.Fatalf("Detect: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported git provider") {
		t.Errorf("Detect error = %q, want containing %q", err.Error(), "unsupported git provider")
	}
	// Both probes attempted before failing.
	if len(prober.callOrder) != 2 {
		t.Errorf("expected 2 probes; got %d", len(prober.callOrder))
	}
}

func TestDetect_StageA_ShortCircuits(t *testing.T) {
	// Even with a prober that would succeed for GitLab, a github.com
	// URL must NOT trigger Stage B (Stage A wins).
	prober := &fakeHTTPProber{
		responses: map[string]fakeHTTPResp{
			"github.com/api/v4/version": {body: []byte(`{"version":"x"}`)},
		},
	}
	prov, err := Detect(context.Background(), "git@github.com:foo/bar.git", prober)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if prov.Kind() != ProviderGitHub {
		t.Errorf("Kind() = %q, want %q", prov.Kind(), ProviderGitHub)
	}
	if len(prober.callOrder) != 0 {
		t.Errorf("Stage A should short-circuit; got %d probes", len(prober.callOrder))
	}
}

func TestNewProvider_KindHostBinding(t *testing.T) {
	gh, err := NewProvider(ProviderGitHub, "gh.acme")
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if gh.Kind() != ProviderGitHub || gh.Host() != "gh.acme" {
		t.Errorf("got Kind=%q Host=%q", gh.Kind(), gh.Host())
	}
	gl, err := NewProvider(ProviderGitLab, "gl.acme")
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if gl.Kind() != ProviderGitLab || gl.Host() != "gl.acme" {
		t.Errorf("got Kind=%q Host=%q", gl.Kind(), gl.Host())
	}
	_, err = NewProvider(ProviderKind("bitbucket"), "x")
	if !errors.Is(err, ErrUnsupportedProvider) {
		t.Errorf("NewProvider(bitbucket) error = %v, want ErrUnsupportedProvider", err)
	}
}

// ---- fake runners --------------------------------------------------

// fakeGit is a GitRunner that returns canned stdout/stderr per argv.
type fakeGit struct {
	responses map[string]fakeGitResp
}

type fakeGitResp struct {
	stdout string
	stderr string
	err    error
}

func (f *fakeGit) Run(_ context.Context, _ string, args ...string) (string, string, error) {
	key := strings.Join(args, " ")
	if r, ok := f.responses[key]; ok {
		return r.stdout, r.stderr, r.err
	}
	return "", "", nil
}

func TestRepoRoot_Success(t *testing.T) {
	g := &fakeGit{responses: map[string]fakeGitResp{
		"rev-parse --show-toplevel": {stdout: "/code/nightme\n"},
	}}
	got, err := RepoRoot(context.Background(), "/code/nightme/subdir", g)
	if err != nil {
		t.Fatalf("RepoRoot: %v", err)
	}
	if got != "/code/nightme" {
		t.Fatalf("RepoRoot = %q, want /code/nightme", got)
	}
}

func TestRepoRoot_NotInRepo(t *testing.T) {
	g := &fakeGit{responses: map[string]fakeGitResp{
		"rev-parse --show-toplevel": {
			stderr: "fatal: not a git repository",
			err:    &fakeExitErr{code: 128},
		},
	}}
	_, err := RepoRoot(context.Background(), "/nowhere", g)
	if err == nil {
		t.Fatal("expected error from RepoRoot in non-repo dir")
	}
}

type fakeExitErr struct{ code int }

func (e *fakeExitErr) Error() string { return "exit" }

// ExitCode satisfies the os/exec exitError interface shape (we
// only need the int method, not the full *exec.ExitError type).
func (e *fakeExitErr) ExitCode() int { return e.code }

// ---- tailLines ----------------------------------------------------

func TestTailLines(t *testing.T) {
	in := "a\nb\nc\nd\ne"
	got := tailLines(in, 3)
	want := "c\nd\ne"
	if got != want {
		t.Fatalf("tailLines = %q, want %q", got, want)
	}

	got = tailLines(in, 100)
	if got != in {
		t.Fatalf("tailLines(100) = %q, want %q", got, in)
	}

	got = tailLines("", 5)
	if got != "" {
		t.Fatalf("tailLines(\"\") = %q, want empty", got)
	}

	// Trailing newline stripped by the function.
	got = tailLines("a\nb\n", 5)
	if got != "a\nb" {
		t.Fatalf("tailLines trailing-nl = %q, want %q", got, "a\nb")
	}
}

// ---- rebuild test (smoke) -----------------------------------------

func TestRebuildContext_NilWhenNotInWorktree(t *testing.T) {
	// cwd is not under a worktree (no slash → repoRootFromCS
	// returns cwd; BranchExists uses repoRoot from cwd which is
	// the same as cwd → symbolic-ref returns "" because we
	// aren't on a `fix/` branch). Result must be the zero Context.
	g := &fakeGit{responses: map[string]fakeGitResp{
		"symbolic-ref --short HEAD": {stdout: "main\n"},
	}}
	cs := &fakeSender{cwd: "/code/nightme"}
	got := RebuildContext(context.Background(), cs, g, nil, nil)
	if got != (Context{}) {
		t.Fatalf("RebuildContext = %+v, want zero", got)
	}
}

// fakeSender satisfies gtw.Sender for tests.
type fakeSender struct{ cwd string }

func (f *fakeSender) ActiveCwd() string             { return f.cwd }
func (f *fakeSender) SetActiveCwd(cwd string) error { f.cwd = cwd; return nil }

// ---- parseRemoteHost: full protocol matrix -------------------------
//
// Covers every git URL shape mentioned in F-50 §3 (self-hosted
// matrix) + the userinfo / port / query / fragment edge cases
// identified in review. parseRemoteHost is the gateway between
// Detect's Stage A URL hint and Stage B API probe — getting the
// host extraction wrong cascades into a wrong Stage B target.

func TestParseRemoteHost(t *testing.T) {
	cases := []struct {
		name   string
		url    string
		want   string
		errIs  error // expected errors.Is target
	}{
		// Standard protocols
		{"https github", "https://github.com/foo/bar.git", "github.com", nil},
		{"http github", "http://github.com/foo/bar", "github.com", nil},
		{"ssh github", "ssh://git@github.com/foo/bar.git", "github.com", nil},
		{"scp legacy", "git@github.com:foo/bar.git", "github.com", nil},
		{"git protocol", "git://github.com/foo/bar.git", "github.com", nil},
		{"gitlab.com", "https://gitlab.com/group/foo/bar.git", "gitlab.com", nil},

		// Self-hosted (URL hint ambiguous → Stage B will probe)
		{"self-hosted gitlab", "https://gitlab.acme.internal/foo/bar.git", "gitlab.acme.internal", nil},
		{"self-hosted GHE", "https://github.acme.internal/foo/bar.git", "github.acme.internal", nil},

		// Userinfo variants (gh / glab auth-helper, PAT in URL)
		{"PAT in URL", "https://ghp_xxx@github.com/foo/bar.git", "github.com", nil},
		{"userinfo with password", "https://oauth2:secret@gitlab.acme.io/foo/bar.git", "gitlab.acme.io", nil},
		{"userinfo + ssh", "ssh://git@github.com:2222/foo/bar.git", "github.com:2222", nil},

		// Port (ssh:// :port; HTTP :port for self-hosted on non-default)
		{"gitlab port", "https://gitlab.acme.internal:8929/foo/bar.git", "gitlab.acme.internal:8929", nil},

		// Query / fragment
		{"query string", "https://github.com/foo/bar.git?ref=main", "github.com", nil},
		{"fragment", "https://github.com/foo/bar.git#readme", "github.com", nil},
		{"query + fragment", "https://github.com/foo/bar.git?ref=main#frag", "github.com", nil},

		// Whitespace tolerance
		{"leading/trailing whitespace", "  https://github.com/foo/bar.git\n", "github.com", nil},

		// Malformed — should NOT silently extract wrong host
		{"empty", "", "", ErrInvalidRemoteURL},
		{"no scheme", "github.com/foo/bar", "", ErrInvalidRemoteURL},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseRemoteHost(c.url)
			if c.errIs != nil {
				if !errors.Is(err, c.errIs) {
					t.Fatalf("parseRemoteHost(%q) err = %v, want errors.Is %v", c.url, err, c.errIs)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRemoteHost(%q) unexpected error: %v", c.url, err)
			}
			if got != c.want {
				t.Errorf("parseRemoteHost(%q) = %q, want %q", c.url, got, c.want)
			}
		})
	}
}

// ---- Detect edge cases ----------------------------------------------

func TestDetect_URLHint_GitProtocol(t *testing.T) {
	// git:// URL must be recognised by Stage A (substring match
	// catches "github.com" / "gitlab" regardless of protocol).
	prov, err := Detect(context.Background(), "git://github.com/foo/bar.git", nil)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if prov.Kind() != ProviderGitHub {
		t.Errorf("Kind() = %q, want %q", prov.Kind(), ProviderGitHub)
	}
}

func TestDetect_InvalidURL_ReturnsInvalidRemoteURL(t *testing.T) {
	// B1 + D3: empty / scheme-less URL must surface as
	// ErrInvalidRemoteURL, not ErrUnsupportedProvider. The two
	// need different user-facing hints (D3 split).
	_, err := Detect(context.Background(), "", nil)
	if !errors.Is(err, ErrInvalidRemoteURL) {
		t.Errorf("Detect(\"\") err = %v, want ErrInvalidRemoteURL", err)
	}
	if errors.Is(err, ErrUnsupportedProvider) {
		t.Errorf("Detect(\"\") err should NOT also be ErrUnsupportedProvider")
	}

	_, err = Detect(context.Background(), "github.com/foo/bar", nil)
	if !errors.Is(err, ErrInvalidRemoteURL) {
		t.Errorf("Detect(plain) err = %v, want ErrInvalidRemoteURL", err)
	}
}

func TestDetect_StageA_PathologicalHosts(t *testing.T) {
	// Stage A should be robust against weird-but-valid inputs.
	cases := []struct {
		name   string
		url    string
		wantK  ProviderKind
		wantH  string
	}{
		{"trailing-slash repo", "https://github.com/foo/bar/", ProviderGitHub, "github.com"},
		{"no .git suffix", "https://github.com/foo/bar", ProviderGitHub, "github.com"},
		{"deep group path", "https://gitlab.com/g1/g2/g3/proj", ProviderGitLab, "gitlab.com"},
		{"PAT embedded in URL", "https://ghp_abc@github.com/foo/bar.git", ProviderGitHub, "github.com"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prov, err := Detect(context.Background(), c.url, nil)
			if err != nil {
				t.Fatalf("Detect(%q): %v", c.url, err)
			}
			if prov.Kind() != c.wantK {
				t.Errorf("Kind() = %q, want %q", prov.Kind(), c.wantK)
			}
			if prov.Host() != c.wantH {
				t.Errorf("Host() = %q, want %q", prov.Host(), c.wantH)
			}
		})
	}
}

func TestDetect_NilProber_UsesExecHTTPProber(t *testing.T) {
	// Stage A only — nil prober should not be invoked because
	// the URL is on github.com (Stage A short-circuits). If it
	// WERE invoked, the test would hang on real network or fail
	// when offline; the assertion is implicit in not hanging.
	prov, err := Detect(context.Background(), "git@github.com:foo/bar.git", nil)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if prov.Kind() != ProviderGitHub {
		t.Errorf("Kind() = %q", prov.Kind())
	}
}

func TestExecHTTPProber_NilPointer_Guarded(t *testing.T) {
	// B2: passing a typed-nil pointer must not panic. The
	// pointer-receiver Probe should re-zero itself.
	var p *ExecHTTPProber // nil
	// Just calling Probe would actually hit the network. The
	// guard kicks in first — verify by checking the function
	// does not panic on a synthetic call that immediately
	// fails (closed test server = connection refused).
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	ts.Close() // close so Probe fails fast

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, _ = p.Probe(ctx, "localhost", "/")
	// No panic = guard works.
}

// ---- ExecHTTPProber end-to-end (httptest.Server) -------------------

func TestExecHTTPProber_End2End(t *testing.T) {
	var gotPath string
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"version":"16.5.0","revision":"abc"}`))
	}))
	defer ts.Close()

	// httptest.NewTLSServer uses a self-signed cert that the
	// default http client rejects. Extract the cert and add it
	// to the client trust store via InsecureSkipVerify (test
	// shortcut; production uses real CA-signed certs).
	host := strings.TrimPrefix(ts.URL, "https://")

	// Case 1: 200 OK with body — happy path
	body, err := (&ExecHTTPProber{InsecureSkipVerify: true}).Probe(
		context.Background(), host, "/api/v4/version")
	if err != nil {
		t.Fatalf("Probe(200): %v", err)
	}
	if !strings.Contains(string(body), `"version":"16.5.0"`) {
		t.Errorf("body = %q, want containing version field", body)
	}
	if gotPath != "/api/v4/version" {
		t.Errorf("path = %q, want /api/v4/version", gotPath)
	}

	// Case 2: 503 — non-200 status returns error
	ts2 := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer ts2.Close()
	host2 := strings.TrimPrefix(ts2.URL, "https://")
	_, err = (&ExecHTTPProber{InsecureSkipVerify: true}).Probe(
		context.Background(), host2, "/api/v4/version")
	if err == nil {
		t.Errorf("Probe(503): expected error, got nil")
	}

	// Case 3: timeout — slow server + tight deadline
	ts3 := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
	}))
	defer ts3.Close()
	host3 := strings.TrimPrefix(ts3.URL, "https://")
	_, err = (&ExecHTTPProber{InsecureSkipVerify: true, Timeout: 100 * time.Millisecond}).Probe(
		context.Background(), host3, "/api/v4/version")
	if err == nil {
		t.Errorf("Probe(timeout): expected error, got nil")
	}
}

// ---- redactForDisplay: credential stripping (security) ------------
//
// F-50 review fix: user-facing error messages must never echo the
// raw remoteURL — it may contain userinfo (PAT / oauth2:token) that
// would leak to the IM channel. These tests pin down the redaction
// rules.

func TestRedactForDisplay(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		// forbiddenSubstrings: must NOT appear in output (security
		// assertion — credential parts must be stripped).
		forbid []string
	}{
		// PAT in URL — classic credential-leak case
		{
			"PAT in https URL",
			"https://ghp_abc123@github.com/owner/repo.git",
			"<redacted>@github.com/owner/repo.git",
			[]string{"ghp_abc123"},
		},
		// oauth2:token userinfo
		{
			"oauth2:secret userinfo",
			"https://oauth2:secret@gitlab.acme.io/group/foo.git",
			"<redacted>@gitlab.acme.io/group/foo.git",
			[]string{"oauth2:secret", "secret"},
		},
		// scp-style: "git@" is the protocol marker, not credential;
		// there is no real userinfo. The output should look like a
		// scp URL with the user stripped, leaving the host/path.
		{
			"scp-style no credentials",
			"git@github.com:owner/repo.git",
			"github.com:owner/repo.git",
			nil,
		},
		// ssh:// + userinfo + port — all must be handled together
		{
			"ssh:// userinfo + port",
			"ssh://git:pass@gitlab.acme.io:2222/foo/bar.git",
			"<redacted>@gitlab.acme.io:2222/foo/bar.git",
			[]string{"git:pass", "pass"},
		},
		// No credentials — pass through
		{
			"plain https",
			"https://github.com/owner/repo.git",
			"github.com/owner/repo.git",
			nil,
		},
		// Query + fragment
		{
			"query + fragment",
			"https://user@github.com/owner/repo.git?ref=main#frag",
			"<redacted>@github.com/owner/repo.git",
			[]string{"user"},
		},
		// Edge: empty / whitespace
		{
			"empty",
			"",
			"",
			nil,
		},
		{
			"whitespace only",
			"   \t\n",
			"",
			nil,
		},
		// Length cap (security + size bound)
		{
			"very long URL",
			"https://github.com/" + strings.Repeat("a", 500) + "/repo.git",
			"", // checked via length assertion below
			nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := redactForDisplay(c.in)
			if c.name == "very long URL" {
				// Length cap test: 256 rune ceiling + ellipsis
				// (note: "…" is 3 bytes in UTF-8; we measure runes).
				if utf8RuneCount(got) > 257 { // 256 + "…"
					t.Errorf("rune count of got = %d, want ≤ 257 (256 cap + ellipsis)", utf8RuneCount(got))
				}
				return
			}
			if got != c.want {
				t.Errorf("redactForDisplay(%q) = %q, want %q", c.in, got, c.want)
			}
			for _, f := range c.forbid {
				if strings.Contains(got, f) {
					t.Errorf("redactForDisplay(%q) = %q contains forbidden substring %q (CREDENTIAL LEAK)",
						c.in, got, f)
				}
			}
		})
	}
}

// fakeProvider satisfies GitProvider for tests. F-50 introduced
// the GitProvider abstraction; tests inject via the Detect
// field — Detect returns the fakeProvider regardless of URL
// hint.
type fakeProvider struct {
	issue *Issue
	err   error
}

func (f *fakeProvider) Kind() ProviderKind  { return ProviderGitHub }
func (f *fakeProvider) Host() string        { return "github.com" }
func (f *fakeProvider) Version() string     { return "" }
func (f *fakeProvider) GetIssue(_ context.Context, _, _ string, id int) (*Issue, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.issue == nil {
		return &Issue{ID: id, Title: "fake", State: "open", Labels: []string{LabelWIP}}, nil
	}
	// Return a copy with the requested ID stamped in.
	c := *f.issue
	c.ID = id
	return &c, nil
}
func (f *fakeProvider) AddLabel(context.Context, string, string, int, string) error {
	return nil
}
func (f *fakeProvider) RemoveLabel(context.Context, string, string, int, string) error {
	return nil
}

// ---- full happy path ---------------------------------------------

// TestRunFix_HappyPath exercises the entire §5.2 main flow:
// preflight → fetch issue → add label → create worktree →
// switch cwd → write gtwContext → render success card.
//
// It uses a real GitRunner wrapping `git` against a temporary
// repo so the worktree add is exercised end-to-end. Skipped if
// git is not on PATH.
func TestRunFix_HappyPath(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; skipping end-to-end happy path")
	}

	// Set up a tiny git repo with a github remote. Resolve the
	// temp dir through EvalSymlinks so we agree with git on the
	// canonical path (macOS temp dirs live under /private/var/...,
	// which differs from what t.TempDir() returns).
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"remote", "add", "origin", "https://github.com/cnlangzi/nightme"},
		{"commit", "--allow-empty", "-q", "-m", "initial"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	sent := []OutMsg{}
	provider := &fakeProvider{issue: &Issue{ID: 42, Title: "Login state expiration", State: "open"}}
	sender := &fakeSender{cwd: dir}
	slot := &fakeContextSlot{}
	drafts := &fakeDraftsMap{}
	deps := HandlerDeps{
		Send: func(_ context.Context, m OutMsg) error { sent = append(sent, m); return nil },
		Git:  ExecGitRunner{},
		// F-50 §1.4 migration: tests inject via the Detect field —
		// Detect returns the fakeProvider regardless of URL hint.
		Detect: func(_ context.Context, _ string, _ HTTPProber) (GitProvider, error) {
			return provider, nil
		},
		Now: func() time.Time {
			return time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
		},
	}

	res, err := RunFix(context.Background(), sender, slot, drafts, deps,
		"chat-1", "msg-1", []string{"42"})
	if err != nil {
		t.Fatalf("RunFix: %v", err)
	}
	if !res.Consumed {
		t.Fatalf("RunFix result = %+v, want Consumed=true", res)
	}

	// gtwContext populated correctly.
	got := slot.Load()
	if got.Issue != 42 {
		t.Errorf("ctx.Issue = %d, want 42", got.Issue)
	}
	if got.Branch != "fix/42-login-state-expiration" {
		t.Errorf("ctx.Branch = %q", got.Branch)
	}
	if got.State != StateFixing {
		t.Errorf("ctx.State = %q, want %q", got.State, StateFixing)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("ctx.UpdatedAt is zero")
	}

	// CWD switched to the new worktree.
	wantWorktree := filepath.Join(filepath.Dir(dir), filepath.Base(dir)+".nightme", "42-login-state-expiration")
	if sender.cwd != wantWorktree {
		t.Errorf("sender.cwd = %q, want %q", sender.cwd, wantWorktree)
	}

	// One success card sent (plus the label-ok path doesn't send
	// anything when AddLabel succeeds; the AddLabel-fail branch
	// would send a warning card).
	if len(sent) != 1 {
		t.Fatalf("Send count = %d, want 1; sent = %+v", len(sent), sent)
	}
	if !strings.Contains(sent[0].Text, "Fix #42") {
		t.Errorf("card text = %q, want containing 'Fix #42'", sent[0].Text)
	}
	if sent[0].ReplyTo != "msg-1" {
		t.Errorf("ReplyTo = %q, want msg-1 (thread under user command)", sent[0].ReplyTo)
	}
}

// TestRunFix_DaemonRecovery exercises the §5.7 path: cwd is inside
// a worktree holding `fix/42-...` but the in-memory gtwContext is
// empty. RunFix should detect this via RebuildContext, restore
// the context, and emit a "Recovered" card instead of creating a
// duplicate worktree.
func TestRunFix_DaemonRecovery(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; skipping daemon-recovery test")
	}

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	wtPath := filepath.Join(dir, "wt")
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"remote", "add", "origin", "https://github.com/cnlangzi/nightme"},
		{"commit", "--allow-empty", "-q", "-m", "initial"},
		// Worktree add creates the branch as a side effect — we
		// don't pre-create it (would cause "branch already exists").
		{"worktree", "add", "-b", "fix/42-recovery-test", wtPath, "HEAD"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	sent := []OutMsg{}
	sender := &fakeSender{cwd: wtPath}
	slot := &fakeContextSlot{}
	deps := HandlerDeps{
		Send: func(_ context.Context, m OutMsg) error { sent = append(sent, m); return nil },
		Git:  ExecGitRunner{},
		Detect: func(_ context.Context, _ string, _ HTTPProber) (GitProvider, error) {
			return &fakeProvider{issue: &Issue{ID: 42, Title: "recovery test", State: "open"}}, nil
		},
		Now: func() time.Time { return time.Now() },
	}

	// Slot is empty (post-restart) — RunFix should rebuild.
	res, err := RunFix(context.Background(), sender, slot, &fakeDraftsMap{}, deps,
		"chat-1", "msg-1", []string{"99"}) // 99 != 42; rebuild should win
	if err != nil {
		t.Fatalf("RunFix: %v", err)
	}
	if !res.Consumed {
		t.Fatal("RunFix not Consumed")
	}
	// Slot was populated with the rebuilt context for issue 42,
	// not the requested 99.
	got := slot.Load()
	if got.Issue != 42 {
		t.Errorf("ctx.Issue = %d, want 42 (rebuilt from branch)", got.Issue)
	}
	if got.Branch != "fix/42-recovery-test" {
		t.Errorf("ctx.Branch = %q, want fix/42-recovery-test", got.Branch)
	}
	// One recovery card was sent.
	if len(sent) != 1 {
		t.Fatalf("Send count = %d, want 1", len(sent))
	}
	if !strings.Contains(sent[0].Text, "Recovered") {
		t.Errorf("card text = %q, want containing 'Recovered'", sent[0].Text)
	}
	if !strings.Contains(sent[0].Text, "#42") {
		t.Errorf("card text = %q, want containing '#42'", sent[0].Text)
	}
}

// ---- reaction routing -------------------------------------------

// TestHandleAction_BranchExists_ConfirmCancellation exercises
// the end-to-end reaction flow that the F-45 §5.3.1 card relies
// on: a draft is stored, a ❌ reaction arrives, the draft is
// taken, the label is rolled back, and a cancellation card is
// sent. We use the fakeGit from earlier + a recording Send to
// observe the side effects without real git/gh.
func TestHandleAction_BranchExists_ConfirmCancellation(t *testing.T) {
	sent := []OutMsg{}
	drafts := &fakeDraftsMap{}
	slot := &fakeContextSlot{}
	cs := &fakeSender{cwd: "/code/nightme"}
	deps := HandlerDeps{
		Send: func(_ context.Context, m OutMsg) error { sent = append(sent, m); return nil },
		Git:  &fakeGit{},
		Detect: func(_ context.Context, _ string, _ HTTPProber) (GitProvider, error) {
			return &fakeProvider{}, nil
		},
	}

	// Pre-populate a branch-exists draft (the kind RunFix
	// would have stored when the worktree add hit a name
	// collision). LabelAdded is false in this path — the
	// label hasn't been added yet at the time the card is
	// emitted, so ❌ should NOT call RemoveLabel.
	drafts.Store("om_card_msg", &Draft{
		Kind: DraftFixBranchExists,
		Payload: FixDraftPayload{
			IssueID:  42,
			Branch:   "fix/42-login-state-expiration",
			Slug:     "42-login-state-expiration",
			Repo:     "cnlangzi/nightme",
			Provider: "github",
			ChatID:   "chat-1",
		},
	})

	consumed, err := HandleAction(context.Background(), deps, cs, slot, drafts, ReactionEvent{
		TargetMsgID: "om_card_msg",
		Emoji:       string(ReactionCancel),
		UserID:      "ou_user_1",
		ChatID:      "chat-1",
	})
	if err != nil {
		t.Fatalf("HandleAction: %v", err)
	}
	if !consumed {
		t.Error("consumed = false, want true (draft matched)")
	}
	// Draft must be taken (one-shot per reaction).
	if got := drafts.Lookup("om_card_msg"); got != nil {
		t.Errorf("draft not taken: %+v", got)
	}
	// One cancellation card sent.
	if len(sent) != 1 {
		t.Fatalf("Send count = %d, want 1; sent = %+v", len(sent), sent)
	}
	if !strings.Contains(sent[0].Text, "Cancelled fix #42") {
		t.Errorf("card text = %q, want containing 'Cancelled fix #42'", sent[0].Text)
	}
}

// TestHandleAction_NoDraftFallsThrough verifies the
// non-consumption path: a reaction on a non-gtw message
// returns (false, nil) so the caller can fall through to
// future handlers (none today; placeholder for F-31+).
func TestHandleAction_NoDraftFallsThrough(t *testing.T) {
	cs := &fakeSender{cwd: "/code/nightme"}
	deps := HandlerDeps{
		Send: func(_ context.Context, _ OutMsg) error { return nil },
		Git:  &fakeGit{},
		Detect: func(_ context.Context, _ string, _ HTTPProber) (GitProvider, error) {
			return nil, nil
		},
	}
	consumed, _ := HandleAction(context.Background(), deps, cs,
		&fakeContextSlot{}, &fakeDraftsMap{},
		ReactionEvent{TargetMsgID: "om_random", Emoji: "👍"},
	)
	if consumed {
		t.Error("consumed = true, want false (no draft matched)")
	}
}

// TestHandleAction_EmptyTargetMsgIDIgnored verifies the
// defensive early-return for malformed events (e.g. SDK
// delivering a half-parsed reaction).
func TestHandleAction_EmptyTargetMsgIDIgnored(t *testing.T) {
	cs := &fakeSender{cwd: "/code/nightme"}
	deps := HandlerDeps{
		Send: func(_ context.Context, _ OutMsg) error { return nil },
		Git:  &fakeGit{},
	}
	consumed, _ := HandleAction(context.Background(), deps, cs,
		&fakeContextSlot{}, &fakeDraftsMap{},
		ReactionEvent{TargetMsgID: "", Emoji: "✅"},
	)
	if consumed {
		t.Error("consumed = true, want false (empty target)")
	}
}

// ---- branch-exists path uses fake ----------------------------------

func TestBranchExists_True(t *testing.T) {
	g := &fakeGit{responses: map[string]fakeGitResp{
		"show-ref --verify --quiet refs/heads/fix/42-foo": {},
	}}
	exists, err := BranchExists(context.Background(), "/code/nightme", "fix/42-foo", g)
	if err != nil {
		t.Fatalf("BranchExists: %v", err)
	}
	if !exists {
		t.Fatal("BranchExists = false, want true")
	}
}

func TestBranchExists_FalseOnMiss(t *testing.T) {
	// "Miss" is signalled by a non-zero exit with EMPTY stderr
	// (git --quiet suppresses all output on a clean miss). A
	// non-empty stderr indicates a real error.
	g := &fakeGit{responses: map[string]fakeGitResp{
		"show-ref --verify --quiet refs/heads/fix/99-missing": {
			err: &fakeExitErr{code: 1},
		},
	}}
	exists, err := BranchExists(context.Background(), "/code/nightme", "fix/99-missing", g)
	if err != nil {
		t.Fatalf("BranchExists: %v", err)
	}
	if exists {
		t.Fatal("BranchExists = true, want false")
	}
}

func TestBranchExists_RealErrorBubblesUp(t *testing.T) {
	g := &fakeGit{responses: map[string]fakeGitResp{
		"show-ref --verify --quiet refs/heads/fix/99-broken": {
			stderr: "fatal: bad ref",
			err:    &fakeExitErr{code: 128},
		},
	}}
	_, err := BranchExists(context.Background(), "/code/nightme", "fix/99-broken", g)
	if err == nil {
		t.Fatal("BranchExists = nil err, want real error")
	}
}

// ---- rebuild further: branch on fix/<id>-* finds matching issue ---

func TestRebuildContext_FoundIssue(t *testing.T) {
	g := &fakeGit{responses: map[string]fakeGitResp{
		"symbolic-ref --short HEAD":            {stdout: "fix/42-login-state-expiration\n"},
		"remote get-url origin":                {stdout: "git@github.com:cnlangzi/nightme.git\n"},
	}}
	cs := &fakeSender{cwd: "/code/nightme.nightme/42-login-state-expiration"}
	ctx := RebuildContext(context.Background(), cs, g, nil, nil)
	if ctx == (Context{}) {
		t.Fatal("RebuildContext = zero, want populated")
	}
	if ctx.Issue != 42 {
		t.Errorf("ctx.Issue = %d, want 42", ctx.Issue)
	}
	if ctx.Branch != "fix/42-login-state-expiration" {
		t.Errorf("ctx.Branch = %q, want fix/42-login-state-expiration", ctx.Branch)
	}
	if ctx.State != StateFixing {
		t.Errorf("ctx.State = %q, want %q", ctx.State, StateFixing)
	}
}

// ---- handler-deps test (smoke) ------------------------------------

func TestRunFix_UsageWhenNoArgs(t *testing.T) {
	sent := []OutMsg{}
	deps := HandlerDeps{
		Send: func(_ context.Context, m OutMsg) error { sent = append(sent, m); return nil },
		Git:  &fakeGit{},
	}
	res, err := RunFix(context.Background(), &fakeSender{cwd: "/code/nightme"},
		&fakeContextSlot{}, &fakeDraftsMap{}, deps,
		"chat-1", "msg-1", []string{})
	if err != nil {
		t.Fatalf("RunFix: %v", err)
	}
	if res == nil || !res.Consumed {
		t.Fatalf("RunFix result = %+v, want Consumed=true", res)
	}
	if len(sent) != 1 {
		t.Fatalf("Send count = %d, want 1", len(sent))
	}
	if !strings.Contains(sent[0].Text, "Usage") {
		t.Fatalf("reply text = %q, want containing 'Usage'", sent[0].Text)
	}
}

func TestRunFix_PreflightRequiresCwd(t *testing.T) {
	sent := []OutMsg{}
	deps := HandlerDeps{
		Send: func(_ context.Context, m OutMsg) error { sent = append(sent, m); return nil },
		Git:  &fakeGit{},
	}
	res, _ := RunFix(context.Background(), &fakeSender{cwd: ""},
		&fakeContextSlot{}, &fakeDraftsMap{}, deps,
		"chat-1", "msg-1", []string{"42"})
	if res == nil || !res.Consumed {
		t.Fatalf("RunFix result = %+v, want Consumed=true", res)
	}
	if len(sent) != 1 || !strings.Contains(sent[0].Text, "No active workspace") {
		t.Fatalf("expected 'No active workspace' reply, got %+v", sent)
	}
}

// fakeContextSlot / fakeDraftsMap satisfy the package's interfaces.
type fakeContextSlot struct {
	c Context
}

func (f *fakeContextSlot) Load() Context   { return f.c }
func (f *fakeContextSlot) Store(c Context) { f.c = c }

type fakeDraftsMap struct {
	m map[string]*Draft
}

func (f *fakeDraftsMap) Store(id string, d *Draft) {
	if f.m == nil {
		f.m = map[string]*Draft{}
	}
	f.m[id] = d
}
func (f *fakeDraftsMap) Take(id string) *Draft {
	d, ok := f.m[id]
	if !ok {
		return nil
	}
	delete(f.m, id)
	return d
}
func (f *fakeDraftsMap) Lookup(id string) *Draft { return f.m[id] }

// suppress unused-time warning when time isn't referenced.
var _ = time.Now
