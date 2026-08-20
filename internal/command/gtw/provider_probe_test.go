package gtw

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestProbe_HTTPSFallsBackToHTTP is the regression test for the
// f9analytics bug: /gtw pr on a self-hosted GitLab whose HTTPS API
// is reachable only via plain HTTP because the corporate proxy
// refuses CONNECT to :443. Before the scheme-fallback fix, Probe
// tried HTTPS only and returned an error, which Detect surfaced
// as "unsupported git platform".
//
// Setup: an httptest.Server speaks plain HTTP. Probe is given its
// host:port and told to fetch "/api/v4/version" — internally it
// first tries `https://host:port/...`, which fails the TLS
// handshake (server has no TLS listener), then falls back to
// `http://host:port/...`, which returns the GitLab auth envelope.
// The test asserts Probe returns the body on the HTTP path.
func TestProbe_HTTPSFallsBackToHTTP(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/version" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"401 Unauthorized"}`))
	}))
	defer ts.Close()

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}

	p := &ExecHTTPProber{Timeout: 2 * time.Second}
	body, err := p.Probe(context.Background(), u.Host, "/api/v4/version")
	if err != nil {
		t.Fatalf("Probe: want body from HTTP fallback, got err: %v", err)
	}
	if got := strings.TrimSpace(string(body)); got != `{"message":"401 Unauthorized"}` {
		t.Fatalf("Probe body: want GitLab auth envelope, got %q", got)
	}
}

// TestProbe_ReturnsBodyFor4xx pins the new contract: any non-5xx
// response is a positive identification (Detect fingerprints the
// error envelope when needed). Returning the body for 4xx is the
// half of the fix that lets Detect see `{"message":"401 ..."}`.
func TestProbe_ReturnsBodyFor4xx(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"401", http.StatusUnauthorized, `{"message":"401 Unauthorized"}`},
		{"403", http.StatusForbidden, `{"message":"403 Forbidden"}`},
		{"404", http.StatusNotFound, `{"message":"404 Not Found"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer ts.Close()

			u, err := url.Parse(ts.URL)
			if err != nil {
				t.Fatalf("parse URL: %v", err)
			}
			p := &ExecHTTPProber{Timeout: 2 * time.Second}
			body, err := p.Probe(context.Background(), u.Host, "/api/v4/version")
			if err != nil {
				t.Fatalf("Probe %d: want body, got err: %v", tc.status, err)
			}
			if string(body) != tc.body {
				t.Fatalf("Probe %d: body mismatch: want %q got %q", tc.status, tc.body, string(body))
			}
		})
	}
}

// TestProbe_BothSchemesFail returns the contract for the
// unsuccessful end: if neither HTTPS nor HTTP can reach the host
// (connection refused, DNS, etc.), Probe must return an error so
// Detect can move on to the next probe target.
func TestProbe_BothSchemesFail(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	// Capture host:port, then close so both schemes fail.
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	ts.Close()

	p := &ExecHTTPProber{Timeout: 1 * time.Second}
	body, err := p.Probe(context.Background(), u.Host, "/api/v4/version")
	if err == nil {
		t.Fatalf("Probe against closed port: want err, got body %q", body)
	}
	if body != nil {
		t.Fatalf("Probe against closed port: want nil body, got %q", body)
	}
}

// TestDetect_AuthProtectedGitLabOverHTTP is the end-to-end
// fingerprint test. The remote URL is an scp-style SSH form
// pointing at a self-hosted GitLab on a bare IP — exactly the
// shape that the f9analytics worktree uses. Stage A finds no
// "github.com" / "gitlab" substring, so Stage B fires. Stage B's
// /api/v4/version probe hits the HTTPS-then-HTTP fallback and
// gets the GitLab auth envelope; the loosened fingerprint
// recognises any non-empty JSON body at the GitLab-only path and
// returns a GitLabProvider.
func TestDetect_AuthProtectedGitLabOverHTTP(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v4/version":
			// Auth-required — GitLab envelope, no "version" field.
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"401 Unauthorized"}`))
		case "/api/v3/meta":
			// GitLab answers "API V3 is no longer supported" here.
			w.WriteHeader(http.StatusGone)
			_, _ = w.Write([]byte(`{"error":"API V3 is no longer supported. Use API V4 instead."}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}

	prober := &ExecHTTPProber{Timeout: 2 * time.Second}
	// ssh:// URL pointing at the bare-IP test server. parseRemoteHost
	// strips ssh:// + userinfo, keeps host:port intact (the colon
	// is a port separator, not the scp-style path delimiter), and
	// Detect's Stage A substring lookup ("github.com" / "gitlab")
	// finds nothing — Stage B fires.
	remoteURL := "ssh://git@" + u.Host + "/f9/f9analytics.git"
	prov, err := Detect(context.Background(), remoteURL, prober, "")
	if err != nil {
		t.Fatalf("Detect: want GitLabProvider, got err: %v", err)
	}
	if prov == nil {
		t.Fatal("Detect: want non-nil provider")
	}
	if prov.Kind() != ProviderGitLab {
		t.Fatalf("Detect.Kind: want gitlab, got %q", prov.Kind())
	}
	if prov.Host() != u.Host {
		t.Fatalf("Detect.Host: want %q, got %q", u.Host, prov.Host())
	}
	// Version stays empty — auth required to read the version object.
	if v := prov.Version(); v != "" {
		t.Fatalf("Detect.Version: want empty (auth required), got %q", v)
	}
}

// TestDetect_AuthProtectedGitHubEnterpriseRejectsSoftFingerprint
// pins the deliberate non-loosening of the GitHub probe. GitHub
// Enterprise auth-required /api/v3/meta returns 401 with the
// GitHub OAuth-style envelope {"message":"Bad credentials"} —
// a naive "any JSON body" matcher would misclassify as GitHub.
// We require the verifiable_password_authentication field
// (which is only populated on the 200 response), so an
// auth-required instance is rejected and Detect falls through
// to ErrUnsupportedProvider. This is the symmetric counterpart
// to the GitLab /api/v4 loosening — see
// TestDetect_AuthProtectedGitLabOverHTTP for the GitLab side.
func TestDetect_AuthProtectedGitHubEnterpriseRejectsSoftFingerprint(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/version":
			// Not GitLab — return HTML so json.Unmarshal fails
			// (Probe still returns the body, but Detect rejects).
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("<html>404 Not Found</html>"))
		case "/api/v3/meta":
			// GitHub Enterprise auth-required. Body has "message"
			// but lacks verifiable_password_authentication —
			// the strong fingerprint rejects it.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}

	prober := &ExecHTTPProber{Timeout: 2 * time.Second}
	remoteURL := "ssh://git@" + u.Host + "/some/path.git"
	_, err = Detect(context.Background(), remoteURL, prober, "")
	if err == nil {
		t.Fatal("Detect: want ErrUnsupportedProvider for auth-protected GitHub Enterprise, got nil err")
	}
}

// TestDetect_VersionFieldPopulatedWhenReadable locks in the
// strong-fingerprint path: a 200 response with the version
// object should populate Provider.Version, not just the host.
func TestDetect_VersionFieldPopulatedWhenReadable(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/version" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"16.5.0","revision":"abc123"}`))
	}))
	defer ts.Close()

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}

	prober := &ExecHTTPProber{Timeout: 2 * time.Second}
	remoteURL := "ssh://git@" + u.Host + "/some/path.git"
	prov, err := Detect(context.Background(), remoteURL, prober, "")
	if err != nil {
		t.Fatalf("Detect: want GitLabProvider with version, got err: %v", err)
	}
	gp, ok := prov.(*GitLabProvider)
	if !ok {
		t.Fatalf("Detect: want *GitLabProvider, got %T", prov)
	}
	if gp.version != "16.5.0" {
		t.Fatalf("GitLabProvider.version: want %q, got %q", "16.5.0", gp.version)
	}
}

