package syswake

import "sync/atomic"

// FakeDetector is the public test helper for syswake. It
// satisfies Detector and adds an Inject() method so tests can
// deterministically fire wake events without touching real OS
// APIs. Construct via NewFake.
//
// Usage:
//
//	d := syswake.NewFake()
//	d.Inject() // fire one synthetic wake
type FakeDetector struct {
	ch     chan struct{}
	closed atomic.Bool
}

// NewFake returns a *FakeDetector backed by an injectable
// channel. Buffer size 16 is generous enough that Inject()
// never blocks under normal test pacing.
func NewFake() *FakeDetector {
	return &FakeDetector{ch: make(chan struct{}, 16)}
}

// WakeChan implements Detector. Returns the channel that
// receives one struct{} per Inject() call until Close().
func (f *FakeDetector) WakeChan() <-chan struct{} { return f.ch }

// Close implements Detector. Idempotent; subsequent calls
// return nil without further effect.
func (f *FakeDetector) Close() error {
	if f.closed.CompareAndSwap(false, true) {
		close(f.ch)
	}
	return nil
}

// Inject pushes one synthetic wake event into the channel.
// Safe for concurrent use. After Close() calls become no-ops;
// recover the "send on closed channel" race so a late Inject
// doesn't take down the test goroutine.
func (f *FakeDetector) Inject() {
	if f.closed.Load() {
		return
	}
	defer func() { _ = recover() }()
	select {
	case f.ch <- struct{}{}:
	default:
	}
}