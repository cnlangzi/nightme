package codex

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/bridgestest"
)

// buildFakeCLI compiles the shared fake agent CLI and returns its
// path. The test skips when the go toolchain is not installed.
func buildFakeCLI(t *testing.T) string {
	t.Helper()
	if !bridgestest.GoToolchainAvailable() {
		t.Skip("go toolchain not on PATH; cannot build the fake CLI")
	}
	bin, err := bridgestest.BuildFakeCLI(t.TempDir(), "fake-codex", bridgestest.FakeCLISource)
	if err != nil {
		t.Fatalf("build fake CLI: %v", err)
	}
	return bin
}

// startFake spawns the fake CLI in the given mode and returns the
// Start error.
func startFake(t *testing.T, bin, mode string) error {
	t.Helper()
	s := NewStarter("codex", "codex", nil)
	s.Init(bin)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := s.Start(ctx, agent.StartConfig{
		Workspace: t.TempDir(),
		Env:       []string{"FAKE_BRIDGE_MODE=" + mode},
	})
	return err
}

// A child that dies during the handshake wrote the real reason to
// stderr. The start error is the only thing the chat layer can
// show, so it has to carry the tail.
func TestStart_ChildCrashCarriesStderrTail(t *testing.T) {
	bin := buildFakeCLI(t)
	err := startFake(t, bin, "crash")
	if err == nil {
		t.Fatal("Start should fail when the child dies during the handshake")
	}
	if !strings.Contains(err.Error(), "fake bridge: fatal: cannot load provider config") {
		t.Errorf("Start error = %v, want the child's stderr line", err)
	}
	if !strings.Contains(err.Error(), "--- stderr tail ---") {
		t.Errorf("Start error = %v, want the stderr tail section", err)
	}
	if !errors.Is(err, ErrSessionClosed) {
		t.Errorf("errors.Is(err, ErrSessionClosed) = false, want true (err = %v)", err)
	}
}

// A child that wrote nothing to stderr gets no tail section — an
// empty header is noise.
func TestStart_ChildSilentOmitsStderrTail(t *testing.T) {
	bin := buildFakeCLI(t)
	err := startFake(t, bin, "silent")
	if err == nil {
		t.Fatal("Start should fail when the child exits during the handshake")
	}
	if strings.Contains(err.Error(), "--- stderr tail ---") {
		t.Errorf("Start error = %v, want no tail section for a silent child", err)
	}
}
