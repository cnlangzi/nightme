package gtw

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
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

// TestProbe_HTTPFallbackSucceedsDespiteStalledHTTPS is the regression
// test for the self-hosted-GitLab-behind-a-dropped-443 failure: Probe
// used to share one context across both schemes, so a host whose :443
// is silently dropped (firewall DROP, not REJECT — no RST, so no fast
// failure) spent the whole budget on the HTTPS attempt and the
// plain-HTTP fallback inherited an already-expired context. A GitLab
// that answers /api/v4/version in 30ms over plain HTTP was reported as
// unreachable.
//
// stallTLSThenServeHTTP reproduces the exact shape: every HTTPS
// connection is accepted and never spoken to, so the TLS handshake
// hangs until its own budget expires; every plain-HTTP connection
// is answered normally.
//
// Waiting out the stalled HTTPS attempt is deliberate — see Probe's
// comment on HTTPS keeping a guaranteed first look — so what this
// pins is that the call returns the HTTP body at all, bounded by the
// budget rather than hanging.
func TestProbe_HTTPFallbackSucceedsDespiteStalledHTTPS(t *testing.T) {
	const envelope = `{"message":"401 Unauthorized"}`
	host := stallTLSThenServeHTTP(t, http.StatusUnauthorized, "application/json", envelope)

	p := &ExecHTTPProber{Timeout: 300 * time.Millisecond}
	start := time.Now()
	body, err := p.Probe(context.Background(), host, "/api/v4/version")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Probe: want the HTTP fallback to succeed, got err: %v", err)
	}
	if got := strings.TrimSpace(string(body)); got != envelope {
		t.Fatalf("Probe body: want %q, got %q", envelope, got)
	}
	if elapsed > 2*p.Timeout {
		t.Fatalf("Probe: elapsed=%v, want the stalled HTTPS attempt bounded by Timeout", elapsed)
	}
}

// TestProbe_HTTPSWinsWhenBothSchemesAnswer pins the preference that
// keeps the race from degenerating into "first answer wins". A host
// can serve different content per port: a captive portal or login
// redirect on :80 in front of a healthy GitLab on :443. Taking the
// fast HTML from :80 fingerprints nothing and reports a reachable
// GitLab as an unsupported platform.
//
// serveTLSAndPortal puts both on ONE address — the same host:port
// Probe uses for both schemes — with the TLS arm slow and correct and
// the plain arm instant and useless.
func TestProbe_HTTPSWinsWhenBothSchemesAnswer(t *testing.T) {
	const version = `{"version":"16.5.0"}`
	host := serveTLSAndPortal(t, version, 250*time.Millisecond, "<html>captive portal</html>")

	// Self-signed cert, so the prober must skip verification to get
	// far enough for the preference to matter.
	p := &ExecHTTPProber{Timeout: 2 * time.Second, InsecureSkipVerify: true}
	body, err := p.Probe(context.Background(), host, "/api/v4/version")
	if err != nil {
		t.Fatalf("Probe: want the HTTPS body, got err: %v", err)
	}
	if got := strings.TrimSpace(string(body)); got != version {
		t.Fatalf("Probe body: want the HTTPS response %q, got %q", version, got)
	}
}

// TestDetect_PortalHTMLIsNotAReachabilityVerdict is the same
// misdiagnosis one layer up: Detect fingerprints whatever Probe
// returns, so a portal answer on both endpoints has to read as
// "responded, unrecognised", never as "unreachable".
func TestDetect_PortalHTMLIsNotAReachabilityVerdict(t *testing.T) {
	host := serveTLSAndPortal(t, "", 250*time.Millisecond, "<html>captive portal</html>")

	p := &ExecHTTPProber{Timeout: 2 * time.Second, InsecureSkipVerify: true}
	_, err := Detect(context.Background(), "ssh://git@"+host+"/team/repo.git", p, "")
	if !errors.Is(err, ErrUnsupportedProvider) {
		t.Fatalf("Detect: want ErrUnsupportedProvider for portal HTML, got %v", err)
	}
	if errors.Is(err, ErrProviderUnreachable) {
		t.Errorf("Detect: a portal answered, so it must not be reported as unreachable: %v", err)
	}
}

