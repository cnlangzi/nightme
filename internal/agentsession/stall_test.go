// Package agentsession — stall detection tests.
//
// A turn that stops making progress wedges the session: the
// injection path refuses new turns, the queue never flushes, the
// message card never settles. The monitor watches the in-flight
// prompt's progress clock and asks the bridge whether its control
// channel still answers. It reports; it never recovers.
package agentsession

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/cnlangzi/nightme/internal/agent"
)

// stallDriver is a bridge driver whose probe answer the test
// controls. It records the calls the daemon made, so a test can
// prove the daemon only probed.
type stallDriver struct {
	mu      sync.Mutex
	pingErr error
	pings   int
	stops   int
	blocks  int
	// onPing runs inside the probe, before it answers. It lets a
	// test make the turn move on while the probe is in flight.
	onPing func()
}

func (d *stallDriver) SendBlocks(context.Context, []agent.ContentBlock) error {
	d.mu.Lock()
	d.blocks++
	d.mu.Unlock()
	return nil
}
func (d *stallDriver) SendPermission(string) error { return nil }
func (d *stallDriver) Reset(context.Context) error { return nil }
func (d *stallDriver) Close() error                { return nil }
func (d *stallDriver) Stop(context.Context) error {
	d.mu.Lock()
	d.stops++
	d.mu.Unlock()
	return nil
}
func (d *stallDriver) Keepalive(context.Context, func(context.Context) error) error {
	return nil
}
func (d *stallDriver) Ping(context.Context) error {
	d.mu.Lock()
	d.pings++
	hook := d.onPing
	err := d.pingErr
	d.mu.Unlock()
	if hook != nil {
		hook()
	}
	return err
}

// counts returns the calls the daemon made to the bridge.
func (d *stallDriver) counts() (pings, stops, blocks int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pings, d.stops, d.blocks
}

// stalledSession returns an AS with a turn in flight whose progress
// clock is an hour old, plus the driver the daemon would probe.
//
// The monitor's timings are installed before the monitor starts, so
// the test never writes a field the monitor goroutine reads. A check
// period of an hour parks the ticker: the tests that drive
// checkStall() directly then see only the probes they asked for.
func stalledSession(t *testing.T, pingErr error, silence, check time.Duration) (*AgentSession, *stallDriver) {
	t.Helper()
	as := makeBareAgentSession(t, "claude", "/tmp")
	d := &stallDriver{pingErr: pingErr}
	as.asMu.Lock()
	as.handle = agent.NewAgent(
		agent.NewInfo("claude", agent.ModeACP, "claude", nil, nil),
		1, make(chan agent.AgentEvent, 4), d)
	as.stat = StatusRunning
	as.asMu.Unlock()
	as.isReady.Store(false)
	as.SetCurrentPrompt(&Prompt{
		ID:             "p_stalled",
		LastMessageID:  "u_stalled",
		LastProgressAt: time.Now().Add(-time.Hour),
	})
	as.stallSilence = silence
	as.stallCheckInterval = check
	as.startStallMonitor()
	t.Cleanup(as.Shutdown)
	return as, d
}

// collectNotices subscribes to the AS's event bus and returns the
// stall notices the daemon publishes.
func collectNotices(as *AgentSession) <-chan EnrichedEvent {
	notices := make(chan EnrichedEvent, 8)
	as.EventBus.Subscribe(func(e EnrichedEvent) bool {
		if e.Kind == KindAgentEvent && e.AgentEvent != nil &&
			e.AgentEvent.Kind == agent.EventAgentNotice {
			notices <- e
		}
		return false
	})
	return notices
}

// waitNotice waits for one notice and returns it.
func waitNotice(t *testing.T, notices <-chan EnrichedEvent) EnrichedEvent {
	t.Helper()
	select {
	case ev := <-notices:
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("no stall notice was published")
		return EnrichedEvent{}
	}
}

