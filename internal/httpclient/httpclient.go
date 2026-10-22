// Package httpclient is the single home for outbound HTTP
// requests in nightme.
//
// The package wraps the standard library's net/http with one
// piece of opinionated default behaviour:
//
//   - Every returned *http.Client uses Go's built-in
//     http.ProxyFromEnvironment when no custom Transport is set.
//     This means HTTP_PROXY, HTTPS_PROXY, and NO_PROXY are
//     honoured automatically — operators behind a corporate or
//     regional proxy (Clash, Surge, v2ray, …) get the right
//     routing without per-call configuration.
//
// The package intentionally does NOT add retry, rate limit,
// logging, or JSON-decode helpers. Each layer that needs those
// (channel/telegram's retry.go + ratelimit.go, bridge/updater's
// version check, etc.) composes them on top of the clients
// returned here. Keeping the base package small keeps the
// dependency surface minimal and the test surface tractable.
//
// When a caller needs to inject a custom Transport (e.g. tests
// pointing at httptest.Server, or a one-off SOCKS5 proxy via
// x/net/proxy.DialContext), they build the http.Client
// themselves — httpclient doesn't pretend to be a complete HTTP
// abstraction.
package httpclient

import (
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// DefaultTimeout is the timeout applied when callers ask for
// Default() without a custom duration. Mirrors the per-call
// timeout we previously inlined across updater / version / dsh
// host code paths.
const DefaultTimeout = 45 * time.Second

// processProxy holds the proxy resolver installed by
// SetProcessProxy. nil means "nothing configured" and every
// reader falls back to http.ProxyFromEnvironment.
var (
	processProxyMu sync.RWMutex
	processProxy   func(*http.Request) (*url.URL, error)
)

// Default returns a fresh *http.Client with DefaultTimeout and
// Go's default Transport. The default Transport uses
// http.ProxyFromEnvironment, so HTTP_PROXY / HTTPS_PROXY /
// NO_PROXY env vars are honoured without further wiring.
//
// Each call returns a new client. Callers that want a long-
// lived singleton should cache the result themselves.
func Default() *http.Client {
	return DefaultWithTimeout(DefaultTimeout)
}

// DefaultWithTimeout returns a fresh *http.Client with the given
// timeout and Go's default Transport (proxy-from-environment
// enabled). Use this when DefaultTimeout is too long for a
// latency-sensitive call (e.g. login flow probes).
func DefaultWithTimeout(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout}
}

// SetProcessProxy installs proxyURL as the outbound proxy for every
// HTTP client that leaves its Transport at the default. It also
// replaces the resolver on http.DefaultTransport so callers that
// build their own &http.Client{} (every channel adapter does) are
// covered.
//
// An empty proxyURL is a no-op, so a config that never sets one
// leaves the standard HTTP_PROXY / HTTPS_PROXY handling exactly as
// it was. Call it once during daemon startup, before the first
// outbound request — http.DefaultTransport must not be mutated
// while requests are in flight.
func SetProcessProxy(proxyURL string) error {
	if proxyURL == "" {
		return nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return fmt.Errorf("parse proxy url %q: %w", proxyURL, err)
	}
	if u.Host == "" {
		return fmt.Errorf("proxy url %q has no host", proxyURL)
	}
	resolve := http.ProxyURL(u)

	processProxyMu.Lock()
	processProxy = resolve
	processProxyMu.Unlock()

	if tr, ok := http.DefaultTransport.(*http.Transport); ok {
		tr.Proxy = resolve
	}
	return nil
}

// ProxyFunc returns the resolver a hand-built Transport should use:
// the process-wide proxy when one is configured, otherwise Go's
// http.ProxyFromEnvironment. Transports that cannot use
// http.DefaultTransport (the Feishu SDK client builds its own) wire
// this in instead of calling http.ProxyFromEnvironment directly, so
// cfg.Proxy / NIGHTME_PROXY reaches them too.
func ProxyFunc() func(*http.Request) (*url.URL, error) {
	processProxyMu.RLock()
	fn := processProxy
	processProxyMu.RUnlock()
	if fn != nil {
		return fn
	}
	return http.ProxyFromEnvironment
}
