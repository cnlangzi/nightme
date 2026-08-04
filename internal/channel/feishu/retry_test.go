package feishu

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// fastRetryConfig: 3 attempts, 1ms initial, 5ms cap, no jitter.
// Lets tests exercise retry paths without sleeping for hundreds of ms.
func fastRetryConfig() RetryConfig {
	return RetryConfig{
		MaxAttempts:    3,
		InitialBackoff: 1 * time.Millisecond,
		MaxBackoff:     5 * time.Millisecond,
		JitterPercent:  0,
	}
}

// transientNetErr mimics net.OpError{Op: "read", Err: ...} with Timeout()==true.
type transientNetErr struct{}

func (transientNetErr) Error() string   { return "transient: i/o timeout" }
func (transientNetErr) Timeout() bool   { return true }
func (transientNetErr) Temporary() bool { return true }

var _ net.Error = transientNetErr{}

func TestIsTransient_NilAndCtxErrors(t *testing.T) {
	t.Parallel()
	if IsTransient(nil) {
		t.Fatal("nil should not be transient")
	}
	if IsTransient(context.Canceled) {
		t.Fatal("context.Canceled should not be transient")
	}
	if IsTransient(context.DeadlineExceeded) {
		t.Fatal("context.DeadlineExceeded should not be transient")
	}
}

func TestIsTransient_NetTimeout(t *testing.T) {
	t.Parallel()
	if !IsTransient(transientNetErr{}) {
		t.Fatal("net.Error.Timeout()==true should be transient")
	}
	if !IsTransient(&net.OpError{Op: "dial", Err: transientNetErr{}}) {
		t.Fatal("wrapped net.OpError with Timeout should be transient")
	}
}

func TestIsTransient_EOFAndSyscall(t *testing.T) {
	t.Parallel()
	if !IsTransient(io.EOF) {
		t.Fatal("io.EOF should be transient")
	}
	if !IsTransient(io.ErrUnexpectedEOF) {
		t.Fatal("io.ErrUnexpectedEOF should be transient")
	}
	if !IsTransient(syscall.ECONNRESET) {
		t.Fatal("syscall.ECONNRESET should be transient")
	}
	if !IsTransient(syscall.EPIPE) {
		t.Fatal("syscall.EPIPE should be transient")
	}
}

func TestIsTransient_SubstringFallback(t *testing.T) {
	t.Parallel()
	cases := []string{
		"read tcp 1.2.3.4:443: connection reset by peer",
		"write: broken pipe",
		"dial tcp: i/o timeout",
		"net/http: TLS handshake timeout",
		"dial tcp 127.0.0.1:443: connection refused",
		"lookup example.com: no such host",
	}
	for _, msg := range cases {
		if !IsTransient(errors.New(msg)) {
			t.Fatalf("expected transient for %q", msg)
		}
	}
}

func TestIsTransient_FeishuCodesNeverRetry(t *testing.T) {
	t.Parallel()
	cases := []string{
		"feishu: reply message failed with code 230011",
		"feishu: reply message failed with code:231003",
		"feishu: rate limited code 230001",
		"feishu: rate limited code:230001",
	}
	for _, msg := range cases {
		if IsTransient(errors.New(msg)) {
			t.Fatalf("Feishu business code should NOT be transient: %q", msg)
		}
	}
}

func TestIsTransient_PermanentErrors(t *testing.T) {
	t.Parallel()
	cases := []string{
		"feishu: patch message failed with code 99991663", // invalid token
		"feishu: create message failed with code 230020",  // generic error
		"some other unrelated error",
		"",
	}
	for _, msg := range cases {
		if IsTransient(errors.New(msg)) {
			t.Fatalf("expected NOT transient: %q", msg)
		}
	}
}

