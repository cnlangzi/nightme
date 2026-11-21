// Package agent — the fatal-stderr contract.
//
// A CLI that reports its own failure on stderr and then goes quiet
// leaves the turn hanging: the bridge is alive, the protocol is
// alive, and nothing ever ends the turn. The contract lets a bridge
// name the stderr lines its own CLI uses for that, so the turn ends
// the moment the CLI says so.
//
// A bridge opts in by passing its own predicate where it already
// reads stderr lines. Most bridges cannot: they are protocol-based
// (the CLI reports failures over its JSON-RPC channel), they read
// stderr as raw bytes with no line granularity, or they merge stderr
// into stdout over a PTY. A bridge whose CLI has no verifiable
// failure format passes no predicate — the contract does nothing for
// it, and no turn is ended by accident.
//
// The predicate lives next to the stderr ring buffer on purpose: both
// describe what the child wrote to stderr, and a bridge that keeps a
// tail of the child's stderr is a bridge that can read its lines.
package agent

import "errors"

// FatalStderr reports whether one stderr line means the CLI gave up
// on the turn it was working on.
//
// The predicate must match a format the CLI is known to emit — a
// literal, a prefix, a regular expression — never a generic word
// like "error" or "failed". A match ends a live turn, so a loose
// predicate kills turns over a CLI that is merely noisy.
//
// An empty line is never fatal.
type FatalStderr func(line string) bool

// FatalStderrError renders a fatal stderr line as the error text the
// chat layer shows.
//
// The CLI's own words are the reason the turn failed: the model name
// it rejected, the exit code, the error code. An operator needs them
// verbatim, so the line is neither trimmed nor reworded.
func FatalStderrError(line string) error {
	return errors.New(line)
}
