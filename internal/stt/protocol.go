package stt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// ProtocolVersion is the wire-protocol version spoken by both
// client and worker. Bump on any incompatible change to the
// request / response shape below; minor additions (new op codes)
// can keep it constant if the worker tolerates the absence of
// fields it doesn't yet understand.
const ProtocolVersion = 1

// Op codes. The worker dispatches on these; the client uses the
// same set in its Request envelope. Adding a new op is a minor
// protocol change that doesn't require a version bump as long as
// the worker returns CodeInvalidRequest on the unknown op.
const (
	OpHealth     = "health"
	OpVersion    = "version"
	OpTranscribe = "transcribe"
	OpShutdown   = "shutdown"
)

// Asset is what a Resolver hands back to the Installer. URL +
// SHA256 + Size are the integrity triple (issue #381 §5);
// Archive + Binary tell the installer how to extract the
// executable; StripDir is the top-level directory inside a model
// tar.bz2 archive that should be removed during extraction.
type Asset struct {
	Tag      string
	Name     string
	URL      string
	SHA256   string
	Size     int64
	Archive  string // "tar.gz" or "zip"
	Binary   string // executable name INSIDE the archive
	StripDir string // for tar.bz2 model archives
}

// Request is the wire envelope sent client → worker. Audio is
// embedded as a base64-encoded field rather than a separate
// length-prefixed blob so the framing rule is one and the same
// for every operation (a 4-byte length prefix followed by exactly
// one JSON payload).
type Request struct {
	Version int             `json:"version"`
	Op      string          `json:"op"`
	Format  string          `json:"format,omitempty"`
	Audio   []byte          `json:"audio,omitempty"`
	Extra   json.RawMessage `json:"extra,omitempty"`
}

// Response is the wire envelope sent worker → client.
type Response struct {
	Version    int    `json:"version"`
	OK         bool   `json:"ok"`
	ErrorCode  string `json:"error_code,omitempty"`
	ErrorMsg   string `json:"error,omitempty"`
	Text       string `json:"text,omitempty"`
	Language   string `json:"language,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

// MaxFrameSize caps individual frames so a malformed peer can't
// OOM us. 32 MiB comfortably covers the 20 MiB audio cap
// (issue #381 §14) with headroom for the JSON envelope.
const MaxFrameSize = 32 << 20

// Frame is one length-prefixed JSON message. ReadFrame /
// WriteFrame own the framing details; the typed payloads (Request /
// Response) are layered on top.
type Frame struct {
	Version int             `json:"version"`
	Op      string          `json:"op,omitempty"`
	Type    string          `json:"type,omitempty"` // "req" or "resp"
	Payload json.RawMessage `json:"payload"`
}

var (
	ErrFrameTooLarge    = errors.New("stt: frame exceeds maximum size")
	ErrShortRead        = errors.New("stt: short read on framed payload")
	ErrProtocolMismatch = errors.New("stt: protocol version mismatch")
)

// WriteFrame writes one framed JSON payload to w. The length prefix
// is a 4-byte big-endian uint32; the payload is the JSON encoding
// of payload.
func WriteFrame(w io.Writer, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("stt: encode frame: %w", err)
	}
	if len(encoded) > MaxFrameSize {
		return ErrFrameTooLarge
	}
	header := [4]byte{
		byte(len(encoded) >> 24),
		byte(len(encoded) >> 16),
		byte(len(encoded) >> 8),
		byte(len(encoded)),
	}
	if _, err := w.Write(header[:]); err != nil {
		return fmt.Errorf("stt: write frame header: %w", err)
	}
	if _, err := w.Write(encoded); err != nil {
		return fmt.Errorf("stt: write frame payload: %w", err)
	}
	return nil
}

// ReadFrame reads one framed JSON payload from r. out must be a
// non-nil pointer.
func ReadFrame(r io.Reader, out any) error {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return io.EOF
		}
		return fmt.Errorf("stt: read frame header: %w", err)
	}
	size := uint32(header[0])<<24 | uint32(header[1])<<16 |
		uint32(header[2])<<8 | uint32(header[3])
	if size == 0 {
		return errors.New("stt: empty frame")
	}
	if size > MaxFrameSize {
		return ErrFrameTooLarge
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		return fmt.Errorf("%w: %v", ErrShortRead, err)
	}
	if err := json.Unmarshal(buf, out); err != nil {
		return fmt.Errorf("stt: decode frame: %w", err)
	}
	return nil
}

// TranscribeOptions configures a single transcribe call. The
// caller fills this in before passing the audio bytes to the
// client. Decoupled from Request so the worker side can be tested
// without the wire format.
type TranscribeOptions struct {
	Format  string        // "ogg" or "wav"
	Audio   []byte        // raw bytes
	Timeout time.Duration // 0 = no extra timeout on top of ctx
}

// TranscribeResult is what the worker returns on a successful
// transcription. Failed transcriptions surface as *Error.
type TranscribeResult struct {
	Text       string
	Language   string
	DurationMS int64
}

// workerHealthCheck is the JSON shape returned by OpHealth.
type workerHealthCheck struct {
	Version int  `json:"version"`
	Ready   bool `json:"ready"`
}

// EncodeRequest marshals a typed Request to JSON bytes (used by
// the client) and frames it for the local transport.
func EncodeRequest(req *Request) ([]byte, error) {
	if req.Version == 0 {
		req.Version = ProtocolVersion
	}
	return json.Marshal(req)
}

// EncodeResponse marshals a typed Response to JSON bytes.
func EncodeResponse(resp *Response) ([]byte, error) {
	if resp.Version == 0 {
		resp.Version = ProtocolVersion
	}
	return json.Marshal(resp)
}

// DecodeResponse unmarshals a Response from JSON bytes and validates
// the protocol version.
func DecodeResponse(data []byte) (*Response, error) {
	var resp Response
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("stt: decode response: %w", err)
	}
	if resp.Version != ProtocolVersion {
		return nil, fmt.Errorf("%w: got %d, want %d",
			ErrProtocolMismatch, resp.Version, ProtocolVersion)
	}
	return &resp, nil
}

// ctxDeadline is a small helper used by both the client (to set the
// per-call deadline on the worker) and the worker side (to honor
// it during decode). Returns the absolute deadline, falling
// back to a default if ctx has none.
func ctxDeadline(ctx context.Context, fallback time.Duration) (time.Time, bool) {
	if d, ok := ctx.Deadline(); ok {
		return d, true
	}
	return time.Now().Add(fallback), false
}
