// Repro for the user-reported failure mode at the chat-session layer.
//
// User scenario:
//
//	1. Open a chat, set /cwd
//	2. Send a prompt → pi answers
//	3. /use claude             (switch active agent; pi stays alive in pool)
//	4. /new                     (resets ALL agents in cwd, including pi)
//	5. Expected: pi resets cleanly → no error
//	    Actual:  bridge reset: pi: new_session: context deadline exceeded
//
// The bridge-level repros in internal/bridge/pi/repro_new_test.go
// pass when driving the bridge directly, so the bug must be in
// the chat session's integration layer. This file tests that
// integration end-to-end against a real pi binary.
//
// Skip when pi is not on PATH. Run:
//
//	go test ./internal/chatsession -run RealPi_New -v

package chatsession

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	pibridge "github.com/cnlangzi/nightme/internal/bridge/pi"
	"github.com/cnlangzi/nightme/internal/registry"
)

// TestRealPi_NewAfterSwitch reproduces the full user-reported flow
// using the production Spawner + ChatSession stack (the same code
// path /use and /new hit in the daemon).
//
// Key difference from the bridge-level repros: this test drives
// EVERY layer:
//
//   - registrySpawner forks a real pi process
//   - ChatSession.LookupActiveAgentSession spawns it
//   - per-AS readpump drains the bridge events channel
//   - cs.PumpEvents consumes the enriched stream
//   - AgentSession.SendBlocks sends the prompt
//   - cs.NewActiveAgentSessions("/new") resets the agent
//
// If the production code path is broken, this test reproduces.
// If the test passes, the bug is environmental (the user's
// specific chat session state, model, cwd, or another live
// agent in the pool — not the production code path per se).
func TestRealPi_NewAfterSwitch(t *testing.T) {
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

	piAgent := pibridge.New("pi", "pi", nil)
	reg := agent.New()
	reg.Register(piAgent)
	// Register a fake "claude" agent so the chat session pool
	// has TWO running AgentSessions when we issue /new — matching
	// the user's actual sequence (use pi → switch to claude → /new).
	claudeAgent := newFakeAgentBuilder("claude")
	reg.Register(claudeAgent)
	spawner := NewRegistrySpawner(reg)

	cs := New("oc_real_pi_new", "pi").
		WithPersistence(csFile, asFile).
		WithSpawner(spawner)
	if err := cs.SetActiveCwd(dir); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}

	// Spawn pi
	as, err := cs.LookupActiveAgentSession()
	if err != nil {
		t.Fatalf("LookupActiveAgentSession (pi): %v", err)
	}
	if as.Agent != "pi" {
		t.Fatalf("expected pi session, got %q", as.Agent)
	}
	t.Logf("spawned pi session, pid=%d", as.PID())

	// Event observation shared state. The subscriber is the
	// production path: cs.PumpEvents reads from as.Events() and
	// routes to AgentEventBus. We poll this state instead of
	// reading as.Events() directly so we don't race with PumpEvents.
	var (
		mu       sync.Mutex
		reply    []string
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
			reply = append(reply, ev.Text)
		case agent.EventAgentResult:
			if ev.Result != nil {
				reply = append(reply, ev.Result.Text)
			}
		case agent.EventAgentDone:
			sawDone = true
		case agent.EventAgentError:
			t.Errorf("pi emitted error: %v", ev.Err)
		}
		return false
	})

	pumpCtx, pumpCancel := context.WithCancel(t.Context())
	defer pumpCancel()
	go cs.PumpEvents(pumpCtx)

	defer func() {
		_, _ = KillAllAgents(&KillCmd{CS: cs, Ctx: context.Background()})
	}()

	// Wait for handle to be ready
	sess := as.Handle()
	if sess == nil {
		t.Fatalf("AgentSession.Handle() returned nil after Spawn")
	}

	// STEP 1: drive a turn via SendBlocks (the same path the gateway uses).
	marker := "REALPI-NEW-CANARY-" + t.Name()
	prompt := "hi — please reply with exactly: " + marker

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	sendStart := time.Now()
	if err := sess.SendBlocks(ctx, []agent.ContentBlock{
		{Type: agent.ContentText, Text: prompt},
	}); err != nil {
		t.Fatalf("SendBlocks: %v (elapsed=%s)", err, time.Since(sendStart))
	}

	// Wait for EventAgentDone
