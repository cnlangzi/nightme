//go:build windows

package daemoncontrol

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// Lock mirrors lock_unix.go's API using Windows LockFileEx
// semantics. See lock_unix.go for the rationale and the
// per-method contracts (TryLock / LockFromFile / File /
// CloseLocalCopy / Close).
//
// Windows / Unix difference worth flagging up front:
//
//   - Unix flock locks the open-file-description (all fds
//     pointing at the same inode share the lock). The Unix
//     LockFromFile + CloseLocalCopy pattern survives fork+exec
//     because the child inherits the fd and the lock state.
//   - Windows LockFileEx locks the HANDLE. Closing the handle
//     releases the lock. There is no equivalent of "inherited
//     fd retains the lock after the parent's copy closes".
//
// The single-instance semantics we care about ("at most one
// nightme daemon per data dir") work identically on both —
// TryLock fails fast on a second opener. The CloseLocalCopy
// semantics only matter for the daemon_lifecycle fd-inheritance
// flow, which is still unix-only; on Windows the local copy is
// always the sole owner.

var ErrLocked = errors.New("daemon control: lock is held")

// lockRange covers a single byte at offset 0. LockFileEx requires
// a (length, offset) pair; we lock byte 0 so any other opener's
// TryLock conflicts immediately.
const lockLength = 1

type Lock struct {
	file *os.File
	owns bool // true if TryLock acquired the lock; false if LockFromFile inherited an fd (no LockFileEx state to release)
}

func TryLock(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}

	handle := windows.Handle(f.Fd())
	var overlapped windows.Overlapped

	err = windows.LockFileEx(
		handle,
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, lockLength, 0,
		&overlapped,
	)
	if err != nil {
		_ = f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}

	return &Lock{file: f, owns: true}, nil
}

// LockFromFile validates an inherited file handle. The caller is
// responsible for having arranged the lock (Windows doesn't
// support lock inheritance across CreateProcess the way Unix
// supports fd inheritance across fork), so owns=false — Close
// skips UnlockFileEx.
func LockFromFile(f *os.File) (*Lock, error) {
	if f == nil {
		return nil, errors.New("daemon control: inherited lock file is nil")
	}
	if _, err := f.Stat(); err != nil {
		return nil, fmt.Errorf("validate inherited lock: %w", err)
	}
	return &Lock{file: f, owns: false}, nil
}

func (l *Lock) File() *os.File {
	if l == nil {
		return nil
	}
	return l.file
}

// CloseLocalCopy closes the file handle without unlocking. On
// Windows the handle IS the lock, so this releases the lock
// immediately. The method exists for symmetry with the Unix
// API so callers that pattern-match on File() / Close() can
// run unchanged on both platforms.
func (l *Lock) CloseLocalCopy() error {
	if l == nil || l.file == nil {
		return nil
	}
	f := l.file
	l.file = nil
	return f.Close()
}

// Close releases the LockFileEx range (if owned by this Lock)
// and then closes the underlying file handle.
func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	f := l.file
	l.file = nil

	if l.owns {
		var overlapped windows.Overlapped
		if err := windows.UnlockFileEx(windows.Handle(f.Fd()), 0, lockLength, 0, &overlapped); err != nil {
			_ = f.Close()
			return fmt.Errorf("unlock: %w", err)
		}
	}
	return f.Close()
}
