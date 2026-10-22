package gtw

// Tests for Detect's failure classification (unreachable host vs
// unrecognised platform vs malformed URL) and for the per-host
// detection cache the /gtw call sites share.
//
// The cache exists because one host appears in every repo a user
// owns, and each /gtw dispatch — plus every per-stamp CollectPR —
// used to re-probe it. The tests assert on the number of Probe
// calls rather than on wall-clock, which is what the cache
// actually controls.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// proberStub answers each probed path from a scripted function and
// records the paths it was asked for, in order, so a test can
// assert both the verdict and how many round-trips detection spent.
type proberStub struct {
	mu    sync.Mutex
	paths []string
	reply func(host, path string) ([]byte, error)
}

func (p *proberStub) Probe(_ context.Context, host, path string) ([]byte, error) {
	p.mu.Lock()
	p.paths = append(p.paths, host+" "+path)
	reply := p.reply
	p.mu.Unlock()
	return reply(host, path)
}

func (p *proberStub) calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.paths...)
}

// answerAll returns a stub that answers every path with body.
func answerAll(body string) *proberStub {
	return &proberStub{reply: func(_, _ string) ([]byte, error) { return []byte(body), nil }}
}

// unreachable answers every path with a transport-level failure,
// the shape a daemon on a network that can't reach the host sees.
func unreachable() *proberStub {
	return &proberStub{
		reply: func(_, _ string) ([]byte, error) {
			return nil, errors.New("dial tcp 10.0.0.9:443: i/o timeout")
		},
	}
}

// TestDetect_UnreachableHostIsNotUnsupported is the core of the
// fix behind #445: a host the daemon can't dial used to surface as
// "unsupported git platform", sending users to hunt for a platform
// adapter that was never missing. Both probes failing at the
// transport layer means the host never answered, which is its own
// diagnosis.
func TestDetect_UnreachableHostIsNotUnsupported(t *testing.T) {
	_, err := Detect(context.Background(), "ssh://git@10.0.0.9/f9/f9netcore.git", unreachable(), "")
	if err == nil {
		t.Fatal("Detect: want unreachable error, got nil")
	}
	if !errors.Is(err, ErrProviderUnreachable) {
		t.Errorf("Detect: want ErrProviderUnreachable, got %v", err)
	}
	if errors.Is(err, ErrUnsupportedProvider) {
		t.Errorf("Detect: unreachable host must not be ErrUnsupportedProvider, got %v", err)
	}
	if errors.Is(err, ErrInvalidRemoteURL) {
		t.Errorf("Detect: unreachable host must not be ErrInvalidRemoteURL, got %v", err)
	}
}

// TestDetect_UnreachableErrorCarriesProbeCause pins that the reply
// can name the actual network failure. "both https and http failed"
// on its own doesn't tell anyone whether to look at DNS, the proxy
// or the firewall; the dial error underneath does. Both endpoints'
// causes are carried — neither is the interesting one on its own.
func TestDetect_UnreachableErrorCarriesProbeCause(t *testing.T) {
	stub := unreachable()
	_, err := Detect(context.Background(), "ssh://git@10.0.0.9/f9/f9netcore.git", stub, "")
	if err == nil {
		t.Fatal("Detect: want error, got nil")
	}
	if !strings.Contains(err.Error(), "i/o timeout") {
		t.Errorf("Detect error %q: want the transport cause", err)
	}
	if got := strings.Count(err.Error(), "i/o timeout"); got != 2 {
		t.Errorf("Detect error %q: want both probes' causes, got %d", err, got)
	}
	if got := stub.calls(); len(got) != 2 {
		t.Errorf("Detect probed %v, want both endpoints", got)
	}
}

