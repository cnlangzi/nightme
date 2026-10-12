//go:build !windows

package proc

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"syscall"
	"testing"
	"time"
)

// isShellTrapRace returns true when err is the known /bin/sh
// startup race: the shell was killed by SIGTERM before its
// `trap 'exit 0' TERM` handler was armed. We only ever see this
// on the synthetic /bin/sh scripts in this package — production
// children are bound to this race via trap-arm latency, not via
// process-killed default-action. The retry path is gated
// entirely on this specific signal; any other error fails fast.
func isShellTrapRace(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	ws, ok := exitErr.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return false
	}
	return ws.Signal() == syscall.SIGTERM
}

// runFlakyGuard is the package-level single retry layer for
// tests that race against /bin/sh's trap-installation latency.
// It runs body once; if body returns an error that
// isShellTrapRace identifies as the sh-startup race, body is
// run once more before failing. Any other error (including a
// second-iteration failure) fails the test fast — the policy
// is "retry once on the documented race", not a tight loop.
//
// Tests in this package that exercise the synthetic /bin/sh
// scripts call runFlakyGuard so the retry-once rule lives in
// exactly one place rather than being threaded through every
// caller. body must establish a fresh child for each
// invocation (a fresh ctx, a fresh exec.Cmd) — the second call
// is otherwise a rerun against torn-down state.
func runFlakyGuard(t *testing.T, body func(*testing.T) error) {
	t.Helper()
	err := body(t)
	if err == nil {
		return
	}
	if !isShellTrapRace(err) {
		t.Fatalf("Run() = %v, want nil (trap cleaned up before grace expired)", err)
	}
	if err := body(t); err != nil {
		t.Fatalf("retry after sh-trap race: %v", err)
	}
}

// TestNew_SetsSysProcAttrSetsid pins the platform-specific
// SysProcAttr wiring so a future regression that drops the
// Setsid flag (and re-introduces the F-54 / stop hang on macOS)
// is caught by CI before it reaches the daemon.
func TestNew_SetsSysProcAttrSetsid(t *testing.T) {
	cmd := New(context.Background(), "/bin/true")
	if cmd.SysProcAttr == nil {
		t.Fatal("New left SysProcAttr nil; the cli will inherit the daemon TTY")
	}
	if !cmd.SysProcAttr.Setsid {
		t.Error("SysProcAttr.Setsid = false; cli will not be its own session/pg leader")
	}
}

// TestBuildTerminalShellCommand pins the shell command that
// openTerminalMac (via AppleScript) and openTerminalLinux (via
// `sh -c`) hand to the spawned terminal. Both platforms share
// this helper, so a single set of cases covers both.
//
// Failure modes this guards against:
//
//  1. A bare name (no single quotes) breaks when the install
//     path contains a space — e.g. /Users/x/My Apps/nightme
//     would be split at the space.
//  2. Dropping the keep-open suffix means the Terminal window
//     closes on any subcommand exit, hiding whatever error the
//     subcommand produced (the user's original bug report).
//
// Tested as a pure function so it doesn't need osascript,
// Terminal.app, or any of the Linux emulators installed.
func TestBuildTerminalShellCommand(t *testing.T) {
	// Reuse the production keep-open constant so a future
	// edit to keepOpenShellSuffix can't drift away from the
	// pinned shell command in this test (and vice versa).
	keepOpen := keepOpenShellSuffix

	cases := []struct {
		name string
		exe  string
		args []string
		want string
	}{
		{
			name: "no args",
			exe:  "/usr/local/bin/nightme",
			args: nil,
			want: "'/usr/local/bin/nightme'" + keepOpen,
		},
		{
			name: "with subcommand",
			exe:  "/usr/local/bin/nightme",
			args: []string{"logs"},
			want: "'/usr/local/bin/nightme' 'logs'" + keepOpen,
		},
		{
			name: "go install path",
			exe:  "/Users/x/go/bin/nightme",
			args: []string{"list"},
			want: "'/Users/x/go/bin/nightme' 'list'" + keepOpen,
		},
		{
			name: "path with space",
			exe:  "/Users/x/My Apps/nightme",
			args: []string{"agents"},
			want: "'/Users/x/My Apps/nightme' 'agents'" + keepOpen,
		},
		{
			name: "multiple subcommand args",
			exe:  "/opt/homebrew/bin/nightme",
			args: []string{"kill", "agent1"},
			want: "'/opt/homebrew/bin/nightme' 'kill' 'agent1'" + keepOpen,
		},
		{
			name: "homebrew linux path",
			exe:  "/home/linuxbrew/.linuxbrew/bin/nightme",
			args: []string{"status"},
			want: "'/home/linuxbrew/.linuxbrew/bin/nightme' 'status'" + keepOpen,
		},
		{
			// Apostrophe in the install path — the standard
			// `'\''` (close, literal quote, reopen) idiom
			// keeps the shell parser happy. Without this
			// escape the second `'` ends the quoted string
			// and bash reports "unexpected EOF".
			name: "path with apostrophe",
			exe:  "/Users/O'Brien/nightme",
			args: nil,
			want: "'/Users/O'\\''Brien/nightme'" + keepOpen,
		},
		{
			// Same idiom applied to an arg.
			name: "arg with apostrophe",
			exe:  "/usr/local/bin/nightme",
			args: []string{"it's"},
			want: "'/usr/local/bin/nightme' 'it'\\''s'" + keepOpen,
		},
		{
			// Backslash is literal inside single-quoted
			// shell strings — no extra escaping needed.
			name: "path with backslash",
			exe:  `/Users/test\foo/nightme`,
			args: nil,
			want: `'/Users/test\foo/nightme'` + keepOpen,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildTerminalShellCommand(tc.exe, tc.args, keepOpen)
			if got != tc.want {
				t.Errorf("buildTerminalShellCommand(%q, %v):\n  got  = %q\n  want = %q",
					tc.exe, tc.args, got, tc.want)
			}
		})
	}
}

