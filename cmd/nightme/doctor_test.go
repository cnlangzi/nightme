package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/daemoncontrol"
)

// TestCrashCaptureSummary_SilentWhenHealthy asserts the section is
// absent in the steady state: an empty (or missing) capture file is
// the normal case, and printing an empty "CRASH CAPTURE" block on
// every doctor run would train users to ignore it.
func TestCrashCaptureSummary_SilentWhenHealthy(t *testing.T) {
	t.Setenv(daemonStderrEnv, "")
	dir := t.TempDir()
	cfg := cfgWithDataDir(dir)

	if _, ok := crashCaptureSummary(cfg, time.Time{}); ok {
		t.Errorf("summary reported for a missing capture file")
	}
	if err := os.WriteFile(filepath.Join(dir, daemonStderrName), nil, 0o600); err != nil {
		t.Fatalf("seed empty file: %v", err)
	}
	if _, ok := crashCaptureSummary(cfg, time.Time{}); ok {
		t.Errorf("summary reported for an empty capture file")
	}
}

// TestCrashCaptureSummary_ShowsPanicHeadline is the payoff: after a
// silent daemon death, `nightme doctor` must name the failure and
// where the full stack lives — without the user knowing the file
// exists.
func TestCrashCaptureSummary_ShowsPanicHeadline(t *testing.T) {
	t.Setenv(daemonStderrEnv, "")
	dir := t.TempDir()
	path := filepath.Join(dir, daemonStderrName)
	crash := "panic: runtime error: invalid memory address or nil pointer dereference\n\ngoroutine 1 [running]:\nmain.runDaemon(...)\n"
	if err := os.WriteFile(path, []byte(crash), 0o600); err != nil {
		t.Fatalf("seed crash: %v", err)
	}

	summary, ok := crashCaptureSummary(cfgWithDataDir(dir), time.Time{})
	if !ok {
		t.Fatal("no summary for a non-empty capture file")
	}
	for _, want := range []string{"CRASH CAPTURE", path, "panic: runtime error"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q:\n%s", want, summary)
		}
	}
}

// fakeDoctorRPCs answers the daemoncontrol calls doctor makes and
// counts them.
type fakeDoctorRPCs struct {
	healthCalls int
	statusCalls int
	healthErr   error
	statusErr   error
	startedAt   time.Time
}

func (f *fakeDoctorRPCs) rpcs() doctorRPCs {
	return doctorRPCs{
		health: func(string, time.Duration) (daemoncontrol.HealthPayload, error) {
			f.healthCalls++
			if f.healthErr != nil {
				return daemoncontrol.HealthPayload{}, f.healthErr
			}
			return daemoncontrol.HealthPayload{Channels: []daemoncontrol.ChannelHealth{}}, nil
		},
		status: func(string, time.Duration) (daemoncontrol.Status, error) {
			f.statusCalls++
			if f.statusErr != nil {
				return daemoncontrol.Status{}, f.statusErr
			}
			return daemoncontrol.Status{StartedAt: f.startedAt}, nil
		},
	}
}

// TestCrashCaptureTitle_DiffersPerOwner pins the three readings of
// one capture file. The capture is appended to and survives every
// restart, so "the file is not empty" says nothing about the daemon
// running now — an eleven-hour-old crash read as a fresh one sends
// the operator after a bug that stopped happening hours ago.
func TestCrashCaptureTitle_DiffersPerOwner(t *testing.T) {
	now := time.Now()
	titles := []string{
		crashCaptureTitle(now.Add(-11*time.Hour), now),
		crashCaptureTitle(now.Add(time.Minute), now),
		crashCaptureTitle(now, time.Time{}),
	}
	seen := map[string]bool{}
	for _, title := range titles {
		if seen[title] {
			t.Errorf("two capture owners share the title %q (%q)", title, titles)
		}
		seen[title] = true
	}
}

// TestRunDoctor_CaptureOwnerFollowsTheRunningDaemon covers the three
// combinations end to end: a capture older than the running daemon, a
// capture the running daemon wrote, and a daemon that is not there
// to be compared against.
func TestRunDoctor_CaptureOwnerFollowsTheRunningDaemon(t *testing.T) {
	t.Setenv(daemonStderrEnv, "")

	now := time.Now()
	cases := []struct {
		name       string
		daemonUp   bool
		startedAt  time.Time
		capturedAt time.Time
		want       string
	}{
		{
			name:       "older than the running daemon",
			daemonUp:   true,
			startedAt:  now,
			capturedAt: now.Add(-11 * time.Hour),
			want:       "LAST CRASH CAPTURE (from an earlier daemon run;",
		},
		{
			name:       "written by the running daemon",
			daemonUp:   true,
			startedAt:  now,
			capturedAt: now.Add(time.Minute),
			want:       "CRASH CAPTURE (the running daemon wrote this",
		},
		{
			name:       "daemon unreachable",
			daemonUp:   false,
			capturedAt: now,
			want:       "LAST CRASH CAPTURE (the last thing a daemon wrote to stderr)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, daemonStderrName)
			if err := os.WriteFile(path, []byte("panic: boom\n"), 0o600); err != nil {
				t.Fatalf("seed capture: %v", err)
			}
			if err := os.Chtimes(path, tc.capturedAt, tc.capturedAt); err != nil {
				t.Fatalf("set the capture mtime: %v", err)
			}

			fake := &fakeDoctorRPCs{startedAt: tc.startedAt}
			if !tc.daemonUp {
				fake.healthErr = errors.New("nightme daemon is not running")
			}
			var out strings.Builder
			err := runDoctorWith(&out, false, cfgWithDataDir(dir), daemoncontrol.Paths{}, fake.rpcs())
			if tc.daemonUp && err != nil {
				t.Fatalf("doctor returned %v, want nil", err)
			}
			if !tc.daemonUp && err == nil {
				t.Fatal("doctor returned nil, want the health RPC error")
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Errorf("doctor output missing %q:\n%s", tc.want, out.String())
			}
		})
	}
}

