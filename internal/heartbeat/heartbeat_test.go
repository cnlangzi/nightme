package heartbeat

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// mockCard implements CardRef for tests. It records every UpdateNote call.
type mockCard struct {
	mu    sync.Mutex
	notes []string
}

func (m *mockCard) UpdateNote(text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notes = append(m.notes, text)
	return nil
}

func (m *mockCard) lastNote() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.notes) == 0 {
		return ""
	}
	return m.notes[len(m.notes)-1]
}

func (m *mockCard) allNotes() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.notes))
	copy(out, m.notes)
	return out
}

// mockProbe implements ProcessProbe for tests.
type mockProbe struct {
	signalErr error
	eof       atomic.Bool
	exitCode  int
}

func (m *mockProbe) Signal(sig os.Signal) error { return m.signalErr }
func (m *mockProbe) StdoutEOF() bool            { return m.eof.Load() }
func (m *mockProbe) ExitCode() int              { return m.exitCode }

// --- Heartbeat.OnEvent tests ---

func TestOnEvent_IncrementsTickCount(t *testing.T) {
	card := &mockCard{}
	probe := &mockProbe{}
	hb := New("⏳", card, probe, 2*time.Second, 30*time.Second)

	hb.OnEvent()
	hb.OnEvent()
	hb.OnEvent()

	if got := hb.TickCount(); got != 3 {
		t.Errorf("TickCount = %d, want 3", got)
	}
}

func TestOnEvent_UpdatesLastEventTime(t *testing.T) {
	card := &mockCard{}
	probe := &mockProbe{}
	hb := New("⏳", card, probe, 2*time.Second, 30*time.Second)

	before := time.Now()
	hb.OnEvent()
	after := time.Now()

	last := hb.LastEventAt()
	if last.Before(before) || last.After(after) {
		t.Errorf("LastEventAt = %v, want in [%v, %v]", last, before, after)
	}
}

func TestOnEvent_UpdatesNoteFormat(t *testing.T) {
	card := &mockCard{}
	probe := &mockProbe{}
	hb := New("⏳", card, probe, 2*time.Second, 30*time.Second)

	hb.OnEvent()

	got := card.lastNote()
	// Expected pattern: "⏳ 1 · HH:MM:SS"
	if !strings.HasPrefix(got, "⏳ 1 · ") {
		t.Errorf("note = %q, want prefix '⏳ 1 · '", got)
	}
	if len(got) != len("⏳ 1 · HH:MM:SS") {
		t.Errorf("note = %q, wrong length", got)
	}
}

func TestOnEvent_AfterStop_NoOp(t *testing.T) {
	card := &mockCard{}
	probe := &mockProbe{}
	hb := New("⏳", card, probe, 2*time.Second, 30*time.Second)

	hb.OnEvent()
	hb.Stop()
	hb.OnEvent() // should be no-op

	if got := hb.TickCount(); got != 1 {
		t.Errorf("TickCount after Stop+OnEvent = %d, want 1", got)
	}
}

// --- Watch / idle / DEAD tests ---

func TestWatch_IdleAboveThreshold_ShowsIdle(t *testing.T) {
	card := &mockCard{}
	probe := &mockProbe{}
	// 50ms interval, 30ms idleMin → fast test
	hb := New("⏳", card, probe, 50*time.Millisecond, 30*time.Millisecond)

	hb.OnEvent()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hb.Watch(ctx)

	// Wait long enough for idle threshold + one tick
	time.Sleep(150 * time.Millisecond)
	cancel()

	notes := card.allNotes()
	// Last note should include "idle"
	if len(notes) == 0 {
		t.Fatal("no notes recorded")
	}
	last := notes[len(notes)-1]
	if !strings.Contains(last, "idle ") {
		t.Errorf("last note = %q, want to contain 'idle '", last)
	}
}

