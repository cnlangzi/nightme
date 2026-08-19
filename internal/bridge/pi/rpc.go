// JSONL framing + request/response correlation for the Pi RPC bridge.
//
// The bridge drives a long-lived `pi --mode rpc` child over real
// stdio pipes (NOT PTY). Every wire message is exactly one JSON
// object followed by a single LF byte; no JSON-RPC envelope. Commands
// carry an optional "id" for correlation; responses echo the same id
// and add "type":"response" + "command" + "success" + optional
// "data"/"error". Events are async server pushes with no id (except
// bash_execution_update, which we ignore at MVP).
//
// The package deliberately avoids importing the existing
// internal/bridge/acp package: the two protocols share no wire
// shape, and ACP currently has known production-blocking defects
// (PTY transport, requestAsync drops responses, PTY-merged
// stdout/stderr) that we do not want to inherit. Once both bridges
// are stable in production we may extract a shared JSONL transport
// helper; until then keep them independent.

package pi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// MaxFrameSize caps the size of a single JSONL frame the bridge will
// accept on stdout. Pi's wire is supposed to be small events, but
// tool results and base64 images can push frames well past the
// bufio.Scanner default 64 KiB. 4 MiB is large enough for a single
// chat-sized image (≈3 MiB base64) and well below process memory
// pressure. Frames larger than this terminate the session.
const MaxFrameSize = 4 * 1024 * 1024

// ErrSessionClosed is returned by request when the session has been
// (or is being) closed. Callers (notably SendBlocks) should treat
// this as a terminal error and not retry.
// ErrSessionClosed is returned by request when the session has been
// (or is being) closed. Callers (notably SendBlocks) should treat
// this as a terminal error and not retry.
var ErrSessionClosed = errors.New("pi: session closed")

// ErrTurnAborted is returned by request when the in-flight turn was
// failed explicitly (e.g. by driver.Stop during /stop). The bridge
// process is still alive and the session is still usable for a
// subsequent SendBlocks; the caller just got notified that this
// particular prompt's response was abandoned.
var ErrTurnAborted = errors.New("pi: turn aborted")

// ErrTurnBusy is returned by SendBlocks when a previous prompt is
// still awaiting its ack response. Pi is single-turn per process
// from the bridge's point of view; concurrent SendBlocks calls
// (across goroutines) are rejected so the request id space stays
// single-tenant per session.
var ErrTurnBusy = errors.New("pi: previous prompt not yet acknowledged")

// ErrFrameTooLarge is returned by readLoop when a stdout frame
// exceeds MaxFrameSize. The session is torn down.
var ErrFrameTooLarge = errors.New("pi: frame too large")

// ErrMalformedJSON is returned by readLoop when a frame cannot be
// decoded as a JSON object. The session is torn down.
var ErrMalformedJSON = errors.New("pi: malformed JSON")

// rpcClient owns the JSONL writer (one line at a time, mutex-
// serialized) and the pending response registry. It is the single
// producer of wire bytes; only its methods (writeCommand, respondUI,
// failPending) should ever touch writer.
//
// pending and idAlloc are protected by pendingMu. writer is
// protected by writeMu. The two locks are never held at the same
// time; we never call back into pending from within a writer lock.
//
// We deliberately do NOT wrap the write side in a bufio.Writer:
// io.Pipe (used in tests) and Linux synchronous pipes (used in
// production) only unblock Write when a reader is actively
// pulling bytes, so an in-process buffer would never flush
// unless a reader were already waiting. Writing a single line at
// a time keeps the back-pressure contract obvious: writeLine
// returns only when the kernel pipe buffer has accepted the
// bytes (or the read end has consumed them on synchronous
// pipes).
type rpcClient struct {
	stdinW    io.WriteCloser
	writeMu   sync.Mutex
	pendingMu sync.Mutex
	pending   map[string]chan responseEnvelope
	nextID    atomic.Int64

	// closed flips to true when failPending has been called; new
	// requests after that point return ErrSessionClosed immediately.
	closed atomic.Bool
}

// newRPCClient wraps the write side of the stdin pipe. The caller
// retains ownership of stdinW for Close.
func newRPCClient(stdin io.WriteCloser) *rpcClient {
	return &rpcClient{
		stdinW:  stdin,
		pending: make(map[string]chan responseEnvelope),
	}
}

// nextRequestID returns a fresh, monotonically increasing id rendered
// as a decimal string. We use string ids rather than integers so the
// wire format mirrors Pi's preference for opaque correlation keys
// (extension UI ids are UUIDs, for example) without further
// translation. A simple counter is fine for our own commands.
func (c *rpcClient) nextRequestID() string {
	n := c.nextID.Add(1)
	// Pad to keep lexical order monotonic for nicer log lines.
	return fmt.Sprintf("req-%06d", n)
}

