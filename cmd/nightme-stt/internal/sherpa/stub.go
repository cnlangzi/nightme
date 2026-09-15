//go:build !cgo_sherpa

// Package sherpa is the speech-recognition backend for nightme-stt.
// This file is the non-CGO stub: it is compiled when the
// `cgo_sherpa` build tag is NOT set. The production worker is
// built with `-tags cgo_sherpa CGO_ENABLED=1` so the real
// SenseVoice binding in sherpa_onnx.go takes over; this stub is
// what every other build (the default `make build`, the -tags
// notray cross-compiles, etc.) sees.
package sherpa

import (
	"context"

	"github.com/cnlangzi/nightme/internal/stt"
)

// New returns a recognizer that emits a deterministic placeholder
// for every transcription. The stub's job is to keep the wire
// protocol working end-to-end on hosts without CGO so smoke tests
// and developer builds don't need a 200 MB model lying around.
//
// Production users will see "voice transcription requires
// installing nightme-stt with `-tags cgo_sherpa`" in their chat
// — the Telegram adapter surfaces that message verbatim (see
// docs/channel/telegram.md §21).
func New(dataDir string) (stt.Recognizer, error) {
	return &stubRecognizer{}, nil
}

type stubRecognizer struct{}

func (s *stubRecognizer) Recognize(ctx context.Context, req stt.RecognizeRequest) (stt.RecognizeResult, error) {
	if len(req.Samples) == 0 {
		return stt.RecognizeResult{}, stt.NewError(stt.CodeTranscriptionFailed, "no samples")
	}
	return stt.RecognizeResult{
		Text:     "[voice transcription requires installing nightme-stt with `-tags cgo_sherpa`]",
		Language: "en",
	}, nil
}

func (s *stubRecognizer) Close() error { return nil }