func TestWithTransientRetry_SuccessNoRetry(t *testing.T) {
	t.Parallel()
	cfg := fastRetryConfig()
	var calls int32
	err := WithTransientRetry(context.Background(), RetryOpts{
		Op: "test", Cfg: cfg, Logger: slog.Default(),
	}, func() error {
		atomic.AddInt32(&calls, 1)
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
}

func TestWithTransientRetry_RetriesOnTransientThenSuccess(t *testing.T) {
	t.Parallel()
	cfg := fastRetryConfig()
	var calls int32
	err := WithTransientRetry(context.Background(), RetryOpts{
		Op: "test", Cfg: cfg, Logger: slog.Default(),
	}, func() error {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			return transientNetErr{}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("calls = %d, want 3 (2 retries + 1 success)", got)
	}
}

func TestWithTransientRetry_NoRetryOnPermanent(t *testing.T) {
	t.Parallel()
	cfg := fastRetryConfig()
	permanent := errors.New("feishu: invalid token code 99991663")
	var calls int32
	err := WithTransientRetry(context.Background(), RetryOpts{
		Op: "test", Cfg: cfg, Logger: slog.Default(),
	}, func() error {
		atomic.AddInt32(&calls, 1)
		return permanent
	})
	if !errors.Is(err, permanent) {
		t.Fatalf("err = %v, want permanent", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("calls = %d, want 1 (no retry on permanent)", got)
	}
}

func TestWithTransientRetry_ExhaustsAfterMaxAttempts(t *testing.T) {
	t.Parallel()
	cfg := fastRetryConfig()
	transient := transientNetErr{}
	var calls int32
	err := WithTransientRetry(context.Background(), RetryOpts{
		Op: "test", Cfg: cfg, Logger: slog.Default(),
	}, func() error {
		atomic.AddInt32(&calls, 1)
		return transient
	})
	if !errors.Is(err, transient) {
		t.Fatalf("err = %v, want transient", err)
	}
	if got := atomic.LoadInt32(&calls); got != int32(cfg.MaxAttempts) {
		t.Fatalf("calls = %d, want %d (MaxAttempts)", got, cfg.MaxAttempts)
	}
}

func TestWithTransientRetry_ContextCancelStopsRetry(t *testing.T) {
	t.Parallel()
	cfg := RetryConfig{
		MaxAttempts:    5,
		InitialBackoff: 100 * time.Millisecond,
		MaxBackoff:     100 * time.Millisecond,
		JitterPercent:  0,
	}
	ctx, cancel := context.WithCancel(context.Background())
	var calls int32
	done := make(chan error, 1)

	go func() {
		done <- WithTransientRetry(ctx, RetryOpts{
			Op: "test", Cfg: cfg, Logger: slog.Default(),
		}, func() error {
			atomic.AddInt32(&calls, 1)
			return transientNetErr{}
		})
	}()

	// Let the first attempt + first backoff run, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("WithTransientRetry did not return after ctx cancel")
	}

	if got := atomic.LoadInt32(&calls); got > int32(cfg.MaxAttempts) {
		t.Fatalf("calls = %d exceeded MaxAttempts=%d", got, cfg.MaxAttempts)
	}
}

func TestWithTransientRetryMsg_RetainsMessageIDOnError(t *testing.T) {
	t.Parallel()
	cfg := fastRetryConfig()
	want := "om_abc123"
	transient := transientNetErr{}
	var calls int32
	got, err := WithTransientRetryMsg(context.Background(), RetryOpts{
		Op: "test", Cfg: cfg, Logger: slog.Default(),
	}, func() (string, error) {
		atomic.AddInt32(&calls, 1)
		return want, transient
	})
	if !errors.Is(err, transient) {
		t.Fatalf("err = %v, want transient", err)
	}
	if got != want {
		t.Fatalf("message_id = %q, want %q (should be retained from last call)", got, want)
	}
	if n := atomic.LoadInt32(&calls); n != int32(cfg.MaxAttempts) {
		t.Fatalf("calls = %d, want %d", n, cfg.MaxAttempts)
	}
}

func TestWithTransientRetry_CtxCancelledAtEntry(t *testing.T) {
	t.Parallel()
	cfg := fastRetryConfig()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	var calls int32
	err := WithTransientRetry(ctx, RetryOpts{
		Op: "test", Cfg: cfg, Logger: slog.Default(),
	}, func() error {
		atomic.AddInt32(&calls, 1)
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("calls = %d, want 0 (ctx cancelled at entry should skip fn)", got)
	}
}

func TestWithTransientRetry_NormalizesZeroConfig(t *testing.T) {
	t.Parallel()
	var calls int32
	// All zero / negative fields should fall back to DefaultRetryConfig
	// without crashing.
	err := WithTransientRetry(context.Background(), RetryOpts{
		Op: "test",
		Cfg: RetryConfig{
			MaxAttempts:    0, // bad
			InitialBackoff: -1, // bad
			MaxBackoff:     0,  // bad
			JitterPercent:  -1, // bad
		},
		Logger: slog.Default(),
	}, func() error {
		atomic.AddInt32(&calls, 1)
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
}

func TestWithTransientRetryMsg_SuccessNoRetry(t *testing.T) {
	t.Parallel()
	cfg := fastRetryConfig()
	want := "om_ok"
	var calls int32
	got, err := WithTransientRetryMsg(context.Background(), RetryOpts{
		Op: "test", Cfg: cfg, Logger: slog.Default(),
	}, func() (string, error) {
		atomic.AddInt32(&calls, 1)
		return want, nil
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got != want {
		t.Fatalf("message_id = %q, want %q", got, want)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("calls = %d, want 1", n)
	}
}

func TestWithTransientRetry_DegradationLogEmitted(t *testing.T) {
	t.Parallel()
	// Capture warn-level log records to verify the degradation log
	// schema is emitted on retry exhaustion.
	rec := &capturingHandler{attrs: &[]any{}}
	logger := slog.New(rec)
	cfg := fastRetryConfig()
	err := WithTransientRetry(context.Background(), RetryOpts{
		Op: "send_test",
		Cfg: cfg,
		Logger: logger,
		Attrs: []any{"chat_id", "oc_test"},
	}, func() error {
		return transientNetErr{}
	})
	if !errors.Is(err, transientNetErr{}) {
		t.Fatalf("err = %v, want transient", err)
	}
	// Look for the degradation log line.
	found := false
	for _, rec := range rec.records {
		if rec.Level == slog.LevelWarn && rec.Msg == "feishu degradation" {
			found = true
			// Spot-check key fields.
			if rec.fields["degradation"] != "retry_exhausted" {
				t.Errorf("degradation = %v, want retry_exhausted", rec.fields["degradation"])
			}
			if rec.fields["op"] != "send_test" {
				t.Errorf("op = %v, want send_test", rec.fields["op"])
			}
			if rec.fields["chat_id"] != "oc_test" {
				t.Errorf("chat_id = %v, want oc_test", rec.fields["chat_id"])
			}
		}
	}
	if !found {
		t.Fatal("no 'feishu degradation' warn log emitted on retry exhaustion")
	}
}

func TestJitter_BoundsRespected(t *testing.T) {
	t.Parallel()
	backoff := 100 * time.Millisecond
	jp := 0.25
	for i := 0; i < 1000; i++ {
		got := jitter(backoff, jp)
		lo := time.Duration(float64(backoff) * 0.75)
		hi := time.Duration(float64(backoff) * 1.25)
		if got < lo || got > hi {
			t.Fatalf("jitter out of bounds: got %v, want [%v, %v]", got, lo, hi)
		}
	}
	// Zero jitter = exact backoff.
	if got := jitter(backoff, 0); got != backoff {
		t.Fatalf("zero jitter should equal backoff: got %v", got)
	}
	// Clamp jp > 1.
	_ = jitter(backoff, 5.0) // must not panic
}

// capturedRecord is a minimal snapshot of an slog record used by the
// degradation log tests.
type capturedRecord struct {
	Level  slog.Level
	Msg    string
	fields map[string]any
}

// capturingHandler is an slog.Handler that appends every record to a
// slice for later inspection.
type capturingHandler struct {
	attrs   *[]any
	records []capturedRecord
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	rec := capturedRecord{Level: r.Level, Msg: r.Message, fields: map[string]any{}}
	r.Attrs(func(a slog.Attr) bool {
		rec.fields[a.Key] = a.Value.Any()
		return true
	})
	h.records = append(h.records, rec)
	return nil
}
func (h *capturingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := *h.attrs
	for _, a := range attrs {
		out = append(out, a)
	}
	return &capturingHandler{attrs: h.attrs, records: h.records}
}
func (h *capturingHandler) WithGroup(string) slog.Handler { return h }