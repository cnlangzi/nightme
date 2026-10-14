// Package syswake detects OS wake-from-sleep events and fans them
// to a per-process callback. Implementations are per-OS, no CGo:
//
//   - darwin:  libsystem_notify.dylib (notify_register_file_descriptor
//     on com.apple.system.powermanagement.systemdidwake).
//   - linux:   github.com/godbus/dbus/v5 system-bus subscription
//     to org.freedesktop.login1.Manager.PrepareForSleep.
//   - windows: hidden HWND_MESSAGE window + powrprof.dll
//     RegisterPowerSettingNotification, pumping WM_POWERBROADCAST
//     PBT_APMRESUMEAUTOMATIC on a goroutine.
//
// All three implementations close cleanly when ctx is cancelled
// or Close() is called; no goroutine leak on daemon shutdown.
//
// Platforms without a detector (freebsd, openbsd, illumos) get a
// stub returning ErrUnsupported — see syswake_other.go.
package syswake

import (
	"context"
	"errors"
	"log/slog"
)

// Detector is the public surface. WakeChan() yields a struct{}
// every time the OS reports a resume-from-sleep. The channel
// returns ok=false after Close() or ctx cancellation.
type Detector interface {
	WakeChan() <-chan struct{}
	Close() error
}

// Config holds the per-process knobs. Logger may be nil (falls
// back to slog.Default). OnWake is called from the detector's
// pump goroutine once per wake event; production runtime sets
// OnWake to a closure that fans out to each WakeKicker channel.
//
// OnWake is invoked synchronously, in the pump goroutine, every
// time a wake fires. Keep it short — the runtime's wake handler
// is just a method call per channel.
type Config struct {
	Logger *slog.Logger

	// OnWake is optional. If set, called from the pump goroutine
	// every time a wake fires. If nil, callers must consume
	// WakeChan() directly.
	OnWake func(context.Context)
}

// New constructs the platform-appropriate Detector. Returns an
// error only on initialization failures that indicate the OS API
// is genuinely unreachable (e.g. unsupported platform). A
// successful-but-quiet no-op detector (linux container without
// systemd-logind, windows Session 0 service) is logged at debug
// rather than returned as an error.
func New(ctx context.Context, cfg Config) (Detector, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return newPlatformDetector(ctx, cfg)
}

// ErrUnsupported is returned by New on platforms with no
// detector implementation (freebsd, openbsd, illumos, etc.).
var ErrUnsupported = errors.New("syswake: platform not supported")