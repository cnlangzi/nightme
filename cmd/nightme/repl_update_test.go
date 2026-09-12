package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/version"
)

// --- stub fixtures ---------------------------------------

// stubLookupForChecker returns a version.LatestTagLookup that
// always returns the supplied tag. The call counter lets
// tests assert on how many times the network seam fired
// without spinning up an httptest server.
func stubLookupForChecker(tag string, calls *atomic.Int32) version.LatestTagLookup {
	return func(_ context.Context, _ string) (string, string, error) {
		if calls != nil {
			calls.Add(1)
		}
		return tag, "nightme.dev", nil
	}
}

// stubCheckerWithTag builds a version.Checker wired to a stub
// Lookup.
func stubCheckerWithTag(tag string) (*version.Checker, *atomic.Int32) {
	var calls atomic.Int32
	c := &version.Checker{
		Lookup:      stubLookupForChecker(tag, &calls),
		HTTPTimeout: 200 * time.Millisecond,
		CacheTTL:    24 * time.Hour,
		Now:         func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) },
	}
	return c, &calls
}

// --- prompt tests ----------------------------------------

// TestPrompt_OutdatedYes exercises the new flow: "y" at the
// Update prompt triggers a download attempt (which will fail
// against the test environment, but the transcript shape up
// through that point is what we pin here).
func TestPrompt_OutdatedYes(t *testing.T) {
	var out bytes.Buffer

	idx := 0
	replies := []struct {
		line string
		err  error
	}{
		{"y\n", nil},
	}
	err := promptForUpdateIfOutdated(context.Background(), &PromptDeps{
		VersionCheck: &version.CheckResult{Latest: "v9.9.9", Outdated: true},
		Out:          &out,
		Reader: func() (string, error) {
			r := replies[idx]
			idx++
			return r.line, r.err
		},
	})
	if err != nil {
		t.Fatalf("promptForUpdateIfOutdated: %v", err)
	}

	got := out.String()
	for _, want := range []string{
		"Update available",
		"9.9.9",
		"Update now?",
		"download failed",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n--- full output ---\n%s", want, got)
		}
	}
	if idx != 1 {
		t.Errorf("Reader called %d times, want 1", idx)
	}
}

// TestPrompt_DeclineInstallKeepsStaging covers the up-to-date
// silent path. When Outdated is false, no prompt fires.
func TestPrompt_DeclineInstallKeepsStaging(t *testing.T) {
	checker, _ := stubCheckerWithTag(version.Version)

	var out bytes.Buffer
	calls := 0
	err := promptForUpdateIfOutdated(context.Background(), &PromptDeps{
		Checker: checker,
		Out:     &out,
		Reader:  func() (string, error) { calls++; return "y\n", nil },
	})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if calls != 0 {
		t.Errorf("Reader called %d times on up-to-date path; want 0", calls)
	}
	if out.Len() != 0 {
		t.Errorf("expected silent output on up-to-date path; got:\n%s", out.String())
	}
}

