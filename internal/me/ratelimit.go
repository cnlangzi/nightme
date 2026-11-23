package me

import (
	"net/http"
	"strings"
)

// rateLimitReason is why a run pauses when the agent is throttled.
const rateLimitReason = "the agent is rate limited (429) — the run waits for the operator"

// rateLimit reports whether an outcome says the underlying agent is
// throttled.
//
// The status is the signal: a 429 is a rate limit, and the bridge
// that read it off the wire hands it over as a number.
//
// The text is the fallback. The other bridges do not report the
// upstream's status yet, so their throttling reaches the run as
// prose — "API Error: 429 ...", "rate_limit_error", "too many
// requests" — and the scan reads every place the agent's words land:
// the outcome's words, its facts and the whole transcript of the turn.
//
// Running the command again makes it worse, so the run pauses and
// waits for the operator to continue it.
func rateLimit(status int, statusText, transcript string, data map[string]string) bool {
	if status == http.StatusTooManyRequests {
		return true
	}
	text := strings.ToLower(statusText + " " + transcript)
	for _, v := range data {
		text += " " + strings.ToLower(v)
	}
	return strings.Contains(text, "429") ||
		strings.Contains(text, "rate limit") ||
		strings.Contains(text, "rate_limit") ||
		strings.Contains(text, "too many requests")
}

// throttled reports whether the command's outcome says the agent is
// throttled.
//
// A command that worked is not throttled, whatever its transcript
// says.
func throttled(res stepResult) bool {
	if res.Success {
		return false
	}
	if rateLimit(res.Status, res.StatusText, res.Transcript, res.Data) {
		return true
	}
	// The transcript the model reads is cut down to its head and
	// tail. The scan reads every line, so a hint in the middle of a
	// long turn is not lost.
	for _, entry := range res.entries {
		if rateLimit(0, entry.Text, "", nil) {
			return true
		}
	}
	return false
}
