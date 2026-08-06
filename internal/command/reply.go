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
// Returns only *SlashOutput (no error) so callers can do
//
//	return command.Reply(ctx, rt, "..."), nil
//
// matching the (*SlashOutput, error) signature of
// SlashCommandFactory.Handle. Callers that need to surface a
// failure construct the SlashOutput directly with a non-nil
// error.
func Reply(_ context.Context, _ RuntimeServices, text string) *SlashOutput {
	return &SlashOutput{
		Reply:    text,
		Consumed: true,
	}
}
