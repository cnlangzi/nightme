package me

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/cnlangzi/nightme/internal/wfe"
)

// writeScript writes a file into dir with the given mode.
func writeScript(t *testing.T, dir, name string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), mode); err != nil {
		t.Fatalf("write script: %v", err)
	}
}

func TestActionRegistry_RegisterAndRun(t *testing.T) {
	r := NewActionRegistry()
	a := &testActionImpl{name: "hello", result: "world"}
	r.Register(a)
	r.Register(a) // re-register same name should be idempotent
	if len(r.List()) != 1 {
		t.Errorf("List = %v, want 1 entry", r.List())
	}
	res, err := r.Run(context.Background(), wfe.ActionSpec{Name: "hello"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Outputs["r"] != "world" {
		t.Errorf("Output[r] = %v", res.Outputs["r"])
	}
}

func TestActionRegistry_UnknownAction(t *testing.T) {
	r := NewActionRegistry()
	_, err := r.Run(context.Background(), wfe.ActionSpec{Name: "nope"})
	if err == nil {
		t.Error("expected error for unknown action")
	}
}

// TestActionRegistry_BuiltinActions — the built-ins the service
// registers at startup are all reachable by name.
func TestActionRegistry_BuiltinActions(t *testing.T) {
	r := NewActionRegistry()
	registerBuiltinActions(r)
	for _, name := range []string{"notify", "email", "github_pr_comment", "github_issue"} {
		if _, ok := r.actions[name]; !ok {
			t.Errorf("action %q is not registered", name)
		}
	}
}

// TestActionRegistry_ScanUserScripts — an executable file in the
// actions dir becomes an action named after the file; a file that is
// not executable is ignored.
//
// Windows has no executable bit: every file looks like 0666 there,
// so the discovery finds nothing. The test does not apply.
func TestActionRegistry_ScanUserScripts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows has no executable bit")
	}
	dir := t.TempDir()
	writeScript(t, dir, "deploy.sh", 0o755)
	writeScript(t, dir, "notes.txt", 0o644)

	r := NewActionRegistry()
	if err := r.ScanUserScripts(dir); err != nil {
		t.Fatalf("ScanUserScripts: %v", err)
	}
	if _, ok := r.actions["deploy"]; !ok {
		t.Errorf("action %q is not registered", "deploy")
	}
	if _, ok := r.actions["notes"]; ok {
		t.Errorf("action %q is registered — the file is not executable", "notes")
	}
}

type testActionImpl struct {
	name   string
	result string
}

func (a *testActionImpl) Name() string { return a.name }
func (a *testActionImpl) Execute(_ context.Context, _ map[string]any, _ map[string]string) (*wfe.ActionResult, error) {
	return &wfe.ActionResult{Outputs: map[string]any{"r": a.result}}, nil
}

var _ Action = (*testActionImpl)(nil)
