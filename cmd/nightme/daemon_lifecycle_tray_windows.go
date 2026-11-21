//go:build windows

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/proc"
)

func onStopRequestDefault() {
	os.Exit(0)
}

func onRestartRequestDefault(cfg *config.Config, logger *slog.Logger) {
	exe, err := os.Executable()
	if err != nil {
		logClickErr(logger, "restart", fmt.Errorf("resolve nightme executable: %w", err))
		return
	}

	// The restart command owns the stop/start handoff and must outlive this daemon.
	cmd := proc.New(context.Background(), exe, "restart")
	stderr, _, closeStderr := openDaemonStderrOrDevNull(cfg, nil, os.Stderr)
	if closeStderr != nil {
		defer closeStderr()
	}
	if stderr != nil {
		cmd.Stderr = stderr
	}
	if err := cmd.Start(); err != nil {
		logClickErr(logger, "restart", fmt.Errorf("start restart process: %w", err))
		return
	}
	_ = cmd.Process.Release()
}
