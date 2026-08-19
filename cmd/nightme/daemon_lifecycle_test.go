//go:build unix

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/cnlangzi/nightme/internal/daemoncontrol"
)

func TestRootLifecycleCommandSurface(t *testing.T) {
	root, _ := newRootCmd()
	for _, name := range []string{"start", "status", "stop", "restart", "logs"} {
		cmd, _, err := root.Find([]string{name})
		if err != nil || cmd == root || cmd.Name() != name {
			t.Fatalf("command %q not registered: cmd=%v err=%v", name, cmd, err)
		}
	}
	if cmd, _, err := root.Find([]string{daemonChildCommand}); err != nil || !cmd.Hidden {
		t.Fatalf("hidden daemon command: cmd=%v err=%v", cmd, err)
	}
	// The leading underscore is the convention that marks this as an
	// internal, non-user-facing subcommand: it can never collide with
	// a real command name, and it is recognizable as a nightme child
	// process in `ps` output. Hidden alone would not say that.
	if !strings.HasPrefix(daemonChildCommand, "_") {
		t.Errorf("daemonChildCommand = %q, want a leading underscore", daemonChildCommand)
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