// writeCommand encodes a typed command envelope and writes one
// JSONL line. It returns the assigned id so the caller can register
// a pending response waiter via expectResponse.
func (c *rpcClient) writeCommand(name string, body any, id string) (string, error) {
	if c.closed.Load() {
		return "", ErrSessionClosed
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("pi: marshal %s: %w", name, err)
	}
	// Inline-build the envelope so we always emit exactly
	// {"id":<id>,"type":<name>,<body>} with id first. We use
	// ordered field emission (id, type, body...) so Pi's parser
	// sees a stable shape independent of Go map ordering.
	var buf bytes.Buffer
	buf.WriteByte('{')
	if id != "" {
		buf.WriteString(`"id":`)
		buf.WriteString(jsonString(id))
		buf.WriteByte(',')
	}
	buf.WriteString(`"type":`)
	buf.WriteString(jsonString(name))

	// Body: strip the outer braces of the marshalled map and
	// append the rest. For an empty `{}` body we omit the
	// trailing comma so we never emit `{...,"type":...,}`.
	bodyBytes := payload
	if len(bodyBytes) >= 2 && bodyBytes[0] == '{' && bodyBytes[len(bodyBytes)-1] == '}' {
		bodyBytes = bodyBytes[1 : len(bodyBytes)-1]
	}
	if len(bytes.TrimSpace(bodyBytes)) > 0 {
		buf.WriteByte(',')
		buf.Write(bodyBytes)
	}
	buf.WriteByte('}')
	if err := c.writeLine(buf.Bytes()); err != nil {
		return "", err
	}
	return id, nil
}

// writeLine atomically writes payload + "\n" to the underlying
// pipe. Holds writeMu for the entire critical section so two
// goroutines cannot interleave halves of two frames.
//
// On Linux pipes with a backing process, the kernel pipe buffer
// (64 KiB by default) absorbs the line; writeLine returns
// immediately. On io.Pipe (used in tests) the write blocks until
// a reader pulls the bytes, which is the desired back-pressure
// contract.
func (c *rpcClient) writeLine(payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed.Load() {
		return ErrSessionClosed
	}
	if _, err := c.stdinW.Write(payload); err != nil {
		return fmt.Errorf("pi: write: %w", err)
	}
	if _, err := c.stdinW.Write([]byte{'\n'}); err != nil {
		return fmt.Errorf("pi: write newline: %w", err)
	}
	return nil
}

// expectResponse registers a channel that will receive the next
// response matching id. The channel has buffer 1 so the read loop
// can dispatch without blocking. Returns the channel to the caller
// so it can select on (ctx, channel).
//
// id is stored under the same JSON-encoded form we wrote on the
// wire (i.e. with surrounding quotes for string ids) so that
// dispatchResponse -- which compares against the raw JSON
// representation of the response's "id" field -- finds the
// matching slot. Without this normalisation a request with id
// "boot" would be stored under key boot but looked up as
// "boot", missing every response.
func (c *rpcClient) expectResponse(id string) chan responseEnvelope {
	ch := make(chan responseEnvelope, 1)
	c.pendingMu.Lock()
	c.pending[bytesToID(json.RawMessage(jsonString(id)))] = ch
	c.pendingMu.Unlock()
	return ch
}

// cancelResponse removes a pending waiter. Called by SendBlocks
// after it has received (or given up on) its response, so a late
// response does not leak a buffered channel.
//
// The map key is normalised through bytesToID(jsonString(id)) to
// match what expectResponse stored; without the normalisation a
// request with id "boot" would be stored under key "boot" but
// cancelled under key boot, leaving the entry behind forever.
func (c *rpcClient) cancelResponse(id string) {
	if id == "" {
		return
	}
	key := bytesToID(json.RawMessage(jsonString(id)))
	c.pendingMu.Lock()
	delete(c.pending, key)
	c.pendingMu.Unlock()
}

// dispatchResponse is called by the read loop for every parsed
// response envelope. If the response id is registered, the envelope
// is delivered; the registration is removed so subsequent late
// responses with the same id are dropped. Unregistered ids (and
// responses that arrive after failPending) are silently dropped.
func (c *rpcClient) dispatchResponse(env responseEnvelope) {
	var idKey string
	if len(env.ID) > 0 {
		idKey = bytesToID(env.ID)
	}
	if idKey == "" {
		return
	}
	c.pendingMu.Lock()
	ch, ok := c.pending[idKey]
	if ok {
		delete(c.pending, idKey)
	}
	c.pendingMu.Unlock()
	if !ok {
		return
	}
	// Non-blocking send; the channel is buffered.
	select {
	case ch <- env:
	default:
	}
}

