//go:build !windows

package opencode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeOpencode writes a stand-in for the opencode CLI that prints
// the given JSON, and returns its path.
func fakeOpencode(t *testing.T, ws, body string) string {
	t.Helper()
	script := filepath.Join(ws, "fake-opencode")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return script
}

// TestEffectiveModel_RunsInWorkspace verifies the lookup runs the
// command in the session's workspace — the stand-in reports its
// working directory as the model's provider.
func TestEffectiveModel_RunsInWorkspace(t *testing.T) {
	ws := t.TempDir()
	script := fakeOpencode(t, ws, `printf '[{"type":"document","info":{"model":{"providerID":"%s","model":"m"}}}]' "$(basename "$PWD")"`)

	model, err := effectiveModel(context.Background(), script, ws)
	if err != nil {
		t.Fatalf("effectiveModel() error = %v", err)
	}
	if want := filepath.Base(ws) + "/m"; model != want {
		t.Errorf("effectiveModel() = %q, want %q", model, want)
	}
}

// TestEffectiveModel_NoModelConfigured verifies a configuration
// without a model resolves to no model — the bridge then leaves the
// session on opencode's own choice.
func TestEffectiveModel_NoModelConfigured(t *testing.T) {
	ws := t.TempDir()
	script := fakeOpencode(t, ws, `printf '[{"type":"document","info":{"theme":"dark"}}]'`)

	model, err := effectiveModel(context.Background(), script, ws)
	if err != nil {
		t.Fatalf("effectiveModel() error = %v", err)
	}
	if model != "" {
		t.Errorf("effectiveModel() = %q, want empty", model)
	}
}

// TestEffectiveModel_CommandFails verifies a failing command is an
// error — the bridge turns it into a soft failure — and that the
// reason the command printed comes with it.
func TestEffectiveModel_CommandFails(t *testing.T) {
	ws := t.TempDir()
	script := fakeOpencode(t, ws, `echo "no such config" >&2; exit 3`)

	_, err := effectiveModel(context.Background(), script, ws)
	if err == nil {
		t.Fatal("effectiveModel() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "no such config") {
		t.Errorf("error = %v, want the command's stderr", err)
	}
}
