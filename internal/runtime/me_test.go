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

// TestBuildPersonalAgent_NoLoopbackChannel — the personal agent
// drives the daemon through the loopback channel. Without that
// channel there is no chat to drive it through, so the agent stays
// off.
func TestBuildPersonalAgent_NoLoopbackChannel(t *testing.T) {
	cfg := &config.Config{Paths: config.PathsConfig{DataDir: t.TempDir()}}
	svc := buildPersonalAgent(cfg, nil, slog.Default())
	if svc != nil {
		t.Fatal("expected no agent without a loopback channel")
	}
}

// TestBuildPersonalAgent_OtherChannelOnly — a daemon whose only
// channel is not the loopback one runs without the agent.
func TestBuildPersonalAgent_OtherChannelOnly(t *testing.T) {
	cfg := &config.Config{Paths: config.PathsConfig{DataDir: t.TempDir()}}
	svc := buildPersonalAgent(cfg, []channel.Channel{newStubChannel("stub", nil)}, slog.Default())
	if svc != nil {
		t.Fatal("expected no agent without a loopback channel")
	}
}

// TestBuildPersonalAgent_StartsWithLoopbackChannel — the agent
// starts once the loopback channel is up.
func TestBuildPersonalAgent_StartsWithLoopbackChannel(t *testing.T) {
	cfg := &config.Config{Paths: config.PathsConfig{DataDir: t.TempDir()}}
	ch := loopback.New()
	svc := buildPersonalAgent(cfg, []channel.Channel{ch}, slog.Default())
	if svc == nil {
		t.Fatal("expected an agent")
	}
	if err := svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
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
