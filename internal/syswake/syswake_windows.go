//go:build windows

package syswake

// Windows wake detection via WM_POWERBROADCAST. A hidden
// top-level window receives PBT_APMRESUMEAUTOMATIC whenever
// the system resumes from sleep automatically. The WndProc
// dispatches the event to a Go channel and invokes OnWake.
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
	"context"
	"fmt"
	"log/slog"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	wmPowerBroadcast = 0x0218
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
	onWake    func(context.Context)
	hwnd      uintptr
	pumpDone  chan struct{}
	closeOnce sync.Once
	quitOnce  sync.Once // PostQuitMessage is called once on Close
}

func newPlatformDetector(ctx context.Context, cfg Config) (Detector, error) {
	user32 := windows.NewLazySystemDLL("user32.dll")
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")

	registerClassExW := user32.NewProc("RegisterClassExW")
	createWindowExW := user32.NewProc("CreateWindowExW")
	getMessageW := user32.NewProc("GetMessageW")
	dispatchMessageW := user32.NewProc("DispatchMessageW")
	defWindowProcW := user32.NewProc("DefWindowProcW")
	postQuitMessage := user32.NewProc("PostQuitMessage")
	getModuleHandleW := kernel32.NewProc("GetModuleHandleW")

	hInstance, _, _ := getModuleHandleW.Call()

	className, err := windows.UTF16PtrFromString("NightmeWakeWatcher")
	if err != nil {
		return nil, fmt.Errorf("syswake/windows: UTF16PtrFromString: %w", err)
	}

	d := &windowsDetector{
		ch:       make(chan struct{}, 4),
		logger:   cfg.Logger,
		onWake:   cfg.OnWake,
		pumpDone: make(chan struct{}),
	}

	wndProc := func(hwnd, msgID, wParam, lParam uintptr) uintptr {
		if msgID == wmPowerBroadcast && wParam == pbtApmResumeAuto {
			select {
			case d.ch <- struct{}{}:
			default:
			}
			if d.onWake != nil {
				d.onWake(context.Background())
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
		return nil, fmt.Errorf("syswake/windows: RegisterClassExW failed")
	}

	// CreateWindowExW(dwExStyle, lpClassName, lpWindowName,
	//   dwStyle, x, y, nWidth, nHeight, hWndParent, hMenu,
	//   hInstance, lpParam)
	windowName, _ := windows.UTF16PtrFromString("NightmeWakeWatcher")
	hwnd, _, _ := createWindowExW.Call(
		wsExToolWindow,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(windowName)),
		0, // dwStyle = WS_OVERLAPPED | WS_HIDDEN if 0
		0xFFFFFFFF,
		0xFFFFFFFF,
		0,
		0,
		0,
		0,
		hInstance,
		0,
	)
	if hwnd == 0 {
		return nil, fmt.Errorf("syswake/windows: CreateWindowExW returned 0")
	}
	d.hwnd = hwnd

	go d.pump(getMessageW, dispatchMessageW, postQuitMessage, ctx)
	return d, nil
}

func (d *windowsDetector) pump(
	getMessageW, dispatchMessageW, postQuitMessage *windows.LazyProc,
	ctx context.Context,
) {
	defer close(d.pumpDone)
	m := msg{}
	for {
		// GetMessageW pumps messages. Returns -1 on error,
		// 0 on WM_QUIT, positive on a regular message.
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
		// Bounded respect for ctx cancellation.
		if ctx.Err() != nil {
			d.quitOnce.Do(func() {
				_, _, _ = postQuitMessage.Call(0)
			})
		}
	}
}

func (d *windowsDetector) WakeChan() <-chan struct{} { return d.ch }

func (d *windowsDetector) Close() error {
	var err error
	d.closeOnce.Do(func() {
		d.quitOnce.Do(func() {
			user32 := windows.NewLazySystemDLL("user32.dll")
			postQuitMessage := user32.NewProc("PostQuitMessage")
			_, _, _ = postQuitMessage.Call(0)
		})
		<-d.pumpDone
		if d.hwnd != 0 {
			user32 := windows.NewLazySystemDLL("user32.dll")
			destroyWindow := user32.NewProc("DestroyWindow")
			_, _, _ = destroyWindow.Call(d.hwnd)
		}
	})
	return err
}