waitTurn1:
	for {
		mu.Lock()
		done := sawDone
		mu.Unlock()
		if done {
			break waitTurn1
		}
		select {
		case <-ctx.Done():
			mu.Lock()
			t.Fatalf("turn 1: no EventAgentDone within 90s; events=%v", eventLog)
			mu.Unlock()
		case <-time.After(50 * time.Millisecond):
		}
	}

	mu.Lock()
	replyStr := joinReply(reply)
	mu.Unlock()

	if !sawInit {
		t.Errorf("EventAgentReady never reached the runtime eventHandler; events=%v", eventLog)
	}
	if replyStr == "" {
		t.Fatalf("turn 1: no text reply; events=%v", eventLog)
	}
	if !contains(replyStr, marker) {
		t.Errorf("turn 1: reply did NOT contain marker %q; pi likely did not read input. reply=%q events=%v",
			marker, replyStr, eventLog)
	}
	t.Logf("turn 1 OK in %s (reply=%d chars)", time.Since(sendStart), len(replyStr))

	// Reset the per-turn tracking so we can observe the post-/new
	// EventAgentReady separately.
	mu.Lock()
	sawInit = false
	sawDone = false
	eventLog = eventLog[:0]
	reply = reply[:0]
	mu.Unlock()

	// STEP 2: simulate "switch to claude" — switch the active
	// agent to "claude" so a SECOND AgentSession spawns and
	// joins the pool. This is the exact precondition the user
	// reported: both pi and claude are running, then /new is
	// invoked. Without this step, /new only matches pi.
	t.Logf("step 2: switch to claude (spawn second AgentSession)")
	if err := cs.SetActiveAgent("claude"); err != nil {
		t.Fatalf("SetActiveAgent(claude): %v", err)
	}
	claudeAS, err := cs.LookupActiveAgentSession()
	if err != nil {
		t.Fatalf("LookupActiveAgentSession (claude): %v", err)
	}
	if claudeAS.Agent != "claude" {
		t.Fatalf("expected claude session, got %q", claudeAS.Agent)
	}
	t.Logf("step 2: spawned claude session, pid=%d", claudeAS.PID())

	// Idle so the runtime can settle the active AS swap.
	idle := 2 * time.Second
	t.Logf("step 2: idle %s (let active-AS swap settle)", idle)
	time.Sleep(idle)

	// STEP 3: this is what /new invokes. With a real chat session
	// wrapping TWO bridges, this is where the user reports the
	// timeout on pi's new_session.
	t.Logf("step 3: cs.NewActiveAgentSessions \"/new\" ...")
	newStart := time.Now()
	matched, reset, results, err := cs.NewActiveAgentSessions(ctx, "")
	newElapsed := time.Since(newStart)
	t.Logf("step 3: NewActiveAgentSessions returned in %s (matched=%d reset=%d)",
		newElapsed, matched, reset)
	if err != nil {
		t.Fatalf("step 3: NewActiveAgentSessions returned error: %v", err)
	}
	if matched < 2 {
		t.Fatalf("step 3: NewActiveAgentSessions matched %d agents, expected >=2 (pi + claude)", matched)
	}
	if reset != matched {
		t.Fatalf("step 3: only %d/%d agents reset; results=%+v", reset, matched, results)
	}

	// STEP 3b: poll the Subscriber (production path) for the
	// post-/new EventAgentReady. The subscriber is the same
	// sink that the runtime's event-handler chain would use,
	// so a delivery here proves the post-/new EventAgentReady
	// reached the chat session layer.
	if !waitForReadyViaSubscriber(t, &mu, &sawInit, 10*time.Second) {
		mu.Lock()
		t.Fatalf("step 3b: no EventAgentReady reached the runtime eventHandler after /new; events seen so far=%v",
			eventLog)
		mu.Unlock()
	}
	t.Logf("step 3b OK: post-reset EventAgentReady reached runtime")

	// STEP 4: send another prompt; does the bridge still drive pi?
	mu.Lock()
	sawDone = false
	eventLog = eventLog[:0]
	reply = reply[:0]
	mu.Unlock()
	marker2 := "REALPI-NEW-CANARY-T2"
	if err := sess.SendBlocks(ctx, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "hi again — please reply with exactly: " + marker2},
	}); err != nil {
		t.Fatalf("step 4: SendBlocks returned %v", err)
	}

