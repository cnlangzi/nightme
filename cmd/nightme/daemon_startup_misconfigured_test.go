// daemon_startup_misconfigured_test.go — the regression this whole
// change exists for.
//
// The failure it guards: a channel configured wrong (or pointed at a
// host it cannot reach) makes the daemon refuse to start, and the
// operator gets one sentence that says nothing about which channel or
// why. Both halves have to hold at once — the daemon starts anyway,
// AND the reason reaches the terminal — because either one alone still
// leaves the operator stuck.
//
// It drives the real runtime with the same Deps shape both daemon
// children install, then reads the report back with the parent's own
// functions, so a regression anywhere along that chain fails here.
//
// No build tag: this runs on the linux / windows / darwin CI legs.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentregistry"
	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/daemoncontrol"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/runtime"
)

// misconfiguredChannel is a channel.Channel whose Start does whatever
// the test tells it to — succeed, fail, or panic. Real adapters can't
// stand in for this: whether they fail depends on a network round
// trip, and a test that depends on api.telegram.org answering is a
// test that fails for the wrong reason.
type misconfiguredChannel struct {
	name  string
	start func(context.Context) error
}

func (c *misconfiguredChannel) Name() string                    { return c.name }
func (c *misconfiguredChannel) Start(ctx context.Context) error { return c.start(ctx) }
func (c *misconfiguredChannel) Stop(context.Context) error      { return nil }
func (c *misconfiguredChannel) Incoming() <-chan messages.InboundMessage {
	in := make(chan messages.InboundMessage, 1)
	close(in)
	return in
}
func (c *misconfiguredChannel) Send(context.Context, messages.OutboundMessage) error {
	return nil
}
func (c *misconfiguredChannel) OnPromptEnded(context.Context, string, string, agent.PromptEndReason) {
}
func (c *misconfiguredChannel) HealthSnapshot() (string, json.RawMessage, error) {
	return c.name, json.RawMessage("{}"), nil
}
func (c *misconfiguredChannel) SetLogger(*slog.Logger) {}
func (c *misconfiguredChannel) BuildBlocks(string, []messages.Attachment) []agent.ContentBlock {
	return nil
}

var _ channel.Channel = (*misconfiguredChannel)(nil)

// runDaemonWithChannels starts the real runtime over the given
// channels with the report hooks a daemon child installs, and returns
// the report the parent would read plus the terminal lines the parent
// would print.
//
// Returns ("", "") with the error when the daemon never became ready —
// which is itself the failure this file exists to catch.
func runDaemonWithChannels(t *testing.T, chs ...channel.Channel) (terminal string, err error) {
	t.Helper()

	paths := daemoncontrol.Paths{Dir: t.TempDir()}
	report := newStartupReportWriter(startupReportPath(paths))

	ready := make(chan struct{})
	sigCh := make(chan os.Signal, 1)

	defaults := runtime.DefaultDeps()
	deps := runtime.Deps{
		LoadConfig: func() (*config.Config, error) {
			return &config.Config{Primary: "claude", Paths: config.PathsConfig{DataDir: paths.Dir}}, nil
		},
		OpenChatSessions:  defaults.OpenChatSessions,
		OpenAgentSessions: defaults.OpenAgentSessions,
		BuildAgents:       func(cfg *config.Config) *agent.Registry { return agentregistry.Build(cfg, "") },
		NewChannels:       func(*config.Config) ([]channel.Channel, error) { return chs, nil },
		// Exactly what daemon_lifecycle_unix.go / _windows.go install.
		OnReady:           func() { report.SetReady(); close(ready) },
		OnChannelsStarted: report.SetChannels,
	}

	runner := runtime.RunWith(deps, runtime.RunOptions{
		Out: io.Discard, Logger: slog.Default(), SigCh: sigCh,
	})
	done := make(chan error, 1)
	go func() { done <- runner.Run(t.Context()) }()

	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("daemon refused to start: %v\n(the operator would see nothing about why)", err)
	case <-time.After(30 * time.Second):
		t.Fatal("daemon did not become ready within 30s")
	}

	var out bytes.Buffer
	printStartupChannels(&out, waitStartupChannels(paths, os.Getpid(), 5*time.Second))

	sigCh <- os.Interrupt
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("daemon returned %v after a clean shutdown", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("daemon did not shut down within 30s of the signal")
	}
	return out.String(), nil
}

