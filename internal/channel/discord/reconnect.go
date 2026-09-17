package discord

import (
	"fmt"
	"time"
)

// Discord Gateway close codes are documented at
// https://discord.com/developers/docs/topics/opcodes-and-status-codes#close-event-codes.
// Phase 1 only classified the three terminal codes (4004 / 4013 /
// 4014); Phase 3 widens this with the full whitelist plus per-code
// backoff curves so a transient close (e.g. 4008 rate-limited) does
// not pay the same delay as an unknown transport hiccup.
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

// reconnectAction names what the gateway loop should do after a
// close. The mapping from a Discord close code to one of these
// actions is owned by decideOnCloseCode.
type reconnectAction int

const (
	// reconnectActionReconnect — drop the session and open a new
	// IDENTIFY. Used for transport / protocol errors where no
	// server-side state can be reused.
	reconnectActionReconnect reconnectAction = iota + 1

	// reconnectActionResume — re-issue RESUME with the same
	// session_id + last_seq. Discord still has the session.
	reconnectActionResume

	// reconnectActionReIdentify — the session is gone but a new
	// IDENTIFY must be performed; the next loop iteration will
	// detect the missing state and fall back to the IDENTIFY path.
	reconnectActionReIdentify

	// reconnectActionStop — terminal. Daemon exits with the
	// descriptive error.
	reconnectActionStop
)

func (a reconnectAction) String() string {
	switch a {
	case reconnectActionReconnect:
		return "reconnect"
	case reconnectActionResume:
		return "resume"
	case reconnectActionReIdentify:
		return "reidentify"
	case reconnectActionStop:
		return "stop"
	}
	return ""
}

// reconnectDecision is the outcome of decideOnCloseCode. The gateway
// loop uses Action to choose its next step, Backoff to schedule the
// next dial, and Reason for log lines + the operator-facing error
// message on terminal exits.
type reconnectDecision struct {
	Action  reconnectAction
	Backoff time.Duration
	Reason  string
}

// defaultReconnectStrategy holds the backoff curve applied to
// close codes without a per-code override. Tests inject shorter
// backoffs via defaultReconnectStrategy.
var defaultReconnectStrategy = reconnectStrategy{
	initialBackoff: 1 * time.Second,
	maxBackoff:     60 * time.Second,
}

// reconnectStrategy is the backoff envelope applied to generic
// (non-terminal, non-special) close codes. The strategy is a value
// type so tests can substitute shorter schedules without
// touching global state.
type reconnectStrategy struct {
	initialBackoff time.Duration
	maxBackoff     time.Duration
}

// decideOnCloseCode maps a Discord Gateway close code to a
// reconnect decision. The mapping mirrors the table at
// /docs/topics/opcodes-and-status-codes and stays aligned with
// feishu's `decideOnCloseCode` shape (separate Action + Backoff
// so the gateway loop doesn't need a switch of its own).
func decideOnCloseCode(code int) reconnectDecision {
	strategy := defaultReconnectStrategy
	switch code {
	case closeCodeAuthenticationFailed:
		return reconnectDecision{
			Action: reconnectActionStop,
			Reason: "authentication failed (check bot_token)",
		}
	case closeCodeInvalidIntents:
		return reconnectDecision{
			Action: reconnectActionStop,
			Reason: "invalid intent(s) — check intents bitfield",
		}
	case closeCodeDisallowedIntents:
		return reconnectDecision{
			Action: reconnectActionStop,
			Reason: "disallowed intent(s) — enable MESSAGE_CONTENT in Developer Portal",
		}
	case 4003:
		return reconnectDecision{
			Action:  reconnectActionResume,
			Backoff: 1 * time.Second,
			Reason:  "not authenticated (resume dropped, retry resume)",
		}
	case 4007:
		return reconnectDecision{
			Action:  reconnectActionReIdentify,
			Backoff: 1 * time.Second,
			Reason:  "invalid seq (resume dropped, re-identify)",
		}
	case 4009:
		return reconnectDecision{
			Action:  reconnectActionReIdentify,
			Backoff: 5 * time.Second,
			Reason:  "session timed out (re-identify)",
		}
	case 4008:
		return reconnectDecision{
			Action:  reconnectActionReconnect,
			Backoff: 30 * time.Second,
			Reason:  "rate limited (long backoff)",
		}
	default:
		return reconnectDecision{
			Action:  reconnectActionReconnect,
			Backoff: strategy.initialBackoff,
			Reason:  fmt.Sprintf("gateway close %d", code),
		}
	}
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

// isTerminalCloseCode reports whether the close code requires
// the adapter to stop. Thin wrapper over decideOnCloseCode for
// call sites that only need the bool (not the per-code backoff).
func isTerminalCloseCode(code int) bool {
	return decideOnCloseCode(code).Action == reconnectActionStop
}
