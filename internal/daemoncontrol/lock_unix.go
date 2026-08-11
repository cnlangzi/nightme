//go:build !windows

package daemoncontrol

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
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