waitTurn2:
	for {
		mu.Lock()
		done := sawDone
		mu.Unlock()
		if done {
			break waitTurn2
		}
		select {
		case <-ctx.Done():
			mu.Lock()
			t.Fatalf("step 4: no EventAgentDone within 90s; events=%v", eventLog)
			mu.Unlock()
		case <-time.After(50 * time.Millisecond):
		}
	}
	mu.Lock()
	replyStr2 := joinReply(reply)
	mu.Unlock()
	if !contains(replyStr2, marker2) {
		t.Errorf("step 4: pi did NOT respond to second prompt after /new; reply=%q", replyStr2)
	}
	t.Logf("step 4 OK: pi responded to fresh prompt after /new (reply=%d chars)", len(replyStr2))
}

// fakeAgentBuilder is a tiny agent.Agent that pretends to
// spawn a "claude" process. It is used by TestRealPi_NewAfterSwitch
// to put a second running AgentSession in the pool so /new
// iterates >1 entries — the user's exact precondition. The
// fake never emits any events and never closes; it just sits
// in the pool as a stand-in for a real second agent.
type fakeAgentBuilder struct {
	name   string
	events chan agent.AgentEvent
}

func newFakeAgentBuilder(name string) *fakeAgentBuilder {
	return &fakeAgentBuilder{
		name:   name,
		events: make(chan agent.AgentEvent, 16),
	}
}

func (b *fakeAgentBuilder) Name() string                                        { return b.name }
func (b *fakeAgentBuilder) Mode() agent.Mode                                     { return agent.ModePTY }
func (b *fakeAgentBuilder) Command() string                                      { return "fake-" + b.name }
func (b *fakeAgentBuilder) Args() []string                                       { return nil }
func (b *fakeAgentBuilder) Env() []string                                        { return nil }
func (b *fakeAgentBuilder) Detect() error                                        { return nil }
func (b *fakeAgentBuilder) Start(_ context.Context, _ agent.StartConfig) (agent.Agent, error) {
	return b, nil
}
func (b *fakeAgentBuilder) Events() <-chan agent.AgentEvent                      { return b.events }
func (b *fakeAgentBuilder) PID() int                                              { return 99999 }
func (b *fakeAgentBuilder) SendText(_ string) error                               { return nil }
func (b *fakeAgentBuilder) SendBlocks(_ context.Context, _ []agent.ContentBlock) error { return nil }
func (b *fakeAgentBuilder) SendPermission(_ string) error                         { return nil }
func (b *fakeAgentBuilder) New(_ context.Context) error                           { return nil }
func (b *fakeAgentBuilder) Close() error {
	select {
	case <-b.events:
	default:
		close(b.events)
	}
	return nil
}

// waitForReadyViaSubscriber polls the subscriber state for a
// fresh EventAgentReady. Returns true if one was seen within the
// deadline. Polling the subscriber (rather than reading as.Events()
// directly) avoids racing with cs.PumpEvents, which is the
// single-consumer of the AS's eventQueue.
func waitForReadyViaSubscriber(t *testing.T, mu *sync.Mutex, sawInit *bool, deadline time.Duration) bool {
	t.Helper()
	deadlineCh := time.After(deadline)
	for {
		mu.Lock()
		init := *sawInit
		mu.Unlock()
		if init {
			return true
		}
		select {
		case <-deadlineCh:
			return false
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func joinReply(parts []string) string {
	var sb strings.Builder
	for _, s := range parts {
		sb.WriteString(s)
	}
	return sb.String()
}

func contains(s, sub string) bool {
	if sub == "" {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
