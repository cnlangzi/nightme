package runtime

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/channel/loopback"
	"github.com/cnlangzi/nightme/internal/config"
)

// TestStartMeService_NoLoopbackChannel — the service drives the
// daemon through the loopback channel. Without that channel there is
// no chat to drive it through, so the service stays off.
func TestStartMeService_NoLoopbackChannel(t *testing.T) {
	cfg := &config.Config{Paths: config.PathsConfig{DataDir: t.TempDir()}}
	svc := startMeService(context.Background(), cfg, nil, slog.Default())
	if svc != nil {
		t.Fatal("expected no service without a loopback channel")
	}
}

// TestStartMeService_StartsWithLoopbackChannel — the service starts
// once the loopback channel is up. A workflows dir that does not
// exist yet leaves it idle, not broken.
func TestStartMeService_StartsWithLoopbackChannel(t *testing.T) {
	cfg := &config.Config{Paths: config.PathsConfig{DataDir: t.TempDir()}}
	ch := loopback.New()
	svc := startMeService(context.Background(), cfg, []channel.Channel{ch}, slog.Default())
	if svc == nil {
		t.Fatal("expected a service")
	}
	if err := svc.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestStartMeService_OtherChannelOnly — a daemon whose only channel
// is not the loopback one runs without the service.
func TestStartMeService_OtherChannelOnly(t *testing.T) {
	cfg := &config.Config{Paths: config.PathsConfig{DataDir: t.TempDir()}}
	svc := startMeService(context.Background(), cfg, []channel.Channel{newStubChannel("stub", nil)}, slog.Default())
	if svc != nil {
		t.Fatal("expected no service without a loopback channel")
	}
}

func TestWorkflowsDir(t *testing.T) {
	dir, err := workflowsDir(&config.Config{Paths: config.PathsConfig{DataDir: t.TempDir()}})
	if err != nil {
		t.Fatalf("workflowsDir: %v", err)
	}
	if filepath.Base(dir) != "workflows" {
		t.Errorf("dir = %q, want it to end in %q", dir, "workflows")
	}
	if _, err := workflowsDir(&config.Config{}); err == nil {
		t.Error("expected an error for an empty data dir")
	}
}

// TestCountIMChannels — the loopback channel is the daemon's own
// chat: it must not hide the fact that no IM channel is configured.
func TestCountIMChannels(t *testing.T) {
	if got := countIMChannels(nil); got != 0 {
		t.Errorf("count = %d, want 0", got)
	}
	if got := countIMChannels([]channel.Channel{loopback.New()}); got != 0 {
		t.Errorf("count = %d, want 0 — the loopback channel is not an IM channel", got)
	}
	if got := countIMChannels([]channel.Channel{loopback.New(), newStubChannel("stub", nil)}); got != 1 {
		t.Errorf("count = %d, want 1", got)
	}
}
