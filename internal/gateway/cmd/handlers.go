// Package cmd hosts the slash-command handlers that power nightme.
//
// v1.1 (F-26 §6 commit 3): the legacy SessionManager interface has
// been removed. Handlers depend on gateway.Gateway directly:
//   - /cwd:    lookup-or-bind via Gateway.LookupByChat + manager.Register
//   - /run:    spawn via Gateway.SpawnAgent
//   - /kill:   manager.Kill(binding.SessionID)
//
// The gateway's binding table is the source of truth for chat →
// session. Cmd/nightme (the runtime) sets up the manager and
// registers the default commands at startup.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/gateway"
	"github.com/cnlangzi/nightme/internal/session"
)

// ErrChatAlreadyBound is returned by /cwd when a session is already
// running for the requested chat. /kill first.
var ErrChatAlreadyBound = errors.New("session: chat already bound to an existing session")

// Session is the abstract handle returned by
// gateway.LookupSessionByChat — concrete type *session.Session.
type Session = *session.Session

// Responder is the surface handlers use to push replies back to
// the user. Channel adapters implement it via the runtime's
// channelResponder wrapper.
type Responder interface {
	Reply(ctx context.Context, chatID, text string) error
}

// handlerContext bundles the dependencies every handler closes
// over. Built once at runtime startup and shared by every
// dispatch.
type handlerContext struct {
	gw        gateway.Gateway
	agents    *agent.Registry
	responder Responder
}

// handler returns a bound gateway.Command handler that closes over
// the handlerContext's dependencies. Every command on the
// registry follows this pattern.
func (hc *handlerContext) reply(ctx context.Context, chatID, text string) *gateway.CommandResult {
	_ = hc.trySendReply(ctx, chatID, text)
	return &gateway.CommandResult{Reply: text, Consumed: true}
}

// trySendReply is best-effort: nil responder degrades to a no-op so
// tests can skip it.
func (hc *handlerContext) trySendReply(ctx context.Context, chatID, text string) error {
	if hc.responder == nil {
		return nil
	}
	return hc.responder.Reply(ctx, chatID, text)
}

// RegisterDefaultCommands wires the nightme slash command set onto
// gw: /cwd, /run, /kill, /help, /agents.
//
// gw must already have its binding table wired (use New(fallback, mgr)
// at construction time) — commands consult gw.LookupByChat.
func RegisterDefaultCommands(gw gateway.Gateway, agents *agent.Registry, responder Responder) {
	hc := &handlerContext{gw: gw, agents: agents, responder: responder}
	gw.Register(gateway.Command{
		Name:        "cwd",
		Aliases:     []string{"workspace"},
		Description: "Set workspace (session-level)",
		Handler:     hc.cwd,
	})
	gw.Register(gateway.Command{
		Name:        "run",
		Description: "Ensure CLI running (spawn or attach)",
		Handler:     hc.run,
	})
	gw.Register(gateway.Command{
		Name:        "kill",
		Description: "Stop current CLI (keep session)",
		Handler:     hc.kill,
	})
	gw.Register(gateway.Command{
		Name:        "help",
		Aliases:     []string{"?"},
		Description: "Show this help",
		Handler:     hc.help,
	})
	gw.Register(gateway.Command{
		Name:        "agents",
		Description: "List registered agents",
		Handler:     hc.listAgents,
	})
}

