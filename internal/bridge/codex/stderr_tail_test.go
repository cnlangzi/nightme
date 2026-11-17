package codex

import (
	"errors"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/bridgestest"
)

// A child that dies during the handshake wrote the real reason to
// stderr. The start error is the only thing the chat layer can
// show, so it has to carry the tail.
func TestStart_ChildCrashCarriesStderrTail(t *testing.T) {
	bin := bridgestest.BuildFakeCLIT(t, "fake-codex")
	err := bridgestest.StartFake(t, NewStarter("codex", "codex", nil), bin, "crash")
	if err == nil {
		t.Fatal("Start should fail when the child dies during the handshake")
	}
	if !strings.Contains(err.Error(), "fake bridge: fatal: cannot load provider config") {
		t.Errorf("Start error = %v, want the child's stderr line", err)
	}
	if !strings.Contains(err.Error(), agent.StderrTailHeader) {
		t.Errorf("Start error = %v, want the stderr tail section", err)
	}
	if !errors.Is(err, ErrSessionClosed) {
		t.Errorf("errors.Is(err, ErrSessionClosed) = false, want true (err = %v)", err)
	}
}

// A child that wrote nothing to stderr gets no tail section — an
// empty header is noise.
func TestStart_ChildSilentOmitsStderrTail(t *testing.T) {
	bin := bridgestest.BuildFakeCLIT(t, "fake-codex")
	err := bridgestest.StartFake(t, NewStarter("codex", "codex", nil), bin, "silent")
	if err == nil {
		t.Fatal("Start should fail when the child exits during the handshake")
	}
	if strings.Contains(err.Error(), agent.StderrTailHeader) {
		t.Errorf("Start error = %v, want no tail section for a silent child", err)
	}
}
