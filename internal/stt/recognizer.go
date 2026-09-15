package stt

import (
	"context"
	"fmt"
)

// Recognizer is the abstract interface the nightme-stt worker
// delegates speech-to-text to. The production implementation lives
// behind the sherpa-onnx-go binding in cmd/nightme-stt; this
// package defines the contract so the rest of NightMe core can be
// tested without CGO or a model file.
//
// All methods must be safe to call from a single goroutine (the
// worker serializes requests — issue #381 §2 V1 processes one
// transcription at a time).
type Recognizer interface {
	// Recognize runs the model on the decoded samples and returns
	// the transcription. Empty Text with a nil error is a valid
	// outcome (silent audio, model rejected the input, etc.).
	Recognize(ctx context.Context, req RecognizeRequest) (RecognizeResult, error)
	// Close releases model memory. Called once at worker shutdown.
	Close() error
}

// AudioFormat is a container hint passed from the channel
// adapter to the worker. The worker is responsible for decoding
// into PCM suitable for the recognizer; this package does not
// know or care how.
type AudioFormat string

const (
	FormatOGG AudioFormat = "ogg"
	FormatWAV AudioFormat = "wav"
)

// RecognizeRequest is the recognizer-facing input. The worker
// decodes raw audio bytes into Samples before handing them off
// to the recognizer, so recognizer implementations never see
// codec headers.
//
// Recognizers are intentionally long-lived: one recognizer
// is created when the worker first becomes Ready, and
// reused for every subsequent transcription. Loading the
// SenseVoice model on every request would dominate the
// wall-clock cost (issue #381 §3).
type RecognizeRequest struct {
	Format  AudioFormat
	Samples []byte // 16-bit signed little-endian mono PCM at 16 kHz
}

// RecognizeResult is the recognizer-facing output.
type RecognizeResult struct {
	Text     string
	Language string
}

// StaticRecognizer is a Recognizer implementation backed by a
// fixed transcription table. Used by unit tests to exercise the
// worker → client path without loading the SenseVoice model. Not
// used in production.
type StaticRecognizer struct {
	Reply string
}

func (s StaticRecognizer) Recognize(ctx context.Context, req RecognizeRequest) (RecognizeResult, error) {
	if len(req.Samples) == 0 {
		return RecognizeResult{}, NewError(CodeTranscriptionFailed, "no samples")
	}
	return RecognizeResult{Text: s.Reply, Language: "en"}, nil
}

func (s StaticRecognizer) Close() error { return nil }

// Decoder turns a container-formatted audio byte slice into
// raw PCM samples. The production worker wires ffmpeg here;
// the test path uses a no-op decoder that treats the input
// as already-decoded PCM.
type Decoder interface {
	Decode(ctx context.Context, data []byte, format AudioFormat) ([]byte, error)
}

// DecoderFunc is the function-shaped adapter for the
// Decoder interface. Lets callers write `ffmpeg.Decoder` once
// and reuse it.
type DecoderFunc func(ctx context.Context, data []byte, format AudioFormat) ([]byte, error)

func (f DecoderFunc) Decode(ctx context.Context, data []byte, format AudioFormat) ([]byte, error) {
	return f(ctx, data, format)
}

// PassthroughDecoder returns data verbatim. Useful for unit
// tests where the caller has already produced raw PCM samples
// (a deterministic byte string) and just wants the recognizer
// to see them.
type PassthroughDecoder struct{}

func (PassthroughDecoder) Decode(_ context.Context, data []byte, _ AudioFormat) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("stt: empty audio")
	}
	return data, nil
}
