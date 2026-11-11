// me.go — the workflow service the daemon runs alongside its
// channels.
//
// me is the user half of nightme: it drives the daemon through the
// loopback channel, the way a person drives it through Feishu. It
// owns the workflow catalogue, the cron triggers and the per-run
// state; the loopback channel owns the transport.
//
// The service starts once the loopback channel is attached — its
// commands need a chat to land in — and stops before the channels
// do: the runs that are still going are cancelled and given a
// moment to write their final state while their chat is still up.

package runtime

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/channel/loopback"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/me"
)

// startMeService builds and starts the workflow service.
//
// A daemon without a loopback channel — a test build, or a channel
// that failed to start — runs without the service: there is no chat
// to drive the daemon through.
func startMeService(ctx context.Context, cfg *config.Config, chs []channel.Channel, logger *slog.Logger) *me.Service {
	lb := findLoopback(chs)
	if lb == nil {
		logger.Info("run: no loopback channel; the workflow service stays off")
		return nil
	}
	dir, err := workflowsDir(cfg)
	if err != nil {
		logger.Warn("run: workflow service has no workflows dir", "err", err)
		return nil
	}
	svc := me.New(me.Config{WorkflowsDir: dir}, loopback.NewClient(lb))
	svc.SetLogger(logger)
	if err := svc.Start(ctx); err != nil {
		logger.Warn("run: workflow service did not start", "err", err)
		return nil
	}
	logger.Info("run: workflow service started", "dir", dir)
	return svc
}

// stopMeService stops the workflow service. It is a no-op for a
// daemon that never started one.
//
// The stop is bounded: a run that is still going is cancelled and
// the daemon does not wait for it forever.
func stopMeService(svc *me.Service, logger *slog.Logger) {
	if svc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := svc.Stop(ctx); err != nil {
		logger.Warn("run: workflow service stop failed", "err", err)
	}
}

// findLoopback returns the loopback channel among the started
// channels, or nil when there is none.
func findLoopback(chs []channel.Channel) *loopback.Channel {
	for _, ch := range chs {
		if lb, ok := ch.(*loopback.Channel); ok {
			return lb
		}
	}
	return nil
}

// workflowsDir returns <DataDir>/workflows — the directory the
// workflow YAML files live in.
func workflowsDir(cfg *config.Config) (string, error) {
	if cfg == nil || cfg.Paths.DataDir == "" {
		return "", errors.New("cfg.Paths.DataDir is empty")
	}
	base, err := filepath.Abs(cfg.Paths.DataDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "workflows"), nil
}

// countIMChannels counts the channels a person can talk to. The
// loopback channel is the daemon's own chat: it is always there, so
// it must not hide the fact that no IM channel is configured.
func countIMChannels(chs []channel.Channel) int {
	n := 0
	for _, ch := range chs {
		if ch == nil {
			continue
		}
		if _, ok := ch.(*loopback.Channel); ok {
			continue
		}
		n++
	}
	return n
}
