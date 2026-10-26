package runtime

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentregistry"
	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/config"
)

// TestRunDaemon_RegistersHealthForEveryChannel pins the multi-channel
// health contract: every started channel is handed to RegisterHealth.
// The daemoncontrol server serves one health entry per registration,
// so a channel that was never registered is invisible to
// `nightme doctor`.
func TestRunDaemon_RegistersHealthForEveryChannel(t *testing.T) {
	clearAllMgrs(t)

	cfg := &config.Config{
		Primary: "claude",
		Paths:   config.PathsConfig{DataDir: filepath.Join(t.TempDir(), "data")},
	}

	ready := make(chan struct{})
	sigCh := make(chan os.Signal, 1)
	defaults := DefaultDeps()
	deps := Deps{
		LoadConfig:        func() (*config.Config, error) { return cfg, nil },
		OpenChatSessions:  defaults.OpenChatSessions,
		OpenAgentSessions: defaults.OpenAgentSessions,
		BuildAgents:       func(cfg *config.Config) *agent.Registry { return agentregistry.Build(cfg, "") },
		NewChannels: func(*config.Config) ([]channel.Channel, error) {
			return []channel.Channel{
				newStartFuncChannel("feishu", nil),
				newStartFuncChannel("telegram", nil),
			}, nil
		},
		OnReady: func() { close(ready) },
	}

	var mu sync.Mutex
	var registered []string
	deps.RegisterHealth = func(ch channel.Channel) {
		mu.Lock()
		registered = append(registered, ch.Name())
		mu.Unlock()
	}

	runner := RunWith(deps, RunOptions{Out: io.Discard, Logger: slog.Default(), SigCh: sigCh})
	done := make(chan error, 1)
	go func() { done <- runner.Run(t.Context()) }()

	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("daemon exited before becoming ready: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not become ready within 2s")
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		got := append([]string(nil), registered...)
		mu.Unlock()
		if len(got) == 2 {
			if got[0] != "feishu" || got[1] != "telegram" {
				t.Errorf("registered = %v, want [feishu telegram]", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("registered = %v, want both channels registered", got)
		}
		time.Sleep(10 * time.Millisecond)
	}

	sigCh <- os.Interrupt
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("daemon returned %v, want nil after clean shutdown", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("daemon did not shut down within 30s of the signal")
	}
}
