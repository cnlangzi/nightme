package stt

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestRPCClient_HealthLoopback drives the client through a
// Health RPC against an in-process Server using the loopback
// transport. This is the canonical smoke test for the
// frame / protocol / serialization layers all working
// together.
func TestRPCClient_HealthLoopback(t *testing.T) {
	transport, listener := Loopback()
	srv := NewServer(listener, PassthroughDecoder{}, StaticRecognizer{Reply: "ok"}, nil)

	go func() {
		_ = srv.Serve(context.Background())
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	conn, err := transport.Dial(ctx, "")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	client := NewRPCClient(conn)
	if err := client.Health(ctx); err != nil {
		t.Fatalf("Health: %v", err)
	}
}

// TestRPCClient_TranscribeLoopback drives a full transcribe
// round-trip with a canned recognizer response.
func TestRPCClient_TranscribeLoopback(t *testing.T) {
	transport, listener := Loopback()
	srv := NewServer(listener, PassthroughDecoder{}, StaticRecognizer{Reply: "hello"}, nil)

	go func() {
		_ = srv.Serve(context.Background())
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	conn, err := transport.Dial(ctx, "")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	client := NewRPCClient(conn)
	res, err := client.Transcribe(ctx, []byte("test"), "ogg")
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if res.Text != "hello" {
		t.Fatalf("got text=%q, want hello", res.Text)
	}
}

// TestRPCClient_VersionMismatch pins that the client rejects
// a worker speaking the wrong protocol version.
func TestRPCClient_VersionMismatch(t *testing.T) {
	// Custom transport / server that hand-rolls a response
	// with the wrong version.
	transport, listener := Loopback()
	go func() {
		conn, err := listener.Accept(context.Background())
		if err != nil {
			return
		}
		defer conn.Close()
		// Drain the request, send a bogus response.
		var req Request
		_ = conn.ReadFrame(context.Background(), &req)
		_ = conn.WriteFrame(context.Background(), &Response{
			Version: ProtocolVersion + 99,
			OK:      true,
		})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	conn, err := transport.Dial(ctx, "")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	client := NewRPCClient(conn)
	if _, err := client.Version(ctx); !errors.Is(err, ErrProtocolMismatch) {
		t.Fatalf("Version: want ErrProtocolMismatch, got %v", err)
	}
}

// TestRPCClient_EmptyAudioRejected pins the input-validation
// guard in Transcribe: empty audio must fail cleanly rather
// than spinning up the worker.
func TestRPCClient_EmptyAudioRejected(t *testing.T) {
	transport, listener := Loopback()
	srv := NewServer(listener, PassthroughDecoder{}, StaticRecognizer{Reply: "x"}, nil)
	go func() { _ = srv.Serve(context.Background()) }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	conn, err := transport.Dial(ctx, "")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	client := NewRPCClient(conn)
	_, err = client.Transcribe(ctx, nil, "ogg")
	if !IsCode(err, CodeInvalidRequest) {
		t.Fatalf("Transcribe(nil): want CodeInvalidRequest, got %v", err)
	}
}