// failPending aborts every registered waiter with the given error
// and marks the client closed. Called from the lifecycle goroutine
// after cmd.Wait returns AND from readPump on its way out (see the
// note in readPump). Idempotent: the second call finds an empty
// pending map and is effectively a no-op aside from the closed flag.
// failPending aborts every registered waiter with the given error
// and marks the client closed. Called from the lifecycle goroutine
// after cmd.Wait returns AND from readPump on its way out (see the
// note in readPump). Idempotent: the second call finds an empty
// pending map and is effectively a no-op aside from the closed flag.
func (c *rpcClient) failPending(err error) {
	c.closed.Store(true)
	c.pendingMu.Lock()
	pending := c.pending
	c.pending = make(map[string]chan responseEnvelope)
	c.pendingMu.Unlock()
	piLog("rpc.failPending", "waiters", len(pending), "cause", errStr(err))
	for _, ch := range pending {
		close(ch)
	}
}

// failResponse is the targeted twin of failPending: it closes the
// channel registered for a single request id so exactly one
// outstanding request() call returns immediately. Used by driver.Stop
// to unblock the SendBlocks goroutine that's hung on the original
// prompt RPC — pi doesn't respond to that RPC after abort, and
// waiting for promptTimeout (90s) would leave turnActive stuck and
// every subsequent SendBlocks bouncing off ErrTurnBusy.
//
// Returns true if a channel was found and closed. False if id is
// unknown (already responded to, or already cancelled). Does NOT
// mark the client closed — the session is still usable for a
// follow-up prompt.
func (c *rpcClient) failResponse(id string, err error) bool {
	if id == "" {
		return false
	}
	key := bytesToID(json.RawMessage(jsonString(id)))
	c.pendingMu.Lock()
	ch, ok := c.pending[key]
	if ok {
		delete(c.pending, key)
	}
	c.pendingMu.Unlock()
	if !ok {
		piLog("rpc.failResponse no-op", "id", id, "cause", errStr(err))
		return false
	}
	piLog("rpc.failResponse", "id", id, "cause", errStr(err))
	close(ch)
	return true
}

// errStr is a tiny nil-safe error stringifier for log fields.
func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// request is the synchronous helper: it assigns an id, writes the
// command, registers a pending response, and waits (with ctx) for
// either the response or a session failure. On success it returns
// the typed response envelope; on failure it returns the cause.
//
// The caller is responsible for the typed decoding of the response
// `data` field, so this helper just unwraps the envelope.
//
// Ordering: the pending slot is registered BEFORE the command is
// written. This prevents a write-before-register race in which the
// child writes its response faster than the caller reaches
// expectResponse (e.g. on a fast local pipe or under Go
// scheduler pressure): if writeCommand ran first, the response
// would be dispatched against an empty pending map and dropped.
// The symmetric design -- pending-first -- is the same fix ACP
// adopted in internal/bridge/acp/rpc.go:58-94.
func (c *rpcClient) request(ctx context.Context, name string, body any, idHint string) (responseEnvelope, error) {
	if c.closed.Load() {
		return responseEnvelope{}, ErrSessionClosed
	}
	id := idHint
	if id == "" {
		id = c.nextRequestID()
	}
	// reqStart stamps the whole request/response round-trip so
	// the debug log shows exactly where the wait happened when
	// handshake stalls (F-32 incident 2026-08-06).
	reqStart := time.Now()
	// Register the pending slot first so a fast response from
	// the child cannot outrun the map write.
	ch := c.expectResponse(id)
	if _, err := c.writeCommand(name, body, id); err != nil {
		c.cancelResponse(id)
		piLog("rpc.request write failed",
			"id", id, "name", name,
			"elapsed_ms", time.Since(reqStart).Milliseconds(),
			"err", err.Error())
		return responseEnvelope{}, err
	}
	defer c.cancelResponse(id)
	select {
	case env, ok := <-ch:
		if !ok {
			piLog("rpc.request session closed",
				"id", id, "name", name,
				"elapsed_ms", time.Since(reqStart).Milliseconds())
			return responseEnvelope{}, ErrSessionClosed
		}
		piLog("rpc.request ok",
			"id", id, "name", name,
			"elapsed_ms", time.Since(reqStart).Milliseconds(),
			"success", env.Success)
		return env, nil
	case <-ctx.Done():
		piLog("rpc.request ctx cancelled",
			"id", id, "name", name,
			"elapsed_ms", time.Since(reqStart).Milliseconds(),
			"err", ctx.Err().Error())
		return responseEnvelope{}, ctx.Err()
	}
}

// jsonString returns the JSON-encoded form of a Go string with
// surrounding double quotes. Cheaper than a full json.Marshal of a
// one-field struct for the hot path.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// bytesToID extracts the literal form of a JSON id. Pi uses either
// string ids ("req-001") or numeric ids; we keep the raw form so the
// pending map key matches what the wire carries.
func bytesToID(raw json.RawMessage) string {
	// Trim leading/trailing whitespace just in case.
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return ""
	}
	return string(trimmed)
}
