package logging

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
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
	if os.Getenv("GOOS_OVERRIDE_WIN") == "windows" {
		t.Skip("skipping on windows")
	}
	path := filepath.Join(t.TempDir(), "nightme.log")
	if _, err := New(&config.Config{Logging: config.LoggingConfig{File: path}}); err != nil {
		t.Fatalf("New: %v", err)
	}
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
	t.Setenv("HOME", dir)
	if _, err := New(&config.Config{}); err != nil {
		t.Fatalf("New: %v", err)
	}
	path := filepath.Join(dir, ".local", "share", "nightme", "nightme.log")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("default log: %v", err)
	}
}

func TestLogger_TeesToStderr(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "nightme.log")

	// Replace os.Stderr with a pipe so we can capture what the
	// MultiWriter writes to the second sink. Restore on exit.
	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	t.Cleanup(func() {
		os.Stderr = origStderr
		_ = r.Close()
		_ = w.Close()
	})

	lg, err := New(&config.Config{Logging: config.LoggingConfig{File: logPath}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	lg.Info("tee-me", "kind", "double-sink")

	// Close the writer so the read on the consumer side completes.
	_ = w.Close()

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("read stderr: %v", err)
	}
	stderrContent := buf.String()

	// Both sinks must contain the message — file for persistence,
	// stderr for the CLI surface.
	fileContent, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(fileContent), "tee-me") {
		t.Errorf("file missing log line: %q", fileContent)
	}
	if !strings.Contains(stderrContent, "tee-me") {
		t.Errorf("stderr missing log line: %q", stderrContent)
	}
	// Both sinks share the JSON format the parser already validates.
	var record map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stderrContent)), &record); err != nil {
		t.Fatalf("stderr not valid JSON: %v: %q", err, stderrContent)
	}
	if record["msg"] != "tee-me" {
		t.Errorf("stderr msg = %v, want tee-me", record["msg"])
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
