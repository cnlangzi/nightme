//go:build cgo_sherpa

// Package sherpa — SenseVoice (CGO) recognizer backend. This
// file is compiled only when the `cgo_sherpa` build tag is set
// AND CGO is enabled. The matching stub.go is selected when
// the tag is absent so the worker binary still links cleanly on
// hosts without CGO (default `make build`, cross-compile CI
// legs, etc.).
//
// The production worker is built with
//
//	CGO_ENABLED=1 go build -tags cgo_sherpa -ldflags '-Wl,-rpath,$ORIGIN/lib' ./cmd/nightme-stt
//
// so the sherpa-onnx C library is loaded from a sibling `lib/`
// directory at runtime. See docs/channel/telegram.md §21.
package sherpa

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cnlangzi/nightme/internal/stt"
	sherpaonnx "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
)

// New loads the SenseVoice model at <dataDir>/stt/model/ and
// returns a recognizer that uses it. The model directory layout
// matches what sherpa-onnx's prebuilt archives ship:
//
//	<dataDir>/stt/model/model.onnx   SenseVoice weights
//	<dataDir>/stt/model/tokens.txt   BPE vocabulary
//
// The two filenames are fixed by sherpa-onnx-go and cannot be
// remapped via the binding's API.
func New(dataDir string) (stt.Recognizer, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("sherpa: empty data dir")
	}
	modelDir := filepath.Join(dataDir, "stt", "model")
	modelPath := filepath.Join(modelDir, "model.onnx")
	tokensPath := filepath.Join(modelDir, "tokens.txt")

	if _, err := os.Stat(modelPath); err != nil {
		return nil, fmt.Errorf("sherpa: model.onnx not found at %s "+
			"(run `nightme stt install` first): %w", modelPath, err)
	}
	if _, err := os.Stat(tokensPath); err != nil {
		return nil, fmt.Errorf("sherpa: tokens.txt not found at %s: %w", tokensPath, err)
	}

	cfg := sherpaonnx.OfflineRecognizerConfig{}
	cfg.ModelConfig.SenseVoice.Model = modelPath
	cfg.ModelConfig.SenseVoice.Language = "auto"
	cfg.ModelConfig.SenseVoice.UseInverseTextNormalization = 1
	cfg.ModelConfig.Tokens = tokensPath

	rec := sherpaonnx.NewOfflineRecognizer(&cfg)
	if rec == nil {
		return nil, fmt.Errorf("sherpa: NewOfflineRecognizer returned nil " +
			"(check model + tokens integrity; see worker stderr)")
	}
	return &sherpaRecognizer{rec: rec}, nil
}

type sherpaRecognizer struct {
	rec *sherpaonnx.OfflineRecognizer
}

func (s *sherpaRecognizer) Recognize(ctx context.Context, req stt.RecognizeRequest) (stt.RecognizeResult, error) {
	if len(req.Samples) == 0 {
		return stt.RecognizeResult{}, stt.NewError(stt.CodeTranscriptionFailed, "no samples")
	}
	samples := pcmBytesToFloat32(req.Samples)
	stream := sherpaonnx.NewOfflineStream(s.rec)
	stream.AcceptWaveform(16000, samples)
	s.rec.Decode(stream)
	res := stream.GetResult()
	if res == nil {
		return stt.RecognizeResult{}, stt.NewError(stt.CodeTranscriptionFailed, "sherpa returned nil result")
	}
	return stt.RecognizeResult{
		Text:     res.Text,
		Language: res.Lang,
	}, nil
}

func (s *sherpaRecognizer) Close() error {
	if s.rec != nil {
		sherpaonnx.DeleteOfflineRecognizer(s.rec)
		s.rec = nil
	}
	return nil
}

// pcmBytesToFloat32 reinterprets a 16-bit signed little-endian
// PCM buffer as float32 samples in [-1, 1]. SenseVoice's
// preprocessing expects float32 input; the worker feeds it the
// raw int16 bytes that ffmpeg produces, so the conversion is
// always needed at this seam.
//
// The byte count is asserted to be a multiple of 2 — ffmpeg
// guarantees this when the output is s16le, but we panic rather
// than silently produce a malformed sample slice so a future
// format change cannot pass through unnoticed.
func pcmBytesToFloat32(b []byte) []float32 {
	if len(b)%2 != 0 {
		panic(fmt.Sprintf("sherpa: pcmBytesToFloat32: odd byte count %d", len(b)))
	}
	out := make([]float32, len(b)/2)
	for i := range out {
		v := int16(uint16(b[2*i]) | uint16(b[2*i+1])<<8)
		out[i] = float32(v) / 32768.0
	}
	return out
}
