// JSON-RPC 2.0 client used by the codex app-server bridge.
//
// Design notes (see docs/bridge/codex.md §1 + §3 for full rationale):
//   - The transport is a raw `*exec.Cmd` pipe (NOT PTY), so the wire
//     is straight JSON-RPC 2.0 frames terminated by '\n'. We use
//     bufio.Scanner with a 10 MiB max frame size to match the cc-connect
//     reference implementation and avoid runaway allocations on
//     unexpected payloads.
//   - The write side is NOT wrapped in a bufio.Writer: io.Pipe and
//     synchronous Linux pipes only unblock Write once a reader is
//     pulling bytes, so an in-process buffer would never flush unless
//     a reader were already waiting. Writing one line at a time keeps
//     the back-pressure contract obvious.
//   - `pending` is keyed by the raw JSON-encoded id. Numeric ids are
//     serialized via json.Marshal (i.e. "1") and string ids via
//     json.Marshal of a string (i.e. "\"req-001\""). The same key is
//     used by the read loop, so request/response correlation is
//     deterministic.
package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
)

// MaxFrameSize caps the size of a single JSON-RPC frame the bridge
// will accept on stdout. The Codex app-server wire is supposed to be
// small events, but tool results and base64 images can push frames
// well past the bufio.Scanner default 64 KiB. 10 MiB matches the
// cc-connect reference and is large enough for chat-sized payloads.
const MaxFrameSize = 10 * 1024 * 1024

// ErrSessionClosed is returned by request when the session has been
// (or is being) closed. Callers (notably SendBlocks) should treat
// this as terminal and not retry.
var ErrSessionClosed = errors.New("codex: session closed")

// ErrFrameTooLarge is returned by readLoop when a stdout frame
// exceeds MaxFrameSize. The session is torn down.
var ErrFrameTooLarge = errors.New("codex: frame too large")

// ErrMalformedJSON is returned by readLoop when a frame cannot be
// decoded as a JSON-RPC envelope. The session is torn down.
var ErrMalformedJSON = errors.New("codex: malformed JSON")

// JSON-RPC 2.0 reserved errors we surface verbatim.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInternalError  = -32603
)

// rpcClient owns the JSON-RPC write side and the pending response
// registry. It is the single producer of wire bytes; only its methods
// (request / notify / respond) should ever touch writer.
//
// pending and nextID are protected by pendingMu. writer is protected
// by writeMu. The two locks are never held at the same time.
type rpcClient struct {
	wr               io.Writer
	rd               io.Reader
	writeMu          sync.Mutex
	pendingMu        sync.Mutex
	pending          map[string]chan rpcResponse
	nextID           atomic.Int64
	closed           atomic.Bool
	onServerRequest  func(method string, rawID, params json.RawMessage)
	onNotification   func(method string, params json.RawMessage)
	maxFrameBytes    int
}

// rpcResponse is the value delivered to a pending waiter when the
// child responds to one of our requests.
type rpcResponse struct {
	result json.RawMessage
	err    error
}

// newRPCClient wraps an io.ReadWriter (the merged stdin/stdout of the
// child process) plus two callbacks that handle every non-response
// frame:
//
//   - onServerRequest  — frame carries an id AND a method; the bridge
//     must answer with a response. The reply is sent via respond().
//   - onNotification   — frame carries a method but no id; the bridge
//     acknowledges the event with no reply.
func newRPCClient(rw io.ReadWriter,
	onSR func(string, json.RawMessage, json.RawMessage),
	onN func(string, json.RawMessage)) *rpcClient {
	return &rpcClient{
		wr:              rw,
		rd:              rw,
		pending:         make(map[string]chan rpcResponse),
		onServerRequest: onSR,
		onNotification:  onN,
		maxFrameBytes:   MaxFrameSize,
	}
}

// nextRequestID returns a fresh, monotonically increasing id rendered
// as a JSON number literal. We use numeric ids (not strings) because
// the upstream codex app-server expects numeric ids on the wire.
func (c *rpcClient) nextRequestID() json.RawMessage {
	n := c.nextID.Add(1)
	b, _ := json.Marshal(n)
	return b
}