// serveTLSAndPortal serves two different protocols on ONE address:
// connections whose first byte is a TLS ClientHello are wrapped in
// TLS and answered with tlsBody after delay; everything else gets
// portalBody immediately. That is the captive-portal shape — a login
// redirect on :80 in front of a healthy instance on :443 — and it
// needs both protocols on the same host:port because Probe uses one
// address for both schemes.
//
// The TLS certificate is borrowed from a throwaway httptest TLS
// server, so callers need InsecureSkipVerify.
func serveTLSAndPortal(t *testing.T, tlsBody string, delay time.Duration, portalBody string) string {
	t.Helper()
	certServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(tlsBody))
	}))
	defer certServer.Close()
	cert := certServer.TLS.Certificates[0]

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	tlsHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(tlsBody))
	})
	portalHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(portalBody))
	})

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				br := bufio.NewReader(c)
				first, err := br.Peek(1)
				if err != nil {
					_ = c.Close()
					return
				}
				if first[0] == 0x16 { // TLS handshake record
					tc := tls.Server(&prefixConn{Conn: c, r: br}, &tls.Config{
						Certificates: []tls.Certificate{cert},
					})
					_ = (&http.Server{Handler: tlsHandler}).Serve(&oneConnListener{Conn: tc})
					return
				}
				pc := &prefixConn{Conn: c, r: br}
				_ = (&http.Server{Handler: portalHandler}).Serve(&oneConnListener{Conn: pc})
			}(c)
		}
	}()

	return ln.Addr().String()
}

// prefixConn replays already-buffered bytes before reading from the
// underlying connection, so a handler can peek at the first byte and
// still hand the stream to a TLS or HTTP server intact.
type prefixConn struct {
	net.Conn
	r io.Reader
}

func (c *prefixConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// oneConnListener turns a single connection into a net.Listener so
// http.Server can serve exactly it. Accept is called once by Serve.
type oneConnListener struct {
	net.Conn
	served bool
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	if l.served {
		return nil, io.EOF
	}
	l.served = true
	return l.Conn, nil
}

func (l *oneConnListener) Addr() net.Addr { return l.Conn.LocalAddr() }

func (l *oneConnListener) Close() error { return nil }

// stallAllConnections accepts connections and holds every one of
// them open without ever speaking. Unlike
// stallTLSThenServeHTTP no attempt can succeed, so the call can only
// end when a context expires — which is what makes it usable for
// deadline assertions.
func stallAllConnections(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	var held []net.Conn
	var mu sync.Mutex
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			_ = c.Close()
		}
	})

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()

	return ln.Addr().String()
}

// TestProbe_CallerDeadlineStillCapsTheCall guards the other half of
// the fix: giving each attempt its own budget must not let a caller
// with a tighter deadline lose it. A 5s per-attempt budget under a
// 100ms caller deadline has to return in ~100ms. Both schemes stall,
// so only the deadline can end the call.
func TestProbe_CallerDeadlineStillCapsTheCall(t *testing.T) {
	host := stallAllConnections(t)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	p := &ExecHTTPProber{Timeout: 5 * time.Second}
	start := time.Now()
	_, err := p.Probe(ctx, host, "/api/v4/version")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Probe: want the caller's deadline to fire, got nil error")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Probe: elapsed=%v, want the caller's 100ms deadline to win over the 5s per-attempt budget", elapsed)
	}
}

// TestProbe_BothSchemesStallBoundedByOneBudget pins the wall-clock
// win of racing: with neither scheme answering, the call costs ONE
// budget, not one per scheme. Before the race it was 2×.
func TestProbe_BothSchemesStallBoundedByOneBudget(t *testing.T) {
	host := stallAllConnections(t)

	p := &ExecHTTPProber{Timeout: 300 * time.Millisecond}
	start := time.Now()
	_, err := p.Probe(context.Background(), host, "/api/v4/version")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Probe: want error from two stalled schemes, got nil")
	}
	if elapsed > 2*p.Timeout {
		t.Fatalf("Probe: elapsed=%v, want the raced schemes bounded by one %v budget", elapsed, p.Timeout)
	}
}

// TestProbe_DroppedHTTPSStillReachesGitLab end-to-end through Detect:
// the 401 envelope must arrive over HTTP even though HTTPS stalls,
// so the host is classified GitLab rather than unreachable. This is
// the f9netcore shape from the bug report.
func TestProbe_DroppedHTTPSStillReachesGitLab(t *testing.T) {
	host := stallTLSThenServeHTTP(t, http.StatusUnauthorized, "application/json", `{"message":"401 Unauthorized"}`)

	p := &ExecHTTPProber{Timeout: 300 * time.Millisecond}
	prov, err := Detect(context.Background(), "ssh://git@"+host+"/f9/f9netcore.git", p, "")
	if err != nil {
		t.Fatalf("Detect: want GitLabProvider via the HTTP fallback, got err: %v", err)
	}
	if prov.Kind() != ProviderGitLab {
		t.Fatalf("Detect.Kind: want gitlab, got %q", prov.Kind())
	}
	if prov.Host() != host {
		t.Fatalf("Detect.Host: want %q, got %q", host, prov.Host())
	}
}

