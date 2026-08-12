//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCrashCaptureSummary_SilentWhenHealthy asserts the section is
// absent in the steady state: an empty (or missing) capture file is
// the normal case, and printing an empty "CRASH CAPTURE" block on
// every doctor run would train users to ignore it.
func TestCrashCaptureSummary_SilentWhenHealthy(t *testing.T) {
	t.Setenv(daemonStderrEnv, "")
	dir := t.TempDir()
	cfg := cfgWithDataDir(dir)

	if _, ok := crashCaptureSummary(cfg); ok {
		t.Errorf("summary reported for a missing capture file")
	}
	if err := os.WriteFile(filepath.Join(dir, daemonStderrName), nil, 0o600); err != nil {
		t.Fatalf("seed empty file: %v", err)
	}
	if _, ok := crashCaptureSummary(cfg); ok {
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

	summary, ok := crashCaptureSummary(cfgWithDataDir(dir))
	if !ok {
		t.Fatal("no summary for a non-empty capture file")
	}
	for _, want := range []string{"CRASH CAPTURE", path, "panic: runtime error"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q:\n%s", want, summary)
		}
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