// TestProbe_UnreachableReasonSurvivesBothSchemes checks the
// production prober end to end: against a closed port its error
// keeps the per-scheme causes that Detect forwards to the user.
//
// The cause is asserted through errors.As / errors.Is rather than by
// matching text. The wording is the OS's, not ours — Linux renders
// ECONNREFUSED as "connect: connection refused", Windows as
// "connectex: No connection could be made because the target machine
// actively refused it" — so a text assertion here only ever passes on
// the platform it was written on.
func TestProbe_UnreachableReasonSurvivesBothSchemes(t *testing.T) {
	// 127.0.0.1:1 is reserved and refuses connections, so both the
	// HTTPS and the HTTP attempt fail without touching a network.
	const deadHost = "127.0.0.1:1"
	_, err := Detect(context.Background(), "ssh://git@"+deadHost+"/f9/f9netcore.git",
		&ExecHTTPProber{Timeout: 2 * time.Second}, "")
	if !errors.Is(err, ErrProviderUnreachable) {
		t.Fatalf("Detect: want ErrProviderUnreachable, got %v", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "both https and http failed") {
		t.Errorf("Detect error %q: want the scheme-fallback summary", msg)
	}

	// Both schemes' failures must be reachable programmatically, and
	// both must name the endpoint they came from — that is what lets
	// the reply say which path was tried.
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		t.Fatalf("Detect error %v: want a *url.Error in the chain — the per-scheme cause was formatted as text", err)
	}
	if !strings.Contains(urlErr.URL, "/api/v4/version") {
		t.Errorf("Detect error %q: want the /api/v4/version endpoint named", urlErr.URL)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("Detect error %v: want ECONNREFUSED reachable via errors.Is", err)
	}
}

// TestDetect_HostAnsweredButUnrecognised keeps the meaning of
// ErrUnsupportedProvider honest: the host responded, it just isn't
// GitHub or GitLab. A 404 HTML login page on both endpoints used to
// be indistinguishable from a dead host.
func TestDetect_HostAnsweredButUnrecognised(t *testing.T) {
	stub := answerAll("<html>login</html>")
	_, err := Detect(context.Background(), "ssh://git@gateway.acme/f9/f9netcore.git", stub, "")
	if !errors.Is(err, ErrUnsupportedProvider) {
		t.Fatalf("Detect: want ErrUnsupportedProvider, got %v", err)
	}
	if errors.Is(err, ErrProviderUnreachable) {
		t.Errorf("Detect: a host that answered must not be ErrProviderUnreachable, got %v", err)
	}
	if got := stub.calls(); len(got) != 2 {
		t.Errorf("Detect probed %v, want both endpoints", got)
	}
}

// TestDetect_ServerErrorIsNotUnreachable keeps a live 5xx out of the
// unreachable bucket. probeOnce deliberately fails on 5xx so the
// scheme fallback moves on, but the host DID answer — a reverse proxy
// returning 502, a server mid-deploy — so "git host unreachable"
// would send the user to debug the network instead of the server.
func TestDetect_ServerErrorIsNotUnreachable(t *testing.T) {
	stub := &proberStub{reply: func(_, path string) ([]byte, error) {
		return nil, fmt.Errorf("%w: GET https://host%s: 502 Bad Gateway", errProbeStatus, path)
	}}
	_, err := Detect(context.Background(), "ssh://git@gateway.acme/team/repo.git", stub, "")
	if !errors.Is(err, ErrUnsupportedProvider) {
		t.Fatalf("Detect: want ErrUnsupportedProvider when the server answers 5xx, got %v", err)
	}
	if errors.Is(err, ErrProviderUnreachable) {
		t.Errorf("Detect: a 5xx is an answer, not silence — got %v", err)
	}
}