// stallTLSThenServeHTTP starts a TCP listener that mimics a host
// whose :443 is silently dropped while :80 answers normally. It
// peeks at the first byte of every connection: a TLS ClientHello
// (record type 0x16) is held open and never spoken to, so the
// handshake read never returns and the attempt can only end at its
// context deadline; anything else is answered with a fixed
// plain-HTTP response.
//
// Holding a TLS connection open without writing to it is what a
// firewall DROP looks like to a TLS client — the TCP connect
// succeeds, then silence. httptest.NewServer cannot reproduce it:
// it has no TLS listener, so the HTTPS attempt fails fast with a
// handshake error and never consumes the budget the bug depended on.
//
// The split is per-connection rather than one-shot so it holds for
// both of Detect's probe endpoints, not just the first.
func stallTLSThenServeHTTP(t *testing.T, status int, contentType, body string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	var held []net.Conn
	var mu sync.Mutex
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			_ = c.Close()
		}
	})

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				br := bufio.NewReader(c)
				// Every client here sends something immediately —
				// a ClientHello or a request line — so a plain
				// blocking Read is enough to classify it.
				first, err := br.Peek(1)
				if err != nil {
					_ = c.Close()
					return
				}
				if first[0] == 0x16 { // TLS handshake record
					mu.Lock()
					held = append(held, c)
					mu.Unlock()
					return // never write, never close
				}
				defer c.Close()
				if _, err := http.ReadRequest(br); err != nil {
					return
				}
				fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Type: %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
					status, http.StatusText(status), contentType, len(body), body)
			}(c)
		}
	}()

	return ln.Addr().String()
}

// TestProbe_SlowServerStillTimesOut keeps a bound on the other side:
// a host that accepts the connection and then stalls on the HTTP
// attempt must still return within roughly its own budget, not hang.
func TestProbe_SlowServerStillTimesOut(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

	if err == nil {
		t.Fatal("Probe: want error from the stalled handler, got nil")
	}
	if elapsed > 1*time.Second {
		t.Fatalf("Probe: exceeded reasonable bound, elapsed=%v", elapsed)
	}
}

// TestProbeOnce_PackageHelper is a thin smoke test for the
// unexported probeOnce. The big-end tests above already cover
// the user-visible behaviour; this one just pins the helper's
// contract: 2xx returns a body and no error, 5xx returns an error
// wrapping errProbeStatus so the caller can try the next scheme AND
// Detect can tell the host answered.
func TestProbeOnce_PackageHelper(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("oops"))
	}))
	defer ts.Close()

	body, err := probeOnce(context.Background(), &http.Client{}, ts.URL+"/anything")
	if err == nil {
		t.Fatalf("probeOnce 5xx: want error, got body=%q", body)
	}
	if body != nil {
		t.Fatalf("probeOnce 5xx: want nil body, got %q", body)
	}
	if !errors.Is(err, errProbeStatus) {
		t.Fatalf("probeOnce 5xx: want errProbeStatus so Detect can tell a 5xx from silence, got %v", err)
	}

	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer ts2.Close()

	body, err = probeOnce(context.Background(), &http.Client{}, ts2.URL+"/anything")
	if err != nil {
		t.Fatalf("probeOnce 2xx: want body and no error, got err: %v", err)
	}
	if errors.Is(err, errProbeStatus) {
		t.Fatal("probeOnce 2xx: errProbeStatus must not be set on a success")
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("probeOnce 2xx: unmarshal body: %v", err)
	}
	if parsed["ok"] != true {
		t.Fatalf("probeOnce 2xx: parsed=%v", parsed)
	}
}

