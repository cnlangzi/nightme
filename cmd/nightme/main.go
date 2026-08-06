// Package main is the nightme daemon entrypoint.
package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/logging"
)

func main() {
	cfg, err := config.LoadDefault()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	var logger *slog.Logger
	if l, err := logging.New(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	} else {
		logger = l
	}
	// F-46 debug: install logger as slog.Default so all
	// downstream code (handlers_gtw.go, gateway.go, action.go,
	// chatsession.go) that calls slog.Default().Warn(...) lands
	// in the same MultiWriter sink as the plumbed logger. Without
	// this, our F-46 debug logs are silently dropped because
	// slog.Default() returns Go's no-op default logger.
	slog.SetDefault(logger)
	defer func() { _ = logging.Close(logger) }()
	Execute(logger)
}
