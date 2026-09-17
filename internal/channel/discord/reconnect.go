package discord

// Phase 1 reconnect.go: minimal terminal-close-code detection.
//
// Discord documents ~12 Gateway close codes. Phase 1 only
// classifies the three that MUST stop the daemon (token revoked,
// invalid intent, disallowed intent); everything else falls
// through to the gateway's reconnect loop in gateway.go.
//
// Phase 3 extends this with the full whitelist (4000/4001/4003 /
// 4007/4008/4009 as transient, plus per-code backoff curves and
// jitter). The classifier below is the seam Phase 3 plugs into.

const (
	// closeCodeAuthenticationFailed — token revoked, malformed,
	// or otherwise rejected. The reconnect loop must NOT keep
	// trying — surface the error and let the daemon exit.
	closeCodeAuthenticationFailed = 4004

	// closeCodeInvalidIntents — the IDENTIFY payload sent an
	// intent Discord doesn't know about (e.g. typo). Operator
	// misconfiguration; reconnecting won't help.
	closeCodeInvalidIntents = 4013

	// closeCodeDisallowedIntents — IDENTIFY requested a
	// privileged intent (MESSAGE_CONTENT in Phase 1) that the
	// Developer Portal hasn't enabled. Same shape as 4013.
	closeCodeDisallowedIntents = 4014
)

// isTerminalCloseCode reports whether the close code requires
// the adapter to stop (vs. trigger a reconnect). Phase 3 widens
// this with the explicit transient list.
func isTerminalCloseCode(code int) bool {
	switch code {
	case closeCodeAuthenticationFailed,
		closeCodeInvalidIntents,
		closeCodeDisallowedIntents:
		return true
	}
	return false
}

// describeCloseCode returns a short human-readable label for
// the close code. Used in the error message the adapter surfaces
// when the daemon exits — keeps the wire code visible (the
// diagnostic value) without forcing the operator to memorise
// Discord's table.
func describeCloseCode(code int) string {
	switch code {
	case 4000:
		return "unknown error"
	case 4001:
		return "unknown opcode"
	case 4003:
		return "not authenticated"
	case closeCodeAuthenticationFailed:
		return "authentication failed (token revoked or malformed)"
	case 4007:
		return "invalid seq (resume dropped)"
	case 4008:
		return "rate limited"
	case 4009:
		return "session timed out"
	case closeCodeInvalidIntents:
		return "invalid intent(s) in IDENTIFY"
	case closeCodeDisallowedIntents:
		return "disallowed intent(s) — privileged intent not enabled in Developer Portal"
	}
	return ""
}
