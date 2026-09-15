package stt

import (
	"context"
	"fmt"
	"log/slog"
)

// Server runs the worker side of the local RPC protocol. One
// Server is created per nightme-stt process; it owns the
// recognizer lifecycle and serialises transcription requests.
//
// Server is decoupled from main() so tests can spin up an
// in-process Server (loopback transport) without forking
// nightme-stt.
type Server struct {
	listener   Listener
	decoder    Decoder
	recognizer Recognizer
	logger     *slog.Logger
}

// NewServer wires a Server. Recognizer is loaded lazily by
// the caller — the Server does not know whether the production
// sherpa-onnx-go model or a test stub is behind the interface.
func NewServer(listener Listener, decoder Decoder, recognizer Recognizer, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		listener:   listener,
		decoder:    decoder,
		recognizer: recognizer,
		logger:     logger,
	}
}

// Serve accepts inbound connections until ctx is done or
// the listener is closed. V1 processes one connection at a
// time per Listener (issue #381 §2).
func (s *Server) Serve(ctx context.Context) error {
	for {
		conn, err := s.listener.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Listener closed mid-loop. Treat as a normal
			// shutdown signal so the deferred Close chain runs
			// cleanly.
			return nil
		}
		if err := s.handleConn(ctx, conn); err != nil {
			s.logger.Warn("stt worker: connection ended", "err", err)
		}
	}
}

// handleConn runs one client connection to completion. Each
// request is parsed → dispatched → answered; Shutdown closes
// the connection (the client is expected to read the response
// and then dial again if it wants to keep using STT — this is
// how the manager's Stop sequence is supposed to work).
func (s *Server) handleConn(ctx context.Context, conn Conn) error {
	defer conn.Close()
	for {
		var req Request
		if err := conn.ReadFrame(ctx, &req); err != nil {
			return err
		}
		resp := s.dispatch(ctx, &req)
		if err := conn.WriteFrame(ctx, resp); err != nil {
			return err
		}
		if req.Op == OpShutdown {
			return nil
		}
	}
}

// dispatch routes one Request to the right handler. Invalid /
// unsupported requests get a typed error so the client can render
// an actionable message instead of a generic "worker failed".
func (s *Server) dispatch(ctx context.Context, req *Request) *Response {
	if req.Version != ProtocolVersion {
		return &Response{
			Version:   ProtocolVersion,
			OK:        false,
			ErrorCode: CodeProtocolMismatch,
			ErrorMsg: fmt.Sprintf("client version %d, worker expects %d",
				req.Version, ProtocolVersion),
		}
	}
	switch req.Op {
	case OpHealth:
		return &Response{Version: ProtocolVersion, OK: true}
	case OpVersion:
		return &Response{Version: ProtocolVersion, OK: true}
	case OpTranscribe:
		return s.transcribe(ctx, req)
	case OpShutdown:
		return &Response{Version: ProtocolVersion, OK: true}
	default:
		return &Response{
			Version:   ProtocolVersion,
			OK:        false,
			ErrorCode: CodeInvalidRequest,
			ErrorMsg:  fmt.Sprintf("unknown op %q", req.Op),
		}
	}
}

func (s *Server) transcribe(ctx context.Context, req *Request) *Response {
	format := AudioFormat(req.Format)
	switch format {
	case FormatOGG, FormatWAV:
	default:
		return &Response{
			Version:   ProtocolVersion,
			OK:        false,
			ErrorCode: CodeUnsupportedFormat,
			ErrorMsg:  fmt.Sprintf("format %q not supported", req.Format),
		}
	}
	if len(req.Audio) == 0 {
		return &Response{
			Version:   ProtocolVersion,
			OK:        false,
			ErrorCode: CodeInvalidRequest,
			ErrorMsg:  "empty audio",
		}
	}
	if len(req.Audio) > MaxRequestBytes {
		return &Response{
			Version:   ProtocolVersion,
			OK:        false,
			ErrorCode: CodeOversizedRequest,
			ErrorMsg:  fmt.Sprintf("audio %d exceeds limit %d", len(req.Audio), MaxRequestBytes),
		}
	}
	samples, err := s.decoder.Decode(ctx, req.Audio, format)
	if err != nil {
		return &Response{
			Version:   ProtocolVersion,
			OK:        false,
			ErrorCode: CodeTranscriptionFailed,
			ErrorMsg:  fmt.Sprintf("decode: %v", err),
		}
	}
	out, err := s.recognizer.Recognize(ctx, RecognizeRequest{Format: format, Samples: samples})
	if err != nil {
		if e, ok := AsError(err); ok {
			return &Response{
				Version:   ProtocolVersion,
				OK:        false,
				ErrorCode: e.Code,
				ErrorMsg:  e.Message,
			}
		}
		return &Response{
			Version:   ProtocolVersion,
			OK:        false,
			ErrorCode: CodeTranscriptionFailed,
			ErrorMsg:  err.Error(),
		}
	}
	if len(out.Text) > MaxTranscriptBytes {
		out.Text = out.Text[:MaxTranscriptBytes]
	}
	return &Response{
		Version:    ProtocolVersion,
		OK:         true,
		Text:       out.Text,
		Language:   out.Language,
		DurationMS: int64(len(samples) / 32), // 16 kHz * 2 bytes = 32 bytes/sec → bytes/32 = ms
	}
}