func TestWatch_IdleBelowThreshold_NoIdle(t *testing.T) {
	card := &mockCard{}
	probe := &mockProbe{}
	// 50ms interval, 1s idleMin → no idle shown
	hb := New("⏳", card, probe, 50*time.Millisecond, 1*time.Second)

	hb.OnEvent()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hb.Watch(ctx)

	time.Sleep(120 * time.Millisecond)
	cancel()

	notes := card.allNotes()
	if len(notes) == 0 {
		t.Fatal("no notes recorded")
	}
	last := notes[len(notes)-1]
	if strings.Contains(last, "idle ") {
		t.Errorf("last note = %q, should not contain 'idle ' (under threshold)", last)
	}
}

func TestWatch_ProcessExited_ReportsDead(t *testing.T) {
	card := &mockCard{}
	probe := &mockProbe{signalErr: errors.New("process not found"), exitCode: 137}
	hb := New("⏳", card, probe, 30*time.Millisecond, 30*time.Second)

	hb.OnEvent()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hb.Watch(ctx)

	// Wait for at least one tick that detects DEAD
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		last := card.lastNote()
		if strings.Contains(last, "❌") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	last := card.lastNote()
	if !strings.Contains(last, "❌ Claude Code 已退出") {
		t.Errorf("last note = %q, want '❌ Claude Code 已退出'", last)
	}
	if !strings.Contains(last, "exit code: 137") {
		t.Errorf("last note = %q, want to contain exit code", last)
	}
}

func TestWatch_StdoutEOF_ReportsDead(t *testing.T) {
	card := &mockCard{}
	probe := &mockProbe{eof: atomic.Bool{}}
	probe.eof.Store(true)
	hb := New("⏳", card, probe, 30*time.Millisecond, 30*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hb.Watch(ctx)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		last := card.lastNote()
		if strings.Contains(last, "❌") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	last := card.lastNote()
	if !strings.Contains(last, "❌ Claude Code 输出流已关闭") {
		t.Errorf("last note = %q, want '❌ Claude Code 输出流已关闭'", last)
	}
}

func TestWatch_NoEvents_NoCrash(t *testing.T) {
	card := &mockCard{}
	probe := &mockProbe{}
	hb := New("⏳", card, probe, 30*time.Millisecond, 30*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hb.Watch(ctx)

	time.Sleep(100 * time.Millisecond)
	cancel()

	// Should not panic; no notes recorded (no events ever)
	notes := card.allNotes()
	if len(notes) != 0 {
		t.Errorf("notes = %v, want empty (no events)", notes)
	}
}

// --- format.go tests ---

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{5 * time.Second, "5s"},
		{59 * time.Second, "59s"},
		{60 * time.Second, "1m0s"},
		{65 * time.Second, "1m5s"},
		{125 * time.Second, "2m5s"},
		{2 * time.Minute, "2m0s"},
		{-1 * time.Second, "0s"},
	}
	for _, tc := range cases {
		if got := formatDuration(tc.d); got != tc.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// --- OSProcessProbe integration-ish tests ---
// We don't spawn a real process in unit tests (slow + flaky). Instead we
// verify the probe's signal/EOF/exitCode forwarding logic with a hand-built
// exec.Cmd.

func TestOSProcessProbe_SignalNoProcess_ReturnsError(t *testing.T) {
	// exec.Cmd with nil Process — should return os.ErrProcessDone
	cmd := &exec.Cmd{}
	probe := NewOSProcessProbe(cmd, strings.NewReader(""))

	if err := probe.Signal(syscall.SIGUSR1); err == nil {
		t.Error("Signal on nil-process cmd should return error")
	}
}

func TestOSProcessProbe_StdoutEOF_FollowsReader(t *testing.T) {
	cmd := &exec.Cmd{}
	// Reader that returns EOF immediately
	probe := NewOSProcessProbe(cmd, strings.NewReader(""))

	// The drain goroutine needs a moment
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if probe.StdoutEOF() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("StdoutEOF never became true after reader EOF")
}

func TestOSProcessProbe_ExitCode_NoState(t *testing.T) {
	cmd := &exec.Cmd{} // ProcessState is nil
	probe := NewOSProcessProbe(cmd, strings.NewReader(""))

	if got := probe.ExitCode(); got != -1 {
		t.Errorf("ExitCode = %d, want -1 (no ProcessState)", got)
	}
}
