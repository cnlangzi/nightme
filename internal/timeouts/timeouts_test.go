package timeouts

import (
	"testing"
	"time"
)

// TestTimeouts_AllSane guards against accidental regressions in the
// centralised timeout values. Two conditions per value:
//
//   - > 0: a zero or negative value would silently disable the
//     kill path at every call site (a hung process could park
//     the dispatcher forever).
//   - ≤ 24h: a value larger than a day is almost certainly a
//     mis-edit (e.g. forgetting the multiplier). 24h is generous
//     enough to never false-positive on any real policy choice
//     while still catching the "I meant 5 seconds but wrote 5
//     hours" class of typo.
func TestTimeouts_AllSane(t *testing.T) {
	checks := []struct {
		name string
		d    time.Duration
	}{
		{"Shell", Shell},
		{"Agent", Agent},
		{"Hook", Hook},
		{"CLI", CLI},
		{"Reply", Reply},
		{"Review", Review},
	}
	const upperBound = 24 * time.Hour
	for _, c := range checks {
		if c.d <= 0 {
			t.Errorf("%s = %v, want > 0", c.name, c.d)
		}
		if c.d > upperBound {
			t.Errorf("%s = %v, want <= %v (likely unit typo)", c.name, c.d, upperBound)
		}
	}
}