// cwd handles `/cwd [path]`. With no path it returns the current
// session's workspace (or a hint when none is bound). With a path
// it validates the directory and binds the chat to a session with
// that workspace. An already-running session is rejected on the
// bind path so the user explicitly /kill first.
//
// v1.1: this handler goes through Gateway.LookupByChat and
// Gateway.SpawnAgent, NOT a separate SessionManager interface.
func (h *handlerContext) cwd(ctx context.Context, msg *gateway.InboundMessage, args []string) (*gateway.CommandResult, error) {
	if len(args) == 0 {
		binding := h.gw.LookupByChat(msg.ChatID)
		if binding == nil {
			return h.reply(ctx, msg.ChatID, "no workspace set. Send /cwd <path> to bind one."), nil
		}
		return h.reply(ctx, msg.ChatID, fmt.Sprintf("current workspace: %s", binding.Workspace)), nil
	}
	path := args[0]
	if strings.HasPrefix(path, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	} else if !filepath.IsAbs(path) {
		home, err := os.UserHomeDir()
		if err != nil {
			return h.reply(ctx, msg.ChatID, fmt.Sprintf("/cwd: resolve home directory: %v", err)), nil
		}
		path = filepath.Join(home, path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return h.reply(ctx, msg.ChatID, fmt.Sprintf("/cwd: %v", err)), nil
	}
	info, err := os.Stat(abs)
	if err != nil {
		return h.reply(ctx, msg.ChatID, fmt.Sprintf("/cwd: workspace not found: %v", err)), nil
	}
	if !info.IsDir() {
		return h.reply(ctx, msg.ChatID, fmt.Sprintf("/cwd: not a directory: %s", abs)), nil
	}

	// Reject if a session is already running for this chat.
	if binding := h.gw.LookupByChat(msg.ChatID); binding != nil {
		if sess, err := h.gw.LookupSessionByChat(msg.ChatID); err == nil && sess != nil {
			if sess.Status() == session.StatusRunning {
				return h.reply(ctx, msg.ChatID, "session already active, /kill first"), nil
			}
		}
	}

	// Pick the agent name: existing binding's agent, else the
	// first registered agent, else the legacy default "claude".
	agentName := ""
	if binding := h.gw.LookupByChat(msg.ChatID); binding != nil && binding.Agent != "" {
		agentName = binding.Agent
	}
	if agentName == "" && h.agents != nil {
		for _, a := range h.agents.List() {
			if a != nil {
				agentName = a.Name()
				break
			}
		}
	}
	if agentName == "" {
		agentName = "claude"
	}

	// Register a fresh session record (StatusDetached). The
	// runtime provides the manager-backed helper via
	// RegisterSessionOps (set up by cmd/nightme at startup).
	sess, err := h.createRegisteredSession(ctx, abs, agentName, nil)
	if err != nil {
		return h.reply(ctx, msg.ChatID, fmt.Sprintf("/cwd: %v", err)), nil
	}
	h.gw.Bind(msg.ChatID, string(msg.ChatType), sess.ID, abs, agentName)

	return h.reply(ctx, msg.ChatID,
		fmt.Sprintf("Workspace set to %s. Send /run <agent> to start CLI.", abs)), nil
}

// run handles `/run <agent> [args...]`. It is a no-op when the
// session already has a live CLI; otherwise it spawns one via
// Gateway.SpawnAgent.
func (h *handlerContext) run(ctx context.Context, msg *gateway.InboundMessage, args []string) (*gateway.CommandResult, error) {
	if len(args) == 0 {
		return h.reply(ctx, msg.ChatID, "usage: /run <agent> [args...]"), nil
	}
	agentName := args[0]
	extra := args[1:]

	if h.agents != nil {
		if _, err := h.agents.Get(agentName); err != nil {
			return h.reply(ctx, msg.ChatID, fmt.Sprintf("unknown agent: %s", agentName)), nil
		}
	}

	sess, err := h.gw.SpawnAgent(ctx, msg.ChatID, agentName, extra)
	if err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			return h.reply(ctx, msg.ChatID, "no workspace set, send /cwd <path> first"), nil
		}
		if errors.Is(err, agent.ErrUnknownAgent) {
			return h.reply(ctx, msg.ChatID, fmt.Sprintf("unknown agent: %s", agentName)), nil
		}
		return h.reply(ctx, msg.ChatID, fmt.Sprintf("/run: %v", err)), nil
	}

	snap := sess.Snapshot()
	text := fmt.Sprintf("Already running (pid=%d). Connected.", snap.PID)
	if err := h.trySendReply(ctx, msg.ChatID, text); err != nil {
		return nil, err
	}
	return &gateway.CommandResult{Consumed: true, Reply: text}, nil
}

// kill handles `/kill`. The session record is preserved so the
// user can /run again to restart.
func (h *handlerContext) kill(ctx context.Context, msg *gateway.InboundMessage, _ []string) (*gateway.CommandResult, error) {
	sess, err := h.gw.LookupSessionByChat(msg.ChatID)
	if err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			return h.reply(ctx, msg.ChatID, "no session to kill"), nil
		}
		return h.reply(ctx, msg.ChatID, fmt.Sprintf("/kill: %v", err)), nil
	}
	if err := h.killSessionByID(sess.ID); err != nil {
		return h.reply(ctx, msg.ChatID, fmt.Sprintf("/kill: %v", err)), nil
	}
	if err := h.trySendReply(ctx, msg.ChatID, "session killed"); err != nil {
		return nil, err
	}
	return &gateway.CommandResult{Consumed: true, Reply: "session killed"}, nil
}

