// channel_start_test.go — per-channel startup budget and isolation.
//
// These cover the failure modes that used to take the whole daemon
// down: an adapter that never returns from Start, one that panics,
// and one that returns an error. Each must cost at most
// ChannelStartBudget and leave the daemon running.

package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentregistry"
	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/messages"
)

// startFuncChannel is a Channel whose Start behaviour the test
// supplies. Everything else is inert.
type startFuncChannel struct {
	name  string
	start func(context.Context) error
	stop  func(context.Context) error

	stopped chan struct{}
	once    sync.Once
}

func newStartFuncChannel(name string, start func(context.Context) error) *startFuncChannel {
	return &startFuncChannel{
		name:    name,
		start:   start,
		stopped: make(chan struct{}),
	}
}

func (c *startFuncChannel) Name() string { return c.name }
func (c *startFuncChannel) Start(ctx context.Context) error {
	if c.start == nil {
		return nil
	}
	return c.start(ctx)
}

func (c *startFuncChannel) Stop(ctx context.Context) error {
	if c.stop != nil {
		if err := c.stop(ctx); err != nil {
			return err
		}
	}
	c.once.Do(func() { close(c.stopped) })
	return nil
}
func (c *startFuncChannel) waitStopped(d time.Duration) bool {
	select {
	case <-c.stopped:
		return true
	case <-time.After(d):
		return false
	}
}

func (c *startFuncChannel) Incoming() <-chan messages.InboundMessage {
	in := make(chan messages.InboundMessage)
	close(in)
	return in
}
func (c *startFuncChannel) Send(context.Context, messages.OutboundMessage) error { return nil }
func (c *startFuncChannel) OnPromptEnded(context.Context, string, string, agent.PromptEndReason) {
}
func (c *startFuncChannel) HealthSnapshot() (string, json.RawMessage, error) {
	return c.name, json.RawMessage("{}"), nil
}
func (c *startFuncChannel) SetLogger(*slog.Logger) {}
func (c *startFuncChannel) BuildBlocks(string, []messages.Attachment) []agent.ContentBlock {
	return nil
}

var _ channel.Channel = (*startFuncChannel)(nil)

// TestStartChannel_SuccessKeepsStartContextLive pins the trap that
// makes a deadline-carrying context the wrong tool here: adapters
// derive their long-lived receive loop from the context Start is
// given (telegram derives its poll loop from it), so cancelling it
// once Start returns would kill a channel that just connected.
func TestStartChannel_SuccessKeepsStartContextLive(t *testing.T) {
	var startCtx context.Context
	ch := newStartFuncChannel("ok", func(ctx context.Context) error {
		startCtx = ctx
		return nil
	})

	if got := startChannel(t.Context(), ch); got.Status != ChannelStatusConnected {
		t.Fatalf("status = %q, want %q (err=%s)", got.Status, ChannelStatusConnected, got.Error)
	}

	// The budget would have fired by now if the context carried it.
	time.Sleep(50 * time.Millisecond)
	if startCtx == nil {
		t.Fatal("Start was never called")
	}
	select {
	case <-startCtx.Done():
		t.Fatal("start context was cancelled after Start returned; the adapter's receive loop would die with it")
	default:
	}
}

// TestStartChannel_StartErrorIsSkipped covers a channel that refuses
// to start (bad credentials, unreachable API): reported as skipped
// with the adapter's own reason, not as a daemon failure.
func TestStartChannel_StartErrorIsSkipped(t *testing.T) {
	ch := newStartFuncChannel("telegram", func(context.Context) error {
		return errors.New("401 Unauthorized")
	})

	got := startChannel(t.Context(), ch)
	if got.Status != ChannelStatusSkipped {
		t.Errorf("status = %q, want %q", got.Status, ChannelStatusSkipped)
	}
	if !strings.Contains(got.Error, "401 Unauthorized") {
		t.Errorf("error = %q, want it to carry the adapter's reason", got.Error)
	}
	if got.Name != "telegram" {
		t.Errorf("name = %q, want telegram", got.Name)
	}
}

// TestStartChannel_PanicIsIsolated covers an adapter defect. The
// panic becomes an ordinary skip so one bad channel cannot take the
// daemon with it.
func TestStartChannel_PanicIsIsolated(t *testing.T) {
	ch := newStartFuncChannel("panicky", func(context.Context) error {
		panic("boom")
	})

	got := startChannel(t.Context(), ch)
	if got.Status != ChannelStatusSkipped {
		t.Errorf("status = %q, want %q", got.Status, ChannelStatusSkipped)
	}
	if !strings.Contains(got.Error, "boom") {
		t.Errorf("error = %q, want it to name the panic", got.Error)
	}
}

// TestStartChannels_RunsConcurrentlyAndKeepsInputOrder is what stops
// the parallel start from quietly regressing back to one-at-a-time.
// Each channel takes the same 250ms; run serially the phase would
// cost N×250ms, which at N=6 is 1.5s against a budgeted parallel run
// of ~250ms.
//
// The order assertion matters as much as the timing: results must
// come back indexed the way chs was given, not in completion order,
// or `nightme start` would print a different channel list every run.
func TestStartChannels_RunsConcurrentlyAndKeepsInputOrder(t *testing.T) {
	const (
		base     = 250 * time.Millisecond
		channels = 6
	)

	// Descending delays so completion order is the reverse of input
	// order — a completion-ordered implementation would fail the
	// assertions below rather than accidentally passing.
	chs := make([]channel.Channel, 0, channels)
	want := make([]string, 0, channels)
	var slowest, serialFor time.Duration
	for i := range channels {
		name := fmt.Sprintf("ch-%d", i)
		delay := base + time.Duration(channels-i)*20*time.Millisecond
		if delay > slowest {
			slowest = delay
		}
		serialFor += delay
		chs = append(chs, newStartFuncChannel(name, func(context.Context) error {
			time.Sleep(delay)
			return nil
		}))
		want = append(want, name)
	}

	begin := time.Now()
	got := startChannels(t.Context(), chs)
	elapsed := time.Since(begin)

	// Concurrently the phase costs the slowest channel plus slack;
	// serially it costs their sum.
	if budget := slowest + 150*time.Millisecond; elapsed > budget {
		t.Errorf("starting %d channels took %s; want at most %s (the slowest one), not %s — they are not running concurrently",
			channels, elapsed, budget, serialFor)
	}

	if len(got) != channels {
		t.Fatalf("got %d statuses, want %d", len(got), channels)
	}
	for i, name := range want {
		if got[i].Name != name {
			t.Errorf("statuses[%d].Name = %q, want %q (results must follow input order, not completion order)",
				i, got[i].Name, name)
		}
		if got[i].Status != ChannelStatusConnected {
			t.Errorf("statuses[%d].Status = %q, want %q", i, got[i].Status, ChannelStatusConnected)
		}
	}
}

// TestStartChannel_BlockingStartTimesOutAndIsStopped covers the host
// whose api.telegram.org is black-holed: Start never returns. The
// daemon gives up at the budget, and an adapter that finishes
// starting afterwards is stopped so it does not linger with no pump
// draining it.
func TestStartChannel_BlockingStartTimesOutAndIsStopped(t *testing.T) {
	release := make(chan struct{})
	ch := newStartFuncChannel("blackhole", func(context.Context) error {
		<-release
		return nil
	})

	begin := time.Now()
	got := startChannel(t.Context(), ch)
	elapsed := time.Since(begin)

	if got.Status != ChannelStatusTimeout {
		t.Fatalf("status = %q, want %q (err=%s)", got.Status, ChannelStatusTimeout, got.Error)
	}
	if !strings.Contains(got.Error, ChannelStartBudget.String()) {
		t.Errorf("error = %q, want it to name the %s budget", got.Error, ChannelStartBudget)
	}
	if elapsed > ChannelStartBudget+2*time.Second {
		t.Errorf("gave up after %s, want roughly %s", elapsed, ChannelStartBudget)
	}

	close(release)
	if !ch.waitStopped(5 * time.Second) {
		t.Error("adapter that finished starting after the budget was never stopped")
	}
}