// TestPrompt_OutdatedNo covers "user says n at the first
// prompt".
func TestPrompt_OutdatedNo(t *testing.T) {
	checker, _ := stubCheckerWithTag("v9.9.9")
	var out bytes.Buffer

	err := promptForUpdateIfOutdated(context.Background(), &PromptDeps{
		Checker: checker,
		Out:     &out,
		Reader:  func() (string, error) { return "n\n", nil },
	})
	if err != nil {
		t.Fatalf("promptForUpdateIfOutdated: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "Update now?") {
		t.Errorf("output missing prompt:\n%s", got)
	}
	if strings.Contains(got, "Install now?") {
		t.Errorf("should not reach Install prompt after declining Update:\n%s", got)
	}
	if strings.Contains(got, "go install") {
		t.Errorf("did not expect install instructions on 'n':\n%s", got)
	}
}

// TestPrompt_OutdatedEnterOnly treats a bare newline as "no".
func TestPrompt_OutdatedEnterOnly(t *testing.T) {
	checker, _ := stubCheckerWithTag("v9.9.9")
	var out bytes.Buffer

	err := promptForUpdateIfOutdated(context.Background(), &PromptDeps{
		Checker: checker,
		Out:     &out,
		Reader:  func() (string, error) { return "\n", nil },
	})
	if err != nil {
		t.Fatalf("promptForUpdateIfOutdated: %v", err)
	}
	if strings.Contains(out.String(), "Install now?") {
		t.Errorf("empty reply should be treated as 'n', got:\n%s", out.String())
	}
}

// TestPrompt_UpToDateIsSilent verifies the no-prompt path.
func TestPrompt_UpToDateIsSilent(t *testing.T) {
	checker, _ := stubCheckerWithTag("v0.0.1")
	var out bytes.Buffer

	err := promptForUpdateIfOutdated(context.Background(), &PromptDeps{
		Checker: checker,
		Out:     &out,
		Reader:  func() (string, error) { return "y\n", nil },
	})
	if err != nil {
		t.Fatalf("promptForUpdateIfOutdated: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("expected no output when up-to-date, got:\n%s", out.String())
	}
}

// TestPrompt_NetworkFailureIsSilent covers the lookup-down
// case.
func TestPrompt_NetworkFailureIsSilent(t *testing.T) {
	checker := &version.Checker{
		Lookup: func(_ context.Context, _ string) (string, string, error) {
			return "", "", errors.New("network down")
		},
		Now: func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) },
	}
	var out bytes.Buffer

	err := promptForUpdateIfOutdated(context.Background(), &PromptDeps{
		Checker: checker,
		Out:     &out,
		Reader:  func() (string, error) { return "y\n", nil },
	})
	if err != nil {
		t.Fatalf("promptForUpdateIfOutdated: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("expected silent on network failure, got:\n%s", out.String())
	}
}

// TestPrompt_NoReaderIsSilent covers runREPLWith's nil
// Reader path.
func TestPrompt_NoReaderIsSilent(t *testing.T) {
	checker, _ := stubCheckerWithTag("v9.9.9")
	var out bytes.Buffer

	err := promptForUpdateIfOutdated(context.Background(), &PromptDeps{
		Checker: checker,
		Out:     &out,
		// Reader deliberately nil.
	})
	if err != nil {
		t.Fatalf("promptForUpdateIfOutdated: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("expected silent when Reader is nil, got:\n%s", out.String())
	}
}

// TestPrompt_EOFIsTreatedAsNo.
func TestPrompt_EOFIsTreatedAsNo(t *testing.T) {
	checker, _ := stubCheckerWithTag("v9.9.9")
	var out bytes.Buffer

	err := promptForUpdateIfOutdated(context.Background(), &PromptDeps{
		Checker: checker,
		Out:     &out,
		Reader: func() (string, error) {
			return "", io.EOF
		},
	})
	if err != nil {
		t.Fatalf("promptForUpdateIfOutdated: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "Run `nightme update`") {
		t.Errorf("expected EOF hint, got:\n%s", got)
	}
	if strings.Contains(got, "go install") {
		t.Errorf("EOF should not trigger install instructions:\n%s", got)
	}
}

// TestPrompt_ReadErrorIsNonFatal covers a non-EOF read error.
func TestPrompt_ReadErrorIsNonFatal(t *testing.T) {
	checker, _ := stubCheckerWithTag("v9.9.9")
	var out bytes.Buffer

	err := promptForUpdateIfOutdated(context.Background(), &PromptDeps{
		Checker: checker,
		Out:     &out,
		Reader: func() (string, error) {
			return "", errors.New("synthetic read failure")
		},
	})
	if err != nil {
		t.Fatalf("promptForUpdateIfOutdated: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "read error") {
		t.Errorf("expected 'read error' note, got:\n%s", got)
	}
	if strings.Contains(got, "go install") {
		t.Errorf("read error should not trigger install instructions:\n%s", got)
	}
}

// TestPrompt_InvalidAnswerThenNoRePrompt guards against
// the "user mistyped ? then we ask again" trap.
func TestPrompt_InvalidAnswerThenNoRePrompt(t *testing.T) {
	checker, _ := stubCheckerWithTag("v9.9.9")
	var out bytes.Buffer
	calls := 0

	err := promptForUpdateIfOutdated(context.Background(), &PromptDeps{
		Checker: checker,
		Out:     &out,
		Reader: func() (string, error) {
			calls++
			return "???\n", nil
		},
	})
	if err != nil {
		t.Fatalf("promptForUpdateIfOutdated: %v", err)
	}
	if calls != 1 {
		t.Errorf("Reader called %d times, want exactly 1", calls)
	}
	if strings.Contains(out.String(), "go install") {
		t.Errorf("invalid answer should not print install instructions:\n%s", out.String())
	}
}

// TestPrompt_VersionCheckDrivesPrompt covers production:
// runREPLInteractive runs the countdown Check, then passes
// VersionCheck in so promptForUpdateIfOutdated does not hit
// the network again.
func TestPrompt_VersionCheckDrivesPrompt(t *testing.T) {
	var out bytes.Buffer
	calls := 0
	err := promptForUpdateIfOutdated(context.Background(), &PromptDeps{
		VersionCheck: &version.CheckResult{Latest: "v9.9.9", Outdated: true},
		Out:          &out,
		Reader: func() (string, error) {
			calls++
			return "n\n", nil
		},
	})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if calls != 1 {
		t.Errorf("Reader called %d times, want 1", calls)
	}
	got := out.String()
	if !strings.Contains(got, "Update available") {
		t.Errorf("missing availability line:\n%s", got)
	}
	if !strings.Contains(got, "9.9.9") {
		t.Errorf("missing latest version:\n%s", got)
	}
	if !strings.Contains(got, "Update now?") {
		t.Errorf("missing Update prompt:\n%s", got)
	}
	if strings.Contains(got, "Install now?") {
		t.Errorf("declining Update should not reach Install:\n%s", got)
	}
}

// TestPrompt_LookupFiresOnce verifies the stage-1 lookup is
// invoked exactly once even when the user declines the
// install prompt.
func TestPrompt_LookupFiresOnce(t *testing.T) {
	checker, calls := stubCheckerWithTag("v9.9.9")
	var out bytes.Buffer

	err := promptForUpdateIfOutdated(context.Background(), &PromptDeps{
		Checker: checker,
		Out:     &out,
		Reader:  func() (string, error) { return "n\n", nil },
	})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("Lookup fired %d times; want exactly 1", got)
	}
}

// --- countdown tests -------------------------------------

func TestWaitCheckCountdown_InstantResult(t *testing.T) {
	ch := make(chan version.CheckResult, 1)
	ch <- version.CheckResult{Latest: "1.2.3", Outdated: true}

	var out bytes.Buffer
	got := waitCheckCountdown(context.Background(), &out, 5, time.Hour, ch)
	if got.Latest != "1.2.3" {
		t.Errorf("Latest = %q, want 1.2.3", got.Latest)
	}
	s := out.String()
	if !strings.Contains(s, "Checking for updates... 5s") {
		t.Errorf("missing countdown paint:\n%q", s)
	}
}

func TestWaitCheckCountdown_TimeoutSkips(t *testing.T) {
	ch := make(chan version.CheckResult) // never sent
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	var out bytes.Buffer
	got := waitCheckCountdown(ctx, &out, 5, time.Millisecond, ch)
	if got.Latest != "" || got.Outdated {
		t.Errorf("timeout should skip, got %+v", got)
	}
	if !strings.Contains(out.String(), "Checking for updates...") {
		t.Errorf("expected countdown text, got %q", out.String())
	}
}

// --- runREPL integration ---------------------------------

func TestRunREPLWith_NoVersionChatter(t *testing.T) {
	root, reg := newTestRoot()
	var buf bytes.Buffer
	captureREPLIO(root, &buf)
	if err := runREPLWith(root, reg, nil, strings.NewReader(""), &buf); err != nil {
		t.Fatalf("runREPLWith: %v", err)
	}
	if strings.Contains(buf.String(), "Update now?") {
		t.Errorf("runREPLWith must not prompt for update (no reader wired):\n%s", buf.String())
	}
}

// --- CLI flag surface -------------------------------------

func TestUpdate_AllInOneFlags(t *testing.T) {
	root, _ := newTestRoot()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"update", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("update --help: %v", err)
	}
	got := buf.String()
	for _, want := range []string{
		"--tag",
		"--quiet", "-q",
		"--no-install",
		"--no-restart",
		"--yes", "-y",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("update --help missing %q\n%s", want, got)
		}
	}
	for _, mustNot := range []string{"update check", "update download", "update install"} {
		if strings.Contains(got, mustNot) {
			t.Errorf("update --help still mentions subcommand surface %q\n%s", mustNot, got)
		}
	}
}

// --- CLI integration --------------------------------------

// TestUpdate_AllInOneRefusesEmptyDataDir covers the safety
// property: if config.Paths.DataDir is empty, the update
// fails closed.
func TestUpdate_AllInOneRefusesEmptyDataDir(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	root, _ := newTestRoot()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"update", "--tag", "v9.9.9"})
	err := root.Execute()
	if err == nil {
		t.Fatal("update with no config succeeded; want error")
	}
}

// TestUpdate_HelpLongIsSingleVerb pins the user-visible
// shape: --help must NOT list subcommands.
func TestUpdate_HelpLongIsSingleVerb(t *testing.T) {
	root, _ := newTestRoot()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"update", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("update --help: %v", err)
	}
	got := buf.String()
	for _, want := range []string{"check", "download", "install"} {
		if !strings.Contains(got, want) {
			t.Errorf("update --help should describe the three stages, missing %q\n%s", want, got)
		}
	}
	if strings.Contains(got, "Subcommands:") {
		t.Errorf("update --help has Subcommands: header (parent got kids attached):\n%s", got)
	}
}
