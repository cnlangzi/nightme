package stt

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// TestWriteFrameReadFrame_RoundTrip pins the 4-byte big-endian
// length prefix + JSON payload contract. Anything that breaks
// this round-trip silently breaks the worker / client handshake.
func TestWriteFrameReadFrame_RoundTrip(t *testing.T) {
	in := &Request{Version: ProtocolVersion, Op: OpTranscribe, Format: "ogg", Audio: []byte("hello")}
	var buf bytes.Buffer
	if err := WriteFrame(&buf, in); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	if buf.Len() != 4+len(mustMarshal(t, in)) {
		t.Fatalf("WriteFrame produced %d bytes; want 4+len(payload)", buf.Len())
	}
	var out Request
	if err := ReadFrame(&buf, &out); err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if out.Op != OpTranscribe || out.Format != "ogg" || string(out.Audio) != "hello" {
		t.Fatalf("decoded request mismatch: %+v", out)
	}
}

// TestReadFrame_TooLarge pins the MaxFrameSize guard. We feed
// a length prefix that promises more than MaxFrameSize and
// expect ErrFrameTooLarge.
func TestReadFrame_TooLarge(t *testing.T) {
	oversize := uint32(MaxFrameSize + 1)
	header := [4]byte{
		byte(oversize >> 24),
		byte(oversize >> 16),
		byte(oversize >> 8),
		byte(oversize),
	}
	r := io.MultiReader(bytes.NewReader(header[:]), strings.NewReader(""))
	if err := ReadFrame(r, &Request{}); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("ReadFrame want ErrFrameTooLarge, got %v", err)
	}
}

// TestReadFrame_Empty pins that a zero-length frame is
// rejected. Empty frames would let a buggy peer wedge the
// reader.
func TestReadFrame_Empty(t *testing.T) {
	r := bytes.NewReader([]byte{0, 0, 0, 0})
	if err := ReadFrame(r, &Request{}); err == nil {
		t.Fatalf("ReadFrame: want error for empty frame, got nil")
	}
}

// TestEncodeRequestDecodeResponse_RoundTrip exercises the
// JSON-level helpers used by the production RPCClient.
func TestEncodeRequestDecodeResponse_RoundTrip(t *testing.T) {
	req := &Request{Version: 0, Op: OpHealth}
	if _, err := EncodeRequest(req); err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	// EncodeRequest sets the version on the wire payload
	// (zero → ProtocolVersion). Verify.
	if req.Version != ProtocolVersion {
		t.Fatalf("EncodeRequest did not stamp version: got %d", req.Version)
	}
	resp := &Response{Version: ProtocolVersion, OK: true, Text: "ok"}
	rdata, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	decoded, err := DecodeResponse(rdata)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if !decoded.OK || decoded.Text != "ok" {
		t.Fatalf("decoded response mismatch: %+v", decoded)
	}
}

// TestDecodeResponse_VersionMismatch pins that a worker
// speaking the wrong protocol is rejected up front rather
// than producing a confusing downstream error.
func TestDecodeResponse_VersionMismatch(t *testing.T) {
	resp := &Response{Version: ProtocolVersion + 99, OK: true}
	data, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if _, err := DecodeResponse(data); !errors.Is(err, ErrProtocolMismatch) {
		t.Fatalf("DecodeResponse want ErrProtocolMismatch, got %v", err)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteFrame(&buf, v); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := make([]byte, buf.Len()-4)
	copy(out, buf.Bytes()[4:])
	return out
}