// TestDetectProvider_URLHintIsNotCachedHostWide stops Detect's Stage A
// from poisoning the host-keyed cache. The URL hint matches on the
// whole raw remote URL, so a path segment containing "gitlab" decides
// that one remote only; memoising it host-wide would hand the same
// verdict to every other repo on the host.
func TestDetectProvider_URLHintIsNotCachedHostWide(t *testing.T) {
	stub := &proberStub{reply: func(_, path string) ([]byte, error) {
		if path == "/api/v3/meta" {
			return []byte(`{"verifiable_password_authentication":true}`), nil
		}
		return []byte(`<html>not gitlab</html>`), nil
	}}
	deps := HandlerDeps{Prober: stub, ProviderCache: &ProviderCache{}}

	// Stage A: "gitlab" in the path — answered without any probe.
	hinted, err := detectProvider(context.Background(), deps,
		"ssh://git@ghe.acme/orgs/gitlab-mirror/foo.git", "/tmp/a")
	if err != nil {
		t.Fatalf("hinted detect: %v", err)
	}
	if hinted.Kind() != ProviderGitLab {
		t.Fatalf("hinted detect: kind = %q, want gitlab (Stage A)", hinted.Kind())
	}
	if got := len(stub.calls()); got != 0 {
		t.Fatalf("Stage A probed %v, want no network", stub.calls())
	}

	// Same host, no hint — must reach the probe, not inherit GitLab
	// from the cached URL-hint verdict.
	probed, err := detectProvider(context.Background(), deps,
		"ssh://git@ghe.acme/orgs/bar.git", "/tmp/a")
	if err != nil {
		t.Fatalf("probed detect: %v", err)
	}
	if probed.Kind() != ProviderGitHub {
		t.Fatalf("probed detect: kind = %q, want github — the Stage A hint leaked into the host cache", probed.Kind())
	}
}

// TestDetect_GitHubEnterpriseSurvivesGitLabProbeFailure guards the
// classification against over-reach: a dead /api/v4 says nothing
// about /api/v3, and GitHub Enterprise answers there.
func TestDetect_GitHubEnterpriseSurvivesGitLabProbeFailure(t *testing.T) {
	stub := &proberStub{reply: func(_, path string) ([]byte, error) {
		if path == "/api/v4/version" {
			return nil, errors.New("dial tcp: i/o timeout")
		}
		return []byte(`{"verifiable_password_authentication":true}`), nil
	}}
	prov, err := Detect(context.Background(), "ssh://git@ghe.acme/team/repo.git", stub, "")
	if err != nil {
		t.Fatalf("Detect: want GitHub, got err: %v", err)
	}
	if prov.Kind() != ProviderGitHub {
		t.Fatalf("Detect.Kind: want github, got %q", prov.Kind())
	}
}

// TestDetect_MalformedURLStillWinsOverUnreachable keeps the D3
// split: a malformed URL is the user's typo, not the network's
// fault, and must not be reported as either.
func TestDetect_MalformedURLStillWinsOverUnreachable(t *testing.T) {
	_, err := Detect(context.Background(), "not-a-url", unreachable(), "")
	if !errors.Is(err, ErrInvalidRemoteURL) {
		t.Fatalf("Detect: want ErrInvalidRemoteURL, got %v", err)
	}
}

// TestDetectProvider_SecondCallSameHostDoesNotProbe is the cache's
// headline case: the same host, asked twice, is probed once.
func TestDetectProvider_SecondCallSameHostDoesNotProbe(t *testing.T) {
	stub := answerAll(`{"version":"16.5.0"}`)
	deps := HandlerDeps{Prober: stub, ProviderCache: &ProviderCache{}}
	const remote = "ssh://git@sc.acme/f9/f9netcore.git"

	for i := range 2 {
		prov, err := detectProvider(context.Background(), deps, remote, "/tmp/a")
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if prov.Kind() != ProviderGitLab {
			t.Fatalf("call %d: kind = %q, want gitlab", i, prov.Kind())
		}
	}
	if got := len(stub.calls()); got != 1 {
		t.Fatalf("Probe calls = %d, want 1 (second call must hit the cache)", got)
	}
}

