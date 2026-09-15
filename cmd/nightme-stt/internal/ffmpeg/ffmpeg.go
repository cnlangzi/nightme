// Package ffmpeg provides the audio-decoding half of nightme-stt.
// It shells out to an ffmpeg binary to turn the raw bytes the
// channel adapter downloaded (Telegram delivers voice notes as
// opus-in-ogg; some channels send wav) into 16-bit signed mono PCM
// at 16 kHz, which is what sherpa-onnx-go's SenseVoice pipeline
// expects.
package ffmpeg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"github.com/cnlangzi/nightme/internal/stt"
)

// Config holds the knobs the worker respects at startup. The
// production default is "ffmpeg" on PATH; tests inject a fake
// binary path through Binary so they can assert on the exact
// arguments without polluting $PATH.
type Config struct {
	// Binary is the absolute path (or PATH-relative name) of
	// the ffmpeg executable. Default: "ffmpeg".
	Binary string
	// SampleRate is the PCM sample rate the worker expects the
	// recognizer to consume. SenseVoice is fixed at 16 kHz;
	// 16000 is the only sensible value.
	SampleRate int
	// Channels is the channel count: 1 (mono). Any other value
	// would defeat SenseVoice's normalization.
	Channels int
	// Timeout caps a single Decode call so a runaway ffmpeg
	// cannot wedge the worker forever. Zero = no extra timeout
	// beyond ctx.
	Timeout time.Duration
}

// DefaultConfig returns the production ffmpeg config. The
// recognizer-facing audio format is fixed: 16 kHz mono 16-bit
// signed little-endian PCM.
func DefaultConfig() Config {
	return Config{
		Binary:     "ffmpeg",
		SampleRate: 16000,
		Channels:   1,
		Timeout:    30 * time.Second,
	}
}

// New returns a stt.Decoder backed by the configured ffmpeg
// binary. The decoder is stateless — concurrent Decode calls
// from the worker each spawn their own subprocess — but V1
// only processes one transcription at a time so this isn't
// exercised.
func New(cfg Config) stt.Decoder {
	if cfg.Binary == "" {
		cfg.Binary = "ffmpeg"
	}
	if cfg.SampleRate == 0 {
		cfg.SampleRate = 16000
	}
	if cfg.Channels == 0 {
		cfg.Channels = 1
	}
	return &decoder{cfg: cfg}
}

type decoder struct {
	cfg Config
}

// captureWriter is an io.Writer that appends every byte it
// receives to an in-memory buffer. We use it to bound and
// collect ffmpeg's stdout: io.Pipe would also work but needs
// a second goroutine to drain; captureWriter stays synchronous
// inside Decode so the caller's ctx is the only deadline in
// play.
type captureWriter struct {
	buf bytes.Buffer
}

func (c *captureWriter) Write(p []byte) (int, error) {
	return c.buf.Write(p)
}

func (c *captureWriter) Bytes() []byte { return c.buf.Bytes() }

func (d *decoder) Decode(ctx context.Context, data []byte, format stt.AudioFormat) ([]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("ffmpeg: empty audio")
	}
	// Decode context: when the caller did not set a deadline
	// (the production worker always does via ctxDeadline in
	// protocol.go), fall back to d.cfg.Timeout so a runaway
	// ffmpeg cannot wedge the worker.
	if _, ok := ctx.Deadline(); !ok && d.cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d.cfg.Timeout)
		defer cancel()
	}
	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-i", "pipe:0",
		"-f", "s16le",
		"-ar", fmt.Sprintf("%d", d.cfg.SampleRate),
		"-ac", fmt.Sprintf("%d", d.cfg.Channels),
		"pipe:1",
	}
	cmd := exec.CommandContext(ctx, d.cfg.Binary, args...)
	cmd.Stdin = bytes.NewReader(data)
	out := &captureWriter{}
	cmd.Stdout = out
	// ffmpeg writes diagnostics to stderr — surface them on
	// failure for actionable logs.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg: %w (stderr: %s)", err, stderr.String())
	}
	return out.Bytes(), nil
}