// TestStallMonitor_ProbesOnceSilenceThresholdIsCrossed — the probe
// goes out when the in-flight turn has been quiet for longer than
// the threshold, and not before.
func TestStallMonitor_ProbesOnceSilenceThresholdIsCrossed(t *testing.T) {
	as, d := stalledSession(t, errors.New("no answer"), 50*time.Millisecond, time.Hour)

	// A fresh turn is not stalled: no probe.
	as.SetCurrentPrompt(&Prompt{
		ID:             "p_fresh",
		LastMessageID:  "u_fresh",
		LastProgressAt: time.Now(),
	})
	as.checkStall()
	if pings, _, _ := d.counts(); pings != 0 {
		t.Fatalf("probes = %d on a fresh turn, want 0", pings)
	}

	// The same turn, an hour later: the threshold is crossed.
	as.SetCurrentPrompt(&Prompt{
		ID:             "p_old",
		LastMessageID:  "u_old",
		LastProgressAt: time.Now().Add(-time.Hour),
	})
	as.checkStall()
	if pings, _, _ := d.counts(); pings != 1 {
		t.Fatalf("probes = %d, want 1", pings)
	}
}

// TestStallMonitor_DoesNotProbeAgainBeforeThresholdElapses — one
// probe per silence window: the monitor does not hammer a bridge
// that already failed to answer.
func TestStallMonitor_DoesNotProbeAgainBeforeThresholdElapses(t *testing.T) {
	as, d := stalledSession(t, errors.New("no answer"), time.Hour, time.Hour)

	as.checkStall()
	as.checkStall()
	as.checkStall()
	if pings, _, _ := d.counts(); pings != 1 {
		t.Fatalf("probes = %d, want 1", pings)
	}
}

// TestStallMonitor_TickerProbesTheBridge — the monitor's own ticker
// issues the probe; the test does not drive checkStall() itself.
func TestStallMonitor_TickerProbesTheBridge(t *testing.T) {
	_, d := stalledSession(t, errors.New("no answer"), 30*time.Millisecond, 10*time.Millisecond)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if pings, _, _ := d.counts(); pings > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the monitor never probed the bridge")
}

