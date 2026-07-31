package heartbeat

import (
	"fmt"
	"time"
)

// formatDuration renders a duration as "Xs" when below one minute, or
// "XmYYs" otherwise. Used in the "idle" suffix of the heartbeat note.
//
// Examples:
//
//	formatDuration(5 * time.Second)   → "5s"
//	formatDuration(65 * time.Second)  → "1m5s"
//	formatDuration(2 * time.Minute)   → "2m0s"
//	formatDuration(125 * time.Second) → "2m5s"
func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	totalSec := int(d.Seconds())
	if totalSec < 60 {
		return fmt.Sprintf("%ds", totalSec)
	}
	minutes := totalSec / 60
	seconds := totalSec % 60
	return fmt.Sprintf("%dm%ds", minutes, seconds)
}
