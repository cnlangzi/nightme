// Package main (cmd/nightme) — sessionAdapter wires
// *chatsession.Manager into command.SessionService.
//
// Lives in cmd/nightme (NOT in internal/command/services) because:
//   - The adapter wraps the concrete *chatsession.Manager type,
//     which the command/services package MUST NOT depend on (F-51
//     §3.2 dependency arrow).
//   - cmd/nightme is the one place that holds both the command
//     layer and the chatsession concrete implementation, so the
//     adapter naturally lives here.
//
// The adapter is constructed at runtime startup (see run.go).
package main

import (
	"context"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command/services"
	"github.com/cnlangzi/nightme/internal/registry"
)

// sessionAdapter exposes *chatsession.Manager as
// services.SessionService. It does NOT hold per-chat state; each
// call to Get / GetOrCreate asks the manager for the current
// ChatSession (or creates one), and returns it as a services.Session.
type sessionAdapter struct {
	mgr            *chatsession.Manager
	defaultPrimary string
}

// newSessionAdapter constructs the adapter. defaultPrimary is
// used by GetOrCreate (the agent name when no session exists
// yet).
func newSessionAdapter(mgr *chatsession.Manager, defaultPrimary string) *sessionAdapter {
	return &sessionAdapter{mgr: mgr, defaultPrimary: defaultPrimary}
}

// Get implements services.SessionService.
func (a *sessionAdapter) Get(chatID string) services.Session {
	cs := a.mgr.Get(chatID)
	if cs == nil {
		return nil
	}
	return &chatSessionAdapter{cs: cs}
}

// GetOrCreate implements services.SessionService.
func (a *sessionAdapter) GetOrCreate(chatID, primaryAgent string) services.Session {
	cs := a.mgr.GetOrCreate(chatID, primaryAgent)
	return &chatSessionAdapter{cs: cs}
}

// chatSessionAdapter wraps a single *chatsession.ChatSession as
// services.Session. Created on every Get / GetOrCreate call —
// cheap (no copying of chat state) but not free; the
// conversations are short-lived (one slash command invocation).
type chatSessionAdapter struct {
	cs *chatsession.ChatSession
}

// ActiveCwd / SetActiveCwd / ActiveAgent / SetActiveAgent /
// PrimaryAgent — direct passthrough.
func (s *chatSessionAdapter) ActiveCwd() string            { return s.cs.ActiveCwd() }
func (s *chatSessionAdapter) SetActiveCwd(cwd string) error { return s.cs.SetActiveCwd(cwd) }
func (s *chatSessionAdapter) ActiveAgent() string           { return s.cs.ActiveAgent() }
func (s *chatSessionAdapter) SetActiveAgent(name string) error { return s.cs.SetActiveAgent(name) }
func (s *chatSessionAdapter) PrimaryAgent() string         { return s.cs.PrimaryAgent() }

// LookupActiveAgentSession wraps the concrete AgentSession
// pointer in an adapter. Returns nil when the pool is empty
// (commands must check for nil).
func (s *chatSessionAdapter) LookupActiveAgentSession() (services.AgentSession, error) {
	as, err := s.cs.LookupActiveAgentSession()
	if err != nil || as == nil {
		return nil, err
	}
	return &agentSessionAdapter{as: as}, nil
}

// SetActiveAgentSession is a no-op — *chatsession.ChatSession
// manages its pool via the (activeAgent, activeCwd) key, not a
// direct pointer setter. /gtw and other commands should call
// SetActiveAgent / SetActiveCwd then re-LookupActiveAgentSession.
// F-51 keeps the Session interface contract for future
// extension; the no-op here matches the legacy behavior (the
// pre-F-51 gateway's SetActionHandler was a similar "tell
// the chat session about the active agent" hook).
func (s *chatSessionAdapter) SetActiveAgentSession(as services.AgentSession) {
	// Intentionally a no-op. The session's pool is derived
	// from (activeAgent, activeCwd) on every lookup; commands
	// that need to bind a session should mutate those fields
	// and re-LookupActiveAgentSession.
	_ = as
}