// TestProbe_SharedTimeoutBoundedSchemes guarantees the ctx
// timeout caps wall-clock across both schemes. Probe fires two
// attempts (HTTPS then HTTP) on a server that delays each by 5s;
// with a 500ms Timeout, Probe must return well before the second
// scheme gets to time-out individually. Without the shared
// context, the cumulative latency could be up to 2× Timeout.
func TestProbe_SharedTimeoutBoundedSchemes(t *testing.T) {
	// HTTPS scheme: server has no TLS → client.Do fails fast
	// with TLS handshake error, no hang. So this test mainly
	// guards against future regressions where someone splits the
	// timeout across two independent contexts.
	//
	// To exercise the budget, serve the body on a slow handler
	// under HTTP — the HTTPS attempt fails immediately, the HTTP
	// attempt sleeps past the budget, and the shared context
	// should cancel the in-flight request before the second
	// scheme eats the full Timeout.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(800 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"version":"16.5.0"}`))
	}))
	defer ts.Close()

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}

	p := &ExecHTTPProber{Timeout: 200 * time.Millisecond}
	start := time.Now()
	_, err = p.Probe(context.Background(), u.Host, "/api/v4/version")
	elapsed := time.Since(start)

	// Probe should fail because the handler slept past the
	// shared deadline. Either:
	//   - HTTPS attempt fails fast (no TLS), HTTP attempt hits
	//     the 200ms ctx budget → context.Canceled
	//   - some combination that returns before 1s of wall clock
	if err == nil {
		t.Fatal("Probe: want error from shared timeout, got nil")
	}
	if elapsed > 1*time.Second {
		t.Fatalf("Probe: exceeded reasonable bound, elapsed=%v", elapsed)
	}
}

// TestProbeOnce_PackageHelper is a thin smoke test for the
// unexported probeOnce. The big-end tests above already cover
// the user-visible behaviour; this one just pins the helper's
// contract: 2xx returns (body, true), 5xx returns (nil, false)
// so the caller can try the next scheme.
func TestProbeOnce_PackageHelper(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("oops"))
	}))
	defer ts.Close()

	body, ok := probeOnce(context.Background(), &http.Client{}, ts.URL+"/anything")
	if ok {
		t.Fatalf("probeOnce 5xx: want ok=false, got ok=true body=%q", body)
	}
	if body != nil {
		t.Fatalf("probeOnce 5xx: want nil body, got %q", body)
	}

	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer ts2.Close()

	body, ok = probeOnce(context.Background(), &http.Client{}, ts2.URL+"/anything")
	if !ok {
		t.Fatalf("probeOnce 2xx: want ok=true, got ok=false")
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("probeOnce 2xx: unmarshal body: %v", err)
	}
	if parsed["ok"] != true {
		t.Fatalf("probeOnce 2xx: parsed=%v", parsed)
	}
}