// TestDetectProvider_CacheSharedAcrossReposOnOneHost covers the
// shape that made this expensive: several repos of the same group,
// each detected from its own working directory.
func TestDetectProvider_CacheSharedAcrossReposOnOneHost(t *testing.T) {
	stub := answerAll(`{"version":"16.5.0"}`)
	deps := HandlerDeps{Prober: stub, ProviderCache: &ProviderCache{}}

	for _, remote := range []string{
		"ssh://git@sc.acme/f9/a.git",
		"ssh://git@sc.acme/f9/b.git",
	} {
		prov, err := detectProvider(context.Background(), deps, remote, "/tmp/work")
		if err != nil {
			t.Fatalf("detect %s: %v", remote, err)
		}
		if prov.Kind() != ProviderGitLab {
			t.Fatalf("detect %s: kind = %q, want gitlab", remote, prov.Kind())
		}
	}
	if got := len(stub.calls()); got != 1 {
		t.Fatalf("Probe calls = %d, want 1 across both repos on the host", got)
	}
}

// TestDetectProvider_OriginSwitchProbesAgain keeps the key honest:
// re-pointing origin at another host is a different platform
// question and must not read the old host's answer.
func TestDetectProvider_OriginSwitchProbesAgain(t *testing.T) {
	stub := &proberStub{reply: func(host, path string) ([]byte, error) {
		switch {
		case host == "sc.acme" && path == "/api/v4/version":
			return []byte(`{"version":"16.5.0"}`), nil
		case host == "ghe.acme" && path == "/api/v3/meta":
			return []byte(`{"verifiable_password_authentication":true}`), nil
		}
		return []byte(`<html>not a platform API</html>`), nil
	}}
	deps := HandlerDeps{Prober: stub, ProviderCache: &ProviderCache{}}

	first, err := detectProvider(context.Background(), deps, "ssh://git@sc.acme/f9/a.git", "/tmp/a")
	if err != nil {
		t.Fatalf("first detect: %v", err)
	}
	second, err := detectProvider(context.Background(), deps, "ssh://git@ghe.acme/team/repo.git", "/tmp/b")
	if err != nil {
		t.Fatalf("second detect: %v", err)
	}
	if first.Kind() != ProviderGitLab || second.Kind() != ProviderGitHub {
		t.Fatalf("kinds = %q / %q, want gitlab / github", first.Kind(), second.Kind())
	}
	// One probe for the GitLab host, two for the GitHub Enterprise
	// one (its /api/v4 misses first). A cache keyed on the previous
	// host instead of the new one would report 1.
	if got := len(stub.calls()); got != 3 {
		t.Fatalf("Probe calls = %d (%v), want 3 — the new host must be probed", got, stub.calls())
	}
}

// TestDetectProvider_FailureIsNotCached is the half that keeps the
// cache honest: a host that was unreachable a moment ago may be
// reachable now, and these paths are user-triggered — the next
// command must get a fresh answer rather than a replayed failure.
func TestDetectProvider_FailureIsNotCached(t *testing.T) {
	attempts := 0
	stub := &proberStub{reply: func(_, _ string) ([]byte, error) {
		attempts++
		if attempts <= 2 {
			return nil, errors.New("dial tcp 10.0.0.9:443: i/o timeout")
		}
		return []byte(`{"version":"16.5.0"}`), nil
	}}
	deps := HandlerDeps{Prober: stub, ProviderCache: &ProviderCache{}}
	const remote = "ssh://git@10.0.0.9/f9/f9netcore.git"

	if _, err := detectProvider(context.Background(), deps, remote, "/tmp/a"); !errors.Is(err, ErrProviderUnreachable) {
		t.Fatalf("first call: want ErrProviderUnreachable, got %v", err)
	}
	prov, err := detectProvider(context.Background(), deps, remote, "/tmp/a")
	if err != nil {
		t.Fatalf("second call: want the host retried and recognised, got %v", err)
	}
	if prov.Kind() != ProviderGitLab {
		t.Fatalf("second call: kind = %q, want gitlab", prov.Kind())
	}
	// Two probes on the failed attempt (/api/v4 then /api/v3), one
	// on the retry — the failed verdict was never memoised.
	if got := len(stub.calls()); got != 3 {
		t.Fatalf("Probe calls = %d (%v), want 3", got, stub.calls())
	}
}

