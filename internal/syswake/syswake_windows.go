//go:build windows

package syswake

// Windows wake detection via WM_POWERBROADCAST. A hidden
// top-level window receives PBT_APMRESUMEAUTOMATIC whenever
// the system resumes from sleep automatically. The WndProc
// dispatches the event to a Go channel.
//
// WndProc is bound via windows.NewCallback so the goroutine
// stays pure Go — no CGo. The window is invisible
// (WS_EX_TOOLWINDOW) so it does not appear in the taskbar
// or alt-tab list.
//
// Service-context caveat: in Windows Session 0 (services with
// no interactive desktop), WM_POWERBROADCAST may not be
// delivered to user-created windows. The detector logs at
// debug and falls back to a no-op in that case.

import (
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	wmPowerBroadcast = 0x0218
	// pbtApmResumeAuto and wmQuit both equal 0x0012 — a Win32
	// quirk. PBT_APMRESUMEAUTOMATIC is a wParam value;
	// WM_QUIT is a message id. They are never compared in the
	// same expression.
	pbtApmResumeAuto = 0x0012
	wsExToolWindow   = 0x00000080
	wmQuit           = 0x0012
)

// WNDCLASSEX — used by RegisterClassExW. Field order and
// widths must match the Win32 declaration; unsafe.Sizeof is
// the source of truth on both 32-bit and 64-bit.
type wndClassEx struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

type msg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
}

type windowsDetector struct {
	ch        chan struct{}
	logger    *slog.Logger
	hwnd      uintptr
	threadID  uint32 // OS thread id of the pump goroutine; 0 until started is closed
	pumpDone  chan struct{}
	started   chan struct{} // closed once CreateWindowEx + GetCurrentThreadId have run
	closeOnce sync.Once
}

func newPlatformDetector(cfg Config) (Detector, error) {
	user32 := windows.NewLazySystemDLL("user32.dll")
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")

	registerClassExW := user32.NewProc("RegisterClassExW")
	createWindowExW := user32.NewProc("CreateWindowExW")
	getMessageW := user32.NewProc("GetMessageW")
	dispatchMessageW := user32.NewProc("DispatchMessageW")
	defWindowProcW := user32.NewProc("DefWindowProcW")
	getCurrentThreadId := kernel32.NewProc("GetCurrentThreadId")

	hInstance, _, _ := kernel32.NewProc("GetModuleHandleW").Call()

	className, err := windows.UTF16PtrFromString("NightmeWakeWatcher")
	if err != nil {
		cfg.Logger.Debug("syswake/windows: UTF16PtrFromString failed; wake detector disabled", "err", err)
		return newNoopDetector(), nil
	}

	d := &windowsDetector{
		ch:       make(chan struct{}, 4),
		logger:   cfg.Logger,
		pumpDone: make(chan struct{}),
		started:  make(chan struct{}),
	}

	wndProc := func(hwnd, msgID, wParam, lParam uintptr) uintptr {
		if msgID == wmPowerBroadcast && wParam == pbtApmResumeAuto {
			select {
			case d.ch <- struct{}{}:
			default:
			}
			return 0
		}
		r, _, _ := defWindowProcW.Call(hwnd, msgID, wParam, lParam)
		return r
	}

	wc := wndClassEx{
		cbSize:        uint32(unsafe.Sizeof(wndClassEx{})),
		lpfnWndProc:   windows.NewCallback(wndProc),
		hInstance:     hInstance,
		lpszClassName: className,
	}

	atom, _, _ := registerClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	if atom == 0 {
		// Typical on Session 0 (services, CI runners without an
		// interactive desktop): RegisterClassExW requires a window
		// station that owns a desktop. Fall back to noop so the
		// daemon still boots without wake detection.
		cfg.Logger.Debug("syswake/windows: RegisterClassExW failed (Session 0?); wake detector disabled")
		return newNoopDetector(), nil
	}

	windowName, _ := windows.UTF16PtrFromString("NightmeWakeWatcher")

	// The window must be created AND pumped on the same OS thread
	// — Windows ties a window to its creating thread, and only
	// that thread's GetMessage/DispatchMessage pair receives the
	// window's messages. Locking the goroutine to its OS thread
	// for the whole lifetime (RegisterClassEx is process-global
	// so it can run from any thread) makes the window owner and
	// pump owner identical.
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(d.pumpDone)

		hwnd, _, _ := createWindowExW.Call(
			wsExToolWindow,
			uintptr(unsafe.Pointer(className)),
			uintptr(unsafe.Pointer(windowName)),
			0, // dwStyle
			0xFFFFFFFF,
			0xFFFFFFFF,
			0,
			0,
			0,
			0,
			hInstance,
			0,
		)
		tid, _, _ := getCurrentThreadId.Call()
		d.threadID = uint32(tid)

		if hwnd == 0 {
			close(d.started)
			return
		}
		d.hwnd = hwnd
		close(d.started)

		m := msg{}
		for {
			r, _, _ := getMessageW.Call(
				uintptr(unsafe.Pointer(&m)), 0, 0, 0,
			)
			if int32(r) == -1 {
				return
			}
			if m.Message == wmQuit {
				return
			}
			if r == 0 {
				continue
			}
			_, _, _ = dispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
		}
	}()

	select {
	case <-d.started:
	case <-time.After(5 * time.Second):
		return nil, fmt.Errorf("syswake/windows: pump goroutine did not start within 5s")
	}
	if d.hwnd == 0 {
		<-d.pumpDone // let the failed goroutine exit cleanly
		cfg.Logger.Debug("syswake/windows: CreateWindowExW returned 0; wake detector disabled")
		return newNoopDetector(), nil
	}
	return d, nil
}

func (d *windowsDetector) WakeChan() <-chan struct{} { return d.ch }

func (d *windowsDetector) Close() error {
	var err error
	d.closeOnce.Do(func() {
		// PostThreadMessage targets the pump thread directly;
		// PostQuitMessage is thread-local and would post to
		// whatever goroutine happens to call Close, which is
		// not the pump.
		if d.threadID != 0 {
			user32 := windows.NewLazySystemDLL("user32.dll")
			postThreadMessageW := user32.NewProc("PostThreadMessageW")
			_, _, _ = postThreadMessageW.Call(uintptr(d.threadID), wmQuit, 0, 0)
		}
		<-d.pumpDone
		if d.hwnd != 0 {
			user32 := windows.NewLazySystemDLL("user32.dll")
			destroyWindow := user32.NewProc("DestroyWindow")
			_, _, _ = destroyWindow.Call(d.hwnd)
		}
	})
	return err
}
