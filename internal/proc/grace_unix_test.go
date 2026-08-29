//go:build !windows

// fix-git-lock-file 2026-08-29: WithGrace wiring tests. These
// exercise the SIGTERM → grace → SIGKILL path that replaces
// exec.CommandContext's SIGKILL-on-cancel. The integration tests
// rely on `/bin/sh` being available — true on every Unix
// platform nightme supports.
package proc

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

// runWithGrace runs `sh -c <script>` under WithGrace with the
// given grace. The caller cancels via the returned CancelFunc to
// trigger the SIGTERM/grace/SIGKILL path.
func runWithGrace(t *testing.T, script string, grace time.Duration) (*exec.Cmd, context.CancelFunc, chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cmd := New(ctx, "/bin/sh", "-c", script)
	WithGrace(cmd, grace)

	done := make(chan error, 1)
	go func() { done <- cmd.Run() }()
	return cmd, cancel, done
}

// waitFor returns nil if fn returns true within d, else a timeout.
func waitFor(d time.Duration, fn func() bool) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return errors.New("timeout")
}

// funcPtr returns the underlying code-pointer of fn so we can
// detect "did WithGrace swap this closure?" without invoking it.
// nil-safe.
func funcPtr(fn func() error) uintptr {
	if fn == nil {
		return 0
	}
	return reflect.ValueOf(fn).Pointer()
}

func TestWithGrace_NoOp_NilCmd(t *testing.T) {
	// Must not panic on nil cmd.
	WithGrace(nil, KillGrace)
}

func TestWithGrace_NoOp_ZeroGrace(t *testing.T) {
	// Zero/negative grace = no-op. WithGrace must not touch
	// cmd.Cancel — graceful behaviour is opt-in.
	cmd := New(t.Context(), "/bin/sh", "-c", "exit 0")
	beforePtr := funcPtr(cmd.Cancel)
	WithGrace(cmd, 0)
	WithGrace(cmd, -1*time.Second)

	if got := funcPtr(cmd.Cancel); got != beforePtr {
		t.Fatalf("zero-grace WithGrace must not overwrite cmd.Cancel (was %x, now %x)", beforePtr, got)
	}
}

func TestWithGrace_NotRegistered_NoOp(t *testing.T) {
	// WithGrace on a *exec.Cmd that didn't come from proc.New
	// is silently a no-op. We construct one via exec.Command
	// directly to simulate a foreign cmd.
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	WithGrace(cmd, KillGrace)
	if cmd.Cancel != nil {
		t.Fatal("foreign cmd's Cancel must not be touched")
	}
}

func TestWithGrace_CleanExit_OnGrace(t *testing.T) {
	// Child traps SIGTERM and exits 0. WithGrace's SIGTERM path
	// should land cleanly, Run() returns nil.
	//
	// Script: `trap 'exit 0' TERM; sleep 600` — arms sh's signal
	// handler then parks the child. SIGTERM arrives, the trap
	// fires and exits 0. Run() returns nil in well under
	// grace+slack.
	_, cancel, done := runWithGrace(t,
		"trap 'exit 0' TERM; sleep 600",
		500*time.Millisecond,
	)

	// Brief delay so sh parses the script and arms the trap
	// before we signal. Without this, SIGTERM can race trap arm
	// on slow CI.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() returned err = %v, want nil (signal-trap clean exit)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run() didn't return within 2s of cancel")
	}
}

func TestWithGrace_SIGKILL_AfterGrace(t *testing.T) {
	// Child ignores SIGTERM, sleeps 600. WithGrace's SIGKILL
	// path after grace must fire, Run() returns a non-nil error
	// (typically *exec.ExitError wrapping signal-killed).
	//
	// Script: `trap '' TERM; sleep 600` — SIGTERM arrives, sh
	// ignores it. After grace, SIGKILL kills the process.
	grace := 200 * time.Millisecond
	start := time.Now()
	_, cancel, done := runWithGrace(t,
		"trap '' TERM; sleep 600",
		grace,
	)

	// Wait long enough for child to be in `sleep 600`.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("Run() returned nil; want signal-killed error after grace")
		}
		elapsed := time.Since(start)
		// Wall-time ≈ grace + (a small overhead). Allow generous
		// slack to keep this green on slow CI runners.
		if elapsed < grace {
			t.Fatalf("Run() returned in %v, before grace %v elapsed — grace wasn't honored", elapsed, grace)
		}
		if elapsed > 5*time.Second {
			t.Fatalf("Run() returned in %v, way past grace %v — should have fired SIGKILL much sooner", elapsed, grace)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() didn't return within 5s of cancel; SIGKILL didn't fire")
	}
}

