//go:build windows

package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/daemoncontrol"
)

// TestChildExitedError_CarriesTheChildReason covers the Windows-only
// failure path, and the reason it needs its own test: Windows has no
// readiness pipe, so when the daemon child dies before signalling
// readiness the startup report is the only thing standing between the
// operator and "exit code 7, no idea why". Before the report existed
// every such failure printed the same sentence on every Windows
// machine.
//
// Runs on the Windows CI leg only — the Unix child reports the same
// condition over its readiness pipe instead.
func TestChildExitedError_CarriesTheChildReason(t *testing.T) {
	got := childExitedError(7, "reconcile agent_sessions: pid probe failed",
		`C:\Users\test\.nightme\daemon-stderr.log`).Error()

	if !strings.Contains(got, "daemon child exited before becoming ready") {
		t.Errorf("lost the symptom: %q", got)
	}
	if !strings.Contains(got, "code=7") {
		t.Errorf("lost the exit code: %q", got)
	}
	if !strings.Contains(got, "reconcile agent_sessions: pid probe failed") {
		t.Errorf("lost the child's reason — this is the whole point of the report: %q", got)
	}
	if !strings.Contains(got, `C:\Users\test\.nightme\daemon-stderr.log`) {
		t.Errorf("lost the stderr capture path: %q", got)
	}
}

// TestChildExitedError_WithoutAReason: the child can die before it
// manages to write one (bad config path, no data directory). The
// message must still read as a sentence.
func TestChildExitedError_WithoutAReason(t *testing.T) {
	got := childExitedError(1, "", `C:\nightme\daemon-stderr.log`).Error()
	if strings.Contains(got, "code=1): ;") || strings.Contains(got, "code=1):") {
		t.Errorf("left an empty reason segment behind: %q", got)
	}
	if !strings.Contains(got, "code=1") {
		t.Errorf("lost the exit code: %q", got)
	}
}

// TestChildExitError_LiveChildDoesNotBlock is the regression guard
// for the `nightme start` hang on a slow Windows machine.
//
// The readiness poll asks "did the daemon child exit?" every time the
// named pipe is not there yet. Asking that question by WAITING for
// the child is wrong: the daemon child is long-lived, so the wait
// only ends when it dies — long after the readiness deadline the
// poll is supposed to enforce. A child that has not created its pipe
// yet is simply slow, and the poll must move on.
func TestChildExitError_LiveChildDoesNotBlock(t *testing.T) {
	exited := make(chan *os.ProcessState, 1) // nothing sent: the child is alive

	done := make(chan error, 1)
	go func() {
		done <- childExitError(exited, daemoncontrol.Paths{}, 1234, "")
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("childExitError with a live child = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("childExitError blocked on a live child")
	}
}

// TestChildExitError_ExitedChildReportsTheExit covers the other
// half: once the child is gone, the poll surfaces that instead of
// waiting out the readiness deadline.
func TestChildExitError_ExitedChildReportsTheExit(t *testing.T) {
	exited := make(chan *os.ProcessState, 1)
	exited <- &os.ProcessState{}

	err := childExitError(exited, daemoncontrol.Paths{}, 1234, "")
	if err == nil {
		t.Fatal("childExitError with an exited child = nil, want an error")
	}
}
