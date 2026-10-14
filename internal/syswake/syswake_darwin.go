//go:build darwin

package syswake

// darwin wake detection via libsystem_notify (Darwin notify
// subsystem). Subscribe to
// "com.apple.system.powermanagement.systemdidwake" and pump the
// resulting file descriptor through Go's runtime poller.
//
// Dylib symbol resolution uses github.com/ebitengine/purego —
// a pure-Go alternative to CGo for calling C library functions.
// Equivalent in spirit to syscall.NewLazyDLL on Windows (which is
// used by internal/login/feishu/provider_windows.go for
// kernel32 calls not in x/sys/windows).

import (
	"fmt"
	"os"
	"sync"

	"github.com/ebitengine/purego"
)

// notifyFdFunc matches the C signature:
//
//	uint32_t notify_register_file_descriptor(const char *name,
//	                                        int *fd,
//	                                        int flags,
//	                                        uint32_t *token);
//
// We bind via purego.RegisterFunc so the symbol can be called
// like a Go func. flags is 0 — notify_register_file_descriptor
// fires on every post by default, which is exactly what a
// wake-from-sleep detector needs.
type notifyFdFunc func(name *byte, fd *int32, flags int32, token *uint32) uint32

type darwinDetector struct {
	ch        chan struct{}
	file      *os.File
	token     uint32
	pumpDone  chan struct{}
	closeOnce sync.Once
}

func newPlatformDetector(_ Config) (Detector, error) {
	lib, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_LAZY|purego.RTLD_LOCAL)
	if err != nil {
		return nil, fmt.Errorf("syswake/darwin: dlopen libSystem.B.dib: %w", err)
	}
	sym, err := purego.Dlsym(lib, "notify_register_file_descriptor")
	if err != nil {
		return nil, fmt.Errorf("syswake/darwin: dlsym notify_register_file_descriptor: %w", err)
	}
	var registerFD notifyFdFunc
	purego.RegisterFunc(&registerFD, sym)

	name, err := cString("com.apple.system.powermanagement.systemdidwake")
	if err != nil {
		return nil, fmt.Errorf("syswake/darwin: cString: %w", err)
	}

	var outFD int32
	var token uint32
	if status := registerFD(name, &outFD, 0, &token); status != 0 {
		return nil, fmt.Errorf("syswake/darwin: notify_register_file_descriptor: status=%d", status)
	}

	d := &darwinDetector{
		ch:       make(chan struct{}, 4),
		file:     os.NewFile(uintptr(outFD), "systemdidwake"),
		token:    token,
		pumpDone: make(chan struct{}),
	}
	go d.pump()
	return d, nil
}

// pump blocks on file.Read. The notify fd is pollable (kernel
// queues POLLIN on every fire); os.File.Read parks on Go's
// runtime poller until then. Read returns an error when the
// file is closed via Close(), which unblocks the pump.
func (d *darwinDetector) pump() {
	defer close(d.pumpDone)
	buf := make([]byte, 8)
	for {
		n, err := d.file.Read(buf)
		if err != nil {
			return
		}
		if n <= 0 {
			continue
		}
		select {
		case d.ch <- struct{}{}:
		default:
		}
	}
}

func (d *darwinDetector) WakeChan() <-chan struct{} { return d.ch }

func (d *darwinDetector) Close() error {
	var err error
	d.closeOnce.Do(func() {
		if d.file != nil {
			_ = d.file.Close() // unblocks pump's Read
		}
		<-d.pumpDone
		// notify_cancel is best-effort; kernel cleans up on
		// process termination regardless. Bind lazily on
		// shutdown in case dlopen previously failed.
		if lib, dlerr := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_LAZY|purego.RTLD_LOCAL); dlerr == nil {
			if sym, serr := purego.Dlsym(lib, "notify_cancel"); serr == nil {
				var cancelFn func(token uint32) uint32
				purego.RegisterFunc(&cancelFn, sym)
				_ = cancelFn(d.token)
			}
		}
	})
	return err
}

// cString returns a NUL-terminated C string. The returned
// pointer is freed via runtime.Free via a finalizer-free path:
// purego.RegisterFunc copies the bytes it needs during the
// call, so the buffer can be stack/heap-allocated and reclaimed
// after the call returns.
func cString(s string) (*byte, error) {
	buf := make([]byte, len(s)+1)
	copy(buf, s)
	return &buf[0], nil
}
