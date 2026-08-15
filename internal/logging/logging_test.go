package logging

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/config"
)

func TestLogger_WritesToFile(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "sub", "nightme.log")
	lg, err := New(&config.Config{Logging: config.LoggingConfig{File: logPath}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Close the log file before the test ends. On Windows, an
	// open file handle blocks the t.TempDir cleanup (test reports
	// "The process cannot access the file because it is being
	// used by another process"); on Linux, t.TempDir cleanup
	// works even on open handles. Calling Close here makes the
	// cleanup race-free on every host.
	t.Cleanup(func() { _ = Close(lg) })

	lg.Info("hello", "key", "value")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if strings.TrimSpace(string(data)) == "" {
		t.Fatal("expected log line")
	}
}

func TestLogger_FilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 0600 is a no-op on Windows; " +
			"the production contract is ACL-based on Windows, " +
			"covered by separate tests on internal/config.")
	}
	path := filepath.Join(t.TempDir(), "nightme.log")
	lg, err := New(&config.Config{Logging: config.LoggingConfig{File: path}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = Close(lg) })

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %o, want 0600", got)
	}
}

func TestLogger_Format(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nightme.log")
	lg, err := New(&config.Config{Logging: config.LoggingConfig{File: path}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = Close(lg) })

	lg.Info("formatted", "answer", 42)
	line, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(line))), &record); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if record["msg"] != "formatted" || record["answer"] != float64(42) {
		t.Errorf("unexpected record: %#v", record)
	}
}

func TestLogger_DefaultPath(t *testing.T) {
	dir := t.TempDir()
	// os.UserHomeDir() reads %USERPROFILE% on Windows, $HOME on
	// Unix. Set both so the logger's default-path resolution
	// picks up our temp dir regardless of platform.
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	lg, err := New(&config.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = Close(lg) })

	path := filepath.Join(dir, ".nightme", "nightme.log")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("default log: %v", err)
	}
}

func TestLogger_TeesToStdoutAndStderr(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "nightme.log")

	// Replace os.Stdout and os.Stderr with pipes so we can capture
	// what the MultiWriter writes to each sink. Restore on exit.
	origStdout := os.Stdout
	origStderr := os.Stderr
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stdout: %v", err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stderr: %v", err)
	}
	os.Stdout = wOut
	os.Stderr = wErr
	t.Cleanup(func() {
		os.Stdout = origStdout
		os.Stderr = origStderr
		_ = rOut.Close()
		_ = wOut.Close()
		_ = rErr.Close()
		_ = wErr.Close()
	})

	lg, err := New(&config.Config{Logging: config.LoggingConfig{File: logPath}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = Close(lg) })

	lg.Info("tee-me", "kind", "triple-sink")

	// Close the writers so the reads on the consumer side complete.
	_ = wOut.Close()
	_ = wErr.Close()

	var outBuf, errBuf bytes.Buffer
	if _, err := io.Copy(&outBuf, rOut); err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if _, err := io.Copy(&errBuf, rErr); err != nil {
		t.Fatalf("read stderr: %v", err)
	}
	stdoutContent := outBuf.String()
	stderrContent := errBuf.String()

	// All three sinks must contain the message — file for persistence,
	// stdout and stderr for the CLI surface (different consumers
	// may redirect only one of them).
	fileContent, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(fileContent), "tee-me") {
		t.Errorf("file missing log line: %q", fileContent)
	}
	if !strings.Contains(stdoutContent, "tee-me") {
		t.Errorf("stdout missing log line: %q", stdoutContent)
	}
	if !strings.Contains(stderrContent, "tee-me") {
		t.Errorf("stderr missing log line: %q", stderrContent)
	}
	// Both sinks share the JSON format the parser already validates.
	for name, content := range map[string]string{"stdout": stdoutContent, "stderr": stderrContent} {
		var record map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(content)), &record); err != nil {
			t.Fatalf("%s not valid JSON: %v: %q", name, err, content)
		}
		if record["msg"] != "tee-me" {
			t.Errorf("%s msg = %v, want tee-me", name, record["msg"])
		}
	}
}

func TestLogger_LevelParsing(t *testing.T) {
	cases := map[string]string{"": "INFO", "debug": "DEBUG", "info": "INFO", "warn": "WARN", "error": "ERROR", "TRACE": "INFO"}
	for input, want := range cases {
		if got := levelFor(input).String(); got != want {
			t.Errorf("levelFor(%q) = %s, want %s", input, got, want)
		}
	}
}

// TestNewQuiet_FileOnly is the counterpart to
// TestLogger_TeesToStdoutAndStderr: the forked daemon child must
// NOT tee its log stream to the console, because its stderr is the
// crash-capture file (cmd/nightme/daemon_stderr.go). Teeing there
// would bury the panic stack that file exists for under a full
// duplicate of nightme.log.
func TestNewQuiet_FileOnly(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "nightme.log")

	origStdout := os.Stdout
	origStderr := os.Stderr
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stdout: %v", err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stderr: %v", err)
	}
	os.Stdout = wOut
	os.Stderr = wErr
	t.Cleanup(func() {
		os.Stdout = origStdout
		os.Stderr = origStderr
		_ = rOut.Close()
		_ = rErr.Close()
	})

	lg, err := NewQuiet(&config.Config{Logging: config.LoggingConfig{File: logPath}})
	if err != nil {
		t.Fatalf("NewQuiet: %v", err)
	}
	t.Cleanup(func() { _ = Close(lg) })

	lg.Info("quiet-me", "kind", "file-only")

	_ = wOut.Close()
	_ = wErr.Close()

	var outBuf, errBuf bytes.Buffer
	if _, err := io.Copy(&outBuf, rOut); err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if _, err := io.Copy(&errBuf, rErr); err != nil {
		t.Fatalf("read stderr: %v", err)
	}

	fileContent, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(fileContent), "quiet-me") {
		t.Errorf("file missing log line: %q", fileContent)
	}
	if outBuf.Len() != 0 {
		t.Errorf("stdout got %q, want nothing (quiet logger must not tee)", outBuf.String())
	}
	if errBuf.Len() != 0 {
		t.Errorf("stderr got %q, want nothing — this is the crash-capture stream", errBuf.String())
	}
}
