package main

import (
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/channel/echo"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/runtime"
)

// TestRunDaemon_ReachesReady is the regression test for a daemon
// that could not start at all.
//
// History: `gwImpl.AttachChannels(ch)` sat ~20 lines ABOVE the
// `gwImpl = gateway.New(ir, em)` that constructs the router (a
// leftover of the F-58 dispatch-chain refactor), so every daemon
// start dereferenced a nil *gateway.Router and died with SIGSEGV
// before signalling readiness. It was invisible for two reasons:
// the daemon child's stderr went to /dev/null, and no test ever
// drove runDaemon far enough to touch the wiring — every existing
// run_test.go case exercises a single handler in isolation.
//
// This test drives the real startup path end to end through the
// runtime.Deps seams (echo channel, temp stores, no Feishu login)
// and asserts it reaches OnReady and shuts down cleanly. Any future
// nil-receiver / ordering break in the construction block fails
// here instead of in production.
func TestRunDaemon_ReachesReady(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Primary: "claude",
		Paths:   config.PathsConfig{DataDir: dir},
	}

	ch := echo.New("echo", io.Discard)
	sigCh := make(chan os.Signal, 1)
	ready := make(chan struct{})

	defaults := runtime.DefaultDeps()
	deps := runtime.Deps{
		LoadConfig:        func() (*config.Config, error) { return cfg, nil },
		OpenChatSessions:  defaults.OpenChatSessions,
		OpenAgentSessions: defaults.OpenAgentSessions,
		BuildAgents:       buildRunAgentRegistry,
		NewChannel: func(*config.Config) (channel.Channel, error) {
			return ch, nil
		},
		SkipFeishuLogin: true,
		OnReady:         func() { close(ready) },
	}

	runner := runtime.RunWith(deps, runtime.RunOptions{
		Out:    io.Discard,
		Logger: slog.Default(),
		SigCh:  sigCh,
	})
	done := make(chan error, 1)
	go func() { done <- runner.Run(t.Context()) }()

	select {
	case <-ready:
		// Startup completed: the gateway was constructed, wired,
		// and started. Now prove the channel is actually ATTACHED,
		// not merely constructed: a `gwImpl.AttachChannels(ch)` that
		// went missing would leave startup green while silently
		// black-holing every inbound message. `/help` is the
		// cheapest round trip — it needs no cwd, no agent, no
		// network, and always answers.
		// HasMention: the default WatchMode is WatchModeMention, so a
		// bare group message is dropped by policy before it can
		// produce any output (chatsession.Manager.HandleInbound).
		ch.Inject(messages.InboundMessage{
			ChatID:     "oc_startup_probe",
			UserID:     "u_probe",
			MessageID:  "om_probe",
			Text:       "/help",
			HasMention: true,
		})
		deadline := time.After(15 * time.Second)
		for len(ch.Record()) == 0 {
			select {
			case <-deadline:
				t.Fatal("gateway produced no outbound message for /help — is the channel attached?")
			case err := <-done:
				t.Fatalf("daemon exited while handling the probe: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
		}
		sigCh <- os.Interrupt
	case err := <-done:
		// runDaemon returned before OnReady — startup failed.
		t.Fatalf("daemon exited before becoming ready: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("daemon did not become ready within 30s")
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("daemon returned %v, want nil after clean shutdown", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("daemon did not shut down within 30s of the signal")
	}
}