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
	"net/http"
	"time"
)

// DefaultTimeout is the timeout applied when callers ask for
// Default() without a custom duration. Mirrors the per-call
// timeout we previously inlined across updater / version / dsh
// host code paths.
const DefaultTimeout = 45 * time.Second

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
