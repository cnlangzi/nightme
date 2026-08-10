package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/config"
)

// newNameTestCmd builds a `nightme name` command wired to in-memory
// stdout/stderr buffers. Mirrors the pattern in login_test.go
// (which uses newLoginFeishuCmd + buffer setup) so the runXxx
// function can be called directly without paying cobra's full
// Execute cost.
func newNameTestCmd(t *testing.T) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	cmd := newNameCmd()
	out := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(errBuf)
	cmd.SetContext(context.Background())
	return cmd, out, errBuf
}

// TestName_Show_NoConfig: with no config file present, `nightme name`
// must still print something — the hostname fallback. It must NOT
// error out (showing the name is a read-only operation).
func TestName_Show_NoConfig(t *testing.T) {
	_ = withTempConfig(t) // points NIGHTME_CONFIG at a fresh path
	if err := os.Remove(os.Getenv("NIGHTME_CONFIG")); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove temp config: %v", err)
	}

	cmd, out, errBuf := newNameTestCmd(t)
	if err := runName(cmd, nil); err != nil {
		t.Fatalf("runName show: %v (stderr=%s)", err, errBuf.String())
	}

	got := strings.TrimSpace(out.String())
	if got == "" {
		t.Fatal("show: empty output (expected hostname fallback)")
	}
	if host, _ := os.Hostname(); host != "" && got != host {
		t.Errorf("show output = %q, want hostname %q", got, host)
	}
}

// TestName_Show_WithConfig: seeded config.yaml → show prints the
// configured value (not the hostname).
func TestName_Show_WithConfig(t *testing.T) {
	_ = withTempConfig(t)
	seed := &config.Config{Name: "laptop-living-room"}
	if err := config.Save(seed, os.Getenv("NIGHTME_CONFIG")); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	cmd, out, _ := newNameTestCmd(t)
	if err := runName(cmd, nil); err != nil {
		t.Fatalf("runName show: %v", err)
	}

	if got := strings.TrimSpace(out.String()); got != "laptop-living-room" {
		t.Errorf("show = %q, want laptop-living-room", got)
	}
}

// TestName_Set_RoundTrip: write a name, reload from disk, confirm
// persistence. This is the core "set" contract.
func TestName_Set_RoundTrip(t *testing.T) {
	path := withTempConfig(t)
	if err := config.Save(&config.Config{Primary: "claude"}, path); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	cmd, out, _ := newNameTestCmd(t)
	if err := runName(cmd, []string{"office-desktop"}); err != nil {
		t.Fatalf("runName set: %v", err)
	}

	if !strings.Contains(out.String(), `"office-desktop"`) {
		t.Errorf("stdout missing new name: %s", out.String())
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg.Name != "office-desktop" {
		t.Errorf("Name on disk = %q, want office-desktop", cfg.Name)
	}
}

// TestName_Set_RequiresExistingConfig: writing the name must fail
// when no config file exists — we do NOT silently create one,
// because that would also stamp defaults for Feishu / Agents / etc.
func TestName_Set_RequiresExistingConfig(t *testing.T) {
	_ = withTempConfig(t) // points env at a fresh path; file is absent

	cmd, _, _ := newNameTestCmd(t)
	err := runName(cmd, []string{"new-value"})
	if err == nil {
		t.Fatal("runName set on missing config: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no config file") {
		t.Errorf("error %q should mention missing config file", err.Error())
	}
}

// TestName_EmptyArgShows: an empty or whitespace-only positional
// arg must be treated as a read request, not a write-with-blank.
// The contract is "trim, then if empty behave like no-arg", which
// matches the shell idiom of `cmd ""` being shorthand for `cmd`.
//
// Covers two sub-cases:
//   - with config.yaml seeded → shows the configured name
//   - without config.yaml     → shows the hostname fallback
//
// In both cases the disk is never touched (read-only path).
func TestName_EmptyArgShows(t *testing.T) {
	cases := []string{"", "   ", "\t\n"}

	t.Run("with_config", func(t *testing.T) {
		path := withTempConfig(t)
		if err := config.Save(&config.Config{Name: "laptop-living-room"}, path); err != nil {
			t.Fatalf("seed save: %v", err)
		}

		for _, arg := range cases {
			cmd, out, _ := newNameTestCmd(t)
			if err := runName(cmd, []string{arg}); err != nil {
				t.Errorf("runName(%q): expected no error, got %v", arg, err)
				continue
			}
			if got := strings.TrimSpace(out.String()); got != "laptop-living-room" {
				t.Errorf("runName(%q): output = %q, want laptop-living-room", arg, got)
			}
		}

		// Disk must be untouched — empty arg never writes.
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		if cfg.Name != "laptop-living-room" {
			t.Errorf("disk modified despite empty arg; Name = %q, want laptop-living-room", cfg.Name)
		}
	})

	t.Run("no_config", func(t *testing.T) {
		_ = withTempConfig(t)
		// Ensure the file is absent so we exercise the missing-file path.
		if err := os.Remove(os.Getenv("NIGHTME_CONFIG")); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove temp config: %v", err)
		}

		for _, arg := range cases {
			cmd, out, _ := newNameTestCmd(t)
			if err := runName(cmd, []string{arg}); err != nil {
				t.Errorf("runName(%q) without config: expected no error, got %v", arg, err)
				continue
			}
			if strings.TrimSpace(out.String()) == "" {
				t.Errorf("runName(%q) without config: empty output (expected hostname)", arg)
			}
		}
	})
}
