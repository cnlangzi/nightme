//go:build !windows

package daemoncontrol

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"

	"github.com/cnlangzi/nightme/internal/proc"
)

var ErrLocked = errors.New("daemon control: lock is held")

type Lock struct {
	file *os.File
}

func TryLock(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("chmod lock %s: %w", path, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return &Lock{file: f}, nil
}

func LockFromFile(f *os.File) (*Lock, error) {
	if f == nil {
		return nil, errors.New("daemon control: inherited lock file is nil")
	}
	if _, err := f.Stat(); err != nil {
		return nil, fmt.Errorf("validate inherited lock: %w", err)
	}
	// The fd arrived via ExtraFiles, so forkExec cleared FD_CLOEXEC
	// on it and os.NewFile does not re-arm it. Without this, every
	// process the daemon later execs (!cmd shell, gtw hooks, agent
	// bridges) inherits the descriptor and keeps the flock alive
	// after the daemon itself exits — flock is bound to the open
	// file description, not the process. `nightme restart` run from
	// inside such a child then stops the daemon successfully but
	// can never reclaim the lock: stopDaemon spins for 15s and
	// startDaemon is never reached. Re-arm here, at the single
	// adoption point.
	//
	// Worth being explicit about the flock shape: the inherited fd
	// is a forkExec-dup of the same open file description that
	// TryLock acquired in the parent — so the LOCK_EX is on the
	// description, and both fds share it. Closing only the parent's
	// copy (Lock.CloseLocalCopy) leaves the inherited copy still
	// holding the lock, and a descendant that further inherits fd
	// 3 continues to hold it after the daemon exits. That is the
	// whole reason CLOEXEC must be armed here, before any exec.
	if err := proc.SetCloseOnExec(f); err != nil {
		return nil, fmt.Errorf("arm inherited lock: %w", err)
	}
	return &Lock{file: f}, nil
}

func (l *Lock) File() *os.File {
	if l == nil {
		return nil
	}
	return l.file
}

func (l *Lock) CloseLocalCopy() error {
	if l == nil || l.file == nil {
		return nil
	}
	f := l.file
	l.file = nil
	return f.Close()
}

func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	f := l.file
	l.file = nil
	if err := unix.Flock(int(f.Fd()), unix.LOCK_UN); err != nil {
		_ = f.Close()
		return fmt.Errorf("unlock: %w", err)
	}
	return f.Close()
}
