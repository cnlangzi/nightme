package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/config"
)

// cfgWithDataDir builds the minimal config the stderr-capture
// helpers read.
func cfgWithDataDir(dir string) *config.Config {
	return &config.Config{Paths: config.PathsConfig{DataDir: dir}}
}

// TestDaemonStderrPath_DefaultsUnderDataDir asserts the capture
// file lives next to the other daemon state, so `nightme logs` and
// the crash dump are found in the same directory.
func TestDaemonStderrPath_DefaultsUnderDataDir(t *testing.T) {
	t.Setenv(daemonStderrEnv, "")
	dir := t.TempDir()

	got, err := daemonStderrPath(cfgWithDataDir(dir))
	if err != nil {
		t.Fatalf("daemonStderrPath: %v", err)
	}
	want := filepath.Join(dir, daemonStderrName)
	if got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
}

// TestDaemonStderrPath_EnvOverride asserts NIGHTME_STDERR_FILE (the
// pre-existing debugging escape hatch) still wins, verbatim.
func TestDaemonStderrPath_EnvOverride(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "my-crash.log")
	t.Setenv(daemonStderrEnv, custom)

	got, err := daemonStderrPath(cfgWithDataDir(t.TempDir()))
	if err != nil {
		t.Fatalf("daemonStderrPath: %v", err)
	}
	if got != custom {
		t.Errorf("path = %q, want the env override %q", got, custom)
	}
}

// TestOpenDaemonStderr_AppendsAcrossRestarts asserts consecutive
// daemon starts accumulate rather than truncate: the crash that
// matters may be the one from two starts ago.
func TestOpenDaemonStderr_AppendsAcrossRestarts(t *testing.T) {
	t.Setenv(daemonStderrEnv, "")
	cfg := cfgWithDataDir(t.TempDir())

	for _, line := range []string{"first\n", "second\n"} {
		f, path, err := openDaemonStderr(cfg)
		if err != nil {
			t.Fatalf("openDaemonStderr: %v", err)
		}
		if _, err := f.WriteString(line); err != nil {
			t.Fatalf("write: %v", err)
		}
		f.Close()
		_ = path
	}

	path, _ := daemonStderrPath(cfg)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	if got := string(data); got != "first\nsecond\n" {
		t.Errorf("capture = %q, want both writes appended", got)
	}
}

// TestOpenDaemonStderr_CreatesMissingDataDir asserts a first-ever
// start (no DataDir yet) still gets a capture file instead of
// silently losing the crash output.
func TestOpenDaemonStderr_CreatesMissingDataDir(t *testing.T) {
	t.Setenv(daemonStderrEnv, "")
	cfg := cfgWithDataDir(filepath.Join(t.TempDir(), "does", "not", "exist"))

	f, path, err := openDaemonStderr(cfg)
	if err != nil {
		t.Fatalf("openDaemonStderr: %v", err)
	}
	defer f.Close()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("capture file not created: %v", err)
	}
}

// TestOpenDaemonStderr_RotatesOversized asserts a stack trace is
// never buried behind (or lost to) an unbounded file: one previous
// generation is kept as <name>.1 and the live file starts empty.
func TestOpenDaemonStderr_RotatesOversized(t *testing.T) {
	t.Setenv(daemonStderrEnv, "")
	dir := t.TempDir()
	cfg := cfgWithDataDir(dir)
	path := filepath.Join(dir, daemonStderrName)

	if err := os.WriteFile(path, []byte(strings.Repeat("x", daemonStderrMaxBytes+1)), 0o600); err != nil {
		t.Fatalf("seed oversized file: %v", err)
	}

	f, _, err := openDaemonStderr(cfg)
	if err != nil {
		t.Fatalf("openDaemonStderr: %v", err)
	}
	defer f.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat live file: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("live capture size = %d, want 0 after rotation", info.Size())
	}
	rotated, err := os.Stat(path + ".1")
	if err != nil {
		t.Fatalf("rotated generation missing: %v", err)
	}
	if rotated.Size() <= daemonStderrMaxBytes {
		t.Errorf("rotated size = %d, want the oversized original", rotated.Size())
	}
}

// TestOpenDaemonStderrOrDevNull_FallsBackOnError is the safety
// contract: a diagnostic aid must never break `nightme start`. When
// the capture path is unusable we fall back to the caller's discard
// handle and warn, rather than returning an error.
func TestOpenDaemonStderrOrDevNull_FallsBackOnError(t *testing.T) {
	dir := t.TempDir()
	// A regular file where a directory must be → MkdirAll fails.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	t.Setenv(daemonStderrEnv, filepath.Join(blocker, "crash.log"))

	discard, err := os.CreateTemp(dir, "discard")
	if err != nil {
		t.Fatalf("temp discard: %v", err)
	}
	defer discard.Close()
	warn, err := os.CreateTemp(dir, "warn")
	if err != nil {
		t.Fatalf("temp warn: %v", err)
	}
	defer warn.Close()

	sink, path, closer := openDaemonStderrOrDevNull(cfgWithDataDir(dir), discard, warn)
	if sink != discard {
		t.Errorf("sink = %v, want the fallback discard handle", sink)
	}
	if path != "" {
		t.Errorf("path = %q, want empty string when capture disabled (contract: callers must not interpolate /dev/null or NUL into diagnostic messages)", path)
	}
	if closer != nil {
		t.Errorf("closer must be nil so the caller's discard handle is not double-closed")
	}
	msg, err := os.ReadFile(warn.Name())
	if err != nil {
		t.Fatalf("read warn: %v", err)
	}
	if !strings.Contains(string(msg), "capture disabled") {
		t.Errorf("warning = %q, want an explanation of the disabled capture", msg)
	}
}

// TestIsDaemonChild guards the switch that decides whether the
// process logs to the console. Getting it wrong is silent and
// expensive: a daemon child that still tees its log stream to
// stderr turns the crash-capture file into a full duplicate of
// nightme.log (9 MB and growing), burying the stack trace the file
// exists for.
func TestIsDaemonChild(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want bool
	}{
		{"daemon child", []string{"nightme", daemonChildCommand, "--channel", "feishu"}, true},
		{"bare repl", []string{"nightme"}, false},
		{"user command", []string{"nightme", "list"}, false},
		{"foreground run", []string{"nightme", "run", "--channel", "echo"}, false},
		{"empty argv", nil, false},
		{"substring lookalike", []string{"nightme", "_daemonize"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDaemonChild(tc.argv); got != tc.want {
				t.Errorf("isDaemonChild(%q) = %v, want %v", tc.argv, got, tc.want)
			}
		})
	}
}

// TestChildExitDetail_NilState asserts the no-evidence case reads
// as such instead of panicking or printing "<nil>".
func TestChildExitDetail_NilState(t *testing.T) {
	if got := childExitDetail(nil); got != "exit status unknown" {
		t.Errorf("childExitDetail(nil) = %q", got)
	}
}
