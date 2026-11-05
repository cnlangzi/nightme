package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig drops a config.yaml with the given content into dir
// and returns its path.
func writeConfig(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestLoad_ReadsReviewAgent covers R1: the review section is part of
// the config schema and loads into Config.Review.Agent.
func TestLoad_ReadsReviewAgent(t *testing.T) {
	path := writeConfig(t, t.TempDir(), "primary: cc\nreview:\n  agent: pi\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Review.Agent != "pi" {
		t.Errorf("Review.Agent = %q, want pi", cfg.Review.Agent)
	}
}

// TestLoad_NoReviewSectionReadsEmpty covers the other half of R1: a
// file without a review section yields an empty agent name.
func TestLoad_NoReviewSectionReadsEmpty(t *testing.T) {
	path := writeConfig(t, t.TempDir(), "primary: cc\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Review.Agent != "" {
		t.Errorf("Review.Agent = %q, want empty", cfg.Review.Agent)
	}
}

// TestSave_KeepsReviewSection covers R2: a whole-file save must not
// erase the review key.
func TestSave_KeepsReviewSection(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "primary: cc\nreview:\n  agent: pi\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := Save(cfg, path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load after Save: %v", err)
	}
	if reloaded.Review.Agent != "pi" {
		t.Errorf("Review.Agent = %q after round trip, want pi", reloaded.Review.Agent)
	}
}

// TestSave_DoesNotAddEmptyReviewSection covers the other half of R2:
// a config that never set review must not grow an empty review
// block on the next save.
func TestSave_DoesNotAddEmptyReviewSection(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "primary: cc\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := Save(cfg, path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(data), "review:") {
		t.Errorf("config gained an empty review block:\n%s", data)
	}
}

// TestSetReviewAgent_CreatesFile covers R8: a missing config file is
// created with the review.agent key.
func TestSetReviewAgent_CreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")

	if err := SetReviewAgent(path, "pi"); err != nil {
		t.Fatalf("SetReviewAgent: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Review.Agent != "pi" {
		t.Errorf("Review.Agent = %q, want pi", cfg.Review.Agent)
	}
}

// TestSetReviewAgent_PreservesTheRestOfTheFile covers R6: the write
// touches one node — comments, unmodelled keys and other sections
// survive byte-for-byte.
func TestSetReviewAgent_PreservesTheRestOfTheFile(t *testing.T) {
	dir := t.TempDir()
	src := "# my nightme config\nprimary: cc\nunknown_key: keep-me\nfeishu:\n  app_id: cli_x\n"
	path := writeConfig(t, dir, src)

	if err := SetReviewAgent(path, "pi"); err != nil {
		t.Fatalf("SetReviewAgent: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(data)
	for _, want := range []string{
		"# my nightme config",
		"primary: cc",
		"unknown_key: keep-me",
		"feishu:",
		"app_id: cli_x",
		"review:",
		"agent: pi",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// Every line of the original file is still there, verbatim.
	for _, line := range strings.Split(strings.TrimSpace(src), "\n") {
		if !strings.Contains(got, line+"\n") {
			t.Errorf("line %q did not survive:\n%s", line, got)
		}
	}
}

// TestSetReviewAgent_RefusesMalformedFile covers R7: a config file
// that does not parse is reported and never overwritten.
func TestSetReviewAgent_RefusesMalformedFile(t *testing.T) {
	dir := t.TempDir()
	bad := "primary: [unclosed\n"
	path := writeConfig(t, dir, bad)

	if err := SetReviewAgent(path, "pi"); err == nil {
		t.Fatal("expected an error for a malformed config file")
	}
	data, _ := os.ReadFile(path)
	if string(data) != bad {
		t.Fatalf("malformed file was modified:\n%s", data)
	}
}

// TestSetReviewAgent_UpdatesExistingValue covers the second write to
// the same key.
func TestSetReviewAgent_UpdatesExistingValue(t *testing.T) {
	path := writeConfig(t, t.TempDir(), "primary: cc\nreview:\n  agent: pi\n")

	if err := SetReviewAgent(path, "codex"); err != nil {
		t.Fatalf("SetReviewAgent: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Review.Agent != "codex" {
		t.Errorf("Review.Agent = %q, want codex", cfg.Review.Agent)
	}
}

// TestUnsetReviewAgent_RemovesKeyAndSection covers R10: the key goes
// away, and so does the section it was the only key of.
func TestUnsetReviewAgent_RemovesKeyAndSection(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "primary: cc\nreview:\n  agent: pi\n")

	if err := UnsetReviewAgent(path); err != nil {
		t.Fatalf("UnsetReviewAgent: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(data)
	if strings.Contains(got, "agent: pi") {
		t.Errorf("review.agent survived:\n%s", got)
	}
	if strings.Contains(got, "review:") {
		t.Errorf("the empty review section survived:\n%s", got)
	}
	if !strings.Contains(got, "primary: cc") {
		t.Errorf("the rest of the file was lost:\n%s", got)
	}
}

// TestUnsetReviewAgent_KeepsOtherReviewKeys covers the edge case
// where the review section carries more than the agent key.
func TestUnsetReviewAgent_KeepsOtherReviewKeys(t *testing.T) {
	path := writeConfig(t, t.TempDir(), "review:\n  agent: pi\n  future: keep-me\n")

	if err := UnsetReviewAgent(path); err != nil {
		t.Fatalf("UnsetReviewAgent: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(data)
	if strings.Contains(got, "agent: pi") {
		t.Errorf("review.agent survived:\n%s", got)
	}
	if !strings.Contains(got, "future: keep-me") {
		t.Errorf("the other review key was dropped:\n%s", got)
	}
}

// TestUnsetReviewAgent_MissingFileCreatesNothing covers the
// "nothing is configured" case: the unset succeeds without creating
// a file.
func TestUnsetReviewAgent_MissingFileCreatesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")

	if err := UnsetReviewAgent(path); err != nil {
		t.Fatalf("UnsetReviewAgent: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected no file at %s, stat err = %v", path, err)
	}
}

// TestUnsetReviewAgent_RefusesMalformedFile covers R7 for the unset
// path.
func TestUnsetReviewAgent_RefusesMalformedFile(t *testing.T) {
	dir := t.TempDir()
	bad := "primary: [unclosed\n"
	path := writeConfig(t, dir, bad)

	if err := UnsetReviewAgent(path); err == nil {
		t.Fatal("expected an error for a malformed config file")
	}
	data, _ := os.ReadFile(path)
	if string(data) != bad {
		t.Fatalf("malformed file was modified:\n%s", data)
	}
}
