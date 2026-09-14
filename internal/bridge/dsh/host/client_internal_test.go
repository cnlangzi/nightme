//go:build !windows

// client_internal_test.go — unit tests for PostWithReconnect,
// callWithReconnect, and isTransientTransportError.
//
// Build tag !windows because the flaky transport uses an
// error-text shape matching dsh's "connection refused" on unix.
// The string-match itself is portable; the transport stub is the
// only reason to gate.
//
// Lives in `package host` (internal) because isTransientTransportError
// + callWithReconnect are unexported. PostWithReconnect is exported
// and exercised through the same tests via NewRPCClientWithHTTP.

package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// ─── isTransientTransportError ─────────────────────────────────────

func TestIsTransientTransportError_NilIsNotTransient(t *testing.T) {
	if isTransientTransportError(nil) {
		t.Fatalf("nil error should not be transient")
	}
}

func TestIsTransientTransportError_ConnectionRefused(t *testing.T) {
	// Real shape of the field-reported /review failure: a
	// stdlib *http.Client.Do returning a wrapped *net.OpError.
	err := fmt.Errorf(`dsh.host: POST workspace.create: Post "http://127.0.0.1:3081/api/workspace/create": dial tcp 127.0.0.1:3081: connect: connection refused`)
	if !isTransientTransportError(err) {
		t.Fatalf("expected connection-refused error to be transient; got false")
	}
}

func TestIsTransientTransportError_ConnectionReset(t *testing.T) {
	if !isTransientTransportError(errors.New("read tcp: connection reset by peer")) {
		t.Fatalf("expected connection-reset error to be transient")
	}
}

func TestIsTransientTransportError_NoSuchHost(t *testing.T) {
	if !isTransientTransportError(errors.New("dial tcp: lookup foo.invalid: no such host")) {
		t.Fatalf("expected no-such-host error to be transient")
	}
}

func TestIsTransientTransportError_EOF(t *testing.T) {
	if !isTransientTransportError(errors.New("unexpected EOF")) {
		t.Fatalf("expected EOF error to be transient")
	}
}

func TestIsTransientTransportError_BusinessErrorIsNotTransient(t *testing.T) {
	// HTTP 4xx/5xx + decode mismatch + business !OK must NOT be
	// retried by callWithReconnect — those failures are handled by
	// their callers (they don't clear by retrying).
	business := []error{
		errors.New("dsh.host: workspace.create: gateway/input-invalid: wire field \"request\" failed boundary validation"),
		errors.New("dsh.host: POST session.create: HTTP 500: internal server error"),
		errors.New("dsh.host: decode session.create response: invalid character"),
		errors.New("dsh.host: POST session.create rpcId mismatch"),
	}
	for _, e := range business {
		if isTransientTransportError(e) {
			t.Errorf("business error %q should NOT be transient", e.Error())
		}
	}
}

// ─── PostWithReconnect ────────────────────────────────────────────

// flakyTransport stubs the connection layer: the first `failFirst`
// requests return a connection-refused-shaped error, every later
// request delegates to a real http.Transport (which talks to the
// httptest server). Atomic counter records total attempts so the
// test can assert on retry behaviour.
type flakyTransport struct {
	failFirst int32
	attempts  int32
	fallback  http.RoundTripper
}

func (f *flakyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	n := atomic.AddInt32(&f.attempts, 1)
	if n <= f.failFirst {
		return nil, fmt.Errorf(`Post %q: dial tcp 127.0.0.1:9999: connect: connection refused`, req.URL.String())
	}
	return f.fallback.RoundTrip(req)
}

// successHandler writes the minimal server-response envelope that
// Post accepts (rpcId mismatch is fatal, so the envelope must be
// well-formed). The actual rpcId value is read from the request
// to satisfy Post's rpcId check.
func successHandler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rpcID := extractRPCIDFromBody(t, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"type":"server-response","rpcId":%q,"result":{"ok":true,"value":{}}}`, rpcID)
	}
}

// Test runtime budget: with reconnectMaxAttempts = 8 and
// failFirst = 1, the first attempt fails immediately, the second
// waits respawnDelay(0) = 500ms then succeeds. Wall time ≈ 500ms.

func TestPostWithReconnect_SuccessFirstAttempt_NoRetry(t *testing.T) {
	srv := httptest.NewServer(successHandler(t))
	defer srv.Close()

	cli := &http.Client{Transport: &flakyTransport{failFirst: 0, fallback: http.DefaultTransport}}
	c := NewRPCClientWithHTTP(srv.URL, cli)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := c.PostWithReconnect(ctx, "ping", map[string]any{})
	if err != nil {
		t.Fatalf("PostWithReconnect: %v", err)
	}
}

func TestPostWithReconnect_RetriesOnConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(successHandler(t))
	defer srv.Close()

	cli := &http.Client{Transport: &flakyTransport{failFirst: 1, fallback: http.DefaultTransport}}
	c := NewRPCClientWithHTTP(srv.URL, cli)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := c.PostWithReconnect(ctx, "ping", map[string]any{})
	if err != nil {
		t.Fatalf("PostWithReconnect after one refused: %v", err)
	}
}

func TestPostWithReconnect_NonTransientError_ShortCircuits(t *testing.T) {
	// Business-level error (gateway/input-invalid) should NOT be
	// retried — return immediately on first failure.
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"type":"server-response","rpcId":"x","result":{"ok":false,"error":{"code":"gateway/input-invalid","message":"bad field"}}}`))
	}))
	defer srv.Close()

	c := NewRPCClient(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := c.PostWithReconnect(ctx, "bad", map[string]any{})
	if err == nil {
		t.Fatalf("expected business error")
	}
	if isTransientTransportError(err) {
		t.Errorf("business error should not be classified as transient; got %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("business error should short-circuit; got %d attempts", got)
	}
}

func TestPostWithReconnect_ContextCancelDuringBackoff(t *testing.T) {
	// Cancel the context mid-backoff — callWithReconnect should
	// return ctx.Err() instead of waiting the full 500ms+ window.
	cli := &http.Client{Transport: &flakyTransport{failFirst: 100, fallback: http.DefaultTransport}}
	c := NewRPCClientWithHTTP("http://127.0.0.1:1", cli)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.PostWithReconnect(ctx, "ping", map[string]any{})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected error from cancelled ctx")
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("ctx cancel should short-circuit backoff; elapsed=%s", elapsed)
	}
}

// extractRPCIDFromBody parses the rpcId field out of a
// clientRequest envelope body. Used by successHandler to echo
// back the rpcId the client sent, satisfying Post's rpcId-
// mismatch check.
//
// Cheap JSON parse — the body is < 1 KiB and the field is at the
// top level. Avoids depending on json.Decoder for clarity.
func extractRPCIDFromBody(t *testing.T, body []byte) string {
	t.Helper()
	const want = `"rpcId":"`
	i := indexOf(body, want)
	if i < 0 {
		return ""
	}
	start := i + len(want)
	end := indexOf(body[start:], `"`)
	if end < 0 {
		return ""
	}
	return string(body[start : start+end])
}

// indexOf is a tiny strings.IndexBytes-equivalent so the test
// file doesn't have to import "strings" just for this.
func indexOf(haystack []byte, needle string) int {
	if len(needle) == 0 {
		return 0
	}
	if len(needle) > len(haystack) {
		return -1
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