// writeMu is held only by writeLine below.
func (c *rpcClient) write(payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed.Load() {
		return ErrSessionClosed
	}
	if _, err := c.wr.Write(payload); err != nil {
		return fmt.Errorf("codex: write: %w", err)
	}
	return nil
}

// writeLine writes payload + "\n" atomically. Holds writeMu for the
// entire critical section so two goroutines cannot interleave halves
// of two frames.
func (c *rpcClient) writeLine(payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed.Load() {
		return ErrSessionClosed
	}
	if _, err := c.wr.Write(payload); err != nil {
		return fmt.Errorf("codex: write: %w", err)
	}
	if _, err := c.wr.Write([]byte{'\n'}); err != nil {
		return fmt.Errorf("codex: write newline: %w", err)
	}
	return nil
}

// request sends a JSON-RPC request and synchronously waits for the
// matching response (with ctx cancellation honored).
//
// Ordering: the pending slot is registered BEFORE the request is
// written. This prevents a write-before-register race where the child
// answers faster than the caller reaches expectResponse and the
// response lands against an empty pending map. The same fix is used
// by internal/bridge/acp/rpc.go and internal/bridge/pi/rpc.go.
func (c *rpcClient) request(ctx context.Context, method string, params, out any) error {
	if c.closed.Load() {
		return ErrSessionClosed
	}
	idJSON := c.nextRequestID()
	paramsJSON, err := marshalParams(params)
	if err != nil {
		return err
	}

	key := string(idJSON)
	ch := make(chan rpcResponse, 1)
	c.pendingMu.Lock()
	c.pending[key] = ch
	c.pendingMu.Unlock()

	msg := rpcRequest{
		JSONRPC: "2.0",
		ID:      idJSON,
		Method:  method,
		Params:  paramsJSON,
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		c.removePending(key)
		return fmt.Errorf("codex: marshal %s: %w", method, err)
	}
	if err := c.writeLine(raw); err != nil {
		c.removePending(key)
		return err
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			return ErrSessionClosed
		}
		if resp.err != nil {
			return resp.err
		}
		if out != nil && len(resp.result) > 0 {
			if err := json.Unmarshal(resp.result, out); err != nil {
				return fmt.Errorf("codex: decode %s response: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		c.removePending(key)
		return ctx.Err()
	}
}

// notify sends a JSON-RPC notification (no id, no response expected).
func (c *rpcClient) notify(method string, params any) error {
	paramsJSON, err := marshalParams(params)
	if err != nil {
		return err
	}
	msg := rpcNotification{
		JSONRPC: "2.0",
		Method:  method,
		Params:  paramsJSON,
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("codex: marshal %s: %w", method, err)
	}
	return c.writeLine(raw)
}

// respond writes a JSON-RPC response for a server-initiated request,
// using the same id (raw form) the child used on the wire.
func (c *rpcClient) respond(rawID json.RawMessage, result any) error {
	resultJSON, err := marshalParams(result)
	if err != nil {
		return err
	}
	env := rpcResponseEnvelope{
		JSONRPC: "2.0",
		ID:      rawID,
		Result:  resultJSON,
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return c.writeLine(raw)
}

// respondErr writes a JSON-RPC error response for a server-initiated
// request the bridge cannot honor.
func (c *rpcClient) respondErr(rawID json.RawMessage, code int, msg string) error {
	env := rpcResponseEnvelope{
		JSONRPC: "2.0",
		ID:      rawID,
		Error: &rpcError{
			Code:    code,
			Message: msg,
		},
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return c.writeLine(raw)
}

// dispatchResponse is called by the read loop for every parsed
// response envelope. The id key (string of raw json) is matched
// against the pending map; on match the waiter is delivered and the
// slot is removed so a late duplicate-id response is dropped.
func (c *rpcClient) dispatchResponse(env rpcResponseEnvelope) {
	key := string(bytes.TrimSpace(env.ID))
	if key == "" {
		return
	}
	c.pendingMu.Lock()
	ch, ok := c.pending[key]
	if ok {
		delete(c.pending, key)
	}
	c.pendingMu.Unlock()
	if !ok {
		return
	}
	var r rpcResponse
	if env.Error != nil {
		r.err = env.Error
	} else {
		r.result = env.Result
	}
	select {
	case ch <- r:
	default:
	}
}

// failPending aborts every registered waiter with err and marks the
// client closed. Idempotent. Called by the read loop on EOF/error and
// by the lifecycle goroutine after cmd.Wait returns.
func (c *rpcClient) failPending(err error) {
	if err == nil {
		err = ErrSessionClosed
	}
	c.closed.Store(true)
	c.pendingMu.Lock()
	pending := c.pending
	c.pending = make(map[string]chan rpcResponse)
	c.pendingMu.Unlock()
	for _, ch := range pending {
		select {
		case ch <- rpcResponse{err: err}:
		default:
		}
	}
}

// removePending deletes a single waiter by id key. Used by request
// when the write itself failed or the caller's ctx was cancelled, so
// a late response cannot land on a now-orphan channel.
func (c *rpcClient) removePending(key string) {
	c.pendingMu.Lock()
	delete(c.pending, key)
	c.pendingMu.Unlock()
}

// marshalParams accepts nil (no params field), an already-encoded
// json.RawMessage (pass through), or anything json.Marshal can encode.
func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	if raw, ok := params.(json.RawMessage); ok {
		if len(raw) == 0 {
			return nil, nil
		}
		return raw, nil
	}
	return json.Marshal(params)
}

// readPump runs in its own goroutine and parses every LF-delimited
// frame from rd. Each frame is dispatched:
//
//   - id + no method → response to one of our requests → dispatchResponse
//   - id + method    → server-initiated request → onServerRequest
//   - no id + method → notification             → onNotification
//   - anything else  → malformed (session torn down)
//
// On scanner.Err (which includes ErrTooLong), the loop emits an
// EventAgentError and returns so the lifecycle goroutine can close
// the events channel.
func (c *rpcClient) readPump(ctx context.Context, onError func(error)) {
	defer c.failPending(ErrSessionClosed)
	scanner := bufio.NewScanner(c.rd)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, c.maxFrameBytes)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		c.handleFrame(line, onError)
	}

	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			onError(fmt.Errorf("%w: > %d bytes", ErrFrameTooLarge, c.maxFrameBytes))
			return
		}
		if !errors.Is(err, io.EOF) {
			onError(fmt.Errorf("%w: %v", ErrMalformedJSON, err))
		}
	}
}