// TestRunDoctor_AsksForTheDaemonStartTimeOnlyWithACapture keeps the
// common path cheap: a healthy daemon with no capture must not cost
// an extra round trip.
func TestRunDoctor_AsksForTheDaemonStartTimeOnlyWithACapture(t *testing.T) {
	t.Setenv(daemonStderrEnv, "")
	dir := t.TempDir()

	fake := &fakeDoctorRPCs{startedAt: time.Now()}
	var out strings.Builder
	if err := runDoctorWith(&out, false, cfgWithDataDir(dir), daemoncontrol.Paths{}, fake.rpcs()); err != nil {
		t.Fatalf("doctor returned %v, want nil", err)
	}
	if fake.healthCalls != 1 || fake.statusCalls != 0 {
		t.Errorf("RPC calls = health %d / status %d, want 1 / 0 without a capture",
			fake.healthCalls, fake.statusCalls)
	}

	if err := os.WriteFile(filepath.Join(dir, daemonStderrName), []byte("panic: boom\n"), 0o600); err != nil {
		t.Fatalf("seed capture: %v", err)
	}
	fake = &fakeDoctorRPCs{startedAt: time.Now()}
	out.Reset()
	if err := runDoctorWith(&out, false, cfgWithDataDir(dir), daemoncontrol.Paths{}, fake.rpcs()); err != nil {
		t.Fatalf("doctor returned %v, want nil", err)
	}
	if fake.healthCalls != 1 || fake.statusCalls != 1 {
		t.Errorf("RPC calls = health %d / status %d, want 1 / 1 with a capture",
			fake.healthCalls, fake.statusCalls)
	}
}

// TestCrashHeadline_PrefersFailureBanner asserts the headline skips
// leading noise (the child may print warnings before it dies) and
// locks onto the Go failure banner.
func TestCrashHeadline_PrefersFailureBanner(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]struct{ content, want string }{
		"panic after noise": {
			content: "some warning\nanother line\nfatal error: concurrent map writes\nstack...\n",
			want:    "fatal error: concurrent map writes",
		},
		"no banner falls back to first line": {
			content: "\n\nsomething odd happened\nmore\n",
			want:    "something odd happened",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(name, " ", "_")+".log")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("seed: %v", err)
			}
			if got := crashHeadline(path); got != tc.want {
				t.Errorf("crashHeadline = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCrashHeadline_ComesFromTheLastCrash pins the headline to the
// same end of the file as the section title. The capture file is
// appended to across restarts, so it can hold several crashes: a
// headline from the first one would describe a different daemon run
// than the title, which is derived from the file's last write.
func TestCrashHeadline_ComesFromTheLastCrash(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "capture.log")
	content := "panic: first crash\ngoroutine 1 [running]:\n\npanic: second crash\ngoroutine 2 [running]:\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := crashHeadline(path); got != "panic: second crash" {
		t.Errorf("crashHeadline = %q, want the last crash", got)
	}
}

// TestRunDoctor_HeadlineMatchesTheTitledRun is the end-to-end
// version: the capture holds an old crash and the running daemon
// wrote a newer one, so the section must show the newer crash —
// the one the title claims.
func TestRunDoctor_HeadlineMatchesTheTitledRun(t *testing.T) {
	t.Setenv(daemonStderrEnv, "")
	dir := t.TempDir()
	path := filepath.Join(dir, daemonStderrName)
	content := "panic: the old crash\n\npanic: the new crash\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("seed capture: %v", err)
	}

	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatalf("set the capture mtime: %v", err)
	}
	fake := &fakeDoctorRPCs{startedAt: now.Add(-time.Minute)}
	var out strings.Builder
	if err := runDoctorWith(&out, false, cfgWithDataDir(dir), daemoncontrol.Paths{}, fake.rpcs()); err != nil {
		t.Fatalf("doctor returned %v, want nil", err)
	}
	if !strings.Contains(out.String(), "CRASH CAPTURE (the running daemon wrote this") {
		t.Errorf("doctor output missing the current-daemon title:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "the new crash") {
		t.Errorf("doctor output missing the newest crash:\n%s", out.String())
	}
	if strings.Contains(out.String(), "the old crash") {
		t.Errorf("doctor output shows the older crash:\n%s", out.String())
	}
}
