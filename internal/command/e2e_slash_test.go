// Package command_test — end-to-end test for the slash command
// dispatch path.
//
// The test wires the same components as cmd/nightme/run.go:
//
//	gateway.Channel (echo)
//	  ↓ cs.WithChannel(channelWrap{...})        ← bug fix lives here
//	ChatSession.Channel() = channelWrap
//	  ↓ command.Handle → SlashOutput{Reply, Consumed: true}
//	runtime shim → cs.Channel().Send(...) → echo.Send(...)
//	  ↓
//	echo.Record() captures the OutboundMessage
//
// Without cs.WithChannel binding, the runtime shim's
// `if out.Consumed && out.Reply != "" && cs.Channel() != nil` guard
// fails (Channel() == nil) and every slash command reply is silently
// dropped. This regression surfaced on 2026-08-09 when a user
// reported that "/new produces no response"; the bug also affected
// /cwd, /kill, /use, /watch, /think, /tools — every command whose
// reply routes through the runtime shim's cs.Channel() call.
//
// The test MUST fail before the fix and pass after. It is the
// regression guard for the silent-drop bug.

package command_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/channel/echo"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/command/newcmd"
	"github.com/cnlangzi/nightme/internal/gateway"
)

// ─── Echo bridge ─────────────────────────────────────────────────────
//
// "Echo bridge" = a minimal agent.Agent stub that satisfies the
// Spawner contract without forking any process. newcmd's Handle does
// not actually exercise the agent when selectedCwd is empty
// (RequireActiveCwd replies "No active workspace…"), so the stub
// just needs to exist; it never receives SendBlocks in
// this test path.

// echoAgent implements agent.Agent. Events() is buffered; Close()
// closes it. All Send* / New / Detect are no-ops because the test
// path never drives them.
type echoAgent struct {
	mu     sync.Mutex
	pid    int
	events chan agent.AgentEvent
	closed bool
}

func newEchoAgent(pid int) *echoAgent {
	return &echoAgent{pid: pid, events: make(chan agent.AgentEvent, 4)}
}

