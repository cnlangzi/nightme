//go:build linux

package syswake

// Linux wake detection via systemd-logind's PrepareForSleep
// D-Bus signal. Subscribes on the system bus (not session —
// logind publishes PrepareForSleep only on the system bus).
//
// Signal payload is a single bool: false = just woke up,
// true = about to sleep. We act only on false.
//
// Containers without systemd-logind (no /run/systemd/private
// socket, etc.) fail at AddMatchSignal — we silently fall
// back to a no-op detector. The daemon still runs; wake
// recovery is just unavailable in that environment.

import (
	"context"
	"log/slog"
	"sync"

	"github.com/godbus/dbus/v5"
)

type linuxDetector struct {
	ch        chan struct{}
	conn      *dbus.Conn
	logger    *slog.Logger
	onWake    func(context.Context)
	sigCh     chan *dbus.Signal
	closeOnce sync.Once
	done      chan struct{}
}

func newPlatformDetector(ctx context.Context, cfg Config) (Detector, error) {
	conn, err := dbus.SystemBus()
	if err != nil {
		cfg.Logger.Debug("syswake/linux: no system bus; wake detector disabled", "err", err)
		return newNoopDetector(), nil
	}

	// godbus v5 uses options-style matching (the v4 MatchSignal
	// struct was removed). WithMatchInterface + WithMatchMember
	// are the only two we need; the path is irrelevant since
	// PrepareForSleep is broadcast from the well-known
	// /org/freedesktop/login1 object.
	if err := conn.AddMatchSignal(
		dbus.WithMatchInterface("org.freedesktop.login1.Manager"),
		dbus.WithMatchMember("PrepareForSleep"),
	); err != nil {
		cfg.Logger.Debug("syswake/linux: PrepareForSleep match failed; wake detector disabled", "err", err)
		_ = conn.Close()
		return newNoopDetector(), nil
	}

	sigCh := make(chan *dbus.Signal, 4)
	d := &linuxDetector{
		ch:     make(chan struct{}, 4),
		conn:   conn,
		logger: cfg.Logger,
		onWake: cfg.OnWake,
		sigCh:  sigCh,
		done:   make(chan struct{}),
	}
	go d.pump(ctx)
	return d, nil
}

func (d *linuxDetector) pump(ctx context.Context) {
	defer close(d.done)
	d.conn.Signal(d.sigCh)
	for {
		select {
		case <-ctx.Done():
			return
		case sig, ok := <-d.sigCh:
			if !ok {
				return
			}
			if sig == nil || sig.Name != "PrepareForSleep" {
				continue
			}
			if len(sig.Body) != 1 {
				continue
			}
			sleeping, ok := sig.Body[0].(bool)
			if !ok || sleeping {
				continue
			}
			select {
			case d.ch <- struct{}{}:
			default:
			}
			if d.onWake != nil {
				d.onWake(ctx)
			}
		}
	}
}

func (d *linuxDetector) WakeChan() <-chan struct{} { return d.ch }

func (d *linuxDetector) Close() error {
	var err error
	d.closeOnce.Do(func() {
		err = d.conn.Close() // also removes match rules
	})
	return err
}