// TestRunDaemon_ReadinessIgnoresChannelStart is the regression test
// for the reported bug: one channel that blocks on the network must
// not delay readiness, because readiness is what decides whether the
// daemon survives the parent's 15s gate at all.
func TestRunDaemon_ReadinessIgnoresChannelStart(t *testing.T) {
	clearAllMgrs(t)

	cfg := &config.Config{
		Primary: "claude",
		Paths:   config.PathsConfig{DataDir: t.TempDir()},
	}

	release := make(chan struct{})
	defer close(release)
	slow := newStartFuncChannel("slow", func(context.Context) error {
		<-release
		return nil
	})
	fast := newStartFuncChannel("fast", nil)

	ready := make(chan struct{})
	sigCh := make(chan os.Signal, 1)

	defaults := DefaultDeps()
	deps := Deps{
		LoadConfig:        func() (*config.Config, error) { return cfg, nil },
		OpenChatSessions:  defaults.OpenChatSessions,
		OpenAgentSessions: defaults.OpenAgentSessions,
		BuildAgents:       func(cfg *config.Config) *agent.Registry { return agentregistry.Build(cfg, "") },
		NewChannels: func(*config.Config) ([]channel.Channel, error) {
			return []channel.Channel{slow, fast}, nil
		},
		OnReady: func() { close(ready) },
	}

	startup := make(chan ChannelStartup, 1)
	deps.OnChannelsStarted = func(s ChannelStartup) { startup <- s }

	runner := RunWith(deps, RunOptions{Out: io.Discard, Logger: slog.Default(), SigCh: sigCh})
	done := make(chan error, 1)
	go func() { done <- runner.Run(t.Context()) }()

	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("daemon exited before becoming ready: %v", err)
	case <-time.After(2 * time.Second):
		// The slow channel alone would hold startup for
		// ChannelStartBudget, so readiness arriving sooner proves it
		// was not part of the readiness path.
		t.Fatal("daemon did not become ready within 2s while one channel was still starting")
	}

	sigCh <- os.Interrupt
	select {
	case <-startup:
	case <-time.After(ChannelStartBudget + 10*time.Second):
		t.Fatal("daemon never reported its channel startup outcomes")
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

// TestRunDaemon_StartsWithoutAnyChannel pins the zero-channel case:
// no IM channel configured is a state the daemon serves in, not a
// reason to refuse to start.
func TestRunDaemon_StartsWithoutAnyChannel(t *testing.T) {
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
		NewChannels:       func(*config.Config) ([]channel.Channel, error) { return nil, nil },
		OnReady:           func() { close(ready) },
	}

	runner := RunWith(deps, RunOptions{Out: io.Discard, Logger: slog.Default(), SigCh: sigCh})
	done := make(chan error, 1)
	go func() { done <- runner.Run(t.Context()) }()

	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("daemon refused to start without any channel: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("daemon did not become ready within 30s")
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

// TestRunDaemon_ReportsPerChannelOutcomes pins the report the daemon
// child hands back to `nightme start`: one entry per constructed
// channel, with the reason for each one that did not come up.
func TestRunDaemon_ReportsPerChannelOutcomes(t *testing.T) {
	clearAllMgrs(t)

	cfg := &config.Config{
		Primary: "claude",
		Paths:   config.PathsConfig{DataDir: t.TempDir()},
	}

	good := newStartFuncChannel("feishu", nil)
	broken := newStartFuncChannel("telegram", func(context.Context) error {
		return errors.New("401 Unauthorized")
	})
	release := make(chan struct{})
	slow := newStartFuncChannel("discord", func(context.Context) error {
		<-release
		return nil
	})

	ready := make(chan struct{})
	sigCh := make(chan os.Signal, 1)
	got := make(chan ChannelStartup, 1)

	defaults := DefaultDeps()
	deps := Deps{
		LoadConfig:        func() (*config.Config, error) { return cfg, nil },
		OpenChatSessions:  defaults.OpenChatSessions,
		OpenAgentSessions: defaults.OpenAgentSessions,
		BuildAgents:       func(cfg *config.Config) *agent.Registry { return agentregistry.Build(cfg, "") },
		NewChannels: func(*config.Config) ([]channel.Channel, error) {
			return []channel.Channel{slow, good, broken}, nil
		},
		OnReady:           func() { close(ready) },
		OnChannelsStarted: func(s ChannelStartup) { got <- s },
	}

	runner := RunWith(deps, RunOptions{Out: io.Discard, Logger: slog.Default(), SigCh: sigCh})
	done := make(chan error, 1)
	go func() { done <- runner.Run(t.Context()) }()

	<-ready
	var startup ChannelStartup
	select {
	case startup = <-got:
	case <-time.After(ChannelStartBudget + 10*time.Second):
		close(release)
		t.Fatal("daemon never reported its channel startup outcomes")
	}
	close(release)
	sigCh <- os.Interrupt
	<-done

	if startup.Total != 3 {
		t.Errorf("Total = %d, want 3", startup.Total)
	}
	byName := map[string]ChannelStatus{}
	for _, r := range startup.Results {
		byName[r.Name] = r
	}
	if s := byName["feishu"]; s.Status != ChannelStatusConnected {
		t.Errorf("feishu status = %q, want %q", s.Status, ChannelStatusConnected)
	}
	if s := byName["telegram"]; s.Status != ChannelStatusSkipped ||
		!strings.Contains(s.Error, "401 Unauthorized") {
		t.Errorf("telegram = %+v, want skipped with the adapter's reason", s)
	}
	if s := byName["discord"]; s.Status != ChannelStatusTimeout {
		t.Errorf("discord status = %q, want %q", s.Status, ChannelStatusTimeout)
	}
}
