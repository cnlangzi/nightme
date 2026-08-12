package command

import "github.com/cnlangzi/nightme/internal/chatsession"

// NoActiveCwdReply is the canonical user-facing reply when a
// slash command needs an active workspace but the chat has
// none. Exported so handlers that preflight outside the
// RequireActiveCwd helper (currently internal/command/gtw
// /close and /fix, which do their own preflight to chain
// additional work) stay in lockstep with the helper's
// wording. Keep in sync with RequireActiveCwd's SlashOutput.
const NoActiveCwdReply = "No active workspace. Send /cwd <path> first."

// RequireActiveCwd is a preflight check used by every slash
// command that operates on the current workspace (/cwd /use
// /close /new /gtw etc.). It returns ("", nil) when the
// session has an active workspace; otherwise it returns the
// current cwd (always "" in that case) and a SlashOutput with
// the "send /cwd first" hint reply — the caller should
// return this output directly without further work.
//
// cs == nil is treated identically to SelectedCwd() == "" (both
// indicate "no session yet").
//
// Usage:
//
//	cwd, fail := command.RequireActiveCwd(cs)
//	if fail != nil {
//	    return fail, nil
//	}
//	// ... proceed using cwd
func RequireActiveCwd(cs *chatsession.ChatSession) (cwd string, failOut *SlashOutput) {
	if cs == nil {
		return "", &SlashOutput{
			Reply:    "No active chat session.",
			Consumed: true,
		}
	}
	cwd = cs.SelectedCwd()
	if cwd == "" {
		return "", &SlashOutput{
			Reply:    NoActiveCwdReply,
			Consumed: true,
		}
	}
	return cwd, nil
}
