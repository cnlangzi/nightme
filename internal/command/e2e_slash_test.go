// Package command_test — end-to-end test for the slash command
// dispatch path.
//
// The test wires the same components as cmd/nightme/run.go:
//
//	channel.Channel (echo)
//	  ↓ cs.WithEmitter(channelWrap{...})        ← bug fix lives here
//	ChatSession.Channel() = channelWrap
//	  ↓ command.Handle → SlashOutput{Reply, Consumed: true}
//	runtime shim → cs.Emitter().Send(...) → echo.Send(...)
//	  ↓
//	echo.Record() captures the OutboundMessage
//
// Without cs.WithChannel binding, the runtime shim's
// `if out.Consumed && out.Reply != "" && cs.Emitter() != nil` guard
// fails (Channel() == nil) and every slash command reply is silently
// dropped. This regression surfaced on 2026-08-09 when a user
// reported that "/new produces no response"; the bug also affected
// /cwd, /kill, /use, /watch, /think, /tools — every command whose
// reply routes through the runtime shim's cs.Emitter() call.
//
// The test MUST fail before the fix and pass after. It is the
// regression guard for the silent-drop bug.

package command_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/channel/echo"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	commandServices "github.com/cnlangzi/nightme/internal/command/services"
	"github.com/cnlangzi/nightme/internal/command/newcmd"
	"github.com/cnlangzi/nightme/internal/gateway"
	"github.com/cnlangzi/nightme/internal/gateway/inbound"
	"github.com/cnlangzi/nightme/internal/gateway/outbound"
	"github.com/cnlangzi/nightme/internal/gatewaytest"
	"github.com/cnlangzi/nightme/internal/shell"
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
func (a *echoAgent) RunOnce(ctx context.Context, _ agent.StartConfig, blocks []agent.ContentBlock) (agent.RunResult, error) {
	if err := a.SendBlocks(ctx, blocks); err != nil {
		return agent.RunResult{}, err
	}
	for {
		select {
		case ev, ok := <-a.events:
			if !ok {
				return agent.RunResult{}, errors.New("echoAgent: event stream closed without result")
			}
			switch ev.Kind {
			case agent.EventAgentResult:
				if ev.Result == nil {
					return agent.RunResult{}, errors.New("echoAgent: nil result payload")
				}
				return agent.RunResult{
					Text:       ev.Result.Text,
					Usage:      ev.Result.Usage,
					DurationMs: ev.Result.DurationMs,
					Subtype:    ev.Result.Subtype,
				}, nil
			case agent.EventAgentDone:
				return agent.RunResult{}, errors.New("echoAgent: turn ended without result")
			case agent.EventAgentError:
				if ev.Err != nil {
					return agent.RunResult{}, ev.Err
				}
				return agent.RunResult{}, errors.New("echoAgent: nil error payload")
			}
		case <-ctx.Done():
			return agent.RunResult{}, ctx.Err()
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
) *replyingCommander {
	commander := command.NewCommander(reg)
	return &replyingCommander{
		mgr:       mgr,
		commander: commander,
		primary:   primary,
	}
}

// replyingCommander is a command.Commander wrapper that mirrors
// the v0.x runtime shim: it dispatches via the real Commander
// AND posts the resulting reply / out.Outbound via the chat
// session's Emitter. The inbound layer just routes to it; the
// reply-side plumbing lives here (next to the chat session it
// touches).
type replyingCommander struct {
	mgr       *chatsession.Manager
	commander command.Commander
	primary   string
}

func (r *replyingCommander) Match(text string) (string, bool) {
	return r.commander.Match(text)
}

func (r *replyingCommander) Dispatch(ctx context.Context, rt command.RuntimeServices, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, bool, error) {
	out, handled, err := r.commander.Dispatch(ctx, rt, cs, input)
	if err != nil {
		errText := "❌ " + err.Error()
		ch := cs.Emitter()
		if ch != nil {
			_ = ch.Send(ctx, messages.OutboundMessage{
				ChatID:  input.ChatID,
				Text:    errText,
				ReplyTo: input.MessageID,
			})
		}
		return &command.SlashOutput{Consumed: true, Reply: errText}, true, nil
	}
	if !handled {
		return nil, false, nil
	}
	ch := cs.Emitter()
	if out.Consumed && out.Reply != "" && ch != nil {
		_ = ch.Send(ctx, messages.OutboundMessage{
			ChatID:  input.ChatID,
			Text:    out.Reply,
			ReplyTo: input.MessageID,
		})
	}
	for _, ob := range out.Outbound {
		if ch == nil {
			continue
		}
		if ob.Kind == messages.OutCardPatch {
			_ = ch.Send(ctx, ob)
		} else if ob.Card != nil {
			_, _ = ch.SendCard(ctx, ob)
		} else {
			_ = ch.Send(ctx, ob)
		}
	}
	return out, true, nil
}

// e2eStubMgr satisfies inbound.MessageHandler with no-op
// behaviour; the e2e test never exercises the message
// e2eStubMgr satisfies inbound.MessageHandler with no-op
// behaviour; the e2e test never exercises the message
// (agent-loop) branch.
//
// GetOrCreate is forwarded to the manager the caller provides
// at construction. inbound.tryCommandDispatch needs a real
// *ChatSession (the replyingCommander shim calls
// cs.Emitter() to deliver replies), so the stub must hand
// out real ChatSessions from a real Manager.
type e2eStubMgr struct {
	mgr *chatsession.Manager
}

func (e e2eStubMgr) HandleInbound(_ context.Context, _ *messages.InboundMessage) error { return nil }
func (e e2eStubMgr) GetOrCreate(chatID, primary string) (*chatsession.ChatSession, error) {
	return e.mgr.GetOrCreate(chatID, primary)
}

// e2eStubRouter satisfies inbound.ReactionRouter with no-op
// behaviour.
type e2eStubRouter struct{}

func (e2eStubRouter) Handle(_ context.Context, _ string, _ commandServices.ReactionEvent) bool {
	return false
}

// ─── testChannelWrap (test-only outbound.Emitter adapter) ──────────
//
// Wraps a channel.Channel into an outbound.Emitter so the e2e
// test can exercise the chat-session-bound Emitter path without
// dragging in the runtime wiring from cmd/nightme. PATCH semantics
// are encoded as Kind=OutCardPatch (the wrap just forwards to
// Send with the same Kind).

type testChannelWrap struct {
	ch channel.Channel
}

func (w *testChannelWrap) Send(ctx context.Context, msg messages.OutboundMessage) error {
	return w.ch.Send(ctx, msg)
}

func (w *testChannelWrap) SendCard(ctx context.Context, msg messages.OutboundMessage) (string, error) {
	if err := w.ch.Send(ctx, msg); err != nil {
		return "", err
	}
	return "", nil
}

var _ outbound.Emitter = (*testChannelWrap)(nil)

// noopEmitter is a do-nothing outbound.Emitter used by tests
// that drive the gateway but don't care about outbound flow
// (the e2e harness already wires the real wrap above for the
// chat-session path; this one is for the gateway's own reference).
type noopEmitter struct{}

func (noopEmitter) Send(context.Context, messages.OutboundMessage) error {
	return nil
}
func (noopEmitter) SendCard(context.Context, messages.OutboundMessage) (string, error) {
	return "", nil
}

// ─── Wiring helper ───────────────────────────────────────────────────

// wiredHarness bundles the runtime so each subtest can drive a
// slash command through the gateway and assert on the captured
// outbound messages.
type wiredHarness struct {
	echoCh *echo.Channel
	mgr    *chatsession.Manager
	gw     *gateway.Router
}

func newWiredHarness(t *testing.T) *wiredHarness {
	t.Helper()

	// Echo Channel (channel.Channel impl). nil writer — the test
	// inspects Record() rather than stdout.
	echoCh := echo.New("echo", nil)

	mgr := chatsession.NewManager()
	mgr.WithSpawner(newEchoSpawner())
	// Wire the same Emitter for every new ChatSession via
	// WithEmitter. The Emitter is the test ChannelWrap above —
	// every Send ends up at the echo channel so Record() can be
	// inspected post-dispatch.
	mgr.WithEmitter(&testChannelWrap{ch: echoCh})

	// Slash command registry: just /new for this test. The factory
	// takes *chatsession.Manager; the test exercises both the
	// RequireActiveCwd early-exit reply ("No active workspace…")
	// and, if a fix binds the channel, the real reset path.
	reg := command.NewRegistry()
	reg.Register(newcmd.NewFactory(mgr))

	// Gateway with a no-op MessageDispatcher (the test only
	// exercises the slash command path, never the agent loop).
	// F-58: the dispatch chain is in *inbound.Router; we wire a
	// real commander + no-op stubs for the other three slots.
	noop := &gatewaytest.NoopEmitter{}
	ir := inbound.New(
		&e2eStubMgr{mgr: mgr},
		newRuntimeShim(mgr, reg, "echo"),
		shell.NewDispatcher(nil),
		&e2eStubRouter{},
		noop,
		"echo",
	)
	gw := gateway.New(ir, noop)

	return &wiredHarness{echoCh: echoCh, mgr: mgr, gw: gw}
}

// driveSlash sends one InboundMessage through the gateway and waits
// up to `wait` for the echo channel to capture at least one
// outbound message. Returns the captured messages.
func (h *wiredHarness) driveSlash(t *testing.T, msg *messages.InboundMessage, wait time.Duration) []messages.OutboundMessage {
	t.Helper()
	if _, err := h.gw.DispatchInbound(context.Background(), msg); err != nil {
		t.Fatalf("DispatchInbound(%q): %v", msg.Text, err)
	}

	deadline := time.Now().Add(wait)
	var rec []messages.OutboundMessage
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
// command reply on `cs.Emitter() != nil`. With the buggy wiring
// (`mgr.WithEmitter(testEmitter)` + no `cs.WithChannel` call), the
// guard is false for every chat and the reply is silently dropped.
//
// Driving /new without a selected cwd exercises the cheapest path:
// RequireActiveCwd returns a hardcoded SlashOutput reply ("No
// active workspace…"); if the bug is present, the test sees an
// empty Record().
func TestSlash_E2E_NewReply_ReachesEchoChannel(t *testing.T) {
	h := newWiredHarness(t)

	msg := &messages.InboundMessage{
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
			"binds cs.WithEmitter(...) so cs.Emitter() returns nil and " +
			"every /cmd reply is dropped by the `ch != nil` guard at " +
			"cmd/nightme/run.go:447-454.")
	}

	// The first /new without a workspace replies with the
	// "No active workspace…" preflight message — see
	// internal/command/preflight.go:RequireActiveCwd.
	var reply *messages.OutboundMessage
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
	newMsg := &messages.InboundMessage{
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
	var newReply *messages.OutboundMessage
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
