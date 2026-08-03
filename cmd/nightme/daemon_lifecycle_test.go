//go:build unix

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/cnlangzi/nightme/internal/daemoncontrol"
)

func TestRootLifecycleCommandSurface(t *testing.T) {
	root := newRootCmd()
	for _, name := range []string{"start", "status", "stop", "restart", "logs"} {
		cmd, _, err := root.Find([]string{name})
		if err != nil || cmd == root || cmd.Name() != name {
			t.Fatalf("command %q not registered: cmd=%v err=%v", name, cmd, err)
		}
	}
	if cmd, _, err := root.Find([]string{"_daemon"}); err != nil || !cmd.Hidden {
		t.Fatalf("hidden daemon command: cmd=%v err=%v", cmd, err)
	}
}

func TestStopIsIdempotentWhenDaemonAbsent(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "nightme-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("NIGHTME_PATHS_DATA_DIR", dir)
	cmd := newStopCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "nightme daemon stopped\n" {
		t.Fatalf("output = %q", got)
	}
}

func TestLockHandoffKeepsOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.lock")
	parent, err := daemoncontrol.TryLock(path)
	if err != nil {
		t.Fatal(err)
	}
	dupFD, err := unix.Dup(int(parent.File().Fd()))
	if err != nil {
		t.Fatal(err)
	}
	inherited, err := daemoncontrol.LockFromFile(os.NewFile(uintptr(dupFD), "inherited.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.CloseLocalCopy(); err != nil {
		t.Fatal(err)
	}
	if _, err := daemoncontrol.TryLock(path); !errors.Is(err, daemoncontrol.ErrLocked) {
		t.Fatalf("TryLock during handoff = %v, want ErrLocked", err)
	}
	if err := inherited.Close(); err != nil {
		t.Fatal(err)
	}
}
