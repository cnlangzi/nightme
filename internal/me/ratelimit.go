package me

import (
	"strings"

	"github.com/cnlangzi/nightme/internal/wfe"
)

// rateLimitReason is why a run pauses when the agent is throttled.
const rateLimitReason = "the agent is rate limited (429) — the run waits for the operator"

// rateLimit reports whether an outcome says the underlying agent is
// throttled.
//
// There is no code to read: the agent's verdict reaches the run as
// prose. The turn fails, the settle reports "failed" and an empty
// summary, and the reason sits in the text the agent produced —
// "API Error: 429 ...", "rate_limit_error", "too many requests".
//
// So the text is the signal, and it is read from every place the
// agent's words land: the outcome's summary, its data and the whole
// transcript of the turn.
//
// Running the command again makes it worse, so the run pauses and
// waits for the operator to continue it.
func rateLimit(summary, transcript string, data map[string]string) bool {
	text := strings.ToLower(summary + " " + transcript)
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
	if res.Status == string(wfe.OutcomeOK) || res.Status == string(wfe.OutcomeNoop) {
		return false
	}
	if rateLimit(res.Summary, res.Transcript, res.Data) {
		return true
	}
	// The transcript the model reads is cut down to its head and
	// tail. The scan reads every line, so a hint in the middle of a
	// long turn is not lost.
	for _, entry := range res.entries {
		if rateLimit(entry.Text, "", nil) {
			return true
		}
	}
	return false
}
