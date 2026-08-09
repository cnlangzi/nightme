package pty

import (
	"testing"
	"time"
)

// TestRunOnce_IdleConstant ensures the package-level idle timeout
// constant is reasonable (≥ 1s, ≤ 30s). Catches accidental
// regression to "0" (immediate) or "1h" (effectively never).
//
// The full PTY RunOnce drain is exercised by the integration
// tests in testdata/ that shell out to a real PTY-backed CLI.
// This unit test exists only to pin the timeout constant.
func TestRunOnce_IdleConstant(t *testing.T) {
	if ptyIdleTimeout < time.Second {
		t.Fatalf("ptyIdleTimeout = %v, want ≥ 1s", ptyIdleTimeout)
	}
	if ptyIdleTimeout > 30*time.Second {
		t.Fatalf("ptyIdleTimeout = %v, want ≤ 30s", ptyIdleTimeout)
	}
}