// TestDetectProvider_CacheHitRebindsWorktree pins why the cache
// stores an identity rather than the provider instance: Worktree is
// per-call, so a memoised instance would drag the first caller's
// directory into every later gh / glab invocation.
func TestDetectProvider_CacheHitRebindsWorktree(t *testing.T) {
	deps := HandlerDeps{Prober: answerAll(`{"version":"16.5.0"}`), ProviderCache: &ProviderCache{}}
	const remote = "ssh://git@sc.acme/f9/f9netcore.git"

	for _, want := range []string{"/tmp/a", "/tmp/b"} {
		prov, err := detectProvider(context.Background(), deps, remote, want)
		if err != nil {
			t.Fatalf("detect: %v", err)
		}
		gl, ok := prov.(*GitLabProvider)
		if !ok {
			t.Fatalf("detect: want *GitLabProvider, got %T", prov)
		}
		if gl.Worktree != want {
			t.Fatalf("Worktree = %q, want %q", gl.Worktree, want)
		}
	}
}

// TestDetectProvider_NilCacheIsInert proves R11: a HandlerDeps
// without a ProviderCache behaves exactly as it did before the
// cache existed — every call probes, and nothing panics.
func TestDetectProvider_NilCacheIsInert(t *testing.T) {
	stub := answerAll(`{"version":"16.5.0"}`)
	deps := HandlerDeps{Prober: stub}

	for range 2 {
		if _, err := detectProvider(context.Background(), deps, "ssh://git@sc.acme/f9/a.git", "/tmp/a"); err != nil {
			t.Fatalf("detect with nil cache: %v", err)
		}
	}
	if got := len(stub.calls()); got != 2 {
		t.Fatalf("Probe calls = %d, want 2 (nil cache re-probes every time)", got)
	}
}

// TestRunFixRemote_UnreachableHostSaysSo is the /gtw fix half of
// the user-visible contract: the reply names reachability and the
// transport cause. A user whose daemon can't dial their GitLab
// should not be told the platform is unsupported.
func TestRunFixRemote_UnreachableHostSaysSo(t *testing.T) {
	rig := newFixRemoteRig(t)
	mustGit(t, rig.repoRoot, "remote", "set-url", "origin", "ssh://git@10.0.0.9/f9/f9netcore.git")
	rig.deps.Detect = nil
	rig.deps.Prober = unreachable()

	if _, err := rig.drive(t, "42"); err != nil {
		t.Fatalf("RunFix: %v", err)
	}
	text := rig.rec.lastText()
	if !strings.Contains(text, "无法连接 Git 主机") {
		t.Fatalf("reply = %q, want the unreachable-host message", text)
	}
	if !strings.Contains(text, "i/o timeout") {
		t.Errorf("reply = %q, want the transport cause", text)
	}
	if strings.Contains(text, "暂不支持的 Git 平台") {
		t.Errorf("reply = %q, must not claim the platform is unsupported", text)
	}
}

// TestResolveProvider_UnreachableNamesTheNetworkFailure is the
// user-visible half on the /gtw pr (and /gtw new) path: the reply
// must point at reachability and carry the transport cause, not the
// "we don't support this platform" text.
func TestResolveProvider_UnreachableNamesTheNetworkFailure(t *testing.T) {
	g := &probeGit{}
	g.on([]string{"remote", "get-url", "origin"}, "ssh://git@10.0.0.9/f9/f9netcore.git\n", "", nil)
	deps := HandlerDeps{Git: g, Prober: unreachable(), ProviderCache: &ProviderCache{}}

	_, _, _, err := resolveProvider(context.Background(), Context{RepoRoot: "/tmp/a", Worktree: "/tmp/a"}, deps)
	if err == nil {
		t.Fatal("resolveProvider: want error, got nil")
	}
	if !errors.Is(err, ErrProviderUnreachable) {
		t.Fatalf("resolveProvider: want ErrProviderUnreachable, got %v", err)
	}
	msg := err.Error()
	if strings.Contains(msg, "unsupported git platform") {
		t.Errorf("resolveProvider error %q: must not claim the platform is unsupported", msg)
	}
	if !strings.Contains(msg, "i/o timeout") {
		t.Errorf("resolveProvider error %q: want the transport cause", msg)
	}
}

