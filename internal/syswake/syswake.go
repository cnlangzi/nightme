// Package syswake detects OS wake-from-sleep events and fans them
// to a per-process channel. Implementations are per-OS, no CGo:
//
//   - darwin:  libsystem_notify.dylib (notify_register_file_descriptor
//     on com.apple.system.powermanagement.systemdidwake).
//   - linux:   github.com/godbus/dbus/v5 system-bus subscription
//     to org.freedesktop.login1.Manager.PrepareForSleep.
//   - windows: hidden HWND_MESSAGE window + WM_POWERBROADCAST
//     PBT_APMRESUMEAUTOMATIC on a goroutine pinned to the
//     window's owning OS thread.
//
// All three implementations close cleanly when Close() is
// called; no goroutine leak on daemon shutdown. Callers
// receive wake events via WakeChan().
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
// returns ok=false after Close().
type Detector interface {
	WakeChan() <-chan struct{}
	Close() error
}

// Config holds the per-process knobs. Logger may be nil (falls
// back to slog.Default).
type Config struct {
	Logger *slog.Logger
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
	return newPlatformDetector(cfg)
}

// ErrUnsupported is returned by New on platforms with no
// detector implementation (freebsd, openbsd, illumos, etc.).
var ErrUnsupported = errors.New("syswake: platform not supported")