// agents handles `/agents`.
func (h *handlerContext) listAgents(ctx context.Context, msg *gateway.InboundMessage, _ []string) (*gateway.CommandResult, error) {
	text := renderAgents(h.agents)
	if err := h.trySendReply(ctx, msg.ChatID, text); err != nil {
		return nil, err
	}
	return &gateway.CommandResult{Consumed: true, Reply: text}, nil
}

// help handles `/help`.
func (h *handlerContext) help(ctx context.Context, msg *gateway.InboundMessage, _ []string) (*gateway.CommandResult, error) {
	gw, ok := gwFromContext(ctx)
	if !ok {
		return h.reply(ctx, msg.ChatID, "help: gateway unavailable"), nil
	}
	text := renderHelp(gw.ListCommands())
	if err := h.trySendReply(ctx, msg.ChatID, text); err != nil {
		return nil, err
	}
	return &gateway.CommandResult{Consumed: true, Reply: text}, nil
}

// renderAgents + renderHelp + helper functions.

func renderAgents(reg *agent.Registry) string {
	if reg == nil {
		return "no agents registered"
	}
	agents := reg.List()
	if len(agents) == 0 {
		return "no agents registered"
	}
	var b strings.Builder
	b.WriteString("Registered agents:\n")
	for _, a := range agents {
		if a == nil {
			continue
		}
		name := a.Name()
		cmd := a.Command()
		args := a.Args()
		fmt.Fprintf(&b, "• %s", name)
		if cmd != "" {
			fmt.Fprintf(&b, " — %s", cmd)
		}
		if len(args) > 0 {
			fmt.Fprintf(&b, " %s", strings.Join(args, " "))
		}
		b.WriteString("\n")
	}
	b.WriteString("\nUse /run [name]. Omit name to use the configured default.")
	return b.String()
}

func renderHelp(cmds []gateway.Command) string {
	var b strings.Builder
	b.WriteString("Available commands:\n")
	for _, c := range cmds {
		fmt.Fprintf(&b, "/%s", c.Name)
		if c.Description != "" {
			fmt.Fprintf(&b, " — %s", c.Description)
		}
		b.WriteString("\n")
	}
	b.WriteString("\nWorkflow:\n")
	b.WriteString("  1. /cwd <path>\n")
	b.WriteString("  2. /run <agent>\n")
	b.WriteString("  3. ... work ...\n")
	b.WriteString("  4. /kill    (or restart with /run again)\n")
	b.WriteString("\nAnything else (including unknown /-commands) is sent to the agent.\n")
	return b.String()
}

// gwFromContext extracts the Gateway from the context. The daemon
// is expected to install it via WithGateway before serving the
// chat loop.
func gwFromContext(ctx context.Context) (gateway.Gateway, bool) {
	if ctx == nil {
		return nil, false
	}
	gw, ok := ctx.Value(gateway.GatewayKey{}).(gateway.Gateway)
	return gw, ok
}

// WithGateway installs gw into ctx so handlers that need it
// (currently just /help) can recover it without taking it as a
// closure.
func WithGateway(ctx context.Context, gw gateway.Gateway) context.Context {
	return gateway.WithGateway(ctx, gw)
}

// createRegisteredSession and killSessionByID are the v1.1 thin
// shims over the session.Manager — they exist so the handlers
// don't have to depend on session.Manager directly. The runtime
// registers them via RegisterSessionOps at startup.
type sessionOps struct {
	createRegistered func(ctx context.Context, workspace, agentName string, args []string) (*session.Session, error)
	killByID         func(sid string) error
}

var globalSessionOps sessionOps

// RegisterSessionOps lets the runtime inject the manager-backed
// helpers the slash commands need (register a detached session,
// kill by ID). Called once at startup; idempotent on subsequent
// calls.
func RegisterSessionOps(create func(ctx context.Context, workspace, agentName string, args []string) (*session.Session, error), kill func(sid string) error) {
	globalSessionOps = sessionOps{createRegistered: create, killByID: kill}
}

func (h *handlerContext) createRegisteredSession(ctx context.Context, workspace, agentName string, args []string) (*session.Session, error) {
	if globalSessionOps.createRegistered == nil {
		return nil, errors.New("session ops not registered (runtime bug)")
	}
	return globalSessionOps.createRegistered(ctx, workspace, agentName, args)
}

func (h *handlerContext) killSessionByID(sid string) error {
	if globalSessionOps.killByID == nil {
		return errors.New("session ops not registered (runtime bug)")
	}
	return globalSessionOps.killByID(sid)
}