// TestParseRemoteHost pins the URL → host extraction logic,
// covering the four URL-shape combinations that the public docs
// call out plus the scp-style-with-port case the
// parseRemoteHost/isSCPStylePort fix unblocks. Each case is a
// self-contained URL with one feature under test so a failure
// points at the exact branch.
func TestParseRemoteHost(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string
		// errOK = true means ErrInvalidRemoteURL is the expected
		// outcome. errOK = false means a parsed host is expected.
		errOK bool
	}{
		// URL-style (https://host[:port]/path).
		{"https-no-port", "https://gitlab.acme/path.git", "gitlab.acme", false},
		{"https-with-port", "https://gitlab.acme:8443/path.git", "gitlab.acme:8443", false},
		{"http-with-port", "http://gitlab.acme:8080/path.git", "gitlab.acme:8080", false},
		{"ssh-url-with-port", "ssh://git@gitlab.acme:2222/path.git", "gitlab.acme:2222", false},
		{"https-query-fragment", "https://gitlab.acme/path?ref=main#frag", "gitlab.acme", false},

		// SCP-style (git@host[:port]:path).
		{"scp-no-port", "git@gitlab.acme:path.git", "gitlab.acme", false},
		// scp-with-port is the regression: the second colon used
		// to be confused with the path separator. After the
		// isSCPStylePort fix, host:NNNN is preserved verbatim.
		{"scp-with-port", "git@gitlab.acme:8443:group/proj.git", "gitlab.acme:8443", false},
		{"scp-with-port-deep-path", "git@gitlab.acme:2222:group/sub/proj.git", "gitlab.acme:2222", false},

		// Userinfo (https://user:token@host[:port]/path) — common
		// with gh/glab auth helpers.
		{"https-userinfo-port", "https://oauth2:glpat-xxx@gitlab.acme:8443/path.git", "gitlab.acme:8443", false},
		{"https-userinfo-no-port", "https://oauth2:glpat-xxx@gitlab.acme/path.git", "gitlab.acme", false},

		// Failure modes — these must surface as
		// ErrInvalidRemoteURL, NOT be silently mangled.
		{"empty", "", "", true},
		{"unknown-scheme", "gitlab.acme:path.git", "", true},
		// "scp-with-port-no-path" is a degenerate URL — git would
		// accept the host:port but fail on missing path. Our job
		// here is just to extract the host, which the URL-style
		// port branch handles correctly (digits-only rest → port).
		// Downstream ParseRepoOwner rejects the empty path.
		{"scp-with-port-no-path", "git@gitlab.acme:8443", "gitlab.acme:8443", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRemoteHost(tc.url)
			if tc.errOK {
				if !errors.Is(err, ErrInvalidRemoteURL) {
					t.Fatalf("parseRemoteHost(%q): want ErrInvalidRemoteURL, got host=%q err=%v",
						tc.url, got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRemoteHost(%q): unexpected err: %v", tc.url, err)
			}
			if got != tc.want {
				t.Fatalf("parseRemoteHost(%q): want host=%q, got %q", tc.url, tc.want, got)
			}
		})
	}
}

// TestDetect_SCPWithPortHonoredByProbe is the end-to-end
// counterpart to TestParseRemoteHost's "scp-with-port" case.
// Without the parseRemoteHost fix, Detect's Probe would be
// pointed at `https://host:443/api/v4/version` (default port)
// and miss a self-hosted GitLab listening on, e.g., 8443. The
// httptest server uses a random port picked by NewServer, so
// the URL always carries a non-default port — and the test
// asserts Detect classifies it as GitLab via the SCP-style URL.
func TestDetect_SCPWithPortHonoredByProbe(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/version" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"16.5.0","revision":"abc"}`))
	}))
	defer ts.Close()

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}

	prober := &ExecHTTPProber{Timeout: 2 * time.Second}
	// True SCP-style URL with explicit port: "git@host:NNNN:path".
	// This exercises the isSCPStylePort branch in parseRemoteHost
	// (the second `:` is the path separator, not isPort's
	// digits-then-slash). If a future refactor breaks this shape,
	// the test fails — proving end-to-end that the SCP-with-port
	// extraction still works. parseRemoteHost must preserve
	// host:port, Probe must dial that host:port, the version
	// endpoint must respond 200, and Detect must return a
	// GitLabProvider whose Host() includes the port.
	remoteURL := "git@" + u.Host + ":some/path.git"
	prov, err := Detect(context.Background(), remoteURL, prober, "")
	if err != nil {
		t.Fatalf("Detect: want GitLabProvider with port, got err: %v", err)
	}
	if got := prov.Host(); got != u.Host {
		t.Fatalf("Detect.Host: want %q (with port), got %q", u.Host, got)
	}
}
