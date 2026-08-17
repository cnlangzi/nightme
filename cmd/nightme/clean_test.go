package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"
)

func TestCleanCommandSurface(t *testing.T) {
	root := newRootCmd()
	cmd, _, err := root.Find([]string{"clean"})
	if err != nil || cmd == nil {
		t.Fatalf("clean not registered: err=%v", err)
	}
	if !cmd.Runnable() {
		t.Fatal("clean has no RunE")
	}
	// clean is intentionally flag-free — the operator is expected
	// to know what they are asking for. Reject any drift that adds
	// flags silently.
	count := 0
	cmd.Flags().VisitAll(func(*pflag.Flag) { count++ })
	if count != 0 {
		t.Errorf("clean has %d flags; want 0 (the command is intentionally flag-free)", count)
	}
}

func TestTruncateLog_ExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nightme.log")
	if err := os.WriteFile(path, []byte("hello world\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := truncateLog(&out, path); err != nil {
		t.Fatalf("truncateLog: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Errorf("file size = %d, want 0", info.Size())
	}
	if !strings.Contains(out.String(), "truncated") {
		t.Errorf("expected 'truncated' in output, got %q", out.String())
	}
	if !strings.Contains(out.String(), "12 bytes removed") {
		t.Errorf("expected byte count in output, got %q", out.String())
	}
}

func TestTruncateLog_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nightme.log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := truncateLog(&out, path); err != nil {
		t.Fatalf("truncateLog: %v", err)
	}
	if !strings.Contains(out.String(), "already empty") {
		t.Errorf("expected 'already empty' in output, got %q", out.String())
	}
}

func TestTruncateLog_MissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.log")
	var out bytes.Buffer
	if err := truncateLog(&out, path); err != nil {
		t.Fatalf("truncateLog should treat missing file as no-op, got %v", err)
	}
	if !strings.Contains(out.String(), "missing") {
		t.Errorf("expected 'missing' in output, got %q", out.String())
	}
}

func TestRemoveInbox_Populated(t *testing.T) {
	dir := t.TempDir()
	// Mirror the real layout: each session ID is a subdir with
	// files inside.
	for _, name := range []string{"s_111", "s_222", "oc_abc"} {
		sub := filepath.Join(dir, name)
		if err := os.MkdirAll(sub, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sub, "payload.bin"), []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := removeInbox(&out, dir); err != nil {
		t.Fatalf("removeInbox: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("inbox dir has %d entries left, want 0", len(entries))
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Errorf("inbox dir should still exist: err=%v", err)
	}
	if !strings.Contains(out.String(), "removed 3 entries") {
		t.Errorf("expected 'removed 3 entries' in output, got %q", out.String())
	}
}

func TestRemoveInbox_Empty(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := removeInbox(&out, dir); err != nil {
		t.Fatalf("removeInbox: %v", err)
	}
	if !strings.Contains(out.String(), "already empty") {
		t.Errorf("expected 'already empty' in output, got %q", out.String())
	}
}

func TestRemoveInbox_Missing(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "no-such-inbox")
	var out bytes.Buffer
	if err := removeInbox(&out, missing); err != nil {
		t.Fatalf("removeInbox should treat missing dir as no-op, got %v", err)
	}
	if !strings.Contains(out.String(), "no inbox") {
		t.Errorf("expected 'no inbox' in output, got %q", out.String())
	}
}

// TestRunClean_InboxLivesInHOME is the tripwire for the long-standing
// regression where nightme clean wiped <DataDir>/inbox instead of
// $HOME/.nightme/inbox. The Feishu adapter pins the attachment
// inbox to the home directory (see feishu.defaultInboxBaseDir), so
// clean must do the same.
//
// Setup: HOME points to a temp dir, and the config explicitly sets
// data_dir to a DIFFERENT temp dir. The real inbox is created under
// HOME/.nightme/inbox. After runClean:
//
//   - the HOME-based inbox is empty,
//   - the DataDir location has no inbox created inside it
//     (i.e. nothing was wiped at the wrong location).
func TestRunClean_InboxLivesInHOME(t *testing.T) {
	home := t.TempDir()
	dataDir := t.TempDir()
	// os.UserHomeDir() reads HOME on Unix but USERPROFILE on
	// Windows — setting only HOME is silently ignored on the
	// Windows CI runner, and the test then exercises the real
	// runner's home directory instead of the temp one. HOMEDRIVE
	// + HOMEPATH are the legacy fallback that Go consults when
	// USERPROFILE is empty; setting them keeps `os.UserHomeDir`
	// deterministic on every Windows variant the runner image
	// ships. The three together are belt-and-braces; USERPROFILE
	// alone is enough on modern Windows.
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOMEDRIVE", "")
	t.Setenv("HOMEPATH", "")

	// Write a config that points data_dir at a different location.
	// This is the exact configuration that used to silent-fail:
	// cfg.Paths.DataDir != $HOME/.nightme, so the old code
	// computed /tmp/.../inbox in the wrong place and reported
	// "no inbox" while the real inbox at $HOME/.nightme/inbox
	// stayed intact.
	cfg := struct {
		Paths struct {
			DataDir string `yaml:"data_dir"`
		} `yaml:"paths"`
	}{}
	cfg.Paths.DataDir = dataDir
	out, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfgDir := filepath.Join(home, ".nightme")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), out, 0o600); err != nil {
		t.Fatal(err)
	}

	// Plant fixtures in the actual inbox location (HOME/.nightme).
	realInbox := filepath.Join(home, ".nightme", "inbox")
	for _, name := range []string{"s_111", "oc_abc"} {
		sub := filepath.Join(realInbox, name)
		if err := os.MkdirAll(sub, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sub, "payload.bin"), []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Plant a fake log too so we can prove logs were truncated.
	logPath := filepath.Join(home, ".nightme", "nightme.log")
	if err := os.WriteFile(logPath, []byte("old log content\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := runClean(&buf); err != nil {
		t.Fatalf("runClean: %v\noutput: %s", err, buf.String())
	}

	// The real inbox must be empty.
	entries, err := os.ReadDir(realInbox)
	if err != nil {
		t.Fatalf("read real inbox: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("real inbox has %d entries left, want 0: %v", len(entries), entries)
	}
	// The DataDir path must NOT have grown an inbox out of
	// nowhere — that would mean clean silently created the wrong
	// directory.
	dataDirInbox := filepath.Join(dataDir, "inbox")
	if _, err := os.Stat(dataDirInbox); !os.IsNotExist(err) {
		t.Errorf("DataDir/inbox was created/touched (err=%v); clean should never write to <DataDir>/inbox", err)
	}
	// Log file should have been truncated.
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("log size = %d, want 0", info.Size())
	}
	// Output should report the HOME-based inbox path, not the
	// DataDir-based one.
	if !strings.Contains(buf.String(), realInbox) {
		t.Errorf("output should reference %s, got: %s", realInbox, buf.String())
	}
	if strings.Contains(buf.String(), dataDirInbox) {
		t.Errorf("output should NOT reference bogus %s, got: %s", dataDirInbox, buf.String())
	}
}
