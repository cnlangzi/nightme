//go:build cgo_sherpa

// Package sherpa wraps github.com/k2-fsa/sherpa-onnx-go's
// SenseVoice offline recognizer into the internal/stt
// Recognizer interface used by the worker shell.
//
// Build tag: this file is ONLY compiled when
// `-tags cgo_sherpa` is set. The default build skips it
// entirely so the main nightme binary and the rest of
// the repo can compile without sherpa-onnx's CGO + native
// library dependencies.
//
// Required at build time when this tag is enabled:
//   - github.com/k2-fsa/sherpa-onnx-go (facade)
//   - the platform-specific subpackage
//     (sherpa-onnx-go-linux / sherpa-onnx-go-windows / …)
//     pulled automatically by go.mod
//   - CGO_ENABLED=1 (sherpa-onnx links ONNX Runtime via cgo)
//   - the SenseVoice Small INT8 model on disk at the
//     path passed to New
//
// The recognizer is loaded eagerly in NewSenseVoice —
// SenseVoice is too heavy to instantiate per request
// (issue #381 §3 "Lazy model initialization" still
// applies at the process level: NewSenseVoice is called
// once at worker startup, not per voice message).
package sherpa

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	sherpaonnx "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"github.com/cnlangzi/nightme/internal/stt"
)

// Recognizer wraps a sherpa.OfflineRecognizer configured
// for SenseVoice Small INT8. Implements internal/stt
// Recognizer for the worker shell.
type Recognizer struct {
	rec *sherpaonnx.OfflineRecognizer
}

// NewSenseVoice loads the SenseVoice model from
// `modelDir` and returns a ready-to-use Recognizer.
// modelDir must contain `model.int8.onnx` and
// `tokens.txt` — the canonical layout produced by
// `nightme stt install` (issue #381 §6).
//
// Returns an error when:
//   - modelDir is missing or unreadable
//   - the .onnx or tokens.txt file is absent
//   - sherpa.OfflineRecognizer construction fails
//     (corrupt model, ONNX Runtime missing, etc.)
func NewSenseVoice(modelDir string) (stt.Recognizer, error) {
	if modelDir == "" {
		return nil, fmt.Errorf("sherpa: empty model dir")
	}
	modelPath := filepath.Join(modelDir, "model.int8.onnx")
	tokensPath := filepath.Join(modelDir, "tokens.txt")
	if _, err := os.Stat(modelPath); err != nil {
		return nil, fmt.Errorf("sherpa: model not found at %s (run `nightme stt install`)", modelPath)
	}
	if _, err := os.Stat(tokensPath); err != nil {
		return nil, fmt.Errorf("sherpa: tokens not found at %s (run `nightme stt install`)", tokensPath)
	}

	config := &sherpaonnx.OfflineRecognizerConfig{}
	config.ModelConfig.SenseVoice.Model = modelPath
	config.ModelConfig.SenseVoice.Language = "auto"
	config.ModelConfig.SenseVoice.UseInverseTextNormalization = 1
	config.ModelConfig.Tokens = tokensPath

	rec := sherpaonnx.NewOfflineRecognizer(config)
	if rec == nil {
		return nil, fmt.Errorf("sherpa: NewOfflineRecognizer returned nil (model corrupt?)")
	}
	return &Recognizer{rec: rec}, nil
}

// Recognize implements stt.Recognizer. The caller passes
// already-decoded PCM bytes (16 kHz mono int16 LE) from
// ffmpeg. We treat them as a single utterance and
// return the recognizer's best transcription.
//
// SenseVoice is intentionally language-agnostic in this
// binding (`Language: "auto"`). The model's own language
// prediction is surfaced as the result's Language field
// for logs / diagnostics only — callers do not switch
// behaviour based on it.
func (r *Recognizer) Recognize(ctx context.Context, req stt.RecognizeRequest) (stt.RecognizeResult, error) {
	if r == nil || r.rec == nil {
		return stt.RecognizeResult{}, stt.NewError(stt.ErrCodeNotReady, "recognizer not initialised")
	}
	if len(req.Samples) == 0 {
		return stt.RecognizeResult{}, stt.NewError(stt.ErrCodeTranscriptionFailed, "no samples")
	}
	if len(req.Samples)%2 != 0 {
		return stt.RecognizeResult{}, stt.NewError(stt.ErrCodeTranscriptionFailed, "odd PCM byte count")
	}

	// int16 LE → float32 in [-1, 1]. SenseVoice consumes
	// float32 samples at 16 kHz.
	samples := make([]float32, len(req.Samples)/2)
	for i := range samples {
		lo := uint16(req.Samples[2*i])
		hi := uint16(req.Samples[2*i+1])
		v := int16(lo | hi<<8) // little-endian
		samples[i] = float32(v) / 32768.0
	}

	stream := sherpaonnx.NewOfflineStream(r.rec)
	if stream == nil {
		return stt.RecognizeResult{}, fmt.Errorf("sherpa: NewOfflineStream returned nil")
	}
	// sherpa-onnx-go has no finalizer on OfflineStream
	// (verified — module has zero SetFinalizer hits). The
	// binding's doc on NewOfflineStream says the user must
	// invoke DeleteOfflineStream. Defer it here so every
	// Recognize call (success, error, ctx-cancel) frees
	// the native handle. The result.Text / result.Lang
	// strings are copied into decodeResult before the
	// deferred delete runs.
	defer sherpaonnx.DeleteOfflineStream(stream)
	stream.AcceptWaveform(16000, samples)

	// Decode is synchronous in sherpa-onnx — there is no
	// native cancellation. We run it in a goroutine so the
	// caller's ctx can still abort the wait; the underlying
	// recognition continues until Decode returns but its
	// result is discarded.
	type decodeResult struct {
		text string
		lang string
		err  error
	}
	done := make(chan decodeResult, 1)
	go func() {
		r.rec.Decode(stream)
		result := stream.GetResult()
		if result == nil {
			done <- decodeResult{err: fmt.Errorf("sherpa: nil recognition result")}
			return
		}
		done <- decodeResult{text: result.Text, lang: result.Lang}
	}()
	select {
	case <-ctx.Done():
		return stt.RecognizeResult{}, ctx.Err()
	case res := <-done:
		if res.err != nil {
			return stt.RecognizeResult{}, res.err
		}
		return stt.RecognizeResult{Text: res.text, Language: res.lang}, nil
	}
}

// Close releases the recognizer's native resources.
// Called once at worker shutdown. Idempotent.
func (r *Recognizer) Close() error {
	if r == nil || r.rec == nil {
		return nil
	}
	sherpaonnx.DeleteOfflineRecognizer(r.rec)
	r.rec = nil
	return nil
}