func (a *echoAgent) Events() <-chan agent.AgentEvent { return a.events }
func (a *echoAgent) PID() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pid
}
func (a *echoAgent) Info() agent.Info {
	return agent.NewInfo("echo", agent.ModePTY, "echo", nil, nil)
}
func (a *echoAgent) Detect() error { return nil }
func (a *echoAgent) Start(_ context.Context, _ agent.StartConfig) (*agent.Agent, error) {
	return agent.NewAgent(a.Info(), a.pid, a.events, &echoDriver{inner: a}), nil
}
func (a *echoAgent) SendBlocks(context.Context, []agent.ContentBlock) error {
	return nil
}
func (a *echoAgent) SendPermission(string) error { return nil }
func (a *echoAgent) New(context.Context) error   { return nil }
func (a *echoAgent) Stop(context.Context) error { return agent.ErrNotSupported }
func (a *echoAgent) SetModel(context.Context, string, string) error {
	return agent.ErrNotSupported
}
func (a *echoAgent) RunOnce(ctx context.Context, _ agent.StartConfig, blocks []agent.ContentBlock) (string, error) {
	if err := a.SendBlocks(ctx, blocks); err != nil {
		return "", err
	}
	for {
		select {
		case ev, ok := <-a.events:
			if !ok {
				return "", errors.New("echoAgent: event stream closed without result")
			}
			switch ev.Kind {
			case agent.EventAgentResult:
				if ev.Result == nil {
					return "", errors.New("echoAgent: nil result payload")
				}
				return ev.Result.Text, nil
			case agent.EventAgentDone:
				return "", errors.New("echoAgent: turn ended without result")
			case agent.EventAgentError:
				if ev.Err != nil {
					return "", ev.Err
				}
				return "", errors.New("echoAgent: nil error payload")
			}
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}
func (a *echoAgent) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.closed {
		a.closed = true
		close(a.events)
	}
	return nil
}

var _ agent.Starter = (*echoAgent)(nil)

// echoDriver forwards driver calls back to an echoAgent.
type echoDriver struct{ inner *echoAgent }

func (d *echoDriver) SendBlocks(ctx context.Context, b []agent.ContentBlock) error {
	return d.inner.SendBlocks(ctx, b)
}
func (d *echoDriver) SendPermission(resp string) error {
	return d.inner.SendPermission(resp)
}
func (d *echoDriver) Reset(ctx context.Context) error { return d.inner.New(ctx) }
func (d *echoDriver) Stop(ctx context.Context) error { return d.inner.Stop(ctx) }
func (d *echoDriver) SetModel(ctx context.Context, providerID, modelID string) error {
	return d.inner.SetModel(ctx, providerID, modelID)
}
func (d *echoDriver) Close() error                   { return d.inner.Close() }
// echoSpawner is a Spawner that hands out fresh echoAgent instances.
type echoSpawner struct {
	mu       sync.Mutex
	nextPID int
	last     *echoAgent
}

func newEchoSpawner() *echoSpawner { return &echoSpawner{} }

func (s *echoSpawner) Spawn(_ context.Context, _, _ string, _ []string, _ string) (*agent.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextPID++
	a := newEchoAgent(99000 + s.nextPID)
	s.last = a
	return a.Start(context.Background(), agent.StartConfig{})
}

// ─── Runtime shim (mirror of cmd/nightme/run.go) ─────────────────────
//
// IMPORTANT: this closure mirrors the production shim at
// cmd/nightme/run.go:407-477 verbatim. If the production shim
// changes (e.g., the planned cs.WithChannel fix is added), this
// helper MUST be updated in lockstep — the whole point of the test
// is to mirror production wiring so the regression guard tracks
// reality.
//
// The duplication is deliberate: extracting the shim into a shared
// package would force internal/command to import internal/gateway,
// breaking the F-51 boundary rule documented at
// internal/command/event.go:9-12. Once the bug is fixed we can
// consider extracting; until then, the duplication is the cost of
// the boundary.

func newRuntimeShim(
	mgr *chatsession.Manager,
	reg *command.Registry,
	primary string,
) func(ctx context.Context, msg *gateway.InboundMessage) (*gateway.CommandResult, error) {
	commander := command.NewCommander(reg)
	rt := command.RuntimeServices{
		Config: command.Config{Primary: primary},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return func(ctx context.Context, msg *gateway.InboundMessage) (*gateway.CommandResult, error) {
		if msg == nil {
			return nil, nil
		}

		cs, err := mgr.GetOrCreate(msg.ChatID, primary)
		if err != nil {
			return nil, nil
		}

		input := command.SlashInput{
			ChatID:     msg.ChatID,
			UserID:     msg.UserID,
			Text:       msg.Text,
			MessageID:  msg.MessageID,
			HasMention: msg.HasMention,
		}
		out, handled, err := commander.Dispatch(ctx, rt, cs, input)
		if err != nil {
			errText := "❌ " + err.Error()
			ch := cs.Channel()
			if ch != nil {
				_ = ch.Send(ctx, chatsession.OutboundMessage{
					ChatID:  msg.ChatID,
					Text:    errText,
					ReplyTo: msg.MessageID,
				})
			}
			return &gateway.CommandResult{Consumed: true, Reply: errText}, nil
		}
		if !handled {
			return nil, nil
		}
		ch := cs.Channel()
		if out.Consumed && out.Reply != "" && ch != nil {
			_ = ch.Send(ctx, chatsession.OutboundMessage{
				ChatID:  msg.ChatID,
				Text:    out.Reply,
				ReplyTo: msg.MessageID,
			})
		}
		for _, ob := range out.Outbound {
			if ch == nil {
				continue
			}
			switch {
			case ob.PatchBotMsgID != "":
				_ = ch.Patch(ctx, ob)
			case ob.Card != nil:
				_, _ = ch.SendCard(ctx, ob)
			default:
				_ = ch.Send(ctx, ob)
			}
		}
		return &gateway.CommandResult{
			Consumed: out.Consumed,
			Dropped:  out.Dropped,
			Reply:    out.Reply,
		}, nil
	}
}

// ─── testChannelWrap (test-only chatsession.Channel adapter) ────────
//
// Mirrors cmd/nightme/channel_wrap.go:channelWrap — translates
// gateway.Channel → chatsession.Channel. Lives in this test file
// so we don't drag in cmd/nightme (which is package main, not
// importable from tests). Once the runtime shim is extracted to a
// shared package, this helper goes away and the test imports the
// shared one.

type testChannelWrap struct {
	ch gateway.Channel
}

func (w *testChannelWrap) Send(ctx context.Context, msg chatsession.OutboundMessage) error {
	return w.ch.Send(ctx, gateway.OutboundMessage{
		ChatID:  msg.ChatID,
		Text:    msg.Text,
		ReplyTo: msg.ReplyTo,
	})
}

func (w *testChannelWrap) SendCard(ctx context.Context, msg chatsession.OutboundMessage) (string, error) {
	if err := w.ch.Send(ctx, gateway.OutboundMessage{
		ChatID:  msg.ChatID,
		Text:    msg.Text,
		ReplyTo: msg.ReplyTo,
		Kind:    gateway.OutCard,
	}); err != nil {
		return "", err
	}
	return "", nil
}

func (w *testChannelWrap) Patch(ctx context.Context, msg chatsession.OutboundMessage) error {
	return w.ch.Send(ctx, gateway.OutboundMessage{
		ChatID:  msg.ChatID,
		Text:    msg.PatchResult,
		ReplyTo: msg.PatchBotMsgID,
		Kind:    gateway.OutCardPatch,
	})
}

var _ chatsession.Channel = (*testChannelWrap)(nil)

// ─── Wiring helper ───────────────────────────────────────────────────

// wiredHarness bundles the runtime so each subtest can drive a
// slash command through the gateway and assert on the captured
// outbound messages.
type wiredHarness struct {
	echoCh *echo.Channel
	mgr    *chatsession.Manager
	gw     gateway.Gateway
}

func newWiredHarness(t *testing.T) *wiredHarness {
	t.Helper()

	// Echo Channel (gateway.Channel impl). nil writer — the test
	// inspects Record() rather than stdout.
	echoCh := echo.New("echo", nil)

	// Manager + spawner. Mirror run.go: WithChannelResolver wraps
	// the gateway.Channel as a chatsession.Channel so every new
	// ChatSession gets bound at GetOrCreate time. This replaces
	// the old placeholder (`mgr.WithChannelResolver(nil)` +
	// "runtime shim binds via WithChannel instead" — which the
	// runtime shim never actually did, silently dropping every
	// slash command reply; see the 2026-08-09 regression).
	mgr := chatsession.NewManager()
	mgr.WithSpawner(newEchoSpawner())
	mgr.WithChannelResolver(func(string) chatsession.Channel {
		return &testChannelWrap{ch: echoCh}
	})

	// Slash command registry: just /new for this test. The factory
	// takes *chatsession.Manager; the test exercises both the
	// RequireActiveCwd early-exit reply ("No active workspace…")
	// and, if a fix binds the channel, the real reset path.
	reg := command.NewRegistry()
	reg.Register(newcmd.NewFactory(mgr))

	// Gateway with a no-op MessageDispatcher (the test only
	// exercises the slash command path, never the agent loop).
	gw := gateway.New(func(context.Context, *gateway.InboundMessage) error { return nil })
	gw.WithCommander(newRuntimeShim(mgr, reg, "echo"))

	return &wiredHarness{echoCh: echoCh, mgr: mgr, gw: gw}
}

// driveSlash sends one InboundMessage through the gateway and waits
// up to `wait` for the echo channel to capture at least one
// outbound message. Returns the captured messages.
func (h *wiredHarness) driveSlash(t *testing.T, msg *gateway.InboundMessage, wait time.Duration) []gateway.OutboundMessage {
	t.Helper()
	if _, err := h.gw.DispatchInbound(context.Background(), msg); err != nil {
		t.Fatalf("DispatchInbound(%q): %v", msg.Text, err)
	}

	deadline := time.Now().Add(wait)
	var rec []gateway.OutboundMessage
	for time.Now().Before(deadline) {
		rec = h.echoCh.Record()
		if len(rec) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return rec
}

// ─── Tests ───────────────────────────────────────────────────────────

// TestSlash_E2E_NewReply_ReachesEchoChannel is the regression guard
// for the 2026-08-09 silent-drop bug.
//
// The runtime shim (cmd/nightme/run.go:447-454) guards every slash
// command reply on `cs.Channel() != nil`. With the buggy wiring
// (`mgr.WithChannelResolver(nil)` + no `cs.WithChannel` call), the
// guard is false for every chat and the reply is silently dropped.
//
// Driving /new without a selected cwd exercises the cheapest path:
// RequireActiveCwd returns a hardcoded SlashOutput reply ("No
// active workspace…"); if the bug is present, the test sees an
// empty Record().
func TestSlash_E2E_NewReply_ReachesEchoChannel(t *testing.T) {
	h := newWiredHarness(t)

	msg := &gateway.InboundMessage{
		ChatID:    "oc_e2e_new_1",
		UserID:    "ou_e2e_user",
		Text:      "/new",
		MessageID: "om_e2e_msg_1",
		Time:      time.Now(),
	}

	rec := h.driveSlash(t, msg, 2*time.Second)
	if len(rec) == 0 {
		t.Fatal("no OutboundMessage captured on echo channel within 2s — " +
			"slash command reply was silently dropped. " +
			"This is the 2026-08-09 regression: the runtime shim never " +
			"binds cs.WithChannel(...) so cs.Channel() returns nil and " +
			"every /cmd reply is dropped by the `ch != nil` guard at " +
			"cmd/nightme/run.go:447-454.")
	}

	// The first /new without a workspace replies with the
	// "No active workspace…" preflight message — see
	// internal/command/preflight.go:RequireActiveCwd.
	var reply *gateway.OutboundMessage
	for i := range rec {
		if strings.Contains(rec[i].Text, "workspace") {
			reply = &rec[i]
			break
		}
	}
	if reply == nil {
		t.Fatalf("captured messages do not include the /new preflight reply "+
			"(expected substring %q in Text): %+v",
			"workspace", rec)
	}
	if reply.ChatID != msg.ChatID {
		t.Errorf("reply.ChatID = %q, want %q", reply.ChatID, msg.ChatID)
	}
}

// TestSlash_E2E_NewReply_WithCwd_ReachesEchoChannel exercises the
// /new path AFTER selectedCwd is set. This produces a *different*
// reply from the no-cwd preflight ("No agent session in current
// workspace to reset. Send a message to start one."), proving the
// shim isn't just emitting a constant — it actually ran the
// newcmd Handle and propagated its real reply.
//
// We bypass /cwd and seed selectedCwd directly via the manager so
// this test doesn't depend on /cwd registration.
func TestSlash_E2E_NewReply_WithCwd_ReachesEchoChannel(t *testing.T) {
	h := newWiredHarness(t)
	const chatID = "oc_e2e_new_2"

	// Seed selectedCwd on the ChatSession that the gateway will
	// create when it sees the inbound. Real tmp dir so the
	// underlying CS doesn't reject the value.
	dir := t.TempDir()
	cs, err := h.mgr.GetOrCreate(chatID, "echo")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if err := cs.SetSelectedCwd(dir); err != nil {
		t.Fatalf("SetSelectedCwd(%q): %v", dir, err)
	}

	// Drive /new. selectedCwd is set; the pool is empty;
	// newcmd's Handle returns "No agent session in current
	// workspace to reset. Send a message to start one." (see
	// internal/command/newcmd/cmd.go:108-109).
	newMsg := &gateway.InboundMessage{
		ChatID:    chatID,
		UserID:    "ou_e2e_user",
		Text:      "/new",
		MessageID: "om_e2e_new",
		Time:      time.Now(),
	}
	rec := h.driveSlash(t, newMsg, 2*time.Second)
	if len(rec) == 0 {
		t.Fatal("/new reply (with selectedCwd set) was silently dropped — " +
			"see TestSlash_E2E_NewReply_ReachesEchoChannel")
	}

	// The newcmd reply is unique to /new with cwd set; finding
	// it in the captured set proves the slash command reached
	// the command layer (not just the gateway's text fall-through).
	var newReply *gateway.OutboundMessage
	for i := range rec {
		if strings.Contains(rec[i].Text, "No agent session") {
			newReply = &rec[i]
			break
		}
	}
	if newReply == nil {
		t.Fatalf("/new reply missing from captured messages "+
			"(expected substring %q): %+v",
			"No agent session", rec)
	}
}