// TestResolveProviderAndCollectPR_ShareOneDetection is R10: the
// dispatch path and the per-stamp refresh path answer the same
// question about the same repo, so the second one must not probe
// again.
//
// The remote URL is deliberately shaped so ParseRepoOwner can't
// split it — CollectPR returns fail-soft before it would spawn
// `glab`, keeping the test hermetic while still running both call
// sites' detection.
func TestResolveProviderAndCollectPR_ShareOneDetection(t *testing.T) {
	g := &probeGit{}
	g.on([]string{"remote", "get-url", "origin"}, "ssh://git@git.acme/repo.git\n", "", nil)
	stub := answerAll(`{"version":"16.5.0"}`)
	deps := HandlerDeps{Git: g, Prober: stub, ProviderCache: &ProviderCache{}}

	if _, err := CollectPR(context.Background(), "/tmp/a", "feature/x", deps); err != nil {
		t.Fatalf("CollectPR: %v", err)
	}
	_, _, _, err := resolveProvider(context.Background(),
		Context{RepoRoot: "/tmp/a", Worktree: "/tmp/a"}, deps)
	if err == nil {
		t.Fatal("resolveProvider: want error from the unparsable repo path, got nil")
	}
	if !strings.Contains(err.Error(), "cannot parse owner/repo") {
		t.Fatalf("resolveProvider: unexpected error %v", err)
	}
	if got := len(stub.calls()); got != 1 {
		t.Fatalf("Probe calls = %d, want 1 shared across both call sites", got)
	}
}

// TestCollectPR_NilProviderCacheDoesNotPanic is the R11 acceptance
// for the stamp path specifically — CollectPR runs from a
// background goroutine on every stamp, where a panic would take the
// daemon down rather than fail one refresh.
func TestCollectPR_NilProviderCacheDoesNotPanic(t *testing.T) {
	g := &probeGit{}
	g.on([]string{"remote", "get-url", "origin"}, "ssh://git@git.acme/repo.git\n", "", nil)
	deps := HandlerDeps{Git: g, Prober: answerAll(`{"version":"16.5.0"}`)}

	pr, err := CollectPR(context.Background(), "/tmp/a", "feature/x", deps)
	if err != nil {
		t.Fatalf("CollectPR: %v", err)
	}
	if pr != nil {
		t.Fatalf("CollectPR: want nil PR from the fail-soft owner/repo parse, got %+v", pr)
	}
}

// TestUnreachableError_WrapsSentinelAndCause pins the error type's
// contract in isolation: callers classify with errors.Is, the log
// and the reply read the cause.
func TestUnreachableError_WrapsSentinelAndCause(t *testing.T) {
	cause := errors.New("dial tcp 10.0.0.9:443: connect: connection refused")
	other := errors.New("dial tcp 10.0.0.9:80: connect: connection refused")
	err := error(&unreachableError{errs: []error{cause, other}})

	if !errors.Is(err, ErrProviderUnreachable) {
		t.Errorf("want ErrProviderUnreachable in the chain, got %v", err)
	}
	if !errors.Is(err, cause) || !errors.Is(err, other) {
		t.Errorf("want both transport causes in the chain, got %v", err)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("Error() = %q, want the cause text", err)
	}
}