// TestMisconfiguredChannel_DaemonStartsAndExplainsWhy is the headline
// regression: feishu configured and fine, telegram carrying a bot
// token the API rejects. The daemon must come up (feishu keeps
// working) and `nightme start` must say telegram failed and why.
func TestMisconfiguredChannel_DaemonStartsAndExplainsWhy(t *testing.T) {
	terminal, err := runDaemonWithChannels(t,
		&misconfiguredChannel{name: "feishu", start: func(context.Context) error { return nil }},
		&misconfiguredChannel{
			name: "telegram",
			start: func(context.Context) error {
				return errors.New(`telegram: getMe: 401 {"ok":false,"error_code":401,"description":"Unauthorized"}`)
			},
		},
	)
	if err != nil {
		t.Fatalf("runDaemonWithChannels: %v", err)
	}

	if !strings.Contains(terminal, "channel feishu: connected") {
		t.Errorf("the healthy channel is missing from the startup output:\n%s", terminal)
	}
	if !strings.Contains(terminal, "channel telegram: skipped") {
		t.Errorf("the misconfigured channel was not reported as skipped:\n%s", terminal)
	}
	if !strings.Contains(terminal, "Unauthorized") {
		t.Errorf("the startup output does not carry telegram's reason:\n%s", terminal)
	}
}

// TestMisconfiguredChannel_PanicDoesNotStopTheDaemon: an adapter
// defect is a channel problem, not a daemon problem.
func TestMisconfiguredChannel_PanicDoesNotStopTheDaemon(t *testing.T) {
	terminal, err := runDaemonWithChannels(t,
		&misconfiguredChannel{
			name: "slack",
			start: func(context.Context) error {
				panic("runtime error: index out of range [3]")
			},
		},
	)
	if err != nil {
		t.Fatalf("runDaemonWithChannels: %v", err)
	}
	if !strings.Contains(terminal, "channel slack: skipped") {
		t.Errorf("a panicking channel was not reported as skipped:\n%s", terminal)
	}
	if !strings.Contains(terminal, "index out of range") {
		t.Errorf("the startup output does not carry the panic:\n%s", terminal)
	}
}

// TestNoChannelConfigured_DaemonStarts: nothing configured at all is
// a state the daemon serves in, not a reason to refuse to start. The
// operator still gets the usual "daemon started" line from startDaemon
// and finds the WARNING in the log.
func TestNoChannelConfigured_DaemonStarts(t *testing.T) {
	terminal, err := runDaemonWithChannels(t)
	if err != nil {
		t.Fatalf("daemon refused to start with no channels configured: %v", err)
	}
	if strings.TrimSpace(terminal) != "" {
		t.Errorf("expected no per-channel lines, got:\n%s", terminal)
	}
}

// TestReadinessTimeoutError_CarriesTheChildReason pins the wording
// both platform parents share, because before the startup report
// existed this text was identical no matter what had gone wrong —
// which is exactly what made the Windows failure undiagnosable.
func TestReadinessTimeoutError_CarriesTheChildReason(t *testing.T) {
	withReason := readinessTimeoutError("reconcile agent_sessions: pid probe failed", "/tmp/nightme/daemon-stderr.log").Error()
	if !strings.Contains(withReason, "did not become ready within 15s") {
		t.Errorf("lost the timeout sentence: %q", withReason)
	}
	if !strings.Contains(withReason, "reconcile agent_sessions: pid probe failed") {
		t.Errorf("lost the child's reason: %q", withReason)
	}
	if !strings.Contains(withReason, "/tmp/nightme/daemon-stderr.log") {
		t.Errorf("lost the stderr capture path: %q", withReason)
	}

	// No stderr capture on this host: say so instead of pointing at a
	// path the operator cannot open.
	noCapture := readinessTimeoutError("socket already in use", "").Error()
	if !strings.Contains(noCapture, "socket already in use") {
		t.Errorf("lost the child's reason: %q", noCapture)
	}
	if strings.Contains(noCapture, "; child stderr: ") {
		t.Errorf("promised a stderr file that does not exist: %q", noCapture)
	}

	// Child published nothing: still a usable message, just without a
	// reason segment.
	bare := readinessTimeoutError("", "/tmp/nightme/daemon-stderr.log").Error()
	if strings.Contains(bare, "15s: ;") || strings.Contains(bare, "15s:") {
		t.Errorf("left an empty reason segment behind: %q", bare)
	}
}