// TestStallMonitor_OneNoticePerStallEpisode — a turn whose probe
// keeps failing is reported once, not once per probe.
func TestStallMonitor_OneNoticePerStallEpisode(t *testing.T) {
	as, d := stalledSession(t, errors.New("no answer"), 10*time.Millisecond, time.Hour)
	notices := collectNotices(as)

	as.checkStall()
	ev := waitNotice(t, notices)
	if ev.AgentEvent == nil || !strings.Contains(ev.AgentEvent.Text, "/stop") {
		t.Fatalf("the notice does not name /stop: %+v", ev.AgentEvent)
	}
	if ev.UserMsgID != "u_stalled" {
		t.Fatalf("the notice is anchored to %q, want the stalled message", ev.UserMsgID)
	}

	// A second failed probe inside the same episode stays quiet.
	time.Sleep(20 * time.Millisecond)
	as.checkStall()
	if pings, _, _ := d.counts(); pings < 2 {
		t.Fatalf("probes = %d, want at least 2", pings)
	}
	select {
	case extra := <-notices:
		t.Fatalf("a second notice was published: %+v", extra.AgentEvent)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestStallMonitor_ASecondStallPublishesANewNotice — the episode
// ends with the turn, so a later stall is reported again.
func TestStallMonitor_ASecondStallPublishesANewNotice(t *testing.T) {
	as, _ := stalledSession(t, errors.New("no answer"), 10*time.Millisecond, time.Hour)
	notices := collectNotices(as)

	as.checkStall()
	waitNotice(t, notices)

	// The turn ends; a new one stalls later.
	as.endPrompt(agent.PromptEndClean)
	as.SetCurrentPrompt(&Prompt{
		ID:             "p_second",
		LastMessageID:  "u_second",
		LastProgressAt: time.Now().Add(-time.Hour),
	})
	as.checkStall()
	ev := waitNotice(t, notices)
	if ev.AgentEvent == nil || !strings.Contains(ev.AgentEvent.Text, "/stop") {
		t.Fatalf("the second notice does not name /stop: %+v", ev.AgentEvent)
	}
}

// TestStallMonitor_AnsweredProbePublishesNothing — a bridge that
// answers is busy, not stuck. Nothing reaches the user.
func TestStallMonitor_AnsweredProbePublishesNothing(t *testing.T) {
	as, d := stalledSession(t, nil, 10*time.Millisecond, time.Hour)
	notices := collectNotices(as)

	as.checkStall()
	if pings, _, _ := d.counts(); pings != 1 {
		t.Fatalf("probes = %d, want 1", pings)
	}
	select {
	case ev := <-notices:
		t.Fatalf("an answered probe published a notice: %+v", ev.AgentEvent)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestStallMonitor_DoesNotRecoverTheTurn — the daemon reports; it
// does not interrupt the agent, end the turn, replace the bridge or
// respawn it.
func TestStallMonitor_DoesNotRecoverTheTurn(t *testing.T) {
	as, d := stalledSession(t, errors.New("no answer"), 10*time.Millisecond, time.Hour)
	notices := collectNotices(as)

	handle := as.Handle()
	as.checkStall()
	waitNotice(t, notices)

	if as.CurrentPrompt() == nil {
		t.Fatal("the stall ended the in-flight prompt")
	}
	if as.Handle() != handle {
		t.Fatal("the stall replaced the bridge handle")
	}
	_, stops, blocks := d.counts()
	if stops != 0 {
		t.Fatalf("the stall issued %d Stop calls, want 0", stops)
	}
	if blocks != 0 {
		t.Fatalf("the stall sent %d user messages, want 0", blocks)
	}
}

// TestStallMonitor_ATurnThatMovedOnIsNotStalled — the probe and the
// turn race: the agent answers the turn instead of the probe. A turn
// that ended (or made progress) while the probe was in flight is not
// a stalled one, and the user is not told to end a turn that is
// already over.
func TestStallMonitor_ATurnThatMovedOnIsNotStalled(t *testing.T) {
	as, d := stalledSession(t, errors.New("no answer"), 10*time.Millisecond, time.Hour)
	notices := collectNotices(as)

	// The turn ends while the probe is in flight.
	d.onPing = func() { as.endPrompt(agent.PromptEndClean) }
	as.checkStall()

	select {
	case ev := <-notices:
		t.Fatalf("a turn that ended during the probe was reported: %+v", ev.AgentEvent)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestStallMonitor_ATurnThatProgressedIsNotStalled — the same race,
// won by the turn: the agent's answer arrives while the probe waits.
// The turn made progress, so the probe's silence says nothing.
func TestStallMonitor_ATurnThatProgressedIsNotStalled(t *testing.T) {
	as, d := stalledSession(t, errors.New("no answer"), 10*time.Millisecond, time.Hour)
	notices := collectNotices(as)

	// The prompt's progress clock advances while the probe waits.
	d.onPing = func() {
		as.asMu.Lock()
		as.currentPrompt.LastProgressAt = time.Now()
		as.asMu.Unlock()
	}
	as.checkStall()

	select {
	case ev := <-notices:
		t.Fatalf("a turn that progressed during the probe was reported: %+v", ev.AgentEvent)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestInjectionRefusal_NamesTheWayOutWhenStalled — the refusal the
// injection path returns carries a reason the user can act on.
func TestInjectionRefusal_NamesTheWayOutWhenStalled(t *testing.T) {
	as, d := stalledSession(t, errors.New("no answer"), 10*time.Millisecond, time.Hour)
	as.checkStall()

	err := as.SendBlocks(context.Background(), nil, WithReplyTo("u_review"))
	if !errors.Is(err, ErrInjectionDuringTurn) {
		t.Fatalf("err = %v, want ErrInjectionDuringTurn", err)
	}
	var refused *InjectionRefused
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want an *InjectionRefused", err)
	}
	if !refused.Stalled {
		t.Fatal("the refusal does not report the stall")
	}
	if !strings.Contains(refused.Reason, "/stop") || !strings.Contains(refused.Reason, "/close") {
		t.Fatalf("the refusal does not name /stop and /close: %q", refused.Reason)
	}
	if _, stops, _ := d.counts(); stops != 0 {
		t.Fatal("the refusal stopped the bridge")
	}
}

// TestInjectionRefusal_BusyTurnKeepsTheOldWording — a turn that is
// merely busy is worth waiting out; the refusal names no command.
func TestInjectionRefusal_BusyTurnKeepsTheOldWording(t *testing.T) {
	as := makeBareAgentSession(t, "claude", "/tmp")
	as.SetCurrentPrompt(&Prompt{ID: "p_busy", LastMessageID: "u_busy"})
	as.SetIsReady(false)

	err := as.SendBlocks(context.Background(), nil, WithReplyTo("u_review"))
	var refused *InjectionRefused
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want an *InjectionRefused", err)
	}
	if refused.Stalled {
		t.Fatal("a busy turn is reported as stalled")
	}
	if strings.Contains(refused.Reason, "/stop") {
		t.Fatalf("the busy refusal names /stop: %q", refused.Reason)
	}
}

// TestStallText_IsEnglishAndAscii — the daemon speaks one language
// to its users, and the notice travels channels that render it
// verbatim.
func TestStallText_IsEnglishAndAscii(t *testing.T) {
	for _, text := range []string{
		stallNoticeText(47 * time.Minute),
		stallNoticeText(time.Minute),
		stallNoticeText(90 * time.Second),
		(&InjectionRefused{Reason: "a turn is still in flight"}).Error(),
	} {
		for _, r := range text {
			if r > unicode.MaxASCII {
				t.Fatalf("the text carries a non-ASCII rune %q: %s", r, text)
			}
		}
	}
}

// TestReadpump_RefreshesProgressOnlyWhileAPromptIsInstalled — the
// progress clock belongs to the turn. An event that arrives with no
// prompt installed must not make a wedged turn look alive.
func TestReadpump_RefreshesProgressOnlyWhileAPromptIsInstalled(t *testing.T) {
	as := makeBareAgentSession(t, "claude", "/tmp")
	events := make(chan agent.AgentEvent, 4)
	as.asMu.Lock()
	as.handle = agent.NewAgent(
		agent.NewInfo("claude", agent.ModeACP, "claude", nil, nil),
		1, events, &stallDriver{})
	as.stat = StatusRunning
	as.asMu.Unlock()

	p := &Prompt{
		ID:             "p_progress",
		LastMessageID:  "u_progress",
		LastProgressAt: time.Now().Add(-time.Hour),
	}
	as.SetCurrentPrompt(p)
	before := progressAt(as, p)
	as.startReadPump()
	t.Cleanup(as.Shutdown)

	events <- agent.AgentEvent{Kind: agent.EventAgentText, Text: "hello"}
	waitForProgress(t, as, p, before)

	// The turn ends; a later event belongs to no turn and must not
	// move the ended prompt's clock.
	as.endPrompt(agent.PromptEndClean)
	ended := progressAt(as, p)
	events <- agent.AgentEvent{Kind: agent.EventAgentText, Text: "again"}
	time.Sleep(100 * time.Millisecond)

	if got := progressAt(as, p); !got.Equal(ended) {
		t.Fatalf("the progress clock moved without a prompt: %s -> %s", ended, got)
	}
}

// progressAt reads the prompt's progress clock under the AS lock —
// the readpump writes it.
func progressAt(as *AgentSession, p *Prompt) time.Time {
	as.asMu.RLock()
	defer as.asMu.RUnlock()
	return p.LastProgressAt
}

// waitForProgress waits until the prompt's progress clock moved past
// the given time.
func waitForProgress(t *testing.T, as *AgentSession, p *Prompt, before time.Time) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		as.asMu.RLock()
		got := p.LastProgressAt
		as.asMu.RUnlock()
		if got.After(before) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the progress clock did not advance on an anchored event")
}
