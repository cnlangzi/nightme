package omp

import (
	"errors"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/bridgestest"
)

// A child that dies during the boot handshake wrote the real reason
// to stderr. The start error is the only thing the chat layer can
// show, so it has to carry the tail.
func TestStart_ChildCrashCarriesStderrTail(t *testing.T) {
	bin := bridgestest.BuildFakeCLIT(t, "fake-omp")
	err := bridgestest.StartFake(t, NewStarter("omp", "omp", nil), bin, "crash")
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
	bin := bridgestest.BuildFakeCLIT(t, "fake-omp")
	err := bridgestest.StartFake(t, NewStarter("omp", "omp", nil), bin, "silent")
	if err == nil {
		t.Fatal("Start should fail when the child exits during the handshake")
	}
	if strings.Contains(err.Error(), agent.StderrTailHeader) {
		t.Errorf("Start error = %v, want no tail section for a silent child", err)
	}
}

// omp answers a rejected --resume on stderr and exits 0. The
// sentinel the chat layer switches on must survive the tail.
func TestStart_ResumeRejectionKeepsSentinel(t *testing.T) {
	bin := bridgestest.BuildFakeCLIT(t, "fake-omp")
	err := bridgestest.StartFake(t, NewStarter("omp", "omp", nil), bin, "resume")
	if err == nil {
		t.Fatal("Start should fail when the child rejects the resume")
	}
	if !errors.Is(err, agent.ErrResumeUnhealthy) {
		t.Errorf("errors.Is(err, agent.ErrResumeUnhealthy) = false, want true (err = %v)", err)
	}
	if !strings.Contains(err.Error(), `Session "fake-session" not found.`) {
		t.Errorf("Start error = %v, want the child's stderr line", err)
	}
	if !strings.Contains(err.Error(), agent.StderrTailHeader) {
		t.Errorf("Start error = %v, want the stderr tail section", err)
	}
}

// A child that answers the handshake with a rejection carries its
// own stderr into the error as well.
func TestStart_HandshakeRejectedCarriesStderrTail(t *testing.T) {
	bin := bridgestest.BuildFakeCLIT(t, "fake-omp")
	err := bridgestest.StartFake(t, NewStarter("omp", "omp", nil), bin, "rpc_reject")
	if err == nil {
		t.Fatal("Start should fail when the child rejects the handshake")
	}
	if !strings.Contains(err.Error(), "fake bridge: get_state refused") {
		t.Errorf("Start error = %v, want the child's rejection text", err)
	}
	if !strings.Contains(err.Error(), "fake bridge: rpc mode ready") {
		t.Errorf("Start error = %v, want the child's stderr line", err)
	}
	if !strings.Contains(err.Error(), agent.StderrTailHeader) {
		t.Errorf("Start error = %v, want the stderr tail section", err)
	}
}

// The reset path reports through the same error surface.
func TestReset_NewSessionRejectedCarriesStderrTail(t *testing.T) {
	bin := bridgestest.BuildFakeCLIT(t, "fake-omp")
	ag, err := bridgestest.StartFakeAgent(t, NewStarter("omp", "omp", nil), bin, "rpc")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer ag.Close()

	err = ag.New(t.Context())
	if err == nil {
		t.Fatal("New should fail when the child rejects new_session")
	}
	if !strings.Contains(err.Error(), "fake bridge: new_session refused") {
		t.Errorf("New error = %v, want the child's rejection text", err)
	}
	if !strings.Contains(err.Error(), "fake bridge: rpc mode ready") {
		t.Errorf("New error = %v, want the child's stderr line", err)
	}
	if !strings.Contains(err.Error(), agent.StderrTailHeader) {
		t.Errorf("New error = %v, want the stderr tail section", err)
	}
}
