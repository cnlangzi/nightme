package command

import "context"

// Reply builds a SlashOutput with the given text, marked as
// Consumed. The gateway (or the runtime shim) turns the
// output into a single Outbound message via rt.Channel().Send.
//
// This is the ONE canonical reply helper — all commands use
// it instead of constructing SlashOutput by hand. Keeping
// construction in one place lets us evolve the output shape
// (e.g. add per-reply metadata, drop metadata, log all
// replies) without touching every command.
//
// Returned error is always nil in the current implementation;
// the signature includes error for forward-compat with
// commands that may need to surface failures (e.g. when
// reply involves async I/O in the future).
func Reply(_ context.Context, _ RuntimeServices, text string) (*SlashOutput, error) {
	return &SlashOutput{
		Reply:    text,
		Consumed: true,
	}, nil
}