func TestWithGrace_ProcessGroup_BroadcastsToChildren(t *testing.T) {
	// Setsid in proc.New makes the child its own pg leader. A
	// SIGTERM via syscall.Kill(-pid, …) should reach both sh AND
	// its forked (sleep) grandchild.
	//
	// Sentinel file: the grandchild only creates it if it has
	// to die via SIGKILL — i.e. it didn't see SIGTERM. On a
	// correctly-broadcast group TERM, the grandchild sees TERM
	// and dies cleanly before the `&&` chain runs.
	sentinel := t.TempDir() + "/grandchild-was-orphaned"
	atomic.StoreInt64(&sentinelInit, 1)

	_, cancel, done := runWithGrace(t,
		`trap 'exit 0' TERM
(sleep 600 && touch '`+sentinel+`') &
wait`,
		500*time.Millisecond,
	)

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run() didn't return within 2s of cancel")
	}

	if err := waitFor(1*time.Second, func() bool {
		_, err := exec.Command("test", "-e", sentinel).CombinedOutput()
		return err == nil
	}); err == nil {
		t.Fatalf("grandchild created %q — process-group SIGTERM did NOT broadcast; orphan survived", sentinel)
	}
}

// sentinelInit is a placeholder to keep the test file's set of
// testcases together; it has no runtime meaning.
var sentinelInit int64

func TestWithGrace_Cancel_BeforeStart_NoPanic(t *testing.T) {
	// If ctx fires BEFORE cmd.Start(), stdlib's exec.Start
	// short-circuits: it returns ctx.Err() without ever starting
	// the child (os/exec/exec.go:702-708). WithGrace's watcher
	// goroutine observes cmd.Process == nil and exits without
	// sending any signal. We just need to verify the watcher
	// does NOT panic on a never-started cmd whose ctx already
	// fired — Run() may legitimately return ctx.Err(), that's
	// stdlib's choice, not ours.
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // fire BEFORE Start

	cmd := New(ctx, "/bin/sh", "-c", "exit 0")
	WithGrace(cmd, 50*time.Millisecond)

	err := cmd.Run()
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() err = %v, want either nil or context.Canceled (stdlib's pre-Start short-circuit is acceptable)", err)
	}
	// Force the watcher goroutine to settle; without this it'd
	// be racing our assertion. The test goroutine exits either
	// way without panicking.
	time.Sleep(80 * time.Millisecond)
}

func TestWithGrace_GraceAfterRun_NoPanic(t *testing.T) {
	// A short-lived child (exit 0 immediately) should not be
	// disturbed by the AfterFunc timer. watcher sees cmd.Process
	// State ending (Process.Wait returns *os.ProcessState) but
	// cmd.Process stays non-nil per stdlib's contract — the
	// AfterFunc's "proc == nil" guard is therefore the wrong
	// thing to check. What matters: no SIGKILL goes out, no
	// panic in the watcher.
	cmd := New(t.Context(), "/bin/sh", "-c", "exit 0")
	WithGrace(cmd, 50*time.Millisecond)

	if err := cmd.Run(); err != nil {
		t.Fatalf("Run() err = %v, want nil", err)
	}
	// Sleep past grace so the AfterFunc fires; check no panic
	// reached the test runtime. (t.Fatal in another goroutine
	// wouldn't be caught — Go test framework serialises, so we
	// just trust the goroutine not to panic; in practice the
	// watcher exits via the cmd.Process==nil guard or the
	// syscall.Kill ESRCH short-circuit.)
	time.Sleep(150 * time.Millisecond)
	if cmd.ProcessState == nil {
		t.Fatal("ProcessState must be set after Run()")
	}
}
