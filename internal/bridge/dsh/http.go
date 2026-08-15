// http.go — HTTP RPC client for `dsh --profile web`.
//
// One POST per RPC: the wire is `POST {baseURL}/api/{method}` with
// the clientRequest envelope (see protocol.go). The server returns
// the rpcResponse envelope; we surface either the decoded value or
// the typed error code. All calls share a single net/http.Client;
// per-request contexts allow the runtime to cancel any in-flight
// POST without taking down the whole bridge.
package dsh

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// httpClientTimeout bounds every individual POST. Long-running
// actions (session.prompt, session.history) carry their own
// per-call deadline via ctx — this constant is the upper bound
// when the caller passes no deadline.
const httpClientTimeout = 30 * time.Second

// httpClient wraps net/http.Client with the dsh baseURL fixed at
// construction. Methods are safe for concurrent use; net/http
// already serializes per-connection writes and we use a fresh
// request per call.
type httpClient struct {
	baseURL string
	http    *http.Client
}

// newHTTPClient constructs a client rooted at baseURL (e.g.
// "http://127.0.0.1:3080"). The trailing slash, if any, is dropped
// because URL joining adds one.
func newHTTPClient(baseURL string) *httpClient {
	// strip trailing slash so we can safely concat "/api/<method>"
	for len(baseURL) > 0 && baseURL[len(baseURL)-1] == '/' {
		baseURL = baseURL[:len(baseURL)-1]
	}
	return &httpClient{
		baseURL: baseURL,
		http:    &http.Client{Timeout: httpClientTimeout},
	}
}

// Post issues an RPC. `payload` is JSON-marshaled into the
// clientRequest envelope; the response is decoded into rpcResponse
// and returned.
//
// Returns:
//   - (resp, nil) on transport OK + business OK (resp.Result.OK == true)
//   - (resp, err) on transport OK + business failure (use resp.Result.Error)
//   - (nil, err) on transport / decode / id-mismatch failure
func (h *httpClient) Post(ctx context.Context, method string, payload any) (*rpcResponse, error) {
	rpcID := newRPCID()

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("dsh: marshal payload for %s: %w", method, err)
	}

	envelope := clientRequest{
		Type:    "client-request",
		RPCID:   rpcID,
		Method:  method,
		Payload: payloadBytes,
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("dsh: marshal envelope for %s: %w", method, err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", h.baseURL+"/api/"+method, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("dsh: build request %s: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	httpResp, err := h.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dsh: POST %s: %w", method, err)
	}
	defer httpResp.Body.Close()

	// The dsh server returns HTTP 200 even on business errors
	// (errors ride inside result.error per the wire schema); only
	// non-200 means transport / framework failure (404 unknown
	// method, 415 non-JSON, 500 handler crash).
	if httpResp.StatusCode != http.StatusOK {
		// Read up to 1 KiB of body for error context, then bail.
		bodyBytes, _ := io.ReadAll(io.LimitReader(httpResp.Body, 1024))
		return nil, fmt.Errorf("dsh: POST %s: HTTP %d: %s",
			method, httpResp.StatusCode, string(bodyBytes))
	}

	respBytes, err := io.ReadAll(io.LimitReader(httpResp.Body, 16*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("dsh: read body for %s: %w", method, err)
	}

	var resp rpcResponse
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return nil, fmt.Errorf("dsh: decode %s response: %w (body=%s)", method, err, truncate(string(respBytes), 200))
	}

	// Sanity: rpcId must echo. Server uses a parallel id map keyed
	// on rpcId; mismatch means we hit a stale response or a proxy.
	if resp.RPCID != rpcID {
		return nil, fmt.Errorf("dsh: POST %s rpcId mismatch (sent %s, got %s)", method, rpcID, resp.RPCID)
	}
	if resp.Type != "server-response" {
		return nil, fmt.Errorf("dsh: POST %s unexpected response type=%q", method, resp.Type)
	}

	return &resp, nil
}

// PostRaw is like Post but takes a pre-marshaled RawMessage for
// `payload`. Used by respond() which builds an inline
// json.RawMessage outcome.
func (h *httpClient) PostRaw(ctx context.Context, method string, payload json.RawMessage) (*rpcResponse, error) {
	return h.Post(ctx, method, json.RawMessage(payload))
}

// newRPCID mints a client-request id. We use a random UUID v4
// (no external dep — crypto/rand + RFC 4122 §4.4). Random beats
// time-prefixed for our use because concurrent requests must not
// collide on the server's pending-request map keys during clock
// skew, and we don't need the time-ordering property.
func newRPCID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read on Linux/macOS never errors; if it does
		// (e.g. /dev/urandom missing in a sandbox), fall back to
		// a nanosecond timestamp so RPC IDs are at least unique
		// within the process — better than panicking mid-handshake.
		now := time.Now().UnixNano()
		return fmt.Sprintf("%016x", now)
	}
	// RFC 4122 §4.4: set version (4) and variant (10xx) bits.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b[:])
}

// truncate returns the first n bytes of s, with "…" appended if s
// was longer. Used in error messages to keep logs bounded.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}