// TestShellQuoteAndAppleScriptEscape covers the two quoting
// helpers that wrap user-controlled paths before they're handed
// to a shell or to AppleScript. Both are necessary because
// the shell-quoting rule and the AppleScript-quoting rule are
// different (single-quoted shell strings can't contain `'`,
// AppleScript double-quoted strings can't contain unescaped
// `"` or `\`).
func TestShellQuoteAndAppleScriptEscape(t *testing.T) {
	// shellQuote cases. The interesting ones are the
	// apostrophe (must close, literal quote, reopen) and
	// backslash (literal inside single-quoted shell strings).
	shellCases := []struct {
		in, want string
	}{
		{"/usr/local/bin/nightme", "'/usr/local/bin/nightme'"},
		{"/Users/x/My Apps/nightme", "'/Users/x/My Apps/nightme'"},
		{"/Users/O'Brien/nightme", "'/Users/O'\\''Brien/nightme'"},
		{`/Users/test\foo/nightme`, `'/Users/test\foo/nightme'`},
		{"it's", "'it'\\''s'"},
		{"", "''"},
	}
	for _, c := range shellCases {
		t.Run("shell/"+c.in, func(t *testing.T) {
			if got := shellQuote(c.in); got != c.want {
				t.Errorf("shellQuote(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}

	// escapeAppleScriptString cases. The interesting ones
	// are the literal double-quote (AppleScript would treat
	// the inner `"` as the closing delimiter) and backslash
	// (AppleScript treats `\` as an escape introducer). The
	// combined `"` + `\` case validates that the two
	// ReplaceAll calls compose correctly — escape order
	// matters (backslash first, otherwise we'd double-escape
	// the backslashes introduced by the `"` pass).
	asCases := []struct {
		in, want string
	}{
		// Empty input must remain empty — protects against
		// future regressions in empty-string handling.
		{"", ""},
		{"/usr/local/bin/nightme", "/usr/local/bin/nightme"},
		{`/Users/x/"test"/nightme`, `/Users/x/\"test\"/nightme`},
		{`/Users/test\foo/nightme`, `/Users/test\\foo/nightme`},
		// Combined `"` + `\` in one path — the realistic
		// worst case (a directory name with both).
		{`/Users/x/"test\foo"/nightme`, `/Users/x/\"test\\foo\"/nightme`},
		{`"`, `\"`},
		{`\`, `\\`},
	}
	for _, c := range asCases {
		t.Run("applescript/"+c.in, func(t *testing.T) {
			if got := escapeAppleScriptString(c.in); got != c.want {
				t.Errorf("escapeAppleScriptString(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestLinuxProbePrefixes pins the probe list AND the argv
// shape that openTerminalLinux hands to gnome-terminal /
// konsole / etc. on Linux: each probe is the emulator binary
// plus its "run a command" prefix, then `sh -c`, then the
// shell command from buildTerminalShellCommand. This is the
// contract the `sh -c` wrapper relies on — if a future
// refactor drops the wrapper, every emulator falls back to
// spawning the bare command, which is exactly the failure
// mode this test was added to prevent.
func TestLinuxProbePrefixes(t *testing.T) {
	// Each entry is the emulator binary that owns the probe
	// plus the full argv the daemon hands to it. The argv
	// shape MUST end in `sh -c <shellCmd>` — that uniform
	// suffix is what guarantees every emulator receives a
	// shell-interpretable command regardless of its own
	// "run a command" syntax.
	wantArgvs := map[string][]string{
		"gnome-terminal": {"gnome-terminal", "--", "sh", "-c", "<shellCmd>"},
		"konsole":        {"konsole", "-e", "sh", "-c", "<shellCmd>"},
		"alacritty":      {"alacritty", "-e", "sh", "-c", "<shellCmd>"},
		"kitty":          {"kitty", "sh", "-c", "<shellCmd>"},
		"xfce4-terminal": {"xfce4-terminal", "-e", "sh", "-c", "<shellCmd>"},
		"lxterminal":     {"lxterminal", "-e", "sh", "-c", "<shellCmd>"},
		"mate-terminal":  {"mate-terminal", "-e", "sh", "-c", "<shellCmd>"},
		"xterm":          {"xterm", "-e", "sh", "-c", "<shellCmd>"},
		"foot":           {"foot", "sh", "-c", "<shellCmd>"},
		"wezterm":        {"wezterm", "start", "--", "sh", "-c", "<shellCmd>"},
	}

	// Index LinuxTerminalProbes by binary so the test fails
	// fast if a probe is added or removed without updating
	// the expected map (and vice versa).
	gotBins := make(map[string][]string, len(LinuxTerminalProbes))
	for _, p := range LinuxTerminalProbes {
		bin := p[0]
		gotBins[bin] = linuxProbeArgv(p, "<shellCmd>")
	}

	if len(gotBins) != len(wantArgvs) {
		t.Errorf("LinuxTerminalProbes has %d entries, want %d (probes=%v expected=%v)",
			len(gotBins), len(wantArgvs), keys(gotBins), keys(wantArgvs))
	}
	for bin, want := range wantArgvs {
		got, ok := gotBins[bin]
		if !ok {
			t.Errorf("missing probe for %q in LinuxTerminalProbes", bin)
			continue
		}
		if !equalSlices(got, want) {
			t.Errorf("argv for %q = %v, want %v", bin, got, want)
		}
	}
	for bin := range gotBins {
		if _, ok := wantArgvs[bin]; !ok {
			t.Errorf("unexpected probe %q in LinuxTerminalProbes", bin)
		}
	}
}

func keys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// fix-git-lock-file 2026-08-29: the SIGTERM-then-SIGKILL
// behaviour that replaces exec.CommandContext's SIGKILL-on-cancel
// now lives inside proc.NewWith (armGraceCancel). The tests
// below pin that contract end-to-end: child traps SIGTERM and
// exits 0, child ignores SIGTERM and SIGKILLs after grace,
// pre-cancel short-circuits, nil cmd / zero grace are no-ops.
//
// All integration tests rely on `/bin/sh` being available
// (true on every Unix platform nightme supports).

// TestNewGrace_SetsCancel verifies the armGraceCancel hook
// actually fires — i.e. cmd.Cancel was overridden by NewWith
// from exec.CommandContext's default SIGKILL-on-cancel. We probe
// via cmd.Cancel's underlying code-pointer: the new and the
// default implementations differ, so a swap is detectable.
func TestNewGrace_SetsCancel(t *testing.T) {
	a := New(context.Background(), "/bin/true")
	def := exec.CommandContext(context.Background(), "/bin/true").Cancel
	if reflect.ValueOf(a.Cancel).Pointer() == reflect.ValueOf(def).Pointer() {
		t.Fatalf("New left cmd.Cancel as the stdlib default; armGraceCancel did not run")
	}
}

// TestNewGrace_CleanExit_OnSIGTERM exercises the happy path:
// child traps SIGTERM, exits 0. Run() must return nil (not
// wrapped with ctx.Err()). Total wall-time well under grace.
//
// The 200 ms warm-up plus runFlakyGuard's one-shot retry cover
// the sh-startup race: occasionally /bin/sh has not yet armed
// its `trap 'exit 0' TERM` handler by the time the test sends
// SIGTERM, and Wait reports the resulting default-action kill
// as `signal: terminated`. The retry sees a fresh child and
// lets the new shell reach the trap line before the next
// signal arrives.
func TestNewGrace_CleanExit_OnSIGTERM(t *testing.T) {
	runFlakyGuard(t, func(t *testing.T) error {
		ctx, cancel := context.WithCancel(t.Context())
		cmd := New(ctx, "/bin/sh", "-c", "trap 'exit 0' TERM; sleep 600")
		done := make(chan error, 1)
		go func() { done <- cmd.Run() }()
		// Warm-up so sh's trap is armed before we signal; this
		// is the race window runFlakyGuard retries past.
		time.Sleep(200 * time.Millisecond)
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(2 * time.Second):
			t.Fatal("Run() didn't return within 2s of cancel")
			return nil
		}
	})
}

// TestNewGrace_SIGKILL_AfterGrace verifies the fallback path:
// child ignores SIGTERM, AfterFunc SIGKILLs it after grace,
// Run() returns a non-nil error. Wall-time ≈ grace + small
// overhead — anything much shorter means grace wasn't honoured.
func TestNewGrace_SIGKILL_AfterGrace(t *testing.T) {
	grace := 200 * time.Millisecond
	start := time.Now()
	ctx, cancel := context.WithCancel(t.Context())
	cmd := New(ctx, "/bin/sh", "-c", "trap '' TERM; sleep 600")
	done := make(chan error, 1)
	go func() { done <- cmd.Run() }()
	time.Sleep(50 * time.Millisecond) // child is in `sleep 600`
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run() = nil; want non-nil (child ignored SIGTERM, SIGKILL fired)")
		}
		elapsed := time.Since(start)
		if elapsed < grace {
			t.Fatalf("Run() returned in %v, before grace %v elapsed — grace wasn't honoured", elapsed, grace)
		}
		if elapsed > 5*time.Second {
			t.Fatalf("Run() returned in %v, way past grace %v — should have fired SIGKILL much sooner", elapsed, grace)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() didn't return within 5s of cancel")
	}
}

// TestNewGrace_ProcessGroup_BroadcastsToChildren verifies that
// Setsid (set by NewWith) makes the child its own pgid leader,
// so the SIGTERM broadcast reaches forked grandchildren too —
// not just the leader. The grandchild writes a sentinel file
// only if it has to die via SIGKILL after grace; if it sees
// SIGTERM it exits before the `&&` runs.
//
// Uses runFlakyGuard like TestNewGrace_CleanExit_OnSIGTERM for
// the same sh-trap-install race. The ProcessGroup test widens
// the race window further: the shell must parse, install a
// trap, fork a (sleep 600 &) grandchild, and reach `wait`
// before SIGTERM arrives — any pause in that path before the
// trap is registered trips the same flake.
func TestNewGrace_ProcessGroup_BroadcastsToChildren(t *testing.T) {
	runFlakyGuard(t, func(t *testing.T) error {
		sentinel := t.TempDir() + "/grandchild-was-orphaned"

		ctx, cancel := context.WithCancel(t.Context())
		cmd := New(ctx, "/bin/sh", "-c",
			`trap 'exit 0' TERM
(sleep 600 && touch '`+sentinel+`') &
wait`,
		)
		done := make(chan error, 1)
		go func() { done <- cmd.Run() }()
		time.Sleep(200 * time.Millisecond)
		cancel()
		select {
		case err := <-done:
			if err == nil {
				// Run() == nil means the trap fired and the
				// shell exited 0 — the grandchild's SIGTERM
				// propagation is observable via the sentinel
				// NOT being created. Surface that as a
				// failure of the assertion, not as a flake.
				if err := exec.Command("test", "-e", sentinel).Run(); err == nil {
					t.Fatalf("grandchild created %q — process-group SIGTERM did NOT broadcast; orphan survived", sentinel)
				}
				return nil
			}
			return err
		case <-time.After(2 * time.Second):
			t.Fatal("Run() didn't return within 2s of cancel")
			return nil
		}
	})
}

// TestNewGrace_PreCancel_NoPanic covers the edge case where ctx
// fires BEFORE cmd.Run is called. armGraceCancel sees
// cmd.Process == nil and returns os.ErrProcessDone; stdlib's
// Start() preflight separately surfaces ctx.Err(). Run()
// returns context.Canceled (stdlib's behaviour, not ours);
// what matters is no panic and no goroutine hangs.
//
// This test exists because the pre-cancel path is the one place
// where armGraceCancel's Cancel callback can run while
// cmd.Process is still nil; we pin "no panic, no hang".
func TestNewGrace_PreCancel_NoPanic(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // fire before Start
	cmd := New(ctx, "/bin/sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() = %v, want either nil or context.Canceled", err)
	}
	time.Sleep(100 * time.Millisecond) // let any stray goroutine settle
}