// KillAll wraps the kill result list.
func (s *chatSessionAdapter) KillAll() ([]services.KillResult, error) {
	results, err := s.cs.KillAll()
	if err != nil {
		return nil, err
	}
	out := make([]services.KillResult, len(results))
	for i, r := range results {
		out[i] = services.KillResult{
			Agent:       r.Agent,
			Cwd:         r.Cwd,
			BeforeState: statusString(r.BeforeState),
			Action:      r.Action,
			Error:       r.Error,
		}
	}
	return out, nil
}

// NewActiveAgentSessions runs /new on this session and unwraps
// the result list. The chatsession signature is
// `(matched, reset int, results []ResetResult, firstErr error)`.
func (s *chatSessionAdapter) NewActiveAgentSessions(ctx context.Context, agentName string) (int, []services.AgentSession, []services.ResetResult, error) {
	matched, _, results, err := s.cs.NewActiveAgentSessions(ctx, agentName)
	if err != nil {
		return 0, nil, nil, err
	}
	// NewActiveAgentSessions does not return the AgentSession
	// list directly (only ResetResult which contains it).
	outResults := make([]services.ResetResult, len(results))
	var outSessions []services.AgentSession
	for i, r := range results {
		var as services.AgentSession
		if r.Session != nil {
			as = &agentSessionAdapter{as: r.Session}
			outSessions = append(outSessions, as)
		}
		outResults[i] = services.ResetResult{
			Agent:       r.Agent,
			Cwd:         r.Cwd,
			BeforeState: statusString(r.BeforeState),
			Action:      r.Action,
			Error:       r.Error,
			Session:     as,
		}
	}
	return matched, outSessions, outResults, nil
}

// WatchMode / SetWatchMode / ThinkMode / SetThinkMode /
// ToolsMode / SetToolsMode — passthrough (types are type
// aliases of registry/agent canonical types so no conversion
// needed).
func (s *chatSessionAdapter) WatchMode() services.WatchMode    { return s.cs.WatchMode() }
func (s *chatSessionAdapter) SetWatchMode(m services.WatchMode) error { return s.cs.SetWatchMode(m) }
func (s *chatSessionAdapter) ThinkMode() services.ThinkMode    { return s.cs.ThinkMode() }
func (s *chatSessionAdapter) SetThinkMode(m services.ThinkMode) error { return s.cs.SetThinkMode(m) }
func (s *chatSessionAdapter) ToolsMode() services.ToolsMode    { return s.cs.ToolsMode() }
func (s *chatSessionAdapter) SetToolsMode(m services.ToolsMode) error { return s.cs.SetToolsMode(m) }

// agentSessionAdapter wraps *chatsession.AgentSession as
// services.AgentSession. Method surface is minimal — only what
// commands actually call (per F-51 doc §1.2.4).
type agentSessionAdapter struct {
	as *chatsession.AgentSession
}

// Agent / Cwd are FIELDS on AgentSession, not methods.
// PID() / Handle() are methods.
func (a *agentSessionAdapter) Agent() string   { return a.as.Agent }
func (a *agentSessionAdapter) Cwd() string     { return a.as.Cwd }
func (a *agentSessionAdapter) PID() int        { return a.as.PID() }
func (a *agentSessionAdapter) IsRunning() bool { return a.as.Handle() != nil }
func (a *agentSessionAdapter) ResetCumulative() { a.as.ResetCumulative() }

// statusString converts chatsession.Status (a type alias of
// registry.Status, an int enum) to its canonical string for
// the services.KillResult.BeforeState / services.ResetResult
// .BeforeState fields. The services package is type-pure; it
// doesn't depend on chatsession, so this conversion happens
// in cmd/nightme.
func statusString(s chatsession.Status) string {
	switch s {
	case chatsession.StatusRunning:
		return "running"
	case chatsession.StatusDetached:
		return "detached"
	case chatsession.StatusExited:
		return "exited"
	}
	return ""
}

// Compile-time checks.
var (
	_ services.SessionService = (*sessionAdapter)(nil)
	_ services.Session        = (*chatSessionAdapter)(nil)
	_ services.AgentSession   = (*agentSessionAdapter)(nil)
)

// Compile-time check: services.WatchMode / ThinkMode / ToolsMode
// are alias-compatible with the chatsession / agent canonical
// types.
var (
	_ services.WatchMode = registry.WatchModeAll
)