// handleFrame parses one wire line and dispatches it. Extracted so
// readPump stays readable.
func (c *rpcClient) handleFrame(line []byte, onError func(error)) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(line, &probe); err != nil {
		onError(fmt.Errorf("%w: %s", ErrMalformedJSON, string(line)))
		return
	}

	_, hasID := probe["id"]
	_, hasMethod := probe["method"]
	hasResult := false
	if _, ok := probe["result"]; ok {
		hasResult = true
	}
	_, hasError := probe["error"]

	switch {
	case hasID && !hasMethod:
		// response to one of our requests
		var env rpcResponseEnvelope
		if err := json.Unmarshal(line, &env); err != nil {
			onError(fmt.Errorf("%w: bad response envelope: %v", ErrMalformedJSON, err))
			return
		}
		c.dispatchResponse(env)
	case hasID && hasMethod:
		// server-initiated request: extract method + params and dispatch
		methodBytes, _ := probe["method"]
		var method string
		if err := json.Unmarshal(methodBytes, &method); err != nil {
			onError(fmt.Errorf("%w: bad method on server request: %v", ErrMalformedJSON, err))
			return
		}
		params := probe["params"] // may be nil
		if c.onServerRequest != nil {
			c.onServerRequest(method, probe["id"], params)
		}
	case !hasID && hasMethod:
		// server-pushed notification
		var env rpcNotification
		if err := json.Unmarshal(line, &env); err != nil {
			onError(fmt.Errorf("%w: bad notification envelope: %v", ErrMalformedJSON, err))
			return
		}
		if c.onNotification != nil {
			c.onNotification(env.Method, env.Params)
		}
	default:
		// weird frame: not response, not request, not notification
		if hasResult || hasError {
			// a response-shaped frame with a missing id — surface it
			onError(fmt.Errorf("%w: response without id", ErrMalformedJSON))
			return
		}
		slog.Default().Debug("codex: dropping unknown frame",
			slog.String("raw", string(line)))
	}
}
