package command

import "github.com/cnlangzi/nightme/internal/chatsession"

// RequireActiveCwd is a preflight check used by every slash
// command that operates on the current workspace (/cwd /use
// /kill /new /gtw etc.). It returns ("", nil) when the
// session has an active workspace; otherwise it returns the
// current cwd (always "" in that case) and a SlashOutput with
// the "send /cwd first" hint reply — the caller should
// return this output directly without further work.
//
// ADR 0007 (2026-08-06): signature changed from
// `RequireActiveCwd(sess services.Session)` to
// `RequireActiveCwd(cs *chatsession.ChatSession)` because the
// services.Session interface is gone. cs == nil is treated
// identically to ActiveCwd() == "" (both indicate "no session
// yet").
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
	cwd = cs.ActiveCwd()
	if cwd == "" {
		return "", &SlashOutput{
			Reply:    "No active workspace. Send /cwd <path> first.",
			Consumed: true,
		}
	}
	return cwd, nil
}
