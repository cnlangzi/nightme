// nightme-stt is the local voice-to-text worker. NightMe core spawns
// it as a child process and talks to it over a Unix-socket IPC
// endpoint defined by internal/stt. The worker handles the heavy
// native dependencies (sherpa-onnx, ONNX Runtime, SenseVoice model)
// so NightMe core stays pure-Go for users who never send a Telegram
// Voice message.
//
// See docs/channel/telegram.md §21 for the full architectural
// rationale and issue #381 for the protocol contract.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/cnlangzi/nightme/cmd/nightme-stt/internal/ffmpeg"
	"github.com/cnlangzi/nightme/cmd/nightme-stt/internal/sherpa"
	"github.com/cnlangzi/nightme/internal/stt"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "nightme-stt: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	endpoint := flag.String("endpoint", "", "stt IPC endpoint (default: <data-dir>/stt/stt.sock)")
	dataDir := flag.String("data-dir", "", "nightme data dir (required)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if *dataDir == "" {
		return errors.New("--data-dir is required")
	}

	ep := stt.Endpoint(*endpoint)
	if ep == "" {
		defaultEP, err := stt.DefaultEndpoint(*dataDir)
		if err != nil {
			return fmt.Errorf("resolve endpoint: %w", err)
		}
		ep = defaultEP
	}

	// Lazy recognizer load: this is where the SenseVoice model
	// weights get read into RAM. If the model is missing or the
	// CGO binding fails to load, surface a clean error so the
	// manager can render "run nightme stt install" to the user
	// instead of crashing with a CGo stack trace.
	rec, err := sherpa.New(*dataDir)
	if err != nil {
		return fmt.Errorf("load recognizer: %w", err)
	}
	defer rec.Close()

	dec := ffmpeg.New(ffmpeg.DefaultConfig())

	listener, err := stt.DefaultTransport().Listen(context.Background(), ep)
	if err != nil {
		return fmt.Errorf("listen %s: %w", ep, err)
	}
	defer listener.Close()

	logger.Info("nightme-stt: listening", "endpoint", string(listener.Endpoint()))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := stt.NewServer(listener, dec, rec, logger)

	// Watchdog: if Serve returns without ctx being cancelled
	// (e.g. the listener was closed from another goroutine),
	// surface the error rather than silently exiting 0 — the
	// manager's spawn cycle expects the worker to stay up
	// until SIGTERM.
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()

	select {
	case <-ctx.Done():
		logger.Info("nightme-stt: signal received, shutting down")
		return nil
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("serve: %w", err)
		}
		return nil
	}
}
