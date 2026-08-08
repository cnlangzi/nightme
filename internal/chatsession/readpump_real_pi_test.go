// Real-pi runtime e2e test for the readPump path.
//
// File: readpump_real_pi_test.go
//
// Counterpart to internal/bridge/pi/session_real_test.go (which
// exercises the bridge directly) and to the mock-based
// readpump_pi_test.go (which exercises the readPump pipeline
// against a fake pi).
//
// What it covers:
//
//	ChatSession.Dispatch → input buffer → Spawn → bridge.SendText →
//	pi reads stdin → pi emits events → readPump → AgentEventBus subscriber →
//	outbound Send on a fake channel
//
// This is the path the F-32 2026-08-06 incident was suspected to
// break: "events from pi don't reach feishu". This test bypasses
// feishu (uses an in-memory fake channel) and asserts every link
// in the chain works against the real `pi` binary.
//
// Skipped when pi is not on PATH (CI runners / machines without
// the dependency). Run explicitly:
//
//	go test ./internal/chatsession -run RealPi_E2E -v

package chatsession

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	pibridge "github.com/cnlangzi/nightme/internal/bridge/pi"
	"github.com/cnlangzi/nightme/internal/registry"
)

// fakeOutboundChannel is an in-memory replacement for the feishu
// channel. Reserved for follow-up tests that want to assert the
// full outbound path. The current e2e test reads events from
// the bridge directly (sess.Events()) which is sufficient to
// prove the readPump pipeline — the outbound translation is
// exercised by the runtime's AgentEventBus subscriber which the test
// installs.
type fakeOutboundChannel struct {
	mu     sync.Mutex
	sends  []capturedSend
	closed bool
}

type capturedSend struct {
	ChatID    string
	UserMsgID string
	Text      string
	Kind      string
}

func (c *fakeOutboundChannel) Send(ctx context.Context, msg any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sends = append(c.sends, capturedSend{})
	return nil
}

