package command

import "github.com/cnlangzi/nightme/internal/command/services"

// RequireActiveCwd is a preflight check used by every slash
// command that operates on the current workspace (/cwd /use
// /kill /new /gtw etc.). It returns ("", nil) when the
// session has an active workspace; otherwise it returns the
// current cwd (always "" in that case) and a SlashOutput with
// the "send /cwd first" hint reply — the caller should
// return this output directly without further work.
//
// Usage:
//
//	cwd, fail := command.RequireActiveCwd(sess)
//	if fail != nil {
//	    return fail, nil
//	}
//	// ... proceed using cwd
func RequireActiveCwd(sess services.Session) (cwd string, failOut *SlashOutput) {
	if sess == nil {
		return "", &SlashOutput{
			Reply:    "No active chat session.",
			Consumed: true,
		}
	}
	cwd = sess.ActiveCwd()
	if cwd == "" {
		return "", &SlashOutput{
			Reply:    "No active workspace. Send /cwd <path> first.",
			Consumed: true,
		}
	}
	return cwd, nil
}