// TestParseRemoteHost_CacheKeyShape documents the key the cache is
// built on: host[:port], no path, no credentials. Two repos on one
// host have to collapse to one entry, and credentials must never
// become part of a map key.
func TestParseRemoteHost_CacheKeyShape(t *testing.T) {
	for _, tc := range []struct{ remote, want string }{
		{"ssh://git@sc.acme/f9/a.git", "sc.acme"},
		{"ssh://git@sc.acme/f9/b.git", "sc.acme"},
		{"https://oauth2:glpat-x@sc.acme:8443/f9/a.git", "sc.acme:8443"},
	} {
		got, err := parseRemoteHost(tc.remote)
		if err != nil {
			t.Fatalf("parseRemoteHost(%q): %v", tc.remote, err)
		}
		if got != tc.want {
			t.Errorf("parseRemoteHost(%q) = %q, want %q", tc.remote, got, tc.want)
		}
	}
}

// TestDetect_StageAHintSkipsProbe is a guard on the cache's
// premise: URL-hint hits stay zero-network even with a cache
// wired, so common-case dispatch doesn't pay for the memo either.
func TestDetect_StageAHintSkipsProbe(t *testing.T) {
	stub := unreachable()
	deps := HandlerDeps{Prober: stub, ProviderCache: &ProviderCache{}}

	prov, err := detectProvider(context.Background(), deps, "https://github.com/cnlangzi/nightme.git", "/tmp/a")
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if prov.Kind() != ProviderGitHub {
		t.Fatalf("kind = %q, want github", prov.Kind())
	}
	if got := len(stub.calls()); got != 0 {
		t.Fatalf("Probe calls = %d, want 0 for a github.com URL hint", got)
	}
}

// TestDetectProviderCache_ConcurrentUse is a race-detector guard:
// the stamp path calls CollectPR from a goroutine while dispatch
// paths call resolveProvider on the request goroutine, so lookup
// and store really do race.
func TestDetectProviderCache_ConcurrentUse(t *testing.T) {
	deps := HandlerDeps{Prober: answerAll(`{"version":"16.5.0"}`), ProviderCache: &ProviderCache{}}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			remote := "ssh://git@sc.acme/f9/repo.git"
			if i%2 == 0 {
				remote = "ssh://git@sc.acme/f9/other.git"
			}
			if _, err := detectProvider(context.Background(), deps, remote, "/tmp/a"); err != nil {
				t.Errorf("detect: %v", err)
			}
		}()
	}
	wg.Wait()
}

// TestDetectProvider_ProberDefaultsToExec pins the collapsed
// nil-Prober fallback: with no prober wired, Detect still runs and
// gets the production prober rather than a nil dereference.
func TestDetectProvider_ProberDefaultsToExec(t *testing.T) {
	deps := HandlerDeps{ProviderCache: &ProviderCache{}}
	// 127.0.0.1:1 refuses instantly, so this exercises the real
	// prober without waiting on a live endpoint.
	_, err := detectProvider(context.Background(), deps, "ssh://git@127.0.0.1:1/f9/f9netcore.git", "/tmp/a")
	if !errors.Is(err, ErrProviderUnreachable) {
		t.Fatalf("detect: want ErrProviderUnreachable from the default prober, got %v", err)
	}
}

// TestDetectProvider_URLHintNotSharedAcrossHosts guards the cache's
// one real edge: a github.com URL pins host github.com, and a
// self-hosted GitLab on its own host is still probed normally.
func TestDetectProvider_URLHintNotSharedAcrossHosts(t *testing.T) {
	stub := answerAll(`{"version":"16.5.0"}`)
	deps := HandlerDeps{Prober: stub, ProviderCache: &ProviderCache{}}

	if _, err := detectProvider(context.Background(), deps, "https://github.com/o/r.git", "/tmp/a"); err != nil {
		t.Fatalf("detect github.com: %v", err)
	}
	prov, err := detectProvider(context.Background(), deps, "ssh://git@selfhosted.acme/f9/r.git", "/tmp/a")
	if err != nil {
		t.Fatalf("detect self-hosted: %v", err)
	}
	if prov.Kind() != ProviderGitLab {
		t.Fatalf("kind = %q, want gitlab", prov.Kind())
	}
	if got := len(stub.calls()); got != 1 {
		t.Fatalf("Probe calls = %d, want 1 (only the self-hosted host is probed)", got)
	}
}
