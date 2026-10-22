// daemon_startup_report_test.go — the daemon child → parent channel.
//
// Windows has no readiness pipe, so this file is the only way a
// startup failure reason reaches a Windows terminal. It also carries
// the per-channel outcomes, which the pipe has no room for. Both are
// exercised here rather than only on the platform that needs them.

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/daemoncontrol"
	"github.com/cnlangzi/nightme/internal/runtime"
)

func reportPaths(t *testing.T) daemoncontrol.Paths {
	t.Helper()
	return daemoncontrol.Paths{Dir: t.TempDir()}
}

// TestStartupReport_RoundTrip covers the whole parent-side contract
// for a daemon that came up cleanly: one line per channel, each
// successful one without noise, each failed one with its reason.
func TestStartupReport_RoundTrip(t *testing.T) {
	paths := reportPaths(t)
	w := newStartupReportWriter(startupReportPath(paths))
	w.SetReady()
	w.SetChannels(runtime.ChannelStartup{
		Total: 4,
		Results: []runtime.ChannelStatus{
			{Name: "feishu", Status: runtime.ChannelStatusConnected},
			{Name: "telegram", Status: runtime.ChannelStatusTimeout, Error: "start exceeded 5s budget"},
			{Name: "slack", Status: runtime.ChannelStatusSkipped, Error: "auth.test: invalid_auth"},
			{Name: "discord", Status: runtime.ChannelStatusConnected},
		},
	})

	var buf bytes.Buffer
	printStartupChannels(&buf, waitStartupChannels(paths, os.Getpid(), 2*time.Second))

	got := buf.String()
	for _, want := range []string{
		"channel feishu: connected",
		"channel telegram: timeout (start exceeded 5s budget)",
		"channel slack: skipped (auth.test: invalid_auth)",
		"channel discord: connected",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report output missing %q\ngot:\n%s", want, got)
		}
	}
	if lines := strings.Count(strings.TrimSpace(got), "\n") + 1; lines != 4 {
		t.Errorf("got %d lines, want one per channel:\n%s", lines, got)
	}
}

// TestStartupReport_FailureReasonReachesTheParent is the Windows gap:
// before the report existed, a child that died before readiness left
// the parent with nothing but an exit code.
func TestStartupReport_FailureReasonReachesTheParent(t *testing.T) {
	paths := reportPaths(t)
	w := newStartupReportWriter(startupReportPath(paths))
	w.SetError("nightme daemon is already running")

	if got := startupFailureReason(paths, os.Getpid()); got != "nightme daemon is already running" {
		t.Errorf("startupFailureReason = %q, want the child's recorded reason", got)
	}
	if suffix := detailSuffix(startupFailureReason(paths, os.Getpid())); suffix != ": nightme daemon is already running" {
		t.Errorf("detailSuffix = %q", suffix)
	}
}

// TestStartupReport_IgnoresAnotherProcessesReport guards the stale
// file a SIGKILLed daemon leaves behind: the parent must never
// attribute a previous run's outcome to the child it just spawned.
func TestStartupReport_IgnoresAnotherProcessesReport(t *testing.T) {
	paths := reportPaths(t)
	w := newStartupReportWriter(startupReportPath(paths))
	w.SetError("stale failure from a daemon that was killed mid-startup")

	if got, ok := readStartupReport(startupReportPath(paths), os.Getpid()+1); ok {
		t.Errorf("readStartupReport accepted a report from pid %d: %+v", os.Getpid()+1, got)
	}
	if got := startupFailureReason(paths, os.Getpid()+1); got != "" {
		t.Errorf("startupFailureReason returned %q from another process's report", got)
	}
	if got := startupFailureReason(paths, os.Getpid()); got != "stale failure from a daemon that was killed mid-startup" {
		t.Errorf("startupFailureReason for the writer's own pid = %q", got)
	}
}

// TestStartupReport_MissingFileIsNotAFailure: `nightme start` must
// behave exactly as before when the report was never written (data
// directory not writable, child killed before it got that far).
func TestStartupReport_MissingFileIsNotAFailure(t *testing.T) {
	paths := reportPaths(t)
	if _, ok := readStartupReport(startupReportPath(paths), os.Getpid()); ok {
		t.Fatal("readStartupReport accepted a report that was never written")
	}
	if got := startupFailureReason(paths, os.Getpid()); got != "" {
		t.Errorf("startupFailureReason = %q, want empty", got)
	}

	start := time.Now()
	rep := waitStartupChannels(paths, os.Getpid(), 200*time.Millisecond)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waitStartupChannels blocked for %s on a missing report", elapsed)
	}
	if len(rep.Channels) != 0 {
		t.Errorf("waitStartupChannels invented channels: %+v", rep.Channels)
	}

	var buf bytes.Buffer
	printStartupChannels(&buf, rep)
	if buf.Len() != 0 {
		t.Errorf("printStartupChannels wrote %q for an empty report", buf.String())
	}
}

// TestStartupReport_ReportIsValidJSONAfterEveryTransition: the
// parent polls the file while the child rewrites it, so a partially
// written document must never be observable.
func TestStartupReport_ReportIsValidJSONAfterEveryTransition(t *testing.T) {
	paths := reportPaths(t)
	path := startupReportPath(paths)
	w := newStartupReportWriter(path)

	// Prime with a report large enough that a non-atomic write would
	// leave a truncated document behind mid-rename.
	big := make([]runtime.ChannelStatus, 0, 200)
	for i := range 200 {
		big = append(big, runtime.ChannelStatus{
			Name:   "channel-with-a-deliberately-long-name-" + strings.Repeat("x", 40),
			Status: runtime.ChannelStatusSkipped,
			Error:  strings.Repeat("reason ", 30),
		})
		_, ok := readStartupReport(path, os.Getpid())
		if !ok {
			t.Fatalf("report unreadable while the child was rewriting it (iteration %d)", i)
		}
	}
	w.SetChannels(runtime.ChannelStartup{Total: len(big), Results: big})

	rep, ok := readStartupReport(path, os.Getpid())
	if !ok {
		t.Fatal("final report unreadable")
	}
	if len(rep.Channels) != len(big) {
		t.Fatalf("got %d channels, want %d", len(rep.Channels), len(big))
	}
	if !rep.ChannelsDone {
		t.Error("ChannelsDone = false after SetChannels")
	}
}

// TestStartupReport_LeavesNoTempFiles: the rewrite is temp + rename,
// and a data directory that accumulates .daemon-startup-*.tmp files
// on every daemon start would slowly fill the user's home.
func TestStartupReport_LeavesNoTempFiles(t *testing.T) {
	paths := reportPaths(t)
	w := newStartupReportWriter(startupReportPath(paths))
	w.SetReady()
	w.SetError("boom")
	w.SetChannels(runtime.ChannelStartup{Total: 1})

	entries, err := os.ReadDir(paths.Dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(paths.Dir, daemonStartupReportName)); err != nil {
		t.Errorf("report file missing: %v", err)
	}
}