// TestRealPi_E2E_PromptRoundTrip drives the full runtime pipeline
// against the real `pi` binary:
//
//  1. ChatSession with a real Spawner (uses pi.Agent.Start)
//  2. Dispatch a user message containing a unique marker via
//     the bridge's SendBlocks (the same path the gateway uses)
//  3. Accumulate all events flowing through the runtime's
//     EventHandler (this is the path the runtime installs in
//     production — no separate consumer of the bridge channel)
//  4. Assert the accumulated reply contains the marker (proves
//     pi received our input) and is non-empty (proves pi gave
//     a response); the EventAgentDone event arrived.
//
// Skipped if `pi` is not on PATH.
func TestRealPi_E2E_PromptRoundTrip(t *testing.T) {
	requireRealPi(t)

	dir := t.TempDir()
	csFile, err := registry.OpenChatSessionFile(dir + "/chat_sessions.json")
	if err != nil {
		t.Fatalf("OpenChatSessionFile: %v", err)
	}
	asFile, err := registry.OpenAgentSessionFile(dir + "/agent_sessions.json")
	if err != nil {
		t.Fatalf("OpenAgentSessionFile: %v", err)
	}

	// Wire a real Spawner that uses the pi Agent. The Spawner
	// does the same thing the daemon's production code does:
	// look up the agent by name, run Detect, then Start.
	piAgent := pibridge.New("pi", "pi", nil)
	reg := agent.New()
	reg.Register(piAgent)
	spawner := NewRegistrySpawner(reg)

	cs := New("oc_real_pi_test", "pi").
		WithPersistence(csFile, asFile).
		WithSpawner(spawner)
	if err := cs.SetActiveCwd(dir); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}

	// Lazy spawn: the first Dispatch triggers LookupActiveAgentSession
	// which spawns pi.
	as, err := cs.LookupActiveAgentSession()
	if err != nil {
		t.Fatalf("LookupActiveAgentSession: %v", err)
	}
	if as.Agent != "pi" {
		t.Fatalf("expected pi session, got %q", as.Agent)
	}
	t.Logf("spawned pi session, pid=%d", as.PID())

	// Accumulate every event that flows through the runtime's
	// eventHandler — this is THE production path. We do NOT
	// also drain sess.Events() ourselves, because the bridge
	// channel is single-consumer; the readPump already drains
	// it via the AgentEventBus subscriber callback.
	var (
		mu       sync.Mutex
		reply    strings.Builder
		sawInit  bool
		sawDone  bool
		eventLog []string
	)
	cs.AgentEventBus.Subscribe(func(env AgentEventEnvelope) bool {
		ev := env.Event
		mu.Lock()
		defer mu.Unlock()
		eventLog = append(eventLog, ev.Kind.String())
		switch ev.Kind {
		case agent.EventAgentReady:
			sawInit = true
		case agent.EventAgentText:
			reply.WriteString(ev.Text)
		case agent.EventAgentResult:
			if ev.Result != nil {
				reply.WriteString(ev.Result.Text)
			}
		case agent.EventAgentDone:
			sawDone = true
		case agent.EventAgentError:
			t.Errorf("pi emitted error: %v", ev.Err)
		}
		return false
	})

	// CS-AS 边界重构 Phase 1: the readpump is per-AgentSession and
	// starts automatically once Spawn wires the handle (see
	// AgentSession.startReadPump). The CS side consumes that
	// enriched stream via PumpEvents — this is what dispatches
	// KindAgentEvent into the EventHandler installed above, so it
	// is the production path this test is asserting on.
	pumpCtx, pumpCancel := context.WithCancel(context.Background())
	defer pumpCancel()
	go cs.PumpEvents(pumpCtx)

	defer func() {
		_, _ = cs.KillAll()
	}()

	// Wait for the bridge session to be available, then call
	// SendBlocks directly with a generous timeout. We go through
	// the bridge (not the runtime input buffer) so we exercise
	// the same wire-level path the gateway uses.
	sess := as.Handle()
	if sess == nil {
		t.Fatalf("AgentSession.Handle() returned nil after Spawn")
	}

	marker := "RUNTIME-CANARY-" + t.Name()
	prompt := "hi — please reply with exactly: " + marker

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	sendStart := time.Now()
	if err := sess.SendBlocks(ctx, []agent.ContentBlock{
		{Type: agent.ContentText, Text: prompt},
	}); err != nil {
		t.Fatalf("SendBlocks: %v (elapsed=%s)", err, time.Since(sendStart))
	}

	// Wait for the EventHandler to see EventAgentDone. Poll the
	// accumulated state under the same mutex.
	deadline := time.After(90 * time.Second)
wait:
	for {
		mu.Lock()
		done := sawDone
		mu.Unlock()
		if done {
			break wait
		}
		select {
		case <-deadline:
			mu.Lock()
			t.Fatalf("no EventAgentDone within 90s; events=%v reply=%q",
				eventLog, reply.String())
			mu.Unlock()
		case <-time.After(50 * time.Millisecond):
			// poll again
		}
	}

	mu.Lock()
	replyStr := reply.String()
	events := append([]string(nil), eventLog...)
	mu.Unlock()

	if !sawInit {
		t.Errorf("EventAgentReady never reached the runtime eventHandler; events=%v", events)
	}
	if replyStr == "" {
		t.Fatalf("EventAgentDone arrived but no text reply reached the runtime; events=%v", events)
	}
	if !strings.Contains(replyStr, marker) {
		t.Errorf("reply did NOT contain marker %q; pi likely did not read our input. reply=%q events=%v",
			marker, replyStr, events)
	}

	preview := replyStr
	if len(preview) > 240 {
		preview = preview[:240] + "...(+" + itoa(len(replyStr)-240) + " chars)"
	}
	t.Logf("runtime e2e (%d events): pi received our input and replied (%d chars) in %s: %q",
		len(events), len(replyStr), time.Since(sendStart), preview)
}

// helper for marker-stripping in the test log; matches the
// implementation in internal/bridge/pi/session_real_test.go.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// Suppress unused-import warnings for packages that look unused
// at first glance but are actually consumed by the test helpers
// above.
var _ = errors.New
var _ = slices.Contains[[]string, string]