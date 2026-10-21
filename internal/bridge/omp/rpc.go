// JSONL framing + request/response correlation for the omp RPC
// bridge.
//
// The bridge drives a long-lived `omp --mode rpc` child over
// real stdio pipes (NOT PTY). Every wire message is exactly
// one JSON object followed by a single LF byte; no JSON-RPC
// envelope. Commands carry an optional "id" for correlation;
// responses echo the same id and add "type":"response" +
// "command" + "success" + optional "data"/"error". Events are
// async server pushes with no id; the translator handles them
// in agent.go's readPump.
//
// The package deliberately avoids importing the existing
// internal/bridge/pi package: omp diverged from pi (terminal
// signal moved from agent_settled to agent_end{isTerminal:true};
// thinking is a dedicated content block, not inline <think>
// tags; control command vocabulary is its own). Coupling the
// two bridges would double-impact every wire change.

package omp

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

// MaxFrameSize caps the size of a single JSONL frame the
// bridge will accept on stdout. omp reports maxFrameBytes=1 MiB
// on the wire but a single get_state response in practice is
// ≈33 KiB (carrying the full systemPrompt + dumpTools) and an
// available_commands_update frame is ≈16.7 KiB. 4 MiB matches
// internal/bridge/pi.MaxFrameSize and is large enough for a
// full chat-sized image plus accumulated text. Frames larger
// than this terminate the session.
const MaxFrameSize = 4 * 1024 * 1024

// ErrSessionClosed is returned by request when the session
// has been (or is being) closed. Callers (notably SendBlocks)
// should treat this as a terminal error and not retry.
var ErrSessionClosed = errors.New("omp: session closed")

// ErrTurnAborted is the cause driver.Stop logs when it calls
// failResponse on an in-flight prompt RPC. The waiter never
// sees it as a return value — failResponse closes the pending
// channel unconditionally, which the request() select reads
// as !ok and maps to ErrSessionClosed.
var ErrTurnAborted = errors.New("omp: turn aborted")

// ErrTurnBusy is returned by SendBlocks when a previous
// prompt is still awaiting its ack response. omp is
// single-turn per process from the bridge's point of view;
// concurrent SendBlocks calls are rejected so the request id
// space stays single-tenant per session.
var ErrTurnBusy = errors.New("omp: previous prompt not yet acknowledged")

// ErrFrameTooLarge is returned by readLoop when a stdout
// frame exceeds MaxFrameSize. The session is torn down.
var ErrFrameTooLarge = errors.New("omp: frame too large")

// ErrMalformedJSON is returned by readLoop when a frame cannot
// be decoded as a JSON object.
var ErrMalformedJSON = errors.New("omp: malformed JSON")

// rpcClient owns the JSONL writer (one line at a time, mutex-
// serialized) and the pending response registry. It is the
// single producer of wire bytes; only its methods
// (writeCommand, failPending) should ever touch the writer.
//
// pending and idAlloc are protected by pendingMu. writer is
// protected by writeMu. The two locks are never held at the
// same time; we never call back into pending from within a
// writer lock.
//
// We deliberately do NOT wrap the write side in a
// bufio.Writer: io.Pipe (used in tests) and Linux synchronous
// pipes only unblock Write when a reader is actively pulling
// bytes. Writing one line at a time keeps the back-pressure
// contract obvious.
type rpcClient struct {
	stdinW    io.WriteCloser
	writeMu   sync.Mutex
	pendingMu sync.Mutex
	pending   map[string]chan responseEnvelope
	nextID    atomic.Int64

	// closed flips to true when failPending has been called;
	// new requests after that point return ErrSessionClosed
	// immediately.
	closed atomic.Bool
}

// newRPCClient wraps the write side of the stdin pipe. The
// caller retains ownership of stdinW for Close.
func newRPCClient(stdin io.WriteCloser) *rpcClient {
	return &rpcClient{
		stdinW:  stdin,
		pending: make(map[string]chan responseEnvelope),
	}
}

// nextRequestID returns a fresh, monotonically increasing id
// rendered as a decimal string. We use string ids rather than
// integers so the wire format mirrors omp's preference for
// opaque correlation keys without further translation. A
// simple counter is fine for our own commands.
func (c *rpcClient) nextRequestID() string {
	n := c.nextID.Add(1)
	return fmt.Sprintf("req-%06d", n)
}

// writeCommand encodes a typed command envelope and writes
// one JSONL line. Returns the assigned id so the caller can
// register a pending response waiter via expectResponse.
func (c *rpcClient) writeCommand(name string, body any, id string) (string, error) {
	if c.closed.Load() {
		return "", ErrSessionClosed
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("omp: marshal %s: %w", name, err)
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	if id != "" {
		buf.WriteString(`"id":`)
		buf.WriteString(jsonString(id))
		buf.WriteByte(',')
	}
	buf.WriteString(`"type":`)
	buf.WriteString(jsonString(name))

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
func (c *rpcClient) writeLine(payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed.Load() {
		return ErrSessionClosed
	}
	if _, err := c.stdinW.Write(payload); err != nil {
		return fmt.Errorf("omp: write: %w", err)
	}
	if _, err := c.stdinW.Write([]byte{'\n'}); err != nil {
		return fmt.Errorf("omp: write newline: %w", err)
	}
	return nil
}

// expectResponse registers a channel that will receive the
// next response matching id. The channel has buffer 1 so the
// read loop can dispatch without blocking.
func (c *rpcClient) expectResponse(id string) chan responseEnvelope {
	ch := make(chan responseEnvelope, 1)
	c.pendingMu.Lock()
	c.pending[bytesToID(json.RawMessage(jsonString(id)))] = ch
	c.pendingMu.Unlock()
	return ch
}

// cancelResponse removes a pending waiter.
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
// response envelope.
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
	select {
	case ch <- env:
	default:
	}
}

// failPending aborts every registered waiter with the given
// error and marks the client closed. Idempotent.
func (c *rpcClient) failPending(err error) {
	c.closed.Store(true)
	c.pendingMu.Lock()
	pending := c.pending
	c.pending = make(map[string]chan responseEnvelope)
	c.pendingMu.Unlock()
	_ = err
	oLog("rpc.failPending", "waiters", len(pending), "cause", errStr(err))
	for _, ch := range pending {
		close(ch)
	}
}

// failResponse is the targeted twin of failPending.
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
		oLog("rpc.failResponse no-op", "id", id, "cause", errStr(err))
		return false
	}
	oLog("rpc.failResponse", "id", id, "cause", errStr(err))
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

// request is the synchronous helper: assigns an id, writes the
// command, registers a pending response, and waits (with
// ctx) for either the response or a session failure.
//
// Ordering: the pending slot is registered BEFORE the command
// is written. This prevents a write-before-register race in
// which the child writes its response faster than the caller
// reaches expectResponse.
func (c *rpcClient) request(ctx context.Context, name string, body any, idHint string) (responseEnvelope, error) {
	if c.closed.Load() {
		return responseEnvelope{}, ErrSessionClosed
	}
	id := idHint
	if id == "" {
		id = c.nextRequestID()
	}
	reqStart := time.Now()
	ch := c.expectResponse(id)
	if _, err := c.writeCommand(name, body, id); err != nil {
		c.cancelResponse(id)
		oLog("rpc.request write failed",
			"id", id, "name", name,
			"elapsed_ms", time.Since(reqStart).Milliseconds(),
			"err", err.Error())
		return responseEnvelope{}, err
	}
	defer c.cancelResponse(id)
	select {
	case env, ok := <-ch:
		if !ok {
			oLog("rpc.request session closed",
				"id", id, "name", name,
				"elapsed_ms", time.Since(reqStart).Milliseconds())
			return responseEnvelope{}, ErrSessionClosed
		}
		oLog("rpc.request ok",
			"id", id, "name", name,
			"elapsed_ms", time.Since(reqStart).Milliseconds(),
			"success", env.Success)
		return env, nil
	case <-ctx.Done():
		oLog("rpc.request ctx cancelled",
			"id", id, "name", name,
			"elapsed_ms", time.Since(reqStart).Milliseconds(),
			"err", ctx.Err().Error())
		return responseEnvelope{}, ctx.Err()
	}
}

// jsonString returns the JSON-encoded form of a Go string
// with surrounding double quotes.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// bytesToID extracts the literal form of a JSON id.
func bytesToID(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return ""
	}
	return string(trimmed)
}
