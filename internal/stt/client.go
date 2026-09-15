package stt

import (
	"context"
	"errors"
	"fmt"
)

// Transcriber is the in-process contract NightMe uses to talk
// to voice-to-text. Implementations may be a local RPC client
// (RPCClient below) or a fake (used by the Telegram adapter's
// unit tests so they don't pull in the 200 MB model).
//
// Operations are synchronous: Transcribe blocks until the worker
// responds or the context cancels. Serialisation across
// concurrent Voice messages is the manager's job — the worker
// only handles one transcription at a time in V1.
type Transcriber interface {
	// Health returns nil if the worker is ready to accept a
	// transcription. Used by the manager to verify readiness
	// after a spawn, and by `nightme doctor` to surface
	// component state.
	Health(ctx context.Context) error
	// Version returns the worker's protocol version. The client
	// rejects a response whose Version does not match
	// ProtocolVersion — see issue #381 §4 protocol/version
	// mismatch must fail clearly.
	Version(ctx context.Context) (int, error)
	// Transcribe sends audio bytes to the worker and waits for
	// the text result. Format is the audio container hint
	// ("ogg", "wav", ...).
	Transcribe(ctx context.Context, audio []byte, format string) (TranscribeResult, error)
	// Close releases any held resources (the underlying
	// connection, etc.). The worker process is NOT killed —
	// the manager owns the worker's lifecycle.
	Close() error
}

// RPCClient is the production Transcriber implementation.
// It owns a single connection to nightme-stt; one RPCClient
// instance is held by the Process Manager for the life of
// the worker process. The connection is not safe for
// concurrent use — Transcribe serialises through a mutex.
type RPCClient struct {
	conn Conn
	mu   chan struct{}
}

// NewRPCClient wraps an already-dialed Conn. The Manager owns
// dialing; tests can pass an in-process pipe.
func NewRPCClient(conn Conn) *RPCClient {
	return &RPCClient{
		conn: conn,
		mu:   make(chan struct{}, 1),
	}
}

// Health implements Transcriber.
func (c *RPCClient) Health(ctx context.Context) error {
	_, err := c.call(ctx, &Request{Version: ProtocolVersion, Op: OpHealth})
	return err
}

// Version implements Transcriber.
func (c *RPCClient) Version(ctx context.Context) (int, error) {
	resp, err := c.call(ctx, &Request{Version: ProtocolVersion, Op: OpVersion})
	if err != nil {
		return 0, err
	}
	return resp.Version, nil
}

// Transcribe implements Transcriber.
func (c *RPCClient) Transcribe(ctx context.Context, audio []byte, format string) (TranscribeResult, error) {
	if len(audio) == 0 {
		return TranscribeResult{}, NewError(CodeInvalidRequest, "empty audio")
	}
	if len(audio) > MaxRequestBytes {
		return TranscribeResult{}, NewError(CodeOversizedRequest,
			fmt.Sprintf("audio %d bytes exceeds limit %d", len(audio), MaxRequestBytes))
	}
	resp, err := c.call(ctx, &Request{
		Version: ProtocolVersion,
		Op:      OpTranscribe,
		Format:  format,
		Audio:   audio,
	})
	if err != nil {
		return TranscribeResult{}, err
	}
	if len(resp.Text) > MaxTranscriptBytes {
		// Truncate, never bubble up as an error — the
		// recognizer may have produced a runaway token
		// stream. The user still benefits from the prefix.
		resp.Text = resp.Text[:MaxTranscriptBytes]
	}
	return TranscribeResult{
		Text:       resp.Text,
		Language:   resp.Language,
		DurationMS: resp.DurationMS,
	}, nil
}

// Close implements Transcriber.
func (c *RPCClient) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// MaxRequestBytes is the audio byte budget the client enforces
// before issuing an RPC. Sized to comfortably cover Telegram's
// 50 MB file ceiling and the 10-minute voice duration target
// from issue #381 §14 — over-budget requests are rejected
// without contacting the worker.
const MaxRequestBytes = 20 << 20 // 20 MiB

// MaxTranscriptBytes bounds the text we will accept back from
// the worker before truncating. SenseVoice outputs are short;
// this cap guards against a misbehaving model emitting a wall
// of tokens rather than an utterance.
const MaxTranscriptBytes = 20_000

// call serialises a request/response cycle. Errors from the
// worker arrive as typed *Error values; transport failures
// (peer hangup, frame error, etc.) come back wrapped.
func (c *RPCClient) call(ctx context.Context, req *Request) (*Response, error) {
	if c.conn == nil {
		return nil, NewError(CodeInternal, "stt: client has no connection")
	}
	// Serialise concurrent callers. Without this two
	// goroutines on the same Conn could interleave their
	// length-prefixed frames; the worker only handles one
	// request at a time anyway.
	select {
	case c.mu <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-c.mu }()

	if err := c.conn.WriteFrame(ctx, req); err != nil {
		return nil, err
	}
	var resp Response
	if err := c.conn.ReadFrame(ctx, &resp); err != nil {
		return nil, err
	}
	if resp.Version != ProtocolVersion {
		return nil, fmt.Errorf("%w: got %d, want %d",
			ErrProtocolMismatch, resp.Version, ProtocolVersion)
	}
	if !resp.OK {
		code := resp.ErrorCode
		if code == "" {
			code = CodeInternal
		}
		return nil, NewError(code, resp.ErrorMsg)
	}
	return &resp, nil
}

// writeFrame is a thin wrapper around c.conn.WriteFrame that
// tags the error so callers can tell transport failures apart
// from worker-reported errors.
//
// (Replaced by direct conn.WriteFrame usage in call(); kept
// here as documentation of the transport-failure vs worker-
// error distinction.)
var _ = errors.New // keep the errors import in use
