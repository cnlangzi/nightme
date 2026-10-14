//go:build !darwin && !linux && !windows

package syswake

import "context"

// newPlatformDetector returns ErrUnsupported on platforms
// without a wake-from-sleep detector. Freebsd/openbsd/illumos
// compile this stub so the package as a whole remains valid.
func newPlatformDetector(_ context.Context, _ Config) (Detector, error) {
	return nil, ErrUnsupported
}