package syswake

// noopDetector is a Detector that never produces a wake event
// and never errors on Close. Used as a fallback when a real
// detector's prerequisites are unmet (linux container without
// systemd-logind, windows Session 0 service with no power
// notifications, etc.) so the daemon still boots unchanged.
//
// The channel is never closed — receivers must rely on their
// own context cancellation to break out of the select.
type noopDetector struct {
	ch chan struct{}
}

func newNoopDetector() Detector {
	return &noopDetector{ch: make(chan struct{})}
}

func (n *noopDetector) WakeChan() <-chan struct{} { return n.ch }
func (n *noopDetector) Close() error              { return